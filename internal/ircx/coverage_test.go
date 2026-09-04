package ircx

import (
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

// TestNewTLSAndSASLConfig exercises the TLS and SASL construction
// branches (no connection is made).
func TestNewTLSAndSASLConfig(t *testing.T) {
	cfg := config.IRC{
		Server:   "irc.libera.chat",
		Port:     6697,
		Nick:     "bridge_bot",
		Realname: "bridge",
		SASL: config.SASL{
			Enabled:  true,
			Username: "account",
			Password: "hunter2",
		},
	}
	c := New(cfg, []string{"#a"}, func(_, _, _ string, _ bool) {}, slog.Default())

	if !c.conn.UseTLS {
		t.Error("TLS should be enabled by default")
	}
	if c.conn.TLSConfig == nil || c.conn.TLSConfig.ServerName != "irc.libera.chat" {
		t.Errorf("TLSConfig = %+v", c.conn.TLSConfig)
	}
	if !c.conn.UseSASL || c.conn.SASLLogin != "account" || c.conn.SASLPassword != "hunter2" {
		t.Errorf("SASL config = %v/%q", c.conn.UseSASL, c.conn.SASLLogin)
	}
	if c.conn.Server != "irc.libera.chat:6697" {
		t.Errorf("server = %q", c.conn.Server)
	}
}

func TestConnectFailure(t *testing.T) {
	tlsOff := false
	cfg := config.IRC{
		Server: "127.0.0.1",
		Port:   1, // nothing listens here
		TLS:    &tlsOff,
		Nick:   "x",
	}
	c := New(cfg, nil, func(_, _, _ string, _ bool) {}, slog.Default())
	err := c.Connect()
	if err == nil {
		c.Quit()
		t.Fatal("want connect error")
	}
	if !strings.Contains(err.Error(), "irc connect") {
		t.Errorf("err = %v", err)
	}
}

func TestKickTriggersRejoin(t *testing.T) {
	oldDelay := rejoinDelay
	rejoinDelay = 50 * time.Millisecond
	defer func() { rejoinDelay = oldDelay }()

	srv := newFakeIRCServer(t)
	addr := srv.ln.Addr().(*net.TCPAddr)

	tlsOff := false
	cfg := config.IRC{
		Server: "127.0.0.1", Port: addr.Port, TLS: &tlsOff,
		Nick: "bridge_bot", Realname: "bridge_bot",
	}
	c := New(cfg, []string{"#test"}, func(_, _, _ string, _ bool) {}, slog.Default())
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Quit()

	<-srv.joined // initial join

	srv.send(":op!o@h KICK #test bridge_bot :begone")
	select {
	case ch := <-srv.joined:
		if ch != "#test" {
			t.Errorf("rejoined %q", ch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rejoin after kick")
	}
}
