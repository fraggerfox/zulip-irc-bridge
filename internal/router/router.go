// Package router decides where messages go and what they look like when
// they get there. It is pure logic — no sockets, no clients — so the
// bridging semantics (mapping resolution, direction filters, loop
// prevention, formatting) are exhaustively testable.
package router

import (
	"strconv"
	"strings"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

// maxIRCLines bounds how many IRC lines a single Zulip message may
// expand to; anything longer is truncated with a notice. IRC has no
// message framing, and relaying a pasted 200-line log would both flood
// the channel and get the bot killed by the server.
const maxIRCLines = 8

// ToZulip is a message bound for a Zulip stream/topic.
type ToZulip struct {
	Stream  string
	Topic   string
	Content string
}

// ToIRC is a message bound for an IRC channel. When Action is true the
// lines should be sent as CTCP ACTION (/me).
type ToIRC struct {
	Channel string
	Lines   []string
	Action  bool
}

type Router struct {
	byChannel     map[string]config.Mapping
	byStreamTopic map[string]config.Mapping
	zulipFormat   string
	ircFormat     string
	ignoreNicks   map[string]bool
	botEmail      string
}

func key(stream, topic string) string {
	return strings.ToLower(stream) + "\x00" + strings.ToLower(topic)
}

func New(cfg *config.Config) *Router {
	r := &Router{
		byChannel:     map[string]config.Mapping{},
		byStreamTopic: map[string]config.Mapping{},
		zulipFormat:   cfg.Bridge.ZulipMessageFormat,
		ircFormat:     cfg.Bridge.IRCMessageFormat,
		ignoreNicks:   map[string]bool{},
		botEmail:      strings.ToLower(cfg.Zulip.Email),
	}
	for _, m := range cfg.Mappings {
		r.byChannel[strings.ToLower(m.Channel)] = m
		r.byStreamTopic[key(m.Stream, m.Topic)] = m
	}
	for _, n := range cfg.Bridge.IgnoreNicks {
		r.ignoreNicks[strings.ToLower(n)] = true
	}
	return r
}

// Channels returns every mapped IRC channel (for joining).
func (r *Router) Channels() []string {
	var out []string
	for _, m := range r.byChannel {
		out = append(out, m.Channel)
	}
	return out
}

// FromIRC routes an IRC channel message toward Zulip. ownNick is the
// bot's current IRC nick (which can differ from the configured one
// after a collision fallback). Returns ok=false when the message must
// not be forwarded.
func (r *Router) FromIRC(channel, nick, content string, isAction bool, ownNick string) (ToZulip, bool) {
	if strings.EqualFold(nick, ownNick) || r.ignoreNicks[strings.ToLower(nick)] {
		return ToZulip{}, false
	}
	m, ok := r.byChannel[strings.ToLower(channel)]
	if !ok || m.Direction == config.ZulipToIRC {
		return ToZulip{}, false
	}
	var body string
	if isAction {
		body = "*" + nick + " " + content + "*"
	} else {
		body = strings.ReplaceAll(r.zulipFormat, "{nick}", nick)
		body = strings.ReplaceAll(body, "{content}", content)
	}
	return ToZulip{Stream: m.Stream, Topic: m.Topic, Content: body}, true
}

// FromZulip routes a Zulip stream message toward IRC. Returns ok=false
// when the message must not be forwarded (own echo, unmapped
// stream/topic, direction filter, empty content).
func (r *Router) FromZulip(stream, topic, senderEmail, senderName, content string) (ToIRC, bool) {
	if strings.EqualFold(senderEmail, r.botEmail) {
		return ToIRC{}, false
	}
	m, ok := r.byStreamTopic[key(stream, topic)]
	if !ok || m.Direction == config.IRCToZulip {
		return ToIRC{}, false
	}

	content = strings.TrimRight(content, "\n")
	action := false
	if rest, found := strings.CutPrefix(content, "/me "); found && !strings.Contains(rest, "\n") {
		action = true
		content = senderName + " " + rest
	}

	var lines []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !action {
			formatted := strings.ReplaceAll(r.ircFormat, "{name}", senderName)
			line = strings.ReplaceAll(formatted, "{content}", line)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ToIRC{}, false
	}
	if len(lines) > maxIRCLines {
		kept := lines[:maxIRCLines]
		kept = append(kept, "... ("+strconv.Itoa(len(lines)-maxIRCLines)+" more lines truncated)")
		lines = kept
	}
	return ToIRC{Channel: m.Channel, Lines: lines, Action: action}, true
}
