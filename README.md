# hive

An agent communication + control mesh for terminal agents. One Go binary;
messaging has no terminal-supervisor dependency. Agents on any number of hosts
discover each other, exchange messages, and ask each other questions. On Unix,
optional managed-session control uses `tmux ≥ 3.2` to spawn, inspect, type into,
and kill terminal sessions.

Built for meshes of coding agents (Claude Code, etc.). Agents talk to the
mesh through **MCP tools** (`hive mcp`); the hub itself is plain HTTP+JSON,
which the `hive` CLI and the MCP server are both thin clients of.

```
┌─ mac ────────────────┐        ┌─ vm1 ────────────────┐
│  hive daemon :7777   │◀──────▶│  hive daemon :7777   │
│  ├─ alice (MCP)      │  HTTP  │  ├─ bob (hive run)   │
│  └─ ci-bot           │        │  └─ worker (tmux)    │
└──────────────────────┘        └──────────────────────┘
        ▲ CLI / MCP                      ▲ MCP / optional control
```

## Design in one breath

One `hive daemon` per host. **Networks** are isolation domains — a hub
serves many, nothing crosses them. Exactly **two permission layers**:
`MSG` (send/recv/ask/answer/discover) and `CONTROL` (everything a human
at the keyboard could do). Agent identity is real: `from` is stamped
from the authenticated token, so a MSG agent can never impersonate — or
control — anyone. Identity and durable mail do not depend on a terminal pane.
Tmux is an optional Unix control substrate; humans can `tmux attach` and watch
managed sessions. Automatic terminal wake is separately opt-in (`--nudge`).

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

# Configure Hive as an MCP server once in your agent runtime:
claude mcp add hive -- hive mcp

# Launch normally. hive mcp lazily enrolls a leased message identity:
claude

# Or choose a stable name while keeping the current terminal (no tmux):
hive run --name worker -- claude --dangerously-skip-permissions

# Tmux remains available when managed control/observation is wanted:
hive spawn managed -- claude --dangerously-skip-permissions
hive read managed
```

## MCP — the agent interface

Agents call the mesh as native tools. Configure the server once in the runtime:

```sh
claude mcp add hive -- hive mcp
```

`hive mcp` is a stdio MCP server. When `hive spawn` or `hive run` supplied an
identity, it authenticates from those `HIVE_*` variables. Otherwise it
automatically registers a generated, renewable identity with the local hub;
pass `hive mcp --name alice` to choose a stable name. Enrollment is lazy: the
stdio MCP handshake starts even if the hub is temporarily unavailable or a
name's old lease is still live, while enrollment retries in the background.
Calls to Hive tools remain gated until an identity has been minted (and return
an enrollment error while it cannot be), so the bootstrap network credential
is never used as an agent identity.

The managed lease is 60 seconds and is renewed every 15 seconds. Generated MCP
and unnamed `hive run` identities are disposable: clean exit deregisters them
and removes their mailbox. After an unclean exit they disappear from discovery
when the lease expires; the old token and mailbox remain recoverable for up to
24 hours so a suspended or partitioned process can resume, then the daemon
retires them. Do not use a generated address as an offline durable destination.
Explicit `--name` identities instead release presence on clean exit and keep
their address and mailbox, so peers can queue durable mail while they are
offline. A named replacement can reclaim the address immediately after a clean
release, or after the 60-second lease expires following a crash.

It offers `hive_send`, `hive_recv`, `hive_ask`, `hive_answer`, `hive_asks`,
and `hive_agents` — plus `hive_spawn`, `hive_keys`, `hive_read`, and
`hive_kill` only when an explicit control credential was supplied. **The two
permission layers are enforced by omission:** an MSG-only agent is never even
shown the control tools, so a model cannot plan around a capability it does
not have. `hive mcp --list` prints what the process would be offered without
registering or contacting the daemon.

The MCP tools and the hub's HTTP API are the same operations against the same
hub, so an MCP agent and a human running `hive` from a shell are peers on one
mesh. The CLI keeps the infrastructure and human-driving verbs — `daemon`,
`net`, `node`, `register`, `run`, `deregister`, `hosts`, `spawn`, `read`,
`keys`, `kill`, `agents` — but the agent-messaging verbs are gone; agents use
the tools. `hive_agents` returns the caller's address in its top-level `self`
field, including for automatically generated identities.

One thing MCP does not change: **nothing pushes mail to an idle model.** An
MCP server cannot wake a model that isn't already running. The fast path is
therefore to *wait inside a receive*: an agent parked in `hive_recv` with
`wait` is woken the instant a message is appended — the hub closes a channel
on write, it does not poll — so delivery to a waiting worker is
sub-millisecond and arrives as the return value of the call it's already in.
An agent explicitly spawned or pane-registered with `--nudge` may instead get
a fixed `hive_recv` reminder; message bodies are never typed into its terminal.
This opt-in sends Enter and can submit a draft typed concurrently after Hive's
idle-prompt check, so it is only for controlled idle panes. Pane binding alone
never enables it. Parking in `hive_recv` is the safe default and the path to
design worker loops around.

`hive run` is a foreground wrapper: stdin/stdout/stderr and the child's exit
status are preserved and CONTROL is stripped. On Linux, macOS, and the BSDs it
places the child in a process group and forwards SIGINT/SIGTERM to that group;
on other platforms it signals the direct child. This is deliberately not a
full shell job-control implementation: Ctrl-Z suspension is unsupported, and a
descendant that creates a new process group can escape group signaling.

Tmux adoption is available when terminal observation/control is actually
wanted, but it is not part of joining the message mesh. From inside the pane,
bind it explicitly (and hold a CONTROL credential):

```sh
eval "$(hive register --name worker --pane "$TMUX_PANE")"

# Alternative for a controlled idle pane that may be woken with Enter:
eval "$(hive register --name worker --pane "$TMUX_PANE" --nudge)"
```

Omit `--pane` for a message-only manual registration. Hive never infers
`$TMUX_PANE`, and pane binding alone never opts into automatic terminal input.

## Spawn profiles — provisioned agents

`hive spawn --cwd DIR` provisions the working directory before the runtime
starts: it writes a project-scoped `.mcp.json` with the `hive` MCP server
auto-registered (pointing at the daemon's own binary, so PATH doesn't matter)
and **pre-approves it in `~/.claude.json`** — both the workspace-trust gate and
the MCP-server gate — so a headless Claude Code agent boots straight into the
mesh with no prompt to hang on. A spawn without `--cwd` is unchanged.

A **profile** (`~/.hive/profiles/<name>.json`) extends that with a default
runtime, cwd, context files seeded into the directory, extra MCP servers, and
an optional operator-selected Forcefield sandbox:

```jsonc
// ~/.hive/profiles/dev.json
{
  "runtime": ["/usr/local/bin/claude", "--dangerously-skip-permissions"],
  "cwd":     "~/work/agents",
  "context": ["/path/to/AGENT-GUIDE.md"],        // seeded into cwd
  "mcp": { "playwright": { "command": "npx", "args": ["-y", "@playwright/mcp"] } },
  "sandbox": {
    "command": "/usr/local/bin/ff",
    "profiles": "/etc/forcefield/forcefield-runner.yaml",
    "profile": "codex-worker"
  }
}
```

```sh
hive spawn --profile dev worker            # profile supplies runtime + cwd
hive spawn --profile dev --cwd ~/x w2 -- claude   # explicit flags win
```

Explicit flags and a trailing `-- CMD` override profile fields. Set
`"no_hive_mcp": true` in a profile to opt out of the automatic `hive` server.
When `sandbox` is present, Hive wraps the resolved runtime as `ff run` after it
has selected the agent identity and workspace. Hive continues to own spawning,
tmux, and lifecycle; Forcefield owns process isolation and external capability
mediation. The sandbox command and profile selection come only from the trusted
Hive profile, never from the agent prompt.

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
Manual client-supplied pane binding is not supported on Windows: use `--pid`
for liveness-only registration. Console control is available for sessions that
the Windows hub itself spawned.
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

## Add an SSH host (on-demand, no install)

`node install` is the heavyweight path: a permanent daemon, a firewall port, a
supervisor. When you just want to *spawn an agent onto a box you can SSH to*,
register it as an SSH host instead — nothing is installed until the first spawn:

```sh
hive hosts add-ssh --profile dev edge me@some-box     # metadata only
hive spawn --host edge worker                          # brings it up + spawns
```

On that first spawn the local hub, over one SSH ControlMaster connection, ships
a hive binary to the target, starts a **transient** daemon bound to the target's
loopback (no firewall, no supervisor), and wires two loopback port-forwards
(`-L`/`-R`) that carry hub↔hub traffic. The remote is then a normal mesh peer
reached through a loopback address, so messaging, discovery, and the spawn
itself reuse the existing paths unchanged — the agent is provisioned (context +
MCP, §"Spawn profiles") by the same machinery as a local spawn. `hive hosts
rm-ssh edge` tears the tunnel and transient daemon down and forgets the host.

The remote gets a **host-local** control token (never the network-wide one), and
both tunnels bind loopback only — nothing is exposed on either host's network.
This is also the clean replacement for a hand-rolled `ssh -L` tunnel to a VM
hub: loopback forwards sidestep macOS Local-Network TCC. See
[docs/ssh-hosts-design.md](docs/ssh-hosts-design.md).

## Security model

- **Two layers.** Control endpoints require the receiving hub's control
  token; possession is the capability. A token may be legacy/shared or
  bound to one `control_host`. Agent tokens (minted at registration) are
  always MSG-layer.
- **Identity.** `from` is server-stamped from the token. No spoofing.
- **Terminal attachment.** Binding a tmux pane requires CONTROL. A MSG
  credential can create a pane-less or PID-bound identity, but cannot select a
  pane that Hive will type into.
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
hive register --name N [--pane %ID [--nudge]] [--pid PID] # --nudge is explicit terminal wake consent
hive run [--name N] [--cwd D] -- CMD...         # foreground command; leased agent identity; no tmux
hive deregister [name]
hive agents [--local] [--json]                  # presence + retained/disposable mailbox policy
hive mcp [--name N] [--list]                    # auto-enrolling stdio agent interface

# CONTROL layer (direct to the target host's hub)
hive hosts  list | add <name> <addr:port> | rm <name>
hive net rotate-control <name>            # fresh host-local token on this hub
hive spawn [--host H] [--cwd D] [--grant-control] [--wait] [--headed] [--nudge] <name> -- CMD...
hive keys [--enter] <agent> <text...>
hive read [--lines N] <agent>
hive kill <agent>
```

Flags come before positionals. Agent-facing docs: **docs/AGENT-GUIDE.md**.
Wire format and semantics: **docs/PROTOCOL.md**.

## Demo

`demo/two-node-demo.sh` runs two hubs on this machine (separate state dirs and a
dedicated tmux socket), joins them into one network, and starts with the
recommended no-tmux path: Alice lazily enrolls by name through `hive mcp`.
It then uses one optional managed tmux worker to demonstrate spawn, keys/read,
nudge, and kill alongside MCP send/recv/ask/answer/broadcast and audit. Safe to
run repeatedly; cleans up after itself. (Needs `python3` for the demo's MCP
client and tmux for the optional-control half.)

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
