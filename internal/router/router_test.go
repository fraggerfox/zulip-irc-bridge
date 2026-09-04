package router

import (
	"strings"
	"testing"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Zulip: config.Zulip{Email: "Bot@Example.com"},
		Mappings: []config.Mapping{
			{Channel: "##Chan", Stream: "irc-chan", Topic: "General Chat", Direction: config.Both},
			{Channel: "#readonly", Stream: "irc-ro", Topic: "t", Direction: config.IRCToZulip},
			{Channel: "#announce", Stream: "irc-ann", Topic: "t", Direction: config.ZulipToIRC},
		},
		Bridge: config.Bridge{
			ZulipMessageFormat: "**{nick}**: {content}",
			IRCMessageFormat:   "<{name}> {content}",
			IgnoreNicks:        []string{"OtherBot"},
		},
	}
}

func TestFromIRCBasic(t *testing.T) {
	r := New(testConfig())
	got, ok := r.FromIRC("##chan", "alice", "hello world", false, "bridge_bot")
	if !ok {
		t.Fatal("want ok")
	}
	if got.Stream != "irc-chan" || got.Topic != "General Chat" {
		t.Errorf("routed to %q/%q", got.Stream, got.Topic)
	}
	if got.Content != "**alice**: hello world" {
		t.Errorf("content = %q", got.Content)
	}
}

func TestFromIRCChannelCaseInsensitive(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromIRC("##CHAN", "alice", "x", false, "me"); !ok {
		t.Error("channel matching should be case-insensitive")
	}
}

func TestFromIRCLoopPrevention(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromIRC("##chan", "Bridge_Bot", "x", false, "bridge_bot"); ok {
		t.Error("own nick must be dropped (case-insensitive)")
	}
	if _, ok := r.FromIRC("##chan", "otherbot", "x", false, "me"); ok {
		t.Error("ignore_nicks must be dropped (case-insensitive)")
	}
}

func TestFromIRCDirectionFilter(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromIRC("#announce", "alice", "x", false, "me"); ok {
		t.Error("zulip_to_irc mapping must not forward IRC messages")
	}
	if _, ok := r.FromIRC("#readonly", "alice", "x", false, "me"); !ok {
		t.Error("irc_to_zulip mapping must forward IRC messages")
	}
	if _, ok := r.FromIRC("#unmapped", "alice", "x", false, "me"); ok {
		t.Error("unmapped channel must not forward")
	}
}

func TestFromIRCAction(t *testing.T) {
	r := New(testConfig())
	got, ok := r.FromIRC("##chan", "alice", "waves", true, "me")
	if !ok {
		t.Fatal("want ok")
	}
	if got.Content != "*alice waves*" {
		t.Errorf("action content = %q", got.Content)
	}
}

func TestFromZulipBasic(t *testing.T) {
	r := New(testConfig())
	got, ok := r.FromZulip("irc-chan", "general chat", "user@example.com", "Alice", "hi there")
	if !ok {
		t.Fatal("want ok")
	}
	if got.Channel != "##Chan" {
		t.Errorf("channel = %q", got.Channel)
	}
	if len(got.Lines) != 1 || got.Lines[0] != "<Alice> hi there" {
		t.Errorf("lines = %q", got.Lines)
	}
	if got.Action {
		t.Error("not an action")
	}
}

func TestFromZulipOwnEchoDropped(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromZulip("irc-chan", "general chat", "bot@example.COM", "Bot", "echo"); ok {
		t.Error("bot's own messages must be dropped (case-insensitive)")
	}
}

func TestFromZulipTopicMismatchDropped(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromZulip("irc-chan", "other topic", "u@e.com", "U", "x"); ok {
		t.Error("message in unmapped topic must not forward")
	}
}

func TestFromZulipDirectionFilter(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromZulip("irc-ro", "t", "u@e.com", "U", "x"); ok {
		t.Error("irc_to_zulip mapping must not forward Zulip messages")
	}
	if _, ok := r.FromZulip("irc-ann", "t", "u@e.com", "U", "x"); !ok {
		t.Error("zulip_to_irc mapping must forward Zulip messages")
	}
}

func TestFromZulipMultiline(t *testing.T) {
	r := New(testConfig())
	got, ok := r.FromZulip("irc-chan", "general chat", "u@e.com", "U", "one\n\ntwo\r\nthree\n")
	if !ok {
		t.Fatal("want ok")
	}
	want := []string{"<U> one", "<U> two", "<U> three"}
	if len(got.Lines) != len(want) {
		t.Fatalf("lines = %q", got.Lines)
	}
	for i := range want {
		if got.Lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got.Lines[i], want[i])
		}
	}
}

func TestFromZulipTruncation(t *testing.T) {
	r := New(testConfig())
	content := strings.Repeat("line\n", 20)
	got, ok := r.FromZulip("irc-chan", "general chat", "u@e.com", "U", content)
	if !ok {
		t.Fatal("want ok")
	}
	if len(got.Lines) != maxIRCLines+1 {
		t.Fatalf("len(lines) = %d, want %d", len(got.Lines), maxIRCLines+1)
	}
	last := got.Lines[len(got.Lines)-1]
	if !strings.Contains(last, "12 more lines truncated") {
		t.Errorf("truncation notice = %q", last)
	}
}

func TestFromZulipMeAction(t *testing.T) {
	r := New(testConfig())
	got, ok := r.FromZulip("irc-chan", "general chat", "u@e.com", "Alice", "/me waves")
	if !ok {
		t.Fatal("want ok")
	}
	if !got.Action {
		t.Error("want action")
	}
	if len(got.Lines) != 1 || got.Lines[0] != "Alice waves" {
		t.Errorf("lines = %q", got.Lines)
	}
}

func TestFromZulipEmptyDropped(t *testing.T) {
	r := New(testConfig())
	if _, ok := r.FromZulip("irc-chan", "general chat", "u@e.com", "U", "  \n \n"); ok {
		t.Error("whitespace-only message must not forward")
	}
}

func TestChannels(t *testing.T) {
	r := New(testConfig())
	chans := r.Channels()
	if len(chans) != 3 {
		t.Errorf("channels = %v", chans)
	}
}
