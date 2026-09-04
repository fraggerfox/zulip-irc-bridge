// Package ircx wraps the ergochat/irc-go client with the bridge's
// connection policy: TLS by default, SASL PLAIN with independent
// credentials that hard-fails when rejected, multi-channel join with
// rejoin on kick, and reconnect pacing that never hammers the server
// (the production Libera ban lesson).
package ircx

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

// InboundHandler receives channel messages from IRC.
// action is true for CTCP ACTION (/me) messages.
type InboundHandler func(channel, nick, content string, action bool)

// rejoinDelay is how long to wait before rejoining after a kick;
// a variable so tests can shorten it.
var rejoinDelay = 5 * time.Second

type Client struct {
	conn     *ircevent.Connection
	channels []string
	log      *slog.Logger
}

// NickOf extracts the nick from an IRC source prefix
// ("nick!user@host" or just "nick").
func NickOf(source string) string {
	nick, _, _ := strings.Cut(source, "!")
	return nick
}

// New builds a configured (but unconnected) IRC client that joins the
// given channels and delivers inbound channel messages to onMessage.
func New(cfg config.IRC, channels []string, onMessage InboundHandler, log *slog.Logger) *Client {
	conn := &ircevent.Connection{
		Server:       fmt.Sprintf("%s:%d", cfg.Server, cfg.Port),
		Nick:         cfg.Nick,
		User:         cfg.Nick,
		RealName:     cfg.Realname,
		UseTLS:       cfg.TLSEnabled(),
		QuitMessage:  "bridge shutting down",
		ReconnectFreq: 30 * time.Second,
		KeepAlive:    2 * time.Minute,
		Timeout:      30 * time.Second,
	}
	if cfg.TLSEnabled() {
		conn.TLSConfig = &tls.Config{ServerName: cfg.Server}
	}
	if cfg.SASL.Enabled {
		conn.UseSASL = true
		conn.SASLLogin = cfg.SASL.Username
		conn.SASLPassword = cfg.SASL.Password
		// SASLOptional stays false: if SASL fails, abort the
		// connection instead of silently continuing unauthenticated.
	}

	c := &Client{conn: conn, channels: channels, log: log}

	conn.AddConnectCallback(func(e ircmsg.Message) {
		log.Info("irc connected", "server", conn.Server, "nick", conn.CurrentNick())
		for _, ch := range channels {
			conn.Join(ch)
		}
	})
	conn.AddCallback("KICK", func(e ircmsg.Message) {
		if len(e.Params) >= 2 && e.Params[1] == conn.CurrentNick() {
			ch := e.Params[0]
			log.Warn("kicked from channel, rejoining", "channel", ch)
			time.AfterFunc(rejoinDelay, func() { conn.Join(ch) })
		}
	})
	conn.AddCallback("PRIVMSG", func(e ircmsg.Message) {
		if len(e.Params) < 2 {
			return
		}
		target, content := e.Params[0], e.Params[1]
		if !strings.HasPrefix(target, "#") {
			return // direct message to the bot; not bridged
		}
		nick := NickOf(e.Source)
		if action, ok := parseAction(content); ok {
			onMessage(target, nick, action, true)
		} else {
			onMessage(target, nick, content, false)
		}
	})

	return c
}

// parseAction unwraps a CTCP ACTION ("\x01ACTION waves\x01").
func parseAction(content string) (string, bool) {
	const prefix = "\x01ACTION "
	if strings.HasPrefix(content, prefix) && strings.HasSuffix(content, "\x01") {
		return strings.TrimSuffix(strings.TrimPrefix(content, prefix), "\x01"), true
	}
	return "", false
}

// Connect establishes the connection and starts the read loop in a
// background goroutine (ircevent reconnects internally at
// ReconnectFreq until Quit is called).
func (c *Client) Connect() error {
	if err := c.conn.Connect(); err != nil {
		return fmt.Errorf("irc connect: %w", err)
	}
	go c.conn.Loop()
	return nil
}

// CurrentNick returns the nick actually in use (it can differ from the
// configured one after a collision fallback).
func (c *Client) CurrentNick() string {
	return c.conn.CurrentNick()
}

// Privmsg sends a message line to a channel.
func (c *Client) Privmsg(channel, line string) error {
	return c.conn.Privmsg(channel, line)
}

// Action sends a CTCP ACTION (/me) line to a channel.
func (c *Client) Action(channel, line string) error {
	return c.conn.Action(channel, line)
}

// Quit disconnects cleanly and stops the reconnect loop.
func (c *Client) Quit() {
	c.conn.Quit()
}
