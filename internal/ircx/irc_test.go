package ircx

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

func TestNickOf(t *testing.T) {
	cases := map[string]string{
		"alice!user@host": "alice",
		"alice":           "alice",
		"":                "",
	}
	for in, want := range cases {
		if got := NickOf(in); got != want {
			t.Errorf("NickOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseAction(t *testing.T) {
	if got, ok := parseAction("\x01ACTION waves\x01"); !ok || got != "waves" {
		t.Errorf("parseAction = %q, %v", got, ok)
	}
	if _, ok := parseAction("plain message"); ok {
		t.Error("plain message is not an action")
	}
	if _, ok := parseAction("\x01VERSION\x01"); ok {
		t.Error("other CTCP is not an action")
	}
}

// fakeIRCServer speaks just enough IRC for the client handshake and
// records everything the client sends.
type fakeIRCServer struct {
	ln       net.Listener
	mu       sync.Mutex
	received []string
	conn     net.Conn
	joined   chan string
}

func newFakeIRCServer(t *testing.T) *fakeIRCServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeIRCServer{ln: ln, joined: make(chan string, 8)}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeIRCServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	var nick string
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		s.mu.Lock()
		s.received = append(s.received, line)
		s.mu.Unlock()

		switch {
		case strings.HasPrefix(line, "CAP LS"):
			fmt.Fprintf(conn, ":test CAP * LS :\r\n")
		case strings.HasPrefix(line, "NICK "):
			nick = strings.TrimPrefix(line, "NICK ")
		case strings.HasPrefix(line, "USER "):
			// ircevent completes registration at end-of-MOTD, not 001.
			fmt.Fprintf(conn, ":test 001 %s :welcome\r\n:test 376 %s :End of /MOTD\r\n", nick, nick)
		case strings.HasPrefix(line, "JOIN "):
			ch := strings.TrimPrefix(line, "JOIN ")
			fmt.Fprintf(conn, ":%s!u@h JOIN %s\r\n", nick, ch)
			s.joined <- ch
		case strings.HasPrefix(line, "PING"):
			fmt.Fprintf(conn, "PONG %s\r\n", strings.TrimPrefix(line, "PING"))
		}
	}
}

func (s *fakeIRCServer) send(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.conn, "%s\r\n", line)
}

func (s *fakeIRCServer) sawLine(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.received {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientEndToEnd(t *testing.T) {
	srv := newFakeIRCServer(t)
	addr := srv.ln.Addr().(*net.TCPAddr)

	tlsOff := false
	cfg := config.IRC{
		Server:   "127.0.0.1",
		Port:     addr.Port,
		TLS:      &tlsOff,
		Nick:     "bridge_bot",
		Realname: "bridge_bot",
	}

	type inbound struct {
		channel, nick, content string
		action                 bool
	}
	got := make(chan inbound, 8)
	c := New(cfg, []string{"#test"}, func(channel, nick, content string, action bool) {
		got <- inbound{channel, nick, content, action}
	}, slog.Default())

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Quit()

	select {
	case ch := <-srv.joined:
		if ch != "#test" {
			t.Fatalf("joined %q, want #test", ch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for JOIN")
	}

	// Inbound channel message reaches the handler.
	srv.send(":alice!a@h PRIVMSG #test :hello bridge")
	select {
	case m := <-got:
		if m.channel != "#test" || m.nick != "alice" || m.content != "hello bridge" || m.action {
			t.Errorf("inbound = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	// CTCP ACTION is unwrapped.
	srv.send(":alice!a@h PRIVMSG #test :\x01ACTION waves\x01")
	select {
	case m := <-got:
		if !m.action || m.content != "waves" {
			t.Errorf("action inbound = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for action")
	}

	// A direct message to the bot is not bridged.
	srv.send(":bob!b@h PRIVMSG bridge_bot :psst")
	select {
	case m := <-got:
		t.Errorf("DM should not be delivered, got %+v", m)
	case <-time.After(300 * time.Millisecond):
	}

	// Outbound privmsg and action reach the server.
	if err := c.Privmsg("#test", "outgoing line"); err != nil {
		t.Fatalf("Privmsg: %v", err)
	}
	if err := c.Action("#test", "does a thing"); err != nil {
		t.Fatalf("Action: %v", err)
	}
	waitFor(t, "outbound PRIVMSG", func() bool {
		return srv.sawLine("PRIVMSG #test :outgoing line")
	})
	waitFor(t, "outbound ACTION", func() bool {
		return srv.sawLine("PRIVMSG #test :\x01ACTION does a thing\x01")
	})

	if c.CurrentNick() != "bridge_bot" {
		t.Errorf("CurrentNick = %q", c.CurrentNick())
	}
}
