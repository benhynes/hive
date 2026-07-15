# Design: SSH hosts — spawn agents onto any SSH-reachable machine

Status: **draft for review, pre-integration.** Branch `ssh-hosts` (worktree),
off `mcp-agent-interface` (needs `hive mcp`).

## 1. The gap

Today hive has exactly one way to add a host: `hive node install <ssh-target>`,
which does a **heavyweight, permanent** provision — cross-compiles + ships the
binary, opens a firewall port, installs a systemd/launchd/schtasks supervisor,
and leaves a full peer daemon running indefinitely. It also assumes the new
host is reachable on the tailnet (binds a tailscale IP, dials back to ours).
Control ops then require that persistent daemon.

What's missing is a **lightweight, on-demand** path: "here's an SSH target,
spawn me an agent there, wired into the mesh with the right context and MCP
servers, and don't make me pre-install a permanent daemon or expose a port."
This is what the directive asks for: *add SSH hosts, spawn agents to any SSH
host, set up with appropriate context and MCP servers.*

Two facts make this natural for hive:

- The **SSH runner already exists** in `cmd/hive/node.go` (`sshRunner`:
  ControlMaster connection multiplexing, `uname` preflight, `platformOf`
  GOOS/GOARCH mapping, `scp`, `writeRemote` that pipes secrets over stdin
  rather than argv). It just needs promoting to `internal/ssh` so the hub can
  use it too.
- The user **already runs this pattern by hand.** Per the perf notes, the
  `main` net reaches the `macos-zlt` VM hub through a persistent loopback SSH
  tunnel (`com.hive.tunnel-zlt`, `127.0.0.1:7778 → vm:7777`) because macOS
  Local-Network TCC blocks the daemon-descended binary from dialing the vmnet
  IP. SSH hosts generalize that manual workaround into a first-class feature —
  and because the tunnels are loopback, they sidestep the same TCC denial, so
  **this design lets the `com.hive.tunnel-zlt` launchd hack be retired.** That
  is a concrete near-term win, not just a new capability.

**Validation:** Model A's tunnel routing is proven end-to-end (a `socat`
proof-of-concept standing in for `ssh -L`/`-R`): a transient daemon reached
*only* through loopback forwards distinct from its bind handled join, spawn,
`agents` fan-out, MCP `hive_send`/reply, control `read`, nudge-with-preview,
and `kill` — the whole existing path, unchanged. The SSH auth/transport layer
itself was not exercised (it is what `node.go`'s `sshRunner` already ships), and
none of §7–§9's new work is covered. See §11.

## 2. Two connectivity models

The spawned agent needs to reach a hub, and control ops (read/keys/kill) need to
reach the agent's pane. There are two ways to arrange this over pure SSH.

### Model A — on-demand peer over an SSH tunnel (RECOMMENDED)

The SSH host runs a **transient** `hive daemon` bound to its own loopback,
brought up on demand over SSH (no firewall, no supervisor, no tailnet). The
origin hub and the remote hub talk to each other through **loopback port
forwards on the one SSH ControlMaster connection** (`-L` origin→remote,
`-R` remote→origin). The agent is a normal tmux agent owned by the remote
daemon; its `HIVE_ADDR` is the remote's own loopback. Control ops flow
origin → (tunnel) → remote hub → local tmux, **exactly as they do between
tailnet peers today** — no new control substrate.

```
origin host                          SSH host
┌───────────────────┐   ssh -L/-R   ┌────────────────────────┐
│ hive daemon :7777 │◀════════════▶│ hive daemon :7777 (loop)│
│  (owns tunnels +  │  ControlMaster│  ├─ worker (tmux)       │
│   remote daemon   │   multiplexed │  └─ hive_* via hive mcp │
│   lifecycle)      │   forwards    │      → remote loopback  │
└───────────────────┘               └────────────────────────┘
       ▲ any client / MCP tool
```

**Why recommended:** reuses the entire existing spawn / control / delivery /
`hive mcp` machinery unchanged. The only genuinely new runtime pieces are
(1) SSH-host registration, (2) transient-daemon bring-up, (3) tunnel lifecycle,
(4) MCP + context provisioning (needed by both models anyway). It also degrades
to the proven manual tunnel the user already runs.

### Model B — daemonless, hub drives remote tmux over SSH

No remote daemon at all. The origin hub owns the agent but its pane lives on the
remote; control ops become `ssh host tmux capture-pane|send-keys|kill-session`.
The agent's `hive mcp` reaches the origin hub over an `-R` reverse tunnel
(`HIVE_ADDR = http://127.0.0.1:<fwd>`).

This needs a **new control backend** — an "SSH-tmux" sibling to
`internal/control`'s local-tmux and Windows-console backends — plus per-op SSH
round trips (slower `read`/`keys`) and a way to route control by the agent's
*location* rather than its *host's hub*. More invasive; higher latency.

**Verdict:** ship Model A first. Keep Model B as a future option for hosts where
even a transient daemon is unwanted (locked-down boxes, one-shot containers).
The MCP/context provisioning (§4) is identical for both, so nothing is wasted.

## 3. The SSH-host lifecycle (Model A)

### Register

```
hive hosts add-ssh <name> <ssh-target> [--profile P] [--dest DIR] [--identity KEY]
```

`ssh-target` is anything ssh understands (`user@host`, an `ssh_config` alias).
Registration is **metadata only** — no connection is made until first spawn.
Stored per-net (see §5). `--profile` names a spawn profile (§4).

### First spawn brings the host up

`hive spawn --host <name> worker -- claude` against an SSH host runs:

1. **Connect** — open a ControlMaster master connection (reused for everything
   below and kept warm; `ControlPersist`).
2. **Preflight** — `uname -s -n -m` → `platformOf` (reuse node.go). Confirm
   tmux ≥ 3.2 present (or error with a clear message).
3. **Ensure binary — ship a version-pinned copy; never trust remote PATH.**
   Version skew is a subtle-failure trap: a remote `hive` without `hive mcp`, or
   on a different wire, breaks spawn in confusing ways. So always run a copy at a
   **versioned dest path** under the remote `HIVE_HOME` (e.g.
   `~/.hive/bin/hive-<version>`), shipping it if absent (self-copy on a matching
   platform, else cross-compile from `--src` — reuse node install's shipping
   logic). "Compatible" must be a real check, not a PATH lookup: extend
   `GET /v1/health` to report the binary version, and treat a mismatch as
   ship-and-replace. This makes the origin binary authoritative and eliminates
   "works on my host" skew.
4. **Ensure transient daemon** — start `hive daemon` on the remote bound to
   `127.0.0.1:<port>` under a dedicated `HIVE_HOME` (e.g. `~/.hive`), if not
   already running. No supervisor, no firewall. Health-check over the tunnel.
   Join it to the net: write the net's msg token (and, if the profile grants
   control, a **host-local** control token) into the remote `net.json` over
   stdin — never argv.
5. **Ensure tunnels** — on the ControlMaster: `-L <origLoopback>:127.0.0.1:<port>`
   (origin → remote hub) and `-R <remoteLoopback>:127.0.0.1:<originPort>`
   (remote hub → origin hub). Register the reciprocal hosts entries pointing at
   the loopback forwards, so hub↔hub delivery and `@all` fan-out just work.
   **Port allocation:** each SSH host needs one origin-side (`-L`) and one
   remote-side (`-R`) loopback port. The hub owns a small allocator that hands
   out free `127.0.0.1` ports and avoids collisions across many SSH hosts;
   allocations are held for the host's lifetime and released on teardown. Prefer
   binding `127.0.0.1:0` and reading back the assigned port over a fixed range,
   so two origin hubs on one machine don't fight.
6. **Provision the agent context + MCP** (§4).
7. **Spawn** — forward a normal spawn request to the remote hub over the `-L`
   tunnel. The remote daemon mints the agent token, injects `HIVE_*` via tmux
   `-e`, and (per the profile) the agent's `hive mcp` is already registered.

### Teardown

The origin hub owns lifecycle. Options (flag / profile setting):

- **warm, with an idle timeout (default):** keep the daemon + tunnels up so
  successive spawns skip the full bring-up (SSH connect + uname + daemon start +
  health check, ~2–5s); reap after an idle period with no agents (e.g. 10 min),
  or on origin-daemon shutdown. An idle loopback daemon costs effectively
  nothing, so paying the bring-up once per burst is the wrong trade.
- **ephemeral (`--once` opt-in):** stop the transient daemon and close the
  tunnels the moment the last agent on the host is killed. For locked-down or
  one-shot boxes where a lingering daemon is unwanted.
- `hive hosts rm <name>` tears everything down and forgets the host.

**Reconnect is the hard part (P3), and the reason it's tractable is that the
registry lives on the remote's disk.** SSH tunnels drop on sleep and network
blips — this is exactly why the manual `com.hive.tunnel-zlt` is a launchd job.
The hub must detect a dead ControlMaster (health-poll `GET /v1/health` over the
`-L` forward), rebuild the master connection, and restart the transient daemon
if it died. Because agent registration and inboxes are persisted on the remote,
a daemon restart re-adopts live tmux agents and loses no mail — so reconnect is
a transport concern, not a state-loss one. `ControlPersist` keeps the master
warm between polls.

Because the origin hub owns tunnels + remote-daemon health, control ops from any
client route transparently — the client never needs to know it's an SSH host.

## 4. Context + MCP provisioning (spawn profiles)

This is the "set up with the appropriate context and MCP servers" half, and it's
**orthogonal to host type** — the same profile should apply to a local spawn, a
tailnet peer, or an SSH host. Introduce a **spawn profile**: a named provisioning
spec applied during spawn.

```jsonc
// ~/.hive/profiles/<name>.json  (or inline net config)
{
  "runtime":   ["claude", "--dangerously-skip-permissions"],  // the agent command
  "cwd":       "~/work/repo",            // working dir on the target
  "repo":      "git@github.com:org/repo.git",  // optional: clone if cwd absent
  "context":   ["CLAUDE.md", "docs/AGENT-GUIDE.md"],  // files seeded into cwd
  "mcp": {                                // MCP servers registered for the agent
    "hive": { "command": "hive", "args": ["mcp"] },   // always; the mesh interface
    "playwright": { "command": "npx", "args": ["-y", "@playwright/mcp"] }
  },
  "secrets":   ["ANTHROPIC_API_KEY"]      // brokered env (see §6); MVP: none
}
```

Provisioning steps, run over SSH before the agent starts:

- **cwd / repo** — ensure `cwd` exists; if a `repo` is set and `cwd` is empty,
  `git clone` it (credentials via §6, not in the clone URL).
- **context files** — write the listed context into `cwd` (or `~/.claude/`), so
  the agent boots already knowing the mesh conventions. Ship the current
  `docs/AGENT-GUIDE.md` content by default so a fresh agent knows how to
  `hive_send`/`hive_recv`.
- **MCP registration — write `.mcp.json` AND pre-seed trust, or the agent
  hangs.** Write a project-scoped `.mcp.json` in `cwd` (Claude Code reads it)
  listing the profile's MCP servers. `hive` is always included; since the
  agent's env already carries `HIVE_*`, `hive mcp` authenticates with zero extra
  config. This automates what is today a manual `claude mcp add hive -- hive mcp`
  step, and is the natural home for other MCP servers per the directive.

  **Critical:** `.mcp.json` alone is not enough. Claude Code shows a "Use this
  MCP server?" trust prompt on first encounter, which an autonomous spawned
  agent cannot answer — it deadlocks on first boot. The provisioner must **also
  pre-approve** the servers: pre-seed the project key in the target user's
  `~/.claude.json` (the same trick the perf notes use for digiwin), or set
  `enableAllProjectMcpServers` in the seeded `.claude/settings.json`. Writing
  the config without pre-seeding trust is the single most likely way this
  feature silently hangs, so it is a functional requirement, not a nicety. This
  is Claude-Code-specific; a non-Claude `runtime` needs its own equivalent (the
  provisioner should treat MCP wiring as per-runtime, see §8.5).
- **runtime** — the tmux session runs `runtime` (default `claude`); `HIVE_*` are
  injected by tmux `-e` as today, so the agent is identity-bound and mesh-aware
  on first prompt.

A `--profile` flag selects one; profile fields are overridable by explicit spawn
flags (`--cwd`, trailing `-- CMD`). No profile = today's behavior plus the `hive`
MCP server auto-registered.

## 5. Config schema

Keep the wire and existing configs untouched; add SSH hosts additively.

```go
type NetConfig struct {
    // ... existing fields ...
    Hosts    map[string]string   `json:"hosts"`              // daemon peers: name -> addr:port (unchanged)
    SSHHosts map[string]SSHHost  `json:"ssh_hosts,omitempty"` // NEW
}

type SSHHost struct {
    Target   string `json:"target"`             // ssh target: user@host or alias
    Dest     string `json:"dest,omitempty"`     // remote install dir (default ~/.local/bin)
    Home     string `json:"home,omitempty"`     // remote HIVE_HOME (default ~/.hive)
    Identity string `json:"identity,omitempty"` // ssh key path
    Profile  string `json:"profile,omitempty"`  // default spawn profile
    Lifecycle string `json:"lifecycle,omitempty"` // "ephemeral" | "warm"
}
```

`GET /v1/nets/{net}/hosts` keeps returning `name→addr:port` for daemon peers; SSH
hosts are a **local, origin-side** concept (like the hosts list itself — no push,
no sync), surfaced in `hive hosts list` with a `type` column. The reciprocal
loopback-tunnel hosts entries created at spawn time reuse the existing `Hosts`
map, so hub↔hub routing needs no change.

## 6. Security

- **Auth** — SSH (keys / agent / `--identity`); reuse ControlMaster so one
  authenticated connection backs every op (matches node install).
- **Tokens never in argv** — msg/control tokens and the agent token go over
  stdin (`writeRemote`) or tmux `-e`, exactly as node install / local spawn do.
- **Tunnels are loopback-only** — both forwards bind `127.0.0.1`; nothing is
  exposed on either host's network. No firewall change needed (vs node install).
- **Control scope** — an SSH host that should be able to spawn/control gets a
  **host-local** control token (`ControlHost` set), so the origin's network-wide
  control token is never copied onto a remote box. MSG-only SSH hosts get no
  control token. This reuses the existing local-control machinery.
- **Secrets brokering** — the `secrets` profile field should resolve via a
  broker (the user's `agent-secret` / warren pattern), injected as remote env at
  spawn, **out of scope for the MVP** — start with none and document the hook.
- **Audit** — spawn/kill on the remote are audit-logged by the remote daemon
  where they happen (unchanged). Origin-side tunnel/daemon lifecycle events
  should be audited on the origin too.

## 7. Integration points (for the follow-up work)

- `internal/ssh/` (NEW) — promote `sshRunner` + `platformOf` + binary-shipping
  out of `cmd/hive/node.go` so both the CLI and the hub can use them. `node
  install` becomes a caller.
- `internal/config/config.go` — add `SSHHost` + `SSHHosts`; Load/Save; a
  `profiles/` loader.
- `cmd/hive/hosts.go` — `hive hosts add-ssh` / `rm`; `list` shows type.
- `internal/hub/` (NEW `sshhosts.go`) — the SSH-host manager owned by the hub:
  transient-daemon bring-up (version-pinned binary ship, §3.3), a **loopback
  port allocator** (§3.5), tunnel lifecycle with **health-poll + reconnect**
  (§3 teardown), and teardown policy (warm/idle-timeout by default). The spawn
  path checks "is target an SSH host?" and, if so, ensures the host up then
  forwards the spawn over the tunnel.
- `internal/hub/provision.go` (NEW) — apply a spawn profile: cwd/repo, context
  files, `.mcp.json` **plus MCP-trust pre-seed** (§4 — else the agent hangs on
  the trust prompt), secrets hook. Runs over SSH for SSH hosts and locally for
  local spawns (MCP auto-registration for everyone). MCP wiring is per-runtime
  (Claude Code today).
- `internal/config` profiles + `cmd/hive` `--profile` flag on spawn.
- Docs: extend `docs/PROTOCOL.md` (SSH-host lifecycle, tunnel model) and
  `README.md` ("Add an SSH host" beside "Add a second host").

## 8. Decisions (resolved)

All five are settled; integration follows these.

1. **Orchestrator: the hub owns SSH-host lifecycle.** Control ops from any client
   route transparently and the tunnels + remote daemon survive the spawning CLI
   process exiting. (CLI-orchestration rejected: tunnels would die with the CLI
   and only that client could drive the agent.)
2. **Default lifecycle: warm with an idle timeout.** Ephemeral re-pays the full
   ~2–5s bring-up on every first-spawn for no real resource saving; warm skips it
   for successive spawns and self-cleans on idle. `--once` opts into strict
   ephemeral for locked-down / one-shot hosts. (Flipped from the initial
   ephemeral-default recommendation.)
3. **Profiles: shared `~/.hive/profiles/*.json`, referenced by name.** Reusable
   across nets and hosts; not inlined per-net.
4. **MCP auto-registration for ALL spawns: yes.** Local/tailnet spawns also
   auto-write `.mcp.json` (+ trust pre-seed, §4) with `hive`, removing the manual
   `claude mcp add` step everywhere. Gated behind the profile so it's
   opt-out-able.
5. **Model A only; do NOT pre-build a Model B control-backend seam.** Model A
   needs *zero* new control backend (its whole appeal), so an abstraction for the
   deferred Model B would be speculative YAGNI, and `internal/control` already
   carries an OS-axis seam (tmux vs Windows) that a runtime-axis seam would
   muddy. If Model B ever ships it is a localized refactor. Keep Model A concrete.
   (Note: MCP *provisioning* is per-runtime — Claude-Code-specific today, §4 — but
   that is a provisioner concern, not a control-backend seam.)

## 9. Phased plan

- **P1 — registration + provisioning (no SSH host yet):** spawn profiles +
  `.mcp.json`/context auto-provisioning **including MCP-trust pre-seed** (§4)
  applied to **local** spawns. Delivers the "appropriate context and MCP servers"
  half immediately, testable without SSH — and the trust pre-seed must be
  verified here (spawn a local agent, confirm it boots into `hive_recv` with no
  trust prompt) since it's the likeliest silent-hang.
- **P2 — SSH host bring-up:** `hosts add-ssh`, version-pinned binary ship,
  transient daemon over SSH, port allocator + tunnel manager, forward-spawn.
  Reuse node.go shipping. e2e against an OrbStack/tart VM.
- **P3 — lifecycle + control transparency:** warm/idle teardown, health-poll +
  reconnect (the hard part — rebuild ControlMaster + restart remote daemon,
  re-adopting persisted agents), audit; confirm read/keys/kill route through the
  tunnel from a third-party client.
- **P4 (optional) — Model B** daemonless SSH-tmux backend for minimal hosts;
  **secrets brokering** via the agent-secret/warren hook.

## 10. What this reuses vs builds

Reuses (large): `sshRunner`/ControlMaster, `platformOf`, binary shipping,
`writeRemote` stdin-tokens, the whole spawn/control/delivery path, `hive mcp`,
host-local control tokens, hub↔hub loopback-tunnel routing (already proven in the
user's `main` net).

Builds (small, well-scoped): SSH-host registration + schema, the hub's SSH-host
manager (transient daemon + port allocator + tunnel lifecycle + reconnect), the
profile provisioner (`.mcp.json` + trust pre-seed + context + repo), and CLI
surface (`hosts add-ssh`, `--profile`).

## 11. Validation status

Model A's load-bearing claim — *a transient daemon reached over a loopback
tunnel reuses the entire existing spawn / control / delivery / MCP path
unchanged* — is proven end-to-end with a `socat` proof-of-concept standing in
for `ssh -L`/`-R`. A "remote" hive daemon reached **only** through loopback
forwards distinct from its bind handled, through those forwards:

- **join** (remote joined the net via the `-R` reverse forward, learned origin);
- **spawn** onto the remote, driven from origin, via `-L`;
- **`agents` fan-out** discovering `worker@hostr` across the forward;
- **MCP `hive_send`** origin→remote (via `-L`), received by the agent through its
  own `hive_recv` to its local (remote) hub;
- **reply** remote→origin routed back via `-R`;
- **control `read`** via `-L`, and the **nudge-with-preview** reaching the remote
  pane;
- **kill** through the forward.

Not covered (and not claimed): the SSH auth/transport layer itself (that is what
`node.go`'s `sshRunner` already ships and proves), and all of §7–§9's genuinely
new work — transient-daemon bring-up over real SSH, port allocation, tunnel
lifecycle/reconnect, and the provisioner. Those are the integration deliverables.
