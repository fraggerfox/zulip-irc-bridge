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

## NixOS module

The flake exports a NixOS module that owns the systemd unit: config
validation before every (re)start, secrets via systemd `LoadCredential`
(sops-nix / agenix files can stay root-owned), readiness tied to the
actual IRC connect (`Type=notify`), reconnect-safe restart pacing, and
a `DynamicUser` sandbox.

Add the flake input and import the module:

```nix
{
  inputs.zulip-irc-bridge.url = "github:fraggerfox/zulip-irc-bridge";
  inputs.zulip-irc-bridge.inputs.nixpkgs.follows = "nixpkgs";

  # in your host's module list:
  # zulip-irc-bridge.nixosModules.default
}
```

Then configure the service:

```nix
services.zulip-irc-bridge = {
  enable = true;

  # exposed to the service at
  # /run/credentials/zulip-irc-bridge.service/<name>, read as root
  credentials.zulip_api_key = config.sops.secrets.zulip_api_key.path;

  # rendered to TOML in the nix store — never put secrets here,
  # point *_file settings at credential paths instead
  settings = {
    zulip = {
      site = "https://zulip.example.com";
      email = "irc-bot@zulip.example.com";
      api_key_file = "/run/credentials/zulip-irc-bridge.service/zulip_api_key";
    };
    irc = {
      server = "irc.libera.chat";
      nick = "example_bridge";
    };
    mapping = [
      {
        channel = "##example";
        stream = "irc-example";
        topic = "general chat";
      }
    ];
  };
};
```

`settings` is free-form and mirrors [config.example.toml](config.example.toml);
a broken config fails the deploy at `ExecStartPre` instead of taking a
running bridge down.

## Development

```
nix develop     # go toolchain + tooling
go test -race -cover ./...
go run ./cmd/zulip-irc-bridge -config config.toml -check
```

## License

[BSD 2-Clause](LICENSE)
