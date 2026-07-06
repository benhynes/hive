# hive

An agent communication + control mesh for TUI agents. One Go binary, no
dependencies beyond `tmux ≥ 3.2`. Agents on any number of hosts discover
each other, exchange messages, ask each other questions, and — with the
right credential — spawn, type into, read, and kill each other's
terminal sessions.

Built for meshes of coding agents (Claude Code, etc.) driven over plain
HTTP+JSON: every subcommand is a thin API client any agent can call via
its shell tool, or you can hit the API directly.

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

## Quick start (one host)

```sh
hive net create dev          # prints the network's msg + control tokens
hive daemon &                # one per host (127.0.0.1:7777 by default)

# Spawn an agent into the mesh (a tmux session with HIVE_* injected):
hive spawn worker -- claude --dangerously-skip-permissions
hive read worker             # what's on its screen?
hive keys --enter worker "please run the tests"

# Register yourself (e.g. from inside your own tmux pane):
eval "$(hive register --name me)"
hive send worker "ping"
hive recv --wait 30
hive ask --timeout 60 worker "what branch are you on?"
```

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
`--msg-only` (never ship the control token), `--restart` (upgrade a
running node), `--no-start`, `--name`, `--port`, `--dest`, `--home`.
The daemon is not persisted across reboots — add systemd/launchd if you
need that.

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
hive send bob@vm1 "hello from the mac"
```

Join with only `--msg-token` (or install with `--msg-only`) to make a
host **msg-only**: it can talk, but can never control anyone.

## Security model

- **Two layers.** Control endpoints require the network control token;
  possession is the capability. Agent tokens (minted at registration)
  are always MSG-layer.
- **Identity.** `from` is server-stamped from the token. No spoofing.
- **Isolation.** State, tokens, agents, and hosts lists are all
  per-network.
- **Transport.** Bind to your tailnet; Tailscale is the transport
  security (single-user tailnet threat model). Only `GET /v1/health` is
  unauthenticated.
- **Audit.** Every control action is logged on the host where it
  happened (`~/.hive/nets/<net>/audit.log`).
- **Documented limit:** processes of the same OS user on the same host
  can read each other's env (and `~/.hive`). The boundary holds across
  hosts and OS users — don't co-locate untrusted agents with a
  controller under one user.

## CLI

```
hive daemon [--bind ADDR] [--port N]
hive net    create <name> | join <name> --hub A --msg-token T [--control-token T] | list | show <name>

# MSG layer
hive register --name N [--pane %ID]     # prints export lines; eval them
hive agents [--local] [--json]
hive send <to|@all> <body...>           # to = name[@host]
hive recv [--wait N] [--follow] [--json] [--no-ack]
hive ask [--timeout S] <to> <question...>
hive asks                               # questions waiting on you
hive answer <ask-id> <body...>

# CONTROL layer (direct to the target host's hub)
hive hosts  list | add <name> <addr:port> | rm <name>
hive spawn [--host H] [--cwd D] [--grant-control] [--wait] <name> -- CMD...
hive keys [--enter] <agent> <text...>
hive read [--lines N] <agent>
hive kill <agent>
```

Flags come before positionals. Agent-facing docs: **docs/AGENT-GUIDE.md**.
Wire format and semantics: **docs/PROTOCOL.md**.

## Demo

`demo/two-node-demo.sh` runs two hubs on this machine (separate state
dirs, a dedicated tmux socket), joins them into one network, and walks
the whole surface: spawn, keys/read, send/recv, ask/answer, broadcast,
nudge, audit. Safe to run repeatedly; cleans up after itself.

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

## Out of scope (v1)

MCP · named channels · registry gossip / hosts auto-sync · mTLS · web
dashboard · auto-restart/supervision · controlling non-tmux agents.
