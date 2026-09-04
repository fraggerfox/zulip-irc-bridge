package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimalTOML = `
[zulip]
site = "https://zulip.example.com"
email = "bot@example.com"
api_key = "inline-key"

[irc]
server = "irc.libera.chat"
nick = "bridge_bot"

[[mapping]]
channel = "##chan"
stream = "irc-chan"
topic = "general"
`

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "c.toml", minimalTOML)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IRC.Port != 6697 {
		t.Errorf("default port = %d, want 6697", c.IRC.Port)
	}
	if !c.IRC.TLSEnabled() {
		t.Error("TLS should default to enabled")
	}
	if c.IRC.Realname != "bridge_bot" {
		t.Errorf("realname = %q, want nick", c.IRC.Realname)
	}
	if c.Mappings[0].Direction != Both {
		t.Errorf("direction = %q, want both", c.Mappings[0].Direction)
	}
	if c.Bridge.ZulipMessageFormat == "" || c.Bridge.IRCMessageFormat == "" {
		t.Error("format defaults not applied")
	}
	if c.Bridge.LogLevel != "INFO" {
		t.Errorf("log level = %q, want INFO", c.Bridge.LogLevel)
	}
}

func TestSecretFileWinsOverInline(t *testing.T) {
	dir := t.TempDir()
	keyFile := write(t, dir, "key", "file-key\n")
	// TOML literal string (single quotes): Windows paths contain
	// backslashes, which double-quoted TOML strings treat as escapes.
	cfg := strings.Replace(minimalTOML,
		`api_key = "inline-key"`,
		"api_key = \"inline-key\"\napi_key_file = '"+keyFile+"'", 1)
	p := write(t, dir, "c.toml", cfg)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Zulip.APIKey != "file-key" {
		t.Errorf("api key = %q, want file-key (trimmed, file wins)", c.Zulip.APIKey)
	}
}

func TestMissingSecretFails(t *testing.T) {
	dir := t.TempDir()
	cfg := strings.Replace(minimalTOML, `api_key = "inline-key"`, "", 1)
	p := write(t, dir, "c.toml", cfg)

	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "zulip.api_key") {
		t.Fatalf("want missing api_key error, got %v", err)
	}
}

func TestUnreadableSecretFileFails(t *testing.T) {
	dir := t.TempDir()
	cfg := strings.Replace(minimalTOML,
		`api_key = "inline-key"`,
		`api_key_file = "/nonexistent/key"`, 1)
	p := write(t, dir, "c.toml", cfg)

	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "/nonexistent/key") {
		t.Fatalf("want unreadable file error, got %v", err)
	}
}

func TestSASLValidation(t *testing.T) {
	dir := t.TempDir()
	cfg := minimalTOML + `
[irc.sasl]
enabled = true
username = "acct"
password = "pw"
`
	p := write(t, dir, "c.toml", cfg)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IRC.SASL.Password != "pw" {
		t.Errorf("sasl password = %q", c.IRC.SASL.Password)
	}

	// Enabled without credentials must fail.
	bad := minimalTOML + "\n[irc.sasl]\nenabled = true\n"
	p2 := write(t, dir, "c2.toml", bad)
	if _, err := Load(p2); err == nil ||
		!strings.Contains(err.Error(), "sasl.username") ||
		!strings.Contains(err.Error(), "sasl.password") {
		t.Fatalf("want sasl validation errors, got %v", err)
	}
}

func TestInvalidDirection(t *testing.T) {
	dir := t.TempDir()
	cfg := strings.Replace(minimalTOML, `topic = "general"`,
		"topic = \"general\"\ndirection = \"sideways\"", 1)
	p := write(t, dir, "c.toml", cfg)

	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Fatalf("want invalid direction error, got %v", err)
	}
}

func TestDuplicateChannelFails(t *testing.T) {
	dir := t.TempDir()
	cfg := minimalTOML + `
[[mapping]]
channel = "##CHAN"
stream = "other"
topic = "t"
`
	p := write(t, dir, "c.toml", cfg)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate channel error, got %v", err)
	}
}

func TestNoMappingsFails(t *testing.T) {
	dir := t.TempDir()
	cfg := strings.Split(minimalTOML, "[[mapping]]")[0]
	p := write(t, dir, "c.toml", cfg)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Fatalf("want no-mappings error, got %v", err)
	}
}

func TestUnknownFieldFails(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "c.toml", minimalTOML+"\n[bridge]\ntypo_field = 1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("want unknown-field error, got nil")
	}
}
