# zulip-irc-bridge

Two-way bridge between Zulip streams and IRC channels, in Go. Ground-up
replacement for python-zulip-api's `bridge_with_irc` after running it in
production exposed unfixable design flaws (fork-shared TLS sockets,
upstream #772/PR #917). Runs in production on host `bessie`, bridging
`##badnicks` on Libera.Chat to the `irc-badnicks` stream on
zulip.badnicks.net.

Read [DESIGN.md](DESIGN.md) first for architecture and project history.
Future-work analyses live in `docs/`.

## Architecture invariants (do not break)

- **One connection, one owner.** Each network leg (IRC socket, Zulip
  long-poll, Zulip sender) is owned by exactly one goroutine; the two
  Zulip legs hold *separate* `zulip.Client` instances. Cross-goroutine
  traffic is immutable values over buffered channels, drop-and-count on
  overflow. Never share an HTTP client or the IRC connection.
- **No secrets outside `*_file`.** The TOML config lands in the
  world-readable nix store; secrets are only ever file paths
  (sops/LoadCredential). Never add an option that puts a secret in the
  config, argv, or env.
- **Never reconnect aggressively.** IRC reconnect/backoff floors exist
  because Libera banned the previous bridge's host for reconnect
  flooding. Keep steady-state retry intervals ≥30s (`ircx`), and the
  systemd module's Restart pacing intact.
- **The router stays pure.** Bridging semantics (mapping, formatting,
  loop prevention) live in `internal/router` as functions of their
  inputs — exhaustively table-tested. Push state elsewhere.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/zulip-irc-bridge/` | CLI: `-config`, `-check`, `-version` |
| `internal/config/` | TOML loader, `*_file` secrets, validation |
| `internal/zulip/` | minimal REST client: send + event long-poll |
| `internal/ircx/` | ergochat/irc-go wrapper: TLS, SASL, rejoin |
| `internal/router/` | pure routing/formatting/loop-prevention |
| `internal/bridge/` | goroutine wiring, supervision, sd_notify |
| `nix/module.nix` | NixOS module (`services.zulip-irc-bridge`) |

## Build / test / lint

```
nix develop                      # go 1.26 + gopls + golangci-lint
go test -race -cover ./...       # must be green before every commit
golangci-lint run ./...          # config in .golangci.yml; covers fmt too
nix build .                      # the deployment artifact
```

Toolchain is **go 1.26** (matches golangci-lint's build; 1.27 waits for
its final release in nixpkgs). Tests use fakes: `httptest` servers for
the Zulip client, an in-process fake IRC server for `ircx` (register
completes at end-of-MOTD 376, not 001), failure-injecting stubs for the
bridge pumps. Timing knobs (`rejoinDelay`, `pollBackoffBase`) are
package vars so failure-path tests run in milliseconds. Coverage
expectation: ≥90% per package, router at 100%.

## Workflow

- `main` is protected: every change is a branch + PR, squash-merged.
- **Conventional Commits** everywhere — the PR title becomes the merge
  commit that release-please parses. `feat:`/`fix:` bump the version
  (annotated via `x-release-please-version` in `main.go` and
  `flake.nix`); `docs:`/`ci:`/`chore:`/`test:` don't.
- AI-assisted commits carry an `Assisted-by:` trailer (see README note).
- Dependabot runs weekly (gomod + actions). Its go bumps invalidate
  `vendorHash` in flake.nix; the `fix-vendor-hash` workflow pushes the
  corrected hash automatically (needs the `AUTOFIX_TOKEN` secret to
  retrigger CI).

## Gotchas learned in production

- Windows CI: TOML double-quoted strings treat `\` as escapes — write
  interpolated paths as single-quoted TOML literals in tests.
- The production Zulip stream `irc-badnicks` allows **only** the
  `general chat` topic; mappings to it must use that topic or sends
  400.
- Zulip `display_recipient` is a string for stream messages but an
  array for DMs — decode lazily (`Message.Stream()`), never eagerly.
- Explicit `@**Full Name**` typed on IRC already produces a real Zulip
  mention (server-side parsing); see `docs/mention-conversion.md`
  before building anything fancier.

## Deployment

Consumed by the `serverconf` repo (bessie) as a flake input pinned in
its lock; `modules/zulip-irc/default.nix` there is pure configuration
of this repo's NixOS module. Bumping production = update that lock.
The module gates startup on `-check` (ExecStartPre) and ties systemd
readiness (`Type=notify`) to the actual IRC connect.
