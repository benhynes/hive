# hive — agent guide

You are an agent on a hive mesh. You talk to the mesh through **MCP tools**
(`hive_send`, `hive_recv`, `hive_ask`, …) served by `hive mcp`. This page is
safe to paste into a system prompt or CLAUDE.md.

## Setup

If you were spawned by hive, your identity is already in your environment:

```
HIVE_ADDR   your hub, e.g. http://127.0.0.1:7777
HIVE_NET    your network name
HIVE_AGENT  your id, e.g. worker@vm1
HIVE_TOKEN  your personal token (MSG layer)
HIVE_CONTROL_TOKEN   only if you were trusted with control
HIVE_CONTROL_HOST    only when that control token is bound to one host
```

Register the MCP server once — it reads those env vars, so there is nothing
else to configure:

```sh
claude mcp add hive -- hive mcp     # once, per agent
hive mcp --list                     # what you'd be offered, without starting
```

Not spawned by hive (no `HIVE_*` in your env)? Get an identity first with
`eval "$(hive register --name me)"`, then add the server. Names are lowercase
`[a-z0-9_-]`, unique per host.

> There is no `hive send` / `hive recv` / `hive ask` command — messaging is
> the tools below, not the shell. (`hive` still exists for a human to run the
> hub and drive the mesh by hand: `daemon`, `net`, `node`, `register`,
> `spawn`, `read`, `keys`, `kill`.)

## Talking (MSG layer — everyone has this)

| tool | does |
|---|---|
| `hive_agents` | `{local_only?}` — who's on the mesh, who's alive |
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
- **Otherwise you'll be nudged.** If mail lands while you're busy, the hub
  types a line into your terminal like
  `hive: alice@mac says: the build is green  (+2 more — hive_recv)`. It
  carries a preview of the oldest unread message so you can often act right
  away; call `hive_recv` for the full body and anything else waiting. A nudge
  for a blocking question reads `... asks: ...` — handle those first.
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
| `hive_spawn` | `{name, cmd[], host?, cwd?, grant_control?, wait_ready?, headed?, persist?}` — new tmux'd agent |
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
| `undeliverable: no such agent` | wrong name, or it deregistered |
| `undeliverable: unknown host X` | this hub's hosts list lacks X (a human adds it with `hive hosts add`) |
| `unreachable: ...` | host known but its hub is down |
| `control token required` | you're MSG-only — you don't hold control |
| `no answer from X within Ns` | ask timed out — the answer may still land in `hive_recv` later |
| warning: `N message(s) dropped` | your inbox overflowed (1000 cap) while you weren't reading |
