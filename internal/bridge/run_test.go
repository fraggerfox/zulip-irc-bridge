package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

// minimal fake IRC server (same protocol subset as internal/ircx tests).
type fakeIRC struct {
	ln       net.Listener
	mu       sync.Mutex
	conn     net.Conn
	received []string
	joined   chan string
}

func newFakeIRC(t *testing.T) *fakeIRC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeIRC{ln: ln, joined: make(chan string, 8)}
	go func() {
		conn, err := ln.Accept()
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
			case strings.HasPrefix(line, "NICK "):
				nick = strings.TrimPrefix(line, "NICK ")
			case strings.HasPrefix(line, "USER "):
				fmt.Fprintf(conn, ":test 001 %s :welcome\r\n:test 376 %s :eom\r\n", nick, nick)
			case strings.HasPrefix(line, "JOIN "):
				ch := strings.TrimPrefix(line, "JOIN ")
				fmt.Fprintf(conn, ":%s!u@h JOIN %s\r\n", nick, ch)
				s.joined <- ch
			case strings.HasPrefix(line, "PING"):
				fmt.Fprintf(conn, "PONG%s\r\n", strings.TrimPrefix(line, "PING"))
			}
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeIRC) send(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.conn, "%s\r\n", line)
}

func (s *fakeIRC) sawLine(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.received {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestRunEndToEnd(t *testing.T) {
	ircSrv := newFakeIRC(t)
	ircAddr := ircSrv.ln.Addr().(*net.TCPAddr)

	// Fake Zulip: records sent messages; events long-poll returns one
	// scripted message once, then blocks until request context ends.
	var zmu sync.Mutex
	var sent []string
	eventsServed := false
	zulipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/register":
			fmt.Fprint(w, `{"result":"success","msg":"","queue_id":"q1","last_event_id":-1}`)
		case "/api/v1/events":
			zmu.Lock()
			first := !eventsServed
			eventsServed = true
			zmu.Unlock()
			if first {
				json.NewEncoder(w).Encode(map[string]any{
					"result": "success", "msg": "",
					"events": []map[string]any{{
						"id": 1, "type": "message",
						"message": map[string]any{
							"id": 100, "sender_email": "user@example.com",
							"sender_full_name": "User", "type": "stream",
							"display_recipient": "irc-chan", "subject": "t",
							"content": "from zulip",
						},
					}},
				})
				return
			}
			<-r.Context().Done()
		case "/api/v1/messages":
			r.ParseForm()
			zmu.Lock()
			sent = append(sent, r.PostForm.Get("content"))
			zmu.Unlock()
			fmt.Fprint(w, `{"result":"success","msg":""}`)
		default:
			t.Errorf("unexpected zulip path %s", r.URL.Path)
		}
	}))
	defer zulipSrv.Close()

	tlsOff := false
	cfg := &config.Config{
		Zulip: config.Zulip{Site: zulipSrv.URL, Email: "bot@example.com", APIKey: "k"},
		IRC: config.IRC{
			Server: "127.0.0.1", Port: ircAddr.Port, TLS: &tlsOff,
			Nick: "bridge_bot", Realname: "bridge_bot",
		},
		Mappings: []config.Mapping{
			{Channel: "#chan", Stream: "irc-chan", Topic: "t", Direction: config.Both},
		},
		Bridge: config.Bridge{
			ZulipMessageFormat: "{nick}: {content}",
			IRCMessageFormat:   "<{name}> {content}",
			LogLevel:           "ERROR",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.Default()) }()

	select {
	case <-ircSrv.joined:
	case <-time.After(10 * time.Second):
		t.Fatal("bridge never joined the channel")
	}

	// IRC -> Zulip.
	ircSrv.send(":alice!a@h PRIVMSG #chan :hello zulip")
	waitFor(t, "irc->zulip", func() bool {
		zmu.Lock()
		defer zmu.Unlock()
		return len(sent) == 1 && sent[0] == "alice: hello zulip"
	})

	// Zulip -> IRC (from the scripted event).
	waitFor(t, "zulip->irc", func() bool {
		return ircSrv.sawLine("PRIVMSG #chan :<User> from zulip")
	})

	// Clean shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	waitFor(t, "QUIT on shutdown", func() bool { return ircSrv.sawLine("QUIT") })
}
