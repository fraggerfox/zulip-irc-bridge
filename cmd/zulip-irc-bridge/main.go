// Command zulip-irc-bridge runs a two-way bridge between Zulip streams
// and IRC channels. See config.example.toml for configuration.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/fraggerfox/zulip-irc-bridge/internal/config"
)

var version = "0.1.0-dev"

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "path to config.toml (required)")
	check := flag.Bool("check", false, "validate the configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: -config is required")
		flag.Usage()
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	if *check {
		fmt.Printf("config OK: %d mapping(s)\n", len(cfg.Mappings))
		return 0
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(cfg.Bridge.LogLevel),
	})))

	// Bridge core arrives in later phases (see DESIGN.md).
	slog.Error("bridge core not implemented yet — see DESIGN.md")
	return 1
}

func logLevel(s string) slog.Level {
	switch strings.ToUpper(s) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
