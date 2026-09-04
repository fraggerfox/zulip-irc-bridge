# zulip-irc-bridge

> [!NOTE]
> This project is developed with AI assistance (Claude Fable 5 via Claude
> Code), supervised and reviewed by the maintainer. Commits carry an
> `Assisted-by:` trailer where applicable.

A configurable two-way bridge between [Zulip](https://zulip.com) streams
and IRC channels, written in Go. A ground-up replacement for the
`bridge_with_irc` example in
[python-zulip-api](https://github.com/zulip/python-zulip-api), motivated
by running that script in production and hitting its limits:

- fork-based architecture that corrupts shared TLS sockets and breaks on
  Python 3.14 (upstream
  [#917](https://github.com/zulip/python-zulip-api/pull/917),
  [#772](https://github.com/zulip/python-zulip-api/issues/772))
- no TLS to the IRC server, SASL bolted on (password only, login forced
  to the nick)
- configuration solely via CLI flags — the API key is visible in `ps`
- one hardcoded channel↔stream pair, forced `_zulip` nick suffix

## Design

One goroutine per connection, channels between them, context-based
shutdown; TOML configuration with file-based secrets; SASL PLAIN over
TLS as the default connection mode; multiple channel↔stream mappings,
each optionally one-directional. Ships as a single static binary.

See [DESIGN.md](DESIGN.md) for the architecture and implementation
roadmap.

## Usage

```
zulip-irc-bridge -config config.toml          # run
zulip-irc-bridge -config config.toml -check   # validate config only
```

See [config.example.toml](config.example.toml) for all settings.

## Development

```
nix develop     # go toolchain + tooling
go test -race -cover ./...
go run ./cmd/zulip-irc-bridge -config config.toml -check
```
