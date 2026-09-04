# Future improvement: mention conversion between IRC and Zulip

Status: analysis only, not scheduled. Written 2026-09-04.

## Background

Zulip mentions use `@**Full Name**` (or `@**Full Name|user_id**`) and
trigger notifications. The bridge sends IRC content to Zulip as plain
markdown, and Zulip parses mention syntax server-side — so an IRC user
who types the literal `@**fox**` **already produces a real, pinging
mention today**. What's missing is convenience: IRC users naturally
type `fox: did you see this?` or `@fox ping`, not Zulip syntax.

Because the explicit syntax always works, this feature never needs to
chase 100% recall — conservative heuristics plus that escape hatch
cover everything.

## Forward direction: IRC → Zulip (fuzzy, the hard half)

**Problem.** Zulip→IRC messages render as `<{name}> content` where
`{name}` is the sender's full name, possibly multi-word ("Santhosh
Raju"). An IRC user replying addresses the first token ("santhosh:"),
lowercase, maybe truncated. Conversion is fuzzy matching from an
IRC-typed token to a Zulip identity.

**Candidate set.** Two options for knowing who is mentionable:

- **Passive learning (recommended).** The poller already sees
  `sender_full_name` and `sender_id` on every Zulip→IRC message. Keep
  a small per-mapping cache of recently-seen senders (full name,
  first-token index, user id; TTL of a few hours). Zero extra API
  calls, and the bias is exactly right: only people who actually
  spoke recently in that topic are pingable — the population an IRC
  user would be replying to.
- **`GET /users` roster.** Complete but heavier: new endpoint,
  refresh staleness, and it makes every Zulip user pingable from IRC,
  widening the false-positive annoyance surface. Not worth it for v1.

**Matching rules — precision over recall** (a wrong conversion pings
the wrong human; the cost asymmetry demands conservatism):

1. Convert only *addressing positions*: leading `name:` / `name,`
   (IRC convention) and `@name` tokens. Never bare names
   mid-sentence ("fox" is also a word).
2. Match case-insensitively: exact full name, or unique first token.
   **Ambiguity → don't convert** (two recent "Alex ..." senders means
   `alex:` stays literal).
3. Emit `@**Full Name|user_id**` — the id form is immune to
   display-name collisions and renames, and `sender_id` is already in
   the event payload, so caching it is free.
4. Own bot and `ignore_nicks` are never converted.

## Reverse direction: Zulip → IRC (deterministic, the free half)

A Zulip user typing `@**fox**` currently arrives on IRC as the literal
`@**fox**`. A regex in `FromZulip` rewriting `@**Name**` /
`@**Name|id**` → `Name` (or `@Name`) cleans this up. The syntax is
exact, so there is no fuzziness and no risk — worth shipping even if
the forward half is rejected, and the sensible first increment.

## Architectural fit

The router is deliberately stateless and pure; this adds state (the
recent-senders cache) written by the poller path and read by the IRC
path. It is not a socket, so the one-connection-one-owner rule does
not apply — a small mutex-guarded map (or `sync.Map`) owned by the
router keeps both pumps lock-free except for microsecond lookups.
Testing stays easy: the cache is injectable and the transform is a
pure function of (content, cache contents) — table tests as usual.

## Config surface

- `bridge.convert_mentions = true|false` — default off for one
  release, then flip the default.
- `bridge.mention_ttl` — cache lifetime, default a few hours.

## Effort

Roughly 100–150 lines plus tests: cache (~40), FromIRC transform
(~40), FromZulip strip (~15), config plumbing. A `feat:` PR (minor
version bump). Ship order: reverse-strip first, fuzzy forward second.
