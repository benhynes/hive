# hive wire protocol (v1)

HTTP/1.1 + JSON. One hub per host, default `127.0.0.1:7777` (`--bind`
for a tailnet address). All paths are under `/v1`. Everything except
`GET /v1/health` requires a bearer token scoped to a network:

```
Authorization: Bearer <token>
```

Errors are `{"error": "..."}` with a non-200 status.

## Tokens and layers

| token | who holds it | layer | notes |
|---|---|---|---|
| hub **control** token | hubs, humans, `--grant-control` agents | CONTROL | full access on accepting hub(s); possession is the capability |
| network **msg** token | every member hub | MSG | join/infrastructure credential; can register agents and deliver hub↔hub |
| **per-agent** token | one agent | MSG | minted at registration; identity-bearing |

`from` on every message is stamped by the hub from the authenticated
token — agents cannot supply it. Agent tokens map to `name@host`;
network tokens stamp as `human@host` (optionally annotated for audit
via the advisory `X-Hive-Actor` header).

Tokens are 256-bit hex strings; hubs store only `sha256(token)` and
compare in constant time.

Existing network configs have a shared control token. A config with
`control_host: "vm1"` makes its control token valid only on `vm1`.
Clients carrying that binding refuse to send the token to any other
hub. `hive node install --local-control` creates this form with an
independent token; `hive net rotate-control <net>` replaces the local
hub's token with this form. The MSG token remains shared for routing.

### Endpoint access matrix

| endpoint | access |
|---|---|
| `GET  /v1/health` | none |
| `GET  /metrics` | none; aggregate counts only |
| `POST /v1/nets/{net}/register` | network token (msg or control) |
| `POST /v1/nets/{net}/deregister` | any (self), control (others) |
| `GET  /v1/nets/{net}/agents` | any valid token |
| `POST /v1/nets/{net}/send` | any valid token |
| `POST /v1/nets/{net}/deliver` | network token (hub↔hub) |
| `GET  /v1/nets/{net}/inbox` | own inbox: any agent token · others: control |
| `POST /v1/nets/{net}/ack` | agent token (own inbox only) |
| `GET  /v1/nets/{net}/hosts` | any valid token |
| `POST /v1/nets/{net}/hosts` | control |
| `POST /v1/nets/{net}/control/rotate` | control |
| `POST /v1/nets/{net}/spawn` | control |
| `POST /v1/nets/{net}/keys` | control |
| `GET  /v1/nets/{net}/read` | control |
| `POST /v1/nets/{net}/kill` | control |

A MSG-layer token is rejected (403) on every control endpoint. Every
control action is appended to the acting host's
`~/.hive/nets/<net>/audit.log` (TSV: time, actor, action, target,
detail).

## Names and addressing

Agent/host/network names match `[a-z0-9][a-z0-9_-]{0,31}`. An agent id
is `name@host` — the address carries the routing information, so no
registry synchronization exists or is needed. `@all` is broadcast.

## Envelope

```json
{
  "id":      "16-hex chars, minted once at the origin hub",
  "from":    "alice@mac      (server-stamped)",
  "to":      "bob@vm1 | @all",
  "kind":    "msg | ask | answer",
  "body":    "≤ 8192 bytes",
  "corr_id": "envelope id this answers (kind=answer)",
  "ts":      1730000000000
}
```

`id` is carried unchanged through forwards and retries; inboxes
deduplicate on it (at-least-once delivery + dedup-on-read). Request
bodies are capped at 64 KiB.

## Routing rules

- **Messages** go CLI → local hub → (if the target is remote) target
  hub via `POST /deliver`, authenticated with the network msg token.
  `deliver` only ever delivers locally — it never re-forwards — which
  makes `@all` loop-free (one-hop fan-out from the origin hub, per-host
  results).
- **Control ops** go CLI → target hub **direct** (fewer hops, the
  CONTROL credential never transits an intermediate hub, and the audit
  log lives where the action happened). A host-local credential is
  rejected client-side for any other target host. A hub receiving a
  control op for an agent it doesn't own answers `400 misrouted`.
- Hosts lists are **local to each hub** (`hive hosts add`) — static
  lookup, no push/sync. An unknown or down host yields a fast
  `undeliverable` result, never a queue.

## Endpoints

### `GET /v1/health`
→ `{"api":"hive","v":1,"host":"mac"}`. Unauthenticated; used by
`net join` to learn a peer's host name.

### `GET /metrics`
Prometheus text exposition with aggregate daemon, network, agent-liveness,
persistent-session readiness, inbox-lag, routing-table, and control-scope
gauges. It is
unauthenticated so a read-only collector never holds Hive control, and it
never includes agent names, tokens, addresses, messages, prompts, or panes.

### `POST /register` `{name, pane?, pid?}`
Binds identity for liveness: with `pane` (the caller's `$TMUX_PANE` on
Unix, or `win:<pid>[:<creation>]` on Windows) the hub verifies the pane,
captures its root pid and the process start-epoch (`ps lstart` on Unix,
the process creation FILETIME on Windows) — the epoch check defeats pid
reuse. With neither, the agent is message-only and trusted until
deregistered.
Rejects (409) names taken by a live agent; dead registrations are
reclaimed. → `{agent, token}` (the per-agent token; shown once).

### `POST /send` `{to, kind?, body, corr_id?}`
Builds the envelope (stamping `from`, minting `id`, `ts`), delivers
locally and/or forwards. → `{id, results: {"bob@vm1": "delivered" |
"undeliverable: ..."}}`. Broadcast results are keyed per agent, with
`"@host": "unreachable: ..."` entries for hosts that were down.

### `POST /deliver` `{env}`  *(hub↔hub)*
Validates and delivers locally (or fans out locally for `@all`).
→ `{results: {name: status}}`.

### `GET /inbox?after=N&max=M&wait=S&stat=1&agent=X`
Cursor-based, non-destructive reads. Defaults: `after` = the agent's
stored cursor, `max` 100 (cap 500). `wait` long-polls up to 25 s per
request when nothing is ready. `stat=1` returns bookkeeping only.
`agent=` reads someone else's inbox (control token required).
→ `{msgs: [{seq, env}], cursor, latest, skipped}` — `skipped > 0` means
the inbox overflowed its 1000-message window and messages were dropped
oldest-first before being read; the cursor clamps to the retained
floor, never leaving a silent gap.

### `POST /ack` `{seq}`
Advances the caller's durable cursor (idempotent, never regresses).
Agents ack only their own inbox.

### `GET/POST /hosts` `{op: add|rm, name, addr}`
Read (any token) / mutate (control) this hub's local hosts list.

### `POST /control/rotate` `{token}`
Replaces this hub's control token and binds the replacement to its host
name. The `net.json` write completes before in-memory authentication
switches; after success the old token is rejected. The CLI mints the
token and warns that peers must be rotated separately.

### `POST /spawn` `{name, cmd[], cwd?, grant_control?, wait_ready?, headed?}`
Creates tmux session `hive-<net>-<name>` via `new-session -d -e ...`
(env injected by tmux, never shell-interpolated), registers the agent
bound to its pane, and injects `HIVE_ADDR/HIVE_NET/HIVE_AGENT/
HIVE_TOKEN` (+ `HIVE_CONTROL_TOKEN` with `grant_control`, and
`HIVE_CONTROL_HOST` when that token is host-local).
`wait_ready` polls until the pane stops changing (≤15 s). `headed`
opens a visible terminal window on the target host attached to the
session (Terminal.app on macOS; `$TERMINAL` or a common emulator on
Linux) — best-effort: it needs the daemon to run inside a GUI session,
and failure never fails the spawn.
→ `{agent, session, pane, ready, window?}` — `window` is `"opened"` or
the error when `headed` was requested.

### `POST /keys` `{agent, text, enter?}`
Types into the agent's pane: `send-keys -l` for single-line text,
bracketed `paste-buffer` for multi-line. `enter` presses Enter after.

### `GET /read?agent=X&lines=N`
→ `{agent, screen}` — `capture-pane` output; `lines` adds scrollback.

### `POST /kill` `{agent}`
Kills the spawned tmux session (if any) and deregisters.
→ `{killed, deregistered}`.

## Ask/answer

Pure client-side composition — no server state machine. `ask` records
its inbox high-water mark, sends `kind=ask`, then long-polls **its own
inbox from that private position** for a `kind=answer` whose `corr_id`
equals the ask's envelope id. Regular mail is never consumed (the
cursor is untouched). `answer` finds the ask by id in the inbox and
sends `kind=answer` back to its `from`.

## Nudge

When fresh mail lands for an idle tmux-bound agent (no live long-poll on
its inbox), the hub types one line into its pane, e.g.
`hive: alice@mac says: the build is green  (+2 more — hive_recv)`. The line
carries the sender and a **single-line, length-capped preview** (≤240
bytes) of the oldest unread body, so the agent can often act without a
`recv` round trip; the full body still arrives via `recv`. `ask` mail is
flagged `asks:` rather than `says:`. The preview is whitespace-folded to
one line — a raw newline is never injected, since it would submit early in
a TUI.

Re-arming is keyed on the inbox's latest seq, not a wall-clock window: an
agent is nudged when mail arrives past what it was last nudged about,
subject to a 2 s anti-burst floor and a 30 s reminder cadence for mail it
has been told about but not yet read. A background sweeper re-checks idle
pane-bound agents every few seconds, so a message that lands inside the
anti-burst floor is still announced promptly. (Earlier versions nudged
only as a side effect of delivery, so a message arriving in the quiet
window with no later traffic could go unannounced indefinitely.)

The preview means a message body — sender-supplied bytes — is typed into a
recipient's pane. This is deliberate and within the trust model: mesh
members are trusted, `from` is authenticated, and any control-holder can
already type arbitrary text into any pane. It is a weaker capability than
CONTROL keys, but it is a real change from the previous "bodies are never
injected" rule. Agents are assumed to be TUI agents where typed text is a
prompt, not a shell that would execute it.

## Liveness

An agent is *alive* while its bound pane exists and its pid still has
the recorded start-epoch. Dead agents stay listed (`alive: false`)
until deregistered, killed, or their name is reclaimed by a new
registration.

## MCP

`hive mcp` (see the agent guide) is a **client-side surface, not part of
this wire protocol.** It is a stdio MCP server that speaks JSON-RPC 2.0
to the agent and plain `/v1` HTTP to the hub, exactly as the CLI does —
the hub is unchanged and unaware. Permission layers therefore hold
without any new enforcement: the MCP server holds only the credentials in
its environment, and CONTROL tools are omitted from `tools/list` entirely
when it holds no control token.

`internal/mcp` keeps the protocol (`Server.Handle`) separate from the
framing (`ServeStdio`), so serving MCP from the hub over HTTP would add a
second caller of `Handle` rather than a second implementation. That
endpoint does not exist today.

## Durability

Inboxes are append-only JSONL (`~/.hive/nets/<net>/inbox/<name>.jsonl`)
with a separate cursor file — restarts lose nothing, re-reads are
idempotent. Files compact once they double the 1000-message retained
window. Registry and tokens live in `registry.json` (hashes only).
