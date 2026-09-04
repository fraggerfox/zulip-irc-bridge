package bridge

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fraggerfox/zulip-irc-bridge/internal/router"
	"github.com/fraggerfox/zulip-irc-bridge/internal/zulip"
)

// erroringIRCWriter fails every send.
type erroringIRCWriter struct{}

func (erroringIRCWriter) Privmsg(_, _ string) error { return errors.New("io broken") }
func (erroringIRCWriter) Action(_, _ string) error  { return errors.New("io broken") }

func TestPumpToIRCCountsSendFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan router.ToIRC, 2)
	c := &Counters{}
	go pumpToIRC(ctx, ch, erroringIRCWriter{}, c, slog.Default(), time.Millisecond)

	ch <- router.ToIRC{Channel: "#c", Lines: []string{"a"}}
	ch <- router.ToIRC{Channel: "#c", Lines: []string{"b"}, Action: true}
	waitFor(t, "two drops", func() bool { return c.DroppedToIRC.Load() == 2 })
	if c.ForwardedToIRC.Load() != 0 {
		t.Errorf("forwarded = %d, want 0", c.ForwardedToIRC.Load())
	}
}

// failingPoller fails registration and polling a scripted number of
// times before succeeding.
type failingPoller struct {
	mu            sync.Mutex
	registerFails int
	pollFails     int
	registers     int
}

func (f *failingPoller) RegisterQueue(ctx context.Context) (zulip.Queue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerFails > 0 {
		f.registerFails--
		return zulip.Queue{}, errors.New("register down")
	}
	f.registers++
	return zulip.Queue{ID: "q"}, nil
}

func (f *failingPoller) GetMessages(ctx context.Context, q *zulip.Queue) ([]zulip.Message, error) {
	f.mu.Lock()
	if f.pollFails > 0 {
		f.pollFails--
		f.mu.Unlock()
		return nil, errors.New("transport down")
	}
	f.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *failingPoller) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers
}

func TestPollZulipBacksOffOnFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	old := pollBackoffBase
	pollBackoffBase = 10 * time.Millisecond
	defer func() { pollBackoffBase = old }()

	p := &failingPoller{registerFails: 1, pollFails: 1}
	toIRC := make(chan router.ToIRC, 1)
	c := &Counters{}

	done := make(chan struct{})
	go func() {
		pollZulip(ctx, p, testRouter(), toIRC, c, slog.Default())
		close(done)
	}()

	// register fails once -> backoff -> register succeeds; then one
	// poll failure -> backoff -> steady-state blocking poll.
	waitFor(t, "successful register after failure", func() bool {
		return p.registerCount() == 1
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollZulip did not exit on cancel")
	}
}

func TestLogStatsTicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		logStats(ctx, &Counters{}, slog.Default(), 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // let a few ticks fire
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("logStats did not exit on cancel")
	}
}

func TestNotifyReadySendsDatagram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no AF_UNIX datagram sockets on windows")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "notify.sock")
	addr, err := net.ResolveUnixAddr("unixgram", sock)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", sock)
	notifyReady(slog.Default())

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading datagram: %v", err)
	}
	if string(buf[:n]) != "READY=1" {
		t.Errorf("datagram = %q, want READY=1", buf[:n])
	}
}

func TestNotifyReadyNoSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	notifyReady(slog.Default()) // must not panic or block
	os.Unsetenv("NOTIFY_SOCKET")
}

func TestNotifyReadyBadSocketLogsAndContinues(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/nonexistent/notify.sock")
	notifyReady(slog.Default()) // must not panic
}
