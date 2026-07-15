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
| `POST /v1/nets/{net}/register` | network token; canonical for pane-less/PID registration; legacy pane compatibility requires control |
| `POST /v1/nets/{net}/register/v2` | network token; a non-empty `pane` additionally requires control; safe explicit-nudge boundary |
| `POST /v1/nets/{net}/heartbeat` | agent token (own registration only) |
| `POST /v1/nets/{net}/release` | agent token (own retained lease only) |
| `POST /v1/nets/{net}/deregister` | any (self), control (others) |
| `GET  /v1/nets/{net}/agents` | any valid token |
| `POST /v1/nets/{net}/send` | any valid token |
| `POST /v1/nets/{net}/deliver` | network token (hub↔hub) |
| `GET  /v1/nets/{net}/inbox` | own inbox: any agent token · others: control |
| `POST /v1/nets/{net}/ack` | agent token (own inbox only) |
| `GET  /v1/nets/{net}/hosts` | any valid token |
| `POST /v1/nets/{net}/hosts` | control |
| `POST /v1/nets/{net}/control/rotate` | control |
| `POST /v1/nets/{net}/spawn/v2` | control; safe explicit-nudge boundary |
| `POST /v1/nets/{net}/spawn` | control; legacy compatibility only |
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
→ `{"api":"hive","v":1,"host":"mac","features":["leases","ephemeral_registration","release_presence","explicit_nudge","versioned_pane_mutations"]}`.
Unauthenticated; used by `net join` to learn a peer's host name and by clients
to preflight semantics that cannot safely be negotiated after mutation.
`leases`, `ephemeral_registration`, and `release_presence` advertise the three
managed-identity lifecycle pieces described below. `explicit_nudge` means pane
binding never implies terminal input and every nudge is an explicit request;
new clients require it before any pane-bound register or spawn, including
`nudge: false`. `versioned_pane_mutations` advertises `/register/v2` and
`/spawn/v2`. Those paths give an atomic compatibility boundary: a daemon too
old to understand explicit nudging returns 404 instead of performing a legacy
mutation. Upgrade/restart the target daemon before upgrading control clients
during a rolling deployment.

### `GET /metrics`
Prometheus text exposition with aggregate daemon, network, agent-liveness,
persistent-session readiness, inbox-lag, routing-table, and control-scope
gauges. It is
unauthenticated so a read-only collector never holds Hive control, and it
never includes agent names, tokens, addresses, messages, prompts, or panes.

### `POST /register` and `POST /register/v2` `{name, pane?, nudge?, pid?, lease_seconds?, ephemeral?}`

Binds identity for liveness. Current clients use `/register` for message-only
or PID-bound enrollment and `/register/v2` whenever `pane` is non-empty. The
unversioned pane mutation remains only for compatibility with older clients.

On Unix, a client-supplied `pane` is an explicit tmux pane id such as
`$TMUX_PANE`. The hub verifies it, captures its root pid, and records the
process start epoch (`ps lstart`) to defeat pid reuse. Because a pane lets Hive
inject terminal input through control APIs, binding one requires CONTROL even
though pane-less registration needs only the network MSG token. It does not
enable automatic input: `nudge: true` is a separate opt-in, requires `pane`,
and is false by default. Windows clients cannot supply a controllable pane;
they may use `pid` for liveness only. Opaque `win:<pid>:<creation>` bindings
are created internally for sessions spawned by the Windows hub.

With neither pane nor pid, the agent is message-only. A positive
`lease_seconds` makes its presence renewable; zero preserves the legacy
trusted-until-deregistered behavior. Lease duration is capped at one hour.
`ephemeral: true` requires a positive lease and marks a generated, disposable
identity; explicitly named clients leave it false. Hive's managed clients use a
60-second lease and heartbeat every 15 seconds. Clean deregistration removes an
ephemeral identity and mailbox immediately. After an unclean exit it disappears
from discovery at lease expiry, but its token and mailbox remain recoverable
for up to 24 hours before retirement. A new claimant may reclaim the generated
name during that window; mailbox retirement completes before it becomes
reusable, and stale in-memory readers cannot recreate its files.
Rejects (409) names taken by a live agent; dead registrations are
reclaimed. → `{agent, token, nudge?, nudge_policy, lease_seconds?, lease_expires?, ephemeral?}` (the
per-agent token is shown once; timestamps are Unix milliseconds). The response
echoes `ephemeral: true` so clients can detect daemons that predate disposable
registration support. A pane-bound client first requires the
`explicit_nudge` health feature, calls `/register/v2`, then verifies
`nudge_policy: "explicit"` and the echoed value in this response. If response
verification fails it deregisters the just-minted identity. The versioned path,
not the read-only preflight alone, prevents an older daemon from mutating a pane
before incompatibility is discovered.

### `POST /heartbeat`
Renews the caller's own registration from the current time using the duration
chosen at registration. Only the minted per-agent token can heartbeat; network
credentials cannot assert an agent's presence. For an unleased legacy
registration this is an accepted no-op.
→ `{agent, lease_seconds?, lease_expires?}`.

### `POST /release`
Marks the caller's retained, leased identity offline immediately without
deleting its address or durable mailbox. Only that identity's minted agent
token may release it; ephemeral and unleased registrations are rejected. A
subsequent claimant may reuse the now-dead name and receives the queued mail.
This is the clean-exit path for explicitly named `hive mcp` and `hive run`
sessions.
→ `{agent, lease_seconds, lease_expires}`.

### `GET /agents?local=1`
Returns `{self?, agents, unreachable?}`. For an agent credential, `self` is
derived from the authenticated token rather than client-supplied state. Each
roster entry includes `agent`, `alive`, `controllable`, `nudgeable`, timestamps,
and optional `ephemeral`/`spawned` flags. `local=1` skips peer
fan-out. Offline named identities remain discoverable for durable delivery;
expired ephemeral identities do not.

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

### `POST /spawn/v2` `{name, cmd[], cwd?, grant_control?, wait_ready?, headed?, nudge?, persist?}`

Creates a managed terminal session, registers the agent bound to its opaque
pane handle, and injects `HIVE_ADDR/HIVE_NET/HIVE_AGENT/HIVE_TOKEN` (+
`HIVE_CONTROL_TOKEN` with `grant_control`, and `HIVE_CONTROL_HOST` when that
token is host-local). Unix uses tmux session `hive-<net>-<name>` with
`new-session -d -e ...` (environment values are never shell-interpolated);
Windows uses the console backend.

Current clients require the `explicit_nudge` health feature and call
`/spawn/v2`. The unversioned `/spawn` route remains for old-client wire
compatibility only. Before mutating a persistent live declaration, the server
rejects a request whose nudge policy differs from the existing session.
`wait_ready` polls until the pane stops changing (≤15 s). `headed`
opens a visible terminal window on the target host attached to the
session (Terminal.app on macOS; `$TERMINAL` or a common emulator on
Linux) — best-effort: it needs the daemon to run inside a GUI session,
and failure never fails the spawn.
`nudge: true` separately opts this pane into automatic terminal wake and is
persisted with persistent spawn declarations.
→ `{agent, session, pane, nudge?, nudge_policy, ready, window?}` — `window` is `"opened"` or
the error when `headed` was requested.

### `POST /keys` `{agent, text, enter?}`
Types into the agent's pane: `send-keys -l` for single-line text,
bracketed `paste-buffer` for multi-line. `enter` presses Enter after.

### `GET /read?agent=X&lines=N`
→ `{agent, screen}` — `capture-pane` output; `lines` adds scrollback.

### `POST /kill` `{agent}`
Kills the spawned managed session (if any) and deregisters.
→ `{killed, deregistered}`.

## Ask/answer

Pure client-side composition — no server state machine. `ask` records
its inbox high-water mark, sends `kind=ask`, then long-polls **its own
inbox from that private position** for a `kind=answer` whose `corr_id`
equals the ask's envelope id. Regular mail is never consumed (the
cursor is untouched). `answer` finds the ask by id in the inbox and
sends `kind=answer` back to its `from`.

## Nudge

Automatic terminal wake is off by default, including for pane-bound and spawned
agents. When an agent is explicitly registered or spawned with `nudge: true`,
and fresh mail lands while it is idle (no live long-poll on its inbox), the hub
types and submits one fixed, shell-inert line:
`# hive: unread messages waiting - call the hive_recv tool`. The notice accepts
no envelope input: neither sender names nor peer-supplied message bodies are
typed into the terminal. Full mail remains available through `recv`.

This opt-in presses Enter. The hub captures the pane and proceeds only for a
small set of recognized empty prompts, but that check cannot be atomic with
terminal input: text typed concurrently can be submitted. Use automatic wake
only for controlled idle panes. Long-polling `recv` is the safe default.

Re-arming is keyed on the inbox's latest seq, not a wall-clock window: an
agent is nudged when mail arrives past what it was last nudged about,
subject to a 2 s anti-burst floor and a 30 s reminder cadence for mail it
has been told about but not yet read. A background sweeper re-checks idle
pane-bound agents every few seconds, so a message that lands inside the
anti-burst floor is still announced promptly. (Earlier versions nudged
only as a side effect of delivery, so a message arriving in the quiet
window with no later traffic could go unannounced indefinitely.)

## Liveness

An agent is *alive* while its bound pane exists and its pid still has
the recorded start-epoch. A leased registration must also have a future
`lease_expires`; heartbeats move that deadline. Legacy unbound registrations
retain their trusted-until-deregistered behavior for compatibility.

An expired ephemeral registration is omitted from discovery immediately. Its
token can still heartbeat and recover the same identity during a bounded
24-hour grace period (unless another claimant first reuses the name); after the
grace, the lifecycle sweep retires both record and disposable mailbox. Other
dead or expired agents stay listed (`alive: false`) until deregistered, killed,
renewed, or their name is reclaimed. In particular, `/release` makes a named
managed identity visibly offline at once while preserving its address and
mailbox for durable delivery.

## MCP

`hive mcp` (see the agent guide) is a **client-side surface, not part of
this wire protocol.** It is a stdio MCP server that speaks JSON-RPC 2.0
to the agent and plain `/v1` HTTP to the hub, exactly as the CLI does. With an
injected `HIVE_AGENT`/`HIVE_TOKEN` pair it uses that identity. Without one it
starts the MCP protocol immediately, then uses either the locally configured
network MSG credential or an explicit `HIVE_ADDR`/`HIVE_NET`/MSG `HIVE_TOKEN`
to enroll lazily in the background. Enrollment retries every five seconds
while stdio remains open; each Hive tool call also attempts enrollment and
returns an enrollment error if it still cannot succeed. Once minted, the
per-agent token replaces the bootstrap credential before the tool operation
runs, so calls cannot fall through as network-token `human`.

The sidecar heartbeats its 60-second lease every 15 seconds. On clean exit, a
generated name deregisters and retires its disposable mailbox; an explicit
`--name` calls `/release`, immediately marking presence false while keeping the
named mailbox. Injected identities remain owned by the launcher and are not
cleaned up by the MCP subprocess.

Agent-mode credential resolution never inherits CONTROL from `net.json`; a
control credential must be explicitly supplied in `HIVE_CONTROL_TOKEN`.
CONTROL tools are omitted from `tools/list` entirely when it is absent.

`internal/mcp` keeps the protocol (`Server.Handle`) separate from the
framing (`ServeStdio`), so serving MCP from the hub over HTTP would add a
second caller of `Handle` rather than a second implementation. That
endpoint does not exist today.

## Durability

Retained named inboxes are append-only JSONL
(`~/.hive/nets/<net>/inbox/<name>.jsonl`) with a separate cursor file —
restarts lose nothing and re-reads are idempotent. Generated ephemeral inboxes
use the same format while live and during the post-expiry recovery window, but
are deleted on clean deregistration, name reuse, or final prune. Files compact
once they double the 1000-message retained window. Registry and tokens live in
`registry.json` (hashes only).
