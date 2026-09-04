package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
	"github.com/fraggerfox/zulip-irc-bridge/internal/router"
	"github.com/fraggerfox/zulip-irc-bridge/internal/zulip"
)

func testRouter() *router.Router {
	return router.New(&config.Config{
		Zulip: config.Zulip{Email: "bot@example.com"},
		Mappings: []config.Mapping{
			{Channel: "#chan", Stream: "irc-chan", Topic: "t", Direction: config.Both},
		},
		Bridge: config.Bridge{
			ZulipMessageFormat: "{nick}: {content}",
			IRCMessageFormat:   "<{name}> {content}",
		},
	})
}

type fakeZulipSender struct {
	mu    sync.Mutex
	sent  []string
	fails int // fail this many sends before succeeding
}

func (f *fakeZulipSender) SendWithRetry(ctx context.Context, stream, topic, content string, attempts int, base time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("send failed")
	}
	f.sent = append(f.sent, stream+"/"+topic+": "+content)
	return nil
}

func (f *fakeZulipSender) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

type fakeIRCWriter struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeIRCWriter) Privmsg(ch, line string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, "msg "+ch+" "+line)
	return nil
}

func (f *fakeIRCWriter) Action(ch, line string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, "action "+ch+" "+line)
	return nil
}

func (f *fakeIRCWriter) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPumpToZulipForwardsAndCounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan router.ToZulip, 4)
	sender := &fakeZulipSender{}
	c := &Counters{}
	go pumpToZulip(ctx, ch, sender, c, slog.Default())

	ch <- router.ToZulip{Stream: "s", Topic: "t", Content: "hello"}
	waitFor(t, "forward", func() bool { return c.ForwardedToZulip.Load() == 1 })
	if got := sender.all(); len(got) != 1 || got[0] != "s/t: hello" {
		t.Errorf("sent = %q", got)
	}
}

func TestPumpToZulipDropsAfterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan router.ToZulip, 4)
	sender := &fakeZulipSender{fails: 1}
	c := &Counters{}
	go pumpToZulip(ctx, ch, sender, c, slog.Default())

	ch <- router.ToZulip{Stream: "s", Topic: "t", Content: "doomed"}
	ch <- router.ToZulip{Stream: "s", Topic: "t", Content: "fine"}
	waitFor(t, "drop+forward", func() bool {
		return c.DroppedToZulip.Load() == 1 && c.ForwardedToZulip.Load() == 1
	})
}

func TestPumpToIRCPacingAndActions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan router.ToIRC, 4)
	w := &fakeIRCWriter{}
	c := &Counters{}
	go pumpToIRC(ctx, ch, w, c, slog.Default(), time.Millisecond)

	ch <- router.ToIRC{Channel: "#chan", Lines: []string{"one", "two"}}
	ch <- router.ToIRC{Channel: "#chan", Lines: []string{"waves"}, Action: true}
	waitFor(t, "three lines", func() bool { return c.ForwardedToIRC.Load() == 3 })

	got := w.all()
	want := []string{"msg #chan one", "msg #chan two", "action #chan waves"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// fakePoller scripts a sequence of GetMessages results.
type fakePoller struct {
	mu        sync.Mutex
	registers int
	polls     int
	script    []pollResult
}

type pollResult struct {
	msgs []zulip.Message
	err  error
}

func (f *fakePoller) RegisterQueue(ctx context.Context) (zulip.Queue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers++
	return zulip.Queue{ID: fmt.Sprintf("q%d", f.registers)}, nil
}

func (f *fakePoller) GetMessages(ctx context.Context, q *zulip.Queue) ([]zulip.Message, error) {
	f.mu.Lock()
	if f.polls < len(f.script) {
		res := f.script[f.polls]
		f.polls++
		f.mu.Unlock()
		return res.msgs, res.err
	}
	f.mu.Unlock()
	<-ctx.Done() // steady state: block like a long-poll
	return nil, ctx.Err()
}

func (f *fakePoller) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers, f.polls
}

func streamMsg(content string) zulip.Message {
	return zulip.Message{
		Type:           "stream",
		RawRecipient:   []byte(`"irc-chan"`),
		Topic:          "t",
		SenderEmail:    "user@example.com",
		SenderFullName: "User",
		Content:        content,
	}
}

func TestPollZulipRoutesAndReregisters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &fakePoller{script: []pollResult{
		{msgs: []zulip.Message{streamMsg("first")}},
		{err: zulip.ErrBadQueue},
		{msgs: []zulip.Message{streamMsg("second")}},
	}}
	toIRC := make(chan router.ToIRC, 8)
	c := &Counters{}
	go pollZulip(ctx, p, testRouter(), toIRC, c, slog.Default())

	var got []router.ToIRC
	for len(got) < 2 {
		select {
		case m := <-toIRC:
			got = append(got, m)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out; got %d messages", len(got))
		}
	}
	if got[0].Lines[0] != "<User> first" || got[1].Lines[0] != "<User> second" {
		t.Errorf("messages = %+v", got)
	}
	regs, _ := p.counts()
	if regs != 2 {
		t.Errorf("registers = %d, want 2 (re-register after ErrBadQueue)", regs)
	}
}

func TestPollZulipDropsWhenQueueFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &fakePoller{script: []pollResult{
		{msgs: []zulip.Message{streamMsg("a"), streamMsg("b")}},
	}}
	toIRC := make(chan router.ToIRC, 1) // room for one only
	c := &Counters{}
	go pollZulip(ctx, p, testRouter(), toIRC, c, slog.Default())

	waitFor(t, "drop", func() bool { return c.DroppedToIRC.Load() == 1 })
	if len(toIRC) != 1 {
		t.Errorf("queue len = %d, want 1", len(toIRC))
	}
}

func TestSleepCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleep(ctx, time.Hour) {
		t.Error("cancelled sleep must report false")
	}
	if !sleep(context.Background(), time.Millisecond) {
		t.Error("elapsed sleep must report true")
	}
}
