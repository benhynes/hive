# hive

An agent communication + control mesh for TUI agents. One Go binary, no
dependencies beyond `tmux ≥ 3.2`. Agents on any number of hosts discover
each other, exchange messages, ask each other questions, and — with the
right credential — spawn, type into, read, and kill each other's
terminal sessions.

Built for meshes of coding agents (Claude Code, etc.). Agents talk to the
mesh through **MCP tools** (`hive mcp`); the hub itself is plain HTTP+JSON,
which the `hive` CLI and the MCP server are both thin clients of.

```
┌─ mac ────────────────┐        ┌─ vm1 ────────────────┐
│  hive daemon :7777   │◀──────▶│  hive daemon :7777   │
│  ├─ alice (tmux)     │  HTTP  │  ├─ bob   (tmux)     │
│  └─ ci-bot           │        │  └─ worker (tmux)    │
└──────────────────────┘        └──────────────────────┘
        ▲ CLI / API                      ▲ tmux attach
```

## Design in one breath

One `hive daemon` per host. **Networks** are isolation domains — a hub
serves many, nothing crosses them. Exactly **two permission layers**:
`MSG` (send/recv/ask/answer/discover) and `CONTROL` (everything a human
at the keyboard could do). Agent identity is real: `from` is stamped
from the authenticated token, so a MSG agent can never impersonate — or
control — anyone. tmux is the control substrate; humans can always
`tmux attach` and watch.

## Install

```sh
go build -o ~/bin/hive ./cmd/hive
```

Agents talk to the mesh over **MCP**; the `hive` CLI runs the hub and lets a
human set up and drive the mesh by hand. There is no `hive send`/`recv`/`ask`
— agent messaging is the MCP tools.

## Quick start (one host)

```sh
hive net create dev          # prints the network's msg + control tokens
hive daemon &                # one per host (127.0.0.1:7777 by default)

# Spawn an agent into the mesh (a tmux session with HIVE_* injected). Point it
# at hive over MCP so it can message the rest of the mesh:
hive spawn worker -- claude --dangerously-skip-permissions
hive read worker             # what's on its screen? (human inspection)
hive keys --enter worker "claude mcp add hive -- hive mcp"

# Drive it by hand:
hive keys --enter worker "please run the tests"
```

## MCP — the agent interface

Agents call the mesh as native tools. Register the server once, in the agent:

```sh
claude mcp add hive -- hive mcp
```

`hive mcp` is a stdio MCP server. It authenticates from the same `HIVE_*`
env vars `hive spawn` already injects, so a spawned agent needs no
configuration at all. It offers `hive_send`, `hive_recv`, `hive_ask`,
`hive_answer`, `hive_asks`, and `hive_agents` — plus `hive_spawn`,
`hive_keys`, `hive_read`, and `hive_kill` when the agent holds the
control credential. **The two permission layers are enforced by omission:**
an MSG-only agent is never even shown the control tools, so a model
cannot plan around a capability it does not have. `hive mcp --list` prints
exactly what a given agent would be offered.

The MCP tools and the hub's HTTP API are the same operations against the same
hub, so an MCP agent and a human running `hive` from a shell are peers on one
mesh. The CLI keeps the infrastructure and human-driving verbs — `daemon`,
`net`, `node`, `register`, `hosts`, `spawn`, `read`, `keys`, `kill`, `agents` —
but the agent-messaging verbs are gone; agents use the tools.

One thing MCP does not change: **nothing pushes mail to an idle model.** An
MCP server cannot wake a model that isn't already running. The fast path is
therefore to *wait inside a receive*: an agent parked in `hive_recv` with
`wait` is woken the instant a message is appended — the hub closes a channel
on write, it does not poll — so delivery to a waiting worker is
sub-millisecond and arrives as the return value of the call it's already in.
An agent that is busy elsewhere instead gets a terminal nudge carrying a
preview of the waiting message. Both paths work; parking in `hive_recv` is the
one to design worker loops around.

## Spawn profiles — provisioned agents

`hive spawn --cwd DIR` provisions the working directory before the runtime
starts: it writes a project-scoped `.mcp.json` with the `hive` MCP server
auto-registered (pointing at the daemon's own binary, so PATH doesn't matter)
and **pre-approves it in `~/.claude.json`** — both the workspace-trust gate and
the MCP-server gate — so a headless Claude Code agent boots straight into the
mesh with no prompt to hang on. A spawn without `--cwd` is unchanged.

A **profile** (`~/.hive/profiles/<name>.json`) extends that with a default
runtime, cwd, context files seeded into the directory, and extra MCP servers:

```jsonc
// ~/.hive/profiles/dev.json
{
  "runtime": ["claude", "--dangerously-skip-permissions"],
  "cwd":     "~/work/agents",
  "context": ["/path/to/AGENT-GUIDE.md"],        // seeded into cwd
  "mcp": { "playwright": { "command": "npx", "args": ["-y", "@playwright/mcp"] } }
}
```

```sh
hive spawn --profile dev worker            # profile supplies runtime + cwd
hive spawn --profile dev --cwd ~/x w2 -- claude   # explicit flags win
```

Explicit flags and a trailing `-- CMD` override profile fields. Set
`"no_hive_mcp": true` in a profile to opt out of the automatic `hive` server.

## Add a second host

One command, over ssh:

```sh
hive node install vm1              # or user@host, any ssh alias works
```

That probes the target (`uname`), ships the right binary (self-copy on
a matching platform, cross-compiles from the source tree otherwise),
writes its config and network state over the ssh channel (tokens never
appear in remote argv), starts the daemon, health-checks it from here,
and announces the new host to every hub this one already knows.
Defaults assume a tailnet: the node binds its tailscale IPv4 and dials
back to ours; override with `--bind` / `--hub`. Useful flags:
`--persist` (install a supervisor so the daemon survives reboots — a
systemd unit on Linux, system-wide when root/sudo allows, else a user
unit with best-effort lingering; a launchd agent on macOS; a boot
scheduled task on Windows, admin required and boot-start only),
`--msg-only` (install no control token), `--local-control` (mint a fresh
token accepted only by the new node), `--restart` (upgrade a running
node), `--no-start`, `--name`, `--port`, `--dest`, `--home`.

The default remains a full peer using the existing shared control
token. Prefer `--local-control` for an isolation boundary: messaging
still works network-wide, and commands run on that node can use
`hive spawn --grant-control`, but the original network control token
cannot issue `spawn`/`read`/`keys`/`kill` directly to the node. The
host binding is propagated to granted agents and prevents Hive from
sending or copying that local token to another hub. Run control
commands on the node itself, for example through SSH.

**Windows targets** work too (OpenSSH server required): the installer
detects the platform through cmd/PowerShell, ships `hive.exe` over
scp, opens the daemon port in Windows Defender Firewall when the ssh
user is admin, and pins state with `daemon --home`. Windows hosts are
**full control peers** — where Unix drives sessions through tmux,
Windows uses the classic console API (`internal/control`), so
`spawn`/`read`/`keys`/`kill` work the same way. Pass `--msg-only` to
withhold control or `--local-control` to keep it on the Windows host.
See [docs/windows-control.md](docs/windows-control.md).

Or manually:

```sh
# on vm1 (reachable over your tailnet):
hive daemon --bind 100.x.y.z &
hive net join dev --hub mac:7777 --msg-token <T> --control-token <T>
# hosts lists are local to each hub — tell each side about the other:
hive hosts add mac 100.a.b.c:7777      # on vm1
hive hosts add vm1 100.x.y.z:7777      # on mac
```

Either way:

```sh
hive agents                  # now shows agents on both hosts
hive spawn --host vm1 bob -- claude    # control ops go direct to vm1's hub
hive keys --enter bob@vm1 "run the build"
```

Agents on the two hosts message each other over MCP (`hive_send bob@vm1 ...`);
the CLI above is a human driving the mesh.

Join with only `--msg-token` (or install with `--msg-only`) to make a
host **msg-only**: it can talk, but can never control anyone.

To revoke a previously shared control token on one hub and replace it
with a fresh host-local token:

```sh
hive net rotate-control dev
```

Rotation requires the local daemon so it can update in-memory auth and
`net.json` together. It does not update peers: each peer still accepting
the old shared token must be rotated or reprovisioned separately.

## Security model

- **Two layers.** Control endpoints require the receiving hub's control
  token; possession is the capability. A token may be legacy/shared or
  bound to one `control_host`. Agent tokens (minted at registration) are
  always MSG-layer.
- **Identity.** `from` is server-stamped from the token. No spoofing.
- **Isolation.** State, tokens, agents, and hosts lists are all
  per-network.
- **Transport.** Bind to your tailnet; Tailscale is the transport
  security (single-user tailnet threat model). Only `GET /v1/health` and the
  aggregate, identity-free `GET /metrics` endpoint are unauthenticated.
- **Audit.** Every control action is logged on the host where it
  happened (`~/.hive/nets/<net>/audit.log`).
- **Documented limit:** processes of the same OS user on the same host
  can read each other's env (and `~/.hive`). The boundary holds across
  hosts and OS users — don't co-locate untrusted agents with a
  controller under one user.

## CLI

The CLI runs the hub and lets a human set up and drive the mesh. Agent
messaging is MCP-only (see above) — there are no `send`/`recv`/`ask` verbs.

```
hive daemon [--bind ADDR] [--port N]
hive net    create <name> | join <name> --hub A --msg-token T [--control-token T] | list | show <name>

# Identity + MCP
hive register --name N [--pane %ID]     # prints export lines; eval them
hive agents [--local] [--json]          # list agents across the mesh
hive mcp [--list]                       # stdio MCP server: the agent interface

# CONTROL layer (direct to the target host's hub)
hive hosts  list | add <name> <addr:port> | rm <name>
hive net rotate-control <name>            # fresh host-local token on this hub
hive spawn [--host H] [--cwd D] [--grant-control] [--wait] [--headed] <name> -- CMD...
hive keys [--enter] <agent> <text...>
hive read [--lines N] <agent>
hive kill <agent>
```

Flags come before positionals. Agent-facing docs: **docs/AGENT-GUIDE.md**.
Wire format and semantics: **docs/PROTOCOL.md**.

## Demo

`demo/two-node-demo.sh` runs two hubs on this machine (separate state
dirs, a dedicated tmux socket), joins them into one network, and walks
the whole surface: spawn, keys/read, MCP messaging (send/recv/ask/answer/
broadcast) driven through `hive mcp`, nudge, audit. Safe to run repeatedly;
cleans up after itself. (Needs `python3` for the demo's MCP client.)

## Repo layout

```
cmd/hive/          dispatch: daemon vs client subcommands
internal/config    ~/.hive/config.json + per-net state
internal/proto     envelope, ids, names, tokens
internal/store     JSONL inboxes + cursors, registry, token table
internal/hub       daemon: auth, registry, mailboxes, nudge, audit, hub↔hub
internal/tmux      spawn (-e env), send-keys -l, paste-buffer, capture-pane
internal/client    HTTP client for the CLI (incl. ask/answer composition)
```

## Out of scope

Named channels · registry gossip / hosts auto-sync · mTLS · web
dashboard · controlling non-tmux agents beyond the Windows console backend.
