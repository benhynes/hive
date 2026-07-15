# hive — agent guide

You are an agent on a hive mesh. You talk to the mesh through **MCP tools**
(`hive_send`, `hive_recv`, `hive_ask`, …) served by `hive mcp`. This page is
safe to paste into a system prompt or CLAUDE.md.

## Setup

If you were started by `hive spawn` or `hive run`, your identity is already in
your environment:

```
HIVE_ADDR   your hub, e.g. http://127.0.0.1:7777
HIVE_NET    your network name
HIVE_AGENT  your id, e.g. worker@vm1
HIVE_TOKEN  your personal token (MSG layer)
HIVE_CONTROL_TOKEN   only if you were trusted with control
HIVE_CONTROL_HOST    only when that control token is bound to one host
```

Configure the MCP server once in your runtime:

```sh
claude mcp add hive -- hive mcp     # once at the scope you want
hive mcp --list                     # offered tools; no daemon contact
```

No `HIVE_*` in your environment? `hive mcp` lazily registers a generated,
message-only identity in the background when the runtime starts it. Its MCP
handshake is available immediately; if the hub is down or a requested name is
still live, enrollment retries in the background. A tool call also attempts
enrollment and returns a retryable enrollment error rather than running under
the bootstrap credential if the hub is still unavailable. This path does not
need tmux. It needs either a local network with an MSG credential or explicit
`HIVE_ADDR`, `HIVE_NET`, and MSG `HIVE_TOKEN` variables. Call `hive_agents` to
find the authoritative assigned address in its top-level `self` field.

Managed identities use a 60-second lease renewed every 15 seconds. The default
generated identity is disposable: clean exit removes it and its mailbox. After
a crash it disappears from discovery at lease expiry, remains recoverable by
the same token for up to 24 hours (covering suspend or a partition), and is then
pruned with its mailbox. Use `hive mcp --name me` when other agents need a
stable address: a named sidecar releases presence on clean exit but retains the
address and durable mailbox for offline delivery. A named replacement can
reclaim immediately after a clean release; after a crash it may need to wait
for the 60-second lease to expire.

Tmux is optional terminal control, not enrollment. To adopt a pane deliberately,
hold CONTROL and run this inside it:

```sh
eval "$(hive register --name me --pane "$TMUX_PANE")"
```

Add `--nudge` only if Hive may press Enter in that controlled idle pane. Hive
never infers `$TMUX_PANE`; omit `--pane` for message-only manual registration.
On Windows, manual `--pane` binding is unsupported: `--pid` provides liveness
only, while console control applies to Hive-spawned sessions.

> There is no `hive send` / `hive recv` / `hive ask` command — messaging is
> the tools below, not the shell. (`hive` still exists for a human to run the
> hub and drive the mesh by hand: `daemon`, `net`, `node`, `register`, `run`,
> `deregister`, `agents`, `hosts`, `spawn`, `read`, `keys`, `kill`.)

## Talking (MSG layer — everyone has this)

| tool | does |
|---|---|
| `hive_agents` | `{local_only?}` — your address in `self`, plus the mesh roster and current presence |
| `hive_send` | `{to, body}` — durable message; `to` is `name@host`, a bare name on your host, or `@all` |
| `hive_recv` | `{wait?, max?, peek?}` — read new mail; **acks what it returns** |
| `hive_ask` | `{to, question, timeout?}` — blocks until they answer; returns the answer text |
| `hive_asks` | questions waiting on YOU |
| `hive_answer` | `{ask_id, body}` — answer one |

Rules of the road:

- **Park in `hive_recv` to receive fast.** Nothing pushes mail to an idle
  model. The quickest way to get a message is to already be waiting for it:
  call `hive_recv` with `wait` (up to 25 s) in a loop, and mail comes back as
  the tool result the instant it's sent — sub-millisecond, no nudge involved.
- **Automatic terminal wake is explicit opt-in.** If the identity was spawned
  or pane-registered with `--nudge` and is not long-polling, the hub submits the fixed,
  shell-inert notice `# hive: unread messages waiting - call the hive_recv
  tool`. Peer-supplied message text is never typed into the terminal. The hub
  checks for a recognized empty prompt first, but capture and Enter cannot be
  atomic: a concurrently typed draft can still be submitted. Use this only for
  controlled idle panes. Pane binding by itself does not opt in; every other
  agent receives durable mail safely the next time it calls `hive_recv`.
- **Answer asks promptly.** Someone is blocked waiting on you. `hive_asks`
  lists recent asks (including already-answered ones — answer each `ask_id`
  once). `hive_recv` also returns each ask with its `ask_id`.
- **`hive_recv` acknowledges what it returns.** The next call gives you only
  newer mail. Pass `peek: true` to look without consuming.
- Bodies are capped at 8 KiB. Point to files/URLs instead of pasting blobs.
  Messages may very rarely arrive twice — treat them idempotently.

## Controlling other agents (CONTROL layer)

Only if you hold `HIVE_CONTROL_TOKEN`. With it you also get these tools; **without
it they are not listed at all**, so if you can't see them you don't have
control and there's no point trying — ask a controller via `hive_send`.

| tool | does |
|---|---|
| `hive_spawn` | `{name, cmd[], host?, cwd?, grant_control?, wait_ready?, headed?, nudge?, persist?}` — new managed session (tmux on Unix, console on Windows); `nudge` has the terminal-input caveat above |
| `hive_keys` | `{agent, text, enter?}` — type into an agent's terminal |
| `hive_read` | `{agent, lines?}` — its screen, as text |
| `hive_kill` | `{agent, forget?}` — kill session + deregister |

- These do exactly what a human at the keyboard could do, and every action is
  audit-logged on the host where it happens.
- When `HIVE_CONTROL_HOST` is set, control is intentionally local to that host;
  hive rejects a remote control command before the token leaves your hub.
- Prefer `hive_send`/`hive_ask` over `hive_keys` when the target is a
  hive-aware agent — messages queue durably; keystrokes race with whatever the
  agent is doing. After `hive_keys`, give the TUI a moment, then `hive_read`.

## Patterns

**Wait-for-work loop** (worker agent) — the fast path: stay parked in
`hive_recv` with `wait` and process each result, rather than idling and
relying on the nudge.

```
loop: hive_recv{wait: 25}  →  handle messages  →  repeat
```

A parked `hive_recv{wait}` is woken the instant a message is appended (the hub
closes a channel on write, it does not poll), so delivery to a waiting worker
is sub-millisecond and arrives inside the call you're already in.

**Delegate and collect** (controller):

```
hive_spawn{host: "vm1", name: "w1", cmd: ["claude"]}
hive_keys{agent: "w1@vm1", enter: true,
          text: "fix the failing test in repo X, then hive_send to lead@mac: done"}
hive_recv{wait: 25}   # in a loop, until the worker reports
```

**Blocking question to a peer:**

```
answer = hive_ask{to: "architect@mac", question: "sync or async for the queue?", timeout: 120}
```

## Errors you'll see

| message | meaning |
|---|---|
| `undeliverable: no such agent` | wrong name, or a disposable/generated identity has gone away |
| `undeliverable: unknown host X` | this hub's hosts list lacks X (a human adds it with `hive hosts add`) |
| `unreachable: ...` | host known but its hub is down |
| `control token required` | you're MSG-only — you don't hold control |
| `no answer from X within Ns` | ask timed out — the answer may still land in `hive_recv` later |
| warning: `N message(s) dropped` | your inbox overflowed (1000 cap) while you weren't reading |
