// Package bridge wires the IRC client, the Zulip poller and the Zulip
// sender together with channels and supervises them until the context
// is cancelled.
//
// Concurrency layout (one connection, one owner):
//
//	irc goroutine     ircevent loop; inbound handler enqueues to toZulip
//	ircWriter         drains toIRC, writes via the IRC client, paced
//	poller            Zulip long-poll, own client; enqueues to toIRC
//	sender            drains toZulip, own client, bounded retries
//	stats             periodic forwarded/dropped counters
package bridge

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
	"github.com/fraggerfox/zulip-irc-bridge/internal/ircx"
	"github.com/fraggerfox/zulip-irc-bridge/internal/router"
	"github.com/fraggerfox/zulip-irc-bridge/internal/zulip"
)

const (
	queueSize     = 256
	sendAttempts  = 5
	sendBaseDelay = 2 * time.Second
	// ircLinePacing spaces successive outbound IRC lines to stay clear
	// of server flood limits.
	ircLinePacing = 300 * time.Millisecond
	statsInterval = 10 * time.Minute
)

// Counters tracks forwarded and dropped messages per direction.
type Counters struct {
	ForwardedToZulip atomic.Int64
	ForwardedToIRC   atomic.Int64
	DroppedToZulip   atomic.Int64
	DroppedToIRC     atomic.Int64
}

// ircWriter is the subset of the IRC client the outbound pump needs.
type ircWriter interface {
	Privmsg(channel, line string) error
	Action(channel, line string) error
}

// zulipSender is the subset of the Zulip client the sender pump needs.
type zulipSender interface {
	SendWithRetry(ctx context.Context, stream, topic, content string, attempts int, base time.Duration) error
}

// zulipPoller is the subset of the Zulip client the poll loop needs.
type zulipPoller interface {
	RegisterQueue(ctx context.Context) (zulip.Queue, error)
	GetMessages(ctx context.Context, q *zulip.Queue) ([]zulip.Message, error)
}

// Run starts the bridge and blocks until ctx is cancelled or the IRC
// connection cannot be established at all.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	r := router.New(cfg)
	counters := &Counters{}

	toZulip := make(chan router.ToZulip, queueSize)
	toIRC := make(chan router.ToIRC, queueSize)

	var irc *ircx.Client
	irc = ircx.New(cfg.IRC, r.Channels(), func(channel, nick, content string, action bool) {
		msg, ok := r.FromIRC(channel, nick, content, action, irc.CurrentNick())
		if !ok {
			return
		}
		select {
		case toZulip <- msg:
		default:
			counters.DroppedToZulip.Add(1)
			log.Warn("toZulip queue full, dropping message", "channel", channel)
		}
	}, log)

	if err := irc.Connect(); err != nil {
		return err
	}
	defer irc.Quit()
	notifyReady(log)

	var wg sync.WaitGroup
	run := func(name string, f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
			log.Debug("goroutine exited", "name", name)
		}()
	}

	sender := zulip.New(cfg.Zulip.Site, cfg.Zulip.Email, cfg.Zulip.APIKey)
	run("sender", func() { pumpToZulip(ctx, toZulip, sender, counters, log) })

	pollClient := zulip.New(cfg.Zulip.Site, cfg.Zulip.Email, cfg.Zulip.APIKey)
	run("poller", func() { pollZulip(ctx, pollClient, r, toIRC, counters, log) })

	run("ircWriter", func() { pumpToIRC(ctx, toIRC, irc, counters, log, ircLinePacing) })

	run("stats", func() { logStats(ctx, counters, log, statsInterval) })

	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()
	return nil
}

// pumpToZulip drains the toZulip channel into the Zulip API with
// bounded retries; a message that keeps failing is logged and dropped.
func pumpToZulip(ctx context.Context, ch <-chan router.ToZulip, sender zulipSender, c *Counters, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ch:
			err := sender.SendWithRetry(ctx, m.Stream, m.Topic, m.Content, sendAttempts, sendBaseDelay)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				c.DroppedToZulip.Add(1)
				log.Error("giving up on message to zulip", "stream", m.Stream, "err", err)
				continue
			}
			c.ForwardedToZulip.Add(1)
		}
	}
}

// pumpToIRC drains the toIRC channel onto the IRC connection, pacing
// lines to stay clear of flood limits.
func pumpToIRC(ctx context.Context, ch <-chan router.ToIRC, w ircWriter, c *Counters, log *slog.Logger, pacing time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ch:
			for i, line := range m.Lines {
				if i > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(pacing):
					}
				}
				var err error
				if m.Action {
					err = w.Action(m.Channel, line)
				} else {
					err = w.Privmsg(m.Channel, line)
				}
				if err != nil {
					c.DroppedToIRC.Add(1)
					log.Error("failed to send to irc", "channel", m.Channel, "err", err)
					continue
				}
				c.ForwardedToIRC.Add(1)
			}
		}
	}
}

// pollBackoffBase is the initial poll-failure backoff; a variable so
// tests can shorten it.
var pollBackoffBase = 5 * time.Second

// pollZulip long-polls the Zulip event queue, re-registering when the
// queue expires and backing off on transport errors.
func pollZulip(ctx context.Context, p zulipPoller, r *router.Router, toIRC chan<- router.ToIRC, c *Counters, log *slog.Logger) {
	backoff := pollBackoffBase
	const maxBackoff = 5 * time.Minute

	var queue *zulip.Queue
	for {
		if ctx.Err() != nil {
			return
		}
		if queue == nil {
			q, err := p.RegisterQueue(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("registering event queue failed, backing off", "delay", backoff, "err", err)
				if !sleep(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			queue = &q
			backoff = pollBackoffBase
			log.Info("zulip event queue registered", "queue", q.ID)
		}

		msgs, err := p.GetMessages(ctx, queue)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, zulip.ErrBadQueue) {
				log.Warn("event queue expired, re-registering")
				queue = nil
				continue
			}
			log.Warn("event poll failed, backing off", "delay", backoff, "err", err)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = pollBackoffBase

		for _, msg := range msgs {
			out, ok := r.FromZulip(msg.Stream(), msg.Topic, msg.SenderEmail, msg.SenderFullName, msg.Content)
			if !ok {
				continue
			}
			select {
			case toIRC <- out:
			default:
				c.DroppedToIRC.Add(1)
				log.Warn("toIRC queue full, dropping message", "stream", msg.Stream())
			}
		}
	}
}

func logStats(ctx context.Context, c *Counters, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Info("bridge stats",
				"forwarded_to_zulip", c.ForwardedToZulip.Load(),
				"forwarded_to_irc", c.ForwardedToIRC.Load(),
				"dropped_to_zulip", c.DroppedToZulip.Load(),
				"dropped_to_irc", c.DroppedToIRC.Load(),
			)
		}
	}
}

// sleep waits for d or until ctx is done; reports whether the full
// duration elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// notifyReady tells systemd (Type=notify) the bridge is up. Silently a
// no-op outside systemd.
func notifyReady(log *slog.Logger) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	// The socket path comes from systemd's NOTIFY_SOCKET contract.
	conn, err := net.Dial("unixgram", sock) //nolint:gosec,noctx // local fire-and-forget datagram; path is trusted per sd_notify(3)
	if err != nil {
		log.Warn("sd_notify failed", "err", err)
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		log.Warn("sd_notify write failed", "err", err)
	}
}
