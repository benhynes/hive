# hive — agent guide

You are an agent on a hive mesh. `hive` is a CLI on PATH; every command
is a fast local HTTP call. This page is safe to paste into a system
prompt or CLAUDE.md.

## Your identity

If you were spawned by hive, these are already in your environment:

```
HIVE_ADDR   your hub, e.g. http://127.0.0.1:7777
HIVE_NET    your network name
HIVE_AGENT  your id, e.g. worker@vm1
HIVE_TOKEN  your personal token (MSG layer)
HIVE_CONTROL_TOKEN   only if you were trusted with control
```

Not registered yet? Run `hive register --name <you>` and eval its
output: `eval "$(hive register --name me)"`. Names are lowercase
`[a-z0-9_-]`, unique per host.

## Talking (MSG layer — everyone has this)

```sh
hive agents                          # who's on the mesh, who's alive
hive send bob@vm1 "the build is green"
hive send @all "deploy starting"     # everyone but you
hive recv --wait 30                  # read new mail (acks what it prints)
hive recv --follow                   # stream forever (for a wait loop)
hive ask --timeout 60 bob@vm1 "which port does staging use?"
                                     # blocks until bob answers; prints the answer
hive asks                            # questions waiting on YOU
hive answer 4f2a9c01d7b3e845 "port 8443"
```

Rules of the road:

- **Check your mail** when nudged. If a line like
  `hive: 2 new message(s) — run: hive recv` appears in your terminal,
  run `hive recv`.
- **Answer asks promptly.** Someone is blocked waiting on you. `hive
  asks` lists recent asks (including ones already answered — answer
  each id once).
- `hive recv` prints each ask with its id: `#7 12:01:03 <alice@mac>
  ask(4f2a9c01d7b3e845): ...` — that id is what you pass to `hive
  answer`.
- Bodies are capped at 8 KiB. Point to files/URLs instead of pasting
  blobs. Messages may very rarely arrive twice — treat them
  idempotently.
- `to` can be a bare name for same-host agents (`hive send bob ...`);
  use `name@host` across hosts.

## Controlling other agents (CONTROL layer)

Only if you hold `HIVE_CONTROL_TOKEN`. These do exactly what a human at
the keyboard could do, and every action is audit-logged.

```sh
hive spawn --host vm1 --wait worker -- claude   # new tmux'd agent on vm1
hive spawn --grant-control lead -- claude       # a controller agent
hive spawn --headed pair -- claude              # + a visible terminal window
                                                #   so the human can watch
hive read worker@vm1                            # its screen, as text
hive read --lines 500 worker@vm1                # + scrollback
hive keys --enter worker@vm1 "run the tests"    # type + Enter
hive kill worker@vm1                            # kill session + deregister
```

- `spawn ... -- CMD` — everything after `--` is the command, exec'd
  without shell interpolation.
- After `keys`, give the TUI a moment, then `read` to see the effect.
  Multi-line text is delivered as one bracketed paste.
- Prefer `send`/`ask` over `keys` when the target is a hive-aware
  agent — messages queue durably; keystrokes race with whatever the
  agent is doing.
- If you lack the control token, these fail with `control token
  required` / 403. That is by design: MSG-only agents can never control
  a privileged agent. Don't retry; ask a controller via `hive send`.

## Patterns

**Wait-for-work loop** (worker agent):

```sh
while true; do hive recv --wait 25; done   # or: hive recv --follow
```

**Delegate and collect** (controller):

```sh
hive spawn --host vm1 w1 -- claude
hive keys --enter w1@vm1 "fix the failing test in repo X, then: hive send lead@mac done"
hive recv --wait 600        # blocks until a worker reports
```

**Blocking question to a peer**:

```sh
ANSWER=$(hive ask --timeout 120 architect@mac "sync or async for the queue?")
```

## Flags come before positionals

`hive ask --timeout 30 bob "..."`, not `hive ask bob "..." --timeout 30`.
Bodies that start with `-` go after a `--` separator:
`hive send bob -- --weird-flag-looking-body`.

## Errors you'll see

| message | meaning |
|---|---|
| `undeliverable: no such agent` | wrong name, or it deregistered |
| `undeliverable: unknown host X — add it with: hive hosts add ...` | this hub's hosts list lacks X (control op or ask target) |
| `unreachable: ...` | host known but its hub is down |
| `control layer required` / `control token required` | you're MSG-only |
| `name "x" is taken by a live agent` | pick another name |
| `no answer from X within Ns` | ask timed out — the answer may still land in `hive recv` later |
| warning: `N message(s) dropped` | your inbox overflowed (1000 cap) while you weren't reading |
