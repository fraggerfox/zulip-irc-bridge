# Design and implementation plan

Ground-up Go rewrite of the python-zulip-api IRC bridge, informed by
running the original in production (bessie) and by upstream issues
zulip/python-zulip-api#772 / PR #917.

## Why Go

The workload is a handful of IO-bound loops: an IRC socket, a Zulip
long-poll, an HTTP sender. Goroutines and channels model this directly —
no fork (the original's fatal flaw: shared TLS sockets after
`mp.Process`, broken outright on Python 3.14), no GIL, single static
binary to deploy.

## Architecture

```
main goroutine        config, signal handling (context), supervision
├── irc goroutine     owns the IRC connection (ergochat/irc-go)
├── poller goroutine  Zulip event long-poll — own HTTP client
├── sender goroutine  drains toZulip channel — own HTTP client
└── channels          toZulip / toIRC: buffered chan BridgeMessage
```

Rules that prevent the original's failure modes:

1. **Every connection owned by exactly one goroutine.** IRC writes happen
   only from the irc goroutine, which selects over the connection's
   events and the `toIRC` channel. Zulip's poller and sender each hold
   their own `http.Client` — no shared transport state between loops.
2. **Channels, not shared memory.** Cross-goroutine traffic is immutable
   `BridgeMessage` values over buffered channels. On overflow (Zulip or
   IRC down for a long stretch), drop with a logged warning and a
   counter — bounded memory, quantified loss.
3. **Shutdown via context.** SIGTERM cancels a root context; each
   goroutine exits at its next natural wake-up; IRC sends QUIT; main
   waits with a timeout.

## Configuration

Single TOML file (config.example.toml). Secrets use `*_file`
indirection (sops-nix / systemd LoadCredential friendly): never in
argv, env, or the config file itself. Multiple `[[mapping]]` blocks
bind channel ↔ stream+topic, each with a `direction` of `both`,
`irc_to_zulip`, or `zulip_to_irc`.

## Phases

### Phase 1 — scaffold (done)
- [x] go.mod + flake pinned to go 1.26 (stable; matches golangci-lint), cmd/ layout
- [x] internal/config: TOML loader, `*_file` secrets, validation,
      `-check` mode; table tests
- [x] flake.nix: buildGoModule package + devshell

### Phase 2 — Zulip client (internal/zulip) (done)
- [x] minimal REST client: SendMessage, RegisterQueue, GetEvents
      (long-poll), DeleteQueue; Basic auth; own http.Client per instance
- [x] event loop handling: heartbeats, `BAD_EVENT_QUEUE_ID` →
      re-register, backoff on transport errors
- [x] send retries: bounded attempts with backoff; give up + log per
      message, never crash the loop
- [x] tests against httptest.Server: auth header, retry behavior,
      queue re-registration, long-poll timeout handling

### Phase 3 — IRC client (internal/ircx) (done)
- [x] ergochat/irc-go connection: TLS (default, port 6697), SASL PLAIN
      with independent username/password, hard-fail if SASL enabled but
      rejected (no silent plaintext-auth fallback)
- [x] multi-channel join from mappings; rejoin on kick; reconnect with
      ≥30s steady-state backoff (the Libera ban lesson)
- [x] nick-in-use fallback (suffix underscore); no forced `_zulip` suffix
- [x] tests for the pure parts (nick fallback, channel set derivation);
      connection behavior isolated behind a small interface

### Phase 4 — router (internal/router) (done)
- [x] mapping resolution both directions, per-mapping direction filter
- [x] loop prevention: own bot email, own nick, `ignore_nicks`
- [x] formatting: configurable templates ({nick}/{name}/{content});
      IRC ACTION (/me) → italics and vice versa; multiline Zulip
      messages → one IRC line each, capped with a truncation notice
- [x] full table-test coverage — this is the critical pure component

### Phase 5 — bridge core + ops (internal/bridge) (done)
- [x] Run(ctx, cfg): wire goroutines and channels; supervise; restart
      failed legs with backoff
- [x] periodic per-direction counters (forwarded / dropped) to the log
- [x] clean shutdown: QUIT with message, drain, exit code semantics
- [x] optional sd_notify (Type=notify) support
- [x] `go test -cover` pass; race detector (`go test -race`) clean

### Phase 6 — scratch-channel test + deployment

Stage A — live smoke test, run locally (no deploy):
- [ ] join `##badnicks-test` on Libera (create-on-join; first joiner
      gets ops, needed for the kick test)
- [ ] test mapping reuses the existing `irc-badnicks` stream under a
      separate topic (`bridge test`) — no new stream, bot already
      subscribed, no collision with production traffic
- [ ] run `go run ./cmd/zulip-irc-bridge -config config.toml` from the
      dev machine with the bot's API key in a local secret file
- [ ] test matrix: IRC→Zulip, Zulip→IRC, `/me` both directions,
      multiline + truncation cap, DM to bot ignored, kick → rejoin,
      SIGINT → clean QUIT in channel
- [ ] optional: register a NickServ account for the bot and verify
      SASL PLAIN over TLS (hard-fail path: wrong password must abort)

Stage B — same scratch mapping, deployed:
- [ ] flake `nixosModules.default`: systemd unit (Type=notify,
      DynamicUser, LoadCredential secrets, conservative Restart
      defaults)
- [ ] deploy to bessie bridging `##badnicks-test`; verify under
      systemd: credentials, sd_notify readiness, restart pacing,
      journal legibility

Stage C — cutover:
- [ ] flip the mapping to `##badnicks` / "general chat"
- [ ] serverconf: remove modules/zulip-irc (fetchFromGitHub + patch +
      python env); this flake's module replaces it
- [ ] retire the scratch channel

## Testing policy

Every phase lands with its tests in the same commit. Critical pure
components (config, router) target exhaustive table coverage; network
components (zulip client) are tested against `httptest` servers
including failure paths; `go test -race -cover` runs in CI and before
every commit.

## Non-goals (for now)

Matrix/other protocols, message edits/deletions, IRC channel state
mirroring (topics, joins/parts), per-user puppet nicks.
