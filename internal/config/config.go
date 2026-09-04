// Package config loads and validates the bridge's TOML configuration.
//
// Secrets may be given inline (api_key = "...") or — preferred — as paths
// to files containing only the secret (api_key_file = "/run/secrets/...").
// The *_file variant always wins if both are set.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

type Zulip struct {
	Site       string `toml:"site"`
	Email      string `toml:"email"`
	APIKey     string `toml:"api_key"`
	APIKeyFile string `toml:"api_key_file"`
}

type SASL struct {
	Enabled      bool   `toml:"enabled"`
	Username     string `toml:"username"`
	Password     string `toml:"password"`
	PasswordFile string `toml:"password_file"`
}

type IRC struct {
	Server   string `toml:"server"`
	Port     int    `toml:"port"`
	TLS      *bool  `toml:"tls"`
	Nick     string `toml:"nick"`
	Realname string `toml:"realname"`
	SASL     SASL   `toml:"sasl"`
}

// Direction constrains which way a mapping relays messages.
type Direction string

const (
	Both       Direction = "both"
	IRCToZulip Direction = "irc_to_zulip"
	ZulipToIRC Direction = "zulip_to_irc"
)

type Mapping struct {
	Channel   string    `toml:"channel"`
	Stream    string    `toml:"stream"`
	Topic     string    `toml:"topic"`
	Direction Direction `toml:"direction"`
}

type Bridge struct {
	ZulipMessageFormat string   `toml:"zulip_message_format"`
	IRCMessageFormat   string   `toml:"irc_message_format"`
	IgnoreNicks        []string `toml:"ignore_nicks"`
	LogLevel           string   `toml:"log_level"`
}

type Config struct {
	Zulip    Zulip     `toml:"zulip"`
	IRC      IRC       `toml:"irc"`
	Mappings []Mapping `toml:"mapping"`
	Bridge   Bridge    `toml:"bridge"`
}

// TLSEnabled reports whether the IRC connection should use TLS
// (defaults to true when unset).
func (i IRC) TLSEnabled() bool {
	return i.TLS == nil || *i.TLS
}

func readSecret(inline, file, what string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file) //nolint:gosec // secret path is operator-provided config
		if err != nil {
			return "", fmt.Errorf("reading %s from %s: %w", what, file, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if inline != "" {
		return inline, nil
	}
	return "", fmt.Errorf("missing required setting: %s (or %s_file)", what, what)
}

// Load reads, resolves secrets for, and validates the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // config path comes from the -config flag
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	dec := toml.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) finalize() error {
	var errs []error

	// Defaults.
	if c.IRC.Port == 0 {
		c.IRC.Port = 6697
	}
	if c.IRC.Realname == "" {
		c.IRC.Realname = c.IRC.Nick
	}
	if c.Bridge.ZulipMessageFormat == "" {
		c.Bridge.ZulipMessageFormat = "**{nick}**: {content}"
	}
	if c.Bridge.IRCMessageFormat == "" {
		c.Bridge.IRCMessageFormat = "<{name}> {content}"
	}
	if c.Bridge.LogLevel == "" {
		c.Bridge.LogLevel = "INFO"
	}
	for i := range c.Mappings {
		if c.Mappings[i].Direction == "" {
			c.Mappings[i].Direction = Both
		}
	}

	// Required fields.
	if c.Zulip.Site == "" {
		errs = append(errs, errors.New("zulip.site is required"))
	}
	if c.Zulip.Email == "" {
		errs = append(errs, errors.New("zulip.email is required"))
	}
	if c.IRC.Server == "" {
		errs = append(errs, errors.New("irc.server is required"))
	}
	if c.IRC.Nick == "" {
		errs = append(errs, errors.New("irc.nick is required"))
	}
	if len(c.Mappings) == 0 {
		errs = append(errs, errors.New("at least one [[mapping]] is required"))
	}

	// Secrets.
	key, err := readSecret(c.Zulip.APIKey, c.Zulip.APIKeyFile, "zulip.api_key")
	if err != nil {
		errs = append(errs, err)
	}
	c.Zulip.APIKey = key

	if c.IRC.SASL.Enabled {
		if c.IRC.SASL.Username == "" {
			errs = append(errs, errors.New("irc.sasl.username is required when SASL is enabled"))
		}
		pw, err := readSecret(c.IRC.SASL.Password, c.IRC.SASL.PasswordFile, "irc.sasl.password")
		if err != nil {
			errs = append(errs, err)
		}
		c.IRC.SASL.Password = pw
	}

	// Mappings.
	seen := map[string]bool{}
	for _, m := range c.Mappings {
		switch m.Direction {
		case Both, IRCToZulip, ZulipToIRC:
		default:
			errs = append(errs, fmt.Errorf("mapping %q: invalid direction %q", m.Channel, m.Direction))
		}
		if m.Channel == "" || m.Stream == "" || m.Topic == "" {
			errs = append(errs, fmt.Errorf("mapping %q: channel, stream and topic are all required", m.Channel))
		}
		key := strings.ToLower(m.Channel)
		if seen[key] {
			errs = append(errs, fmt.Errorf("mapping %q: duplicate channel", m.Channel))
		}
		seen[key] = true
	}

	// Log level.
	switch strings.ToUpper(c.Bridge.LogLevel) {
	case "DEBUG", "INFO", "WARN", "WARNING", "ERROR":
	default:
		errs = append(errs, fmt.Errorf("invalid bridge.log_level %q", c.Bridge.LogLevel))
	}

	return errors.Join(errs...)
}
