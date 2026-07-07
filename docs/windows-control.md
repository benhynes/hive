# hive control on Windows

hive's control layer — spawn a session, read what's on its screen, type
into it, kill it — is tmux on Unix. tmux does not exist on Windows, so
the Windows hub implements the same operations against the classic
**console API**. The seam is `internal/control`: shared package-level
functions, `tmux`-backed behind `//go:build !windows`, console-backed
behind `//go:build windows`. The hub and CLI call `control.*` and never
see the difference.

## The mechanism

Each spawn gets its **own console**. `CreateProcess` with
`CREATE_NEW_CONSOLE` (headed) or `CREATE_NO_WINDOW` (headless) plus
`CREATE_UNICODE_ENVIRONMENT` starts the child; `STARTF_USECOUNTCHARS`
sizes the screen buffer to 220×2050 (the tmux `-x 220` width with ~2000
lines of scrollback). Injected env vars ride the UTF-16 environment
block.

To read or type, a process must **attach** to the child's console
(`AttachConsole(pid)`), then open `CONOUT$`/`CONIN$`:

- **read** — `GetConsoleScreenBufferInfo` for the window rect, then
  `ReadConsoleOutputCharacterW` row by row → the rendered character grid.
- **keys** — `WriteConsoleInputW` with KEY_EVENT records (down+up per
  UTF-16 code unit; `VK_RETURN` for Enter).
- **liveness / kill** — `OpenProcess` + `WaitForSingleObject`, and
  `taskkill /T /F` (whole tree) with `TerminateProcess` as fallback.

A process can attach to only **one** console at a time, and
`FreeConsole` would invalidate the daemon's own stdio. So every
attach-requiring op runs in a **short-lived helper**: the daemon
re-execs itself as `hive __conop <op> <pid> <creation> [arg]` with
`DETACHED_PROCESS`, communicating over pipes (unaffected by console
attach/detach). This mirrors tmux's exec-per-op shape and keeps console
control signals away from the daemon. Spawn, liveness, and kill need no
console and run in-process.

This was validated empirically before implementation with a
kernel32-only probe on a real host (Win10 22H2, AMD64, classic conhost):
non-parent `AttachConsole` succeeds, the grid reads back, injected keys
echo and execute, and env injection lands. End-to-end `hive spawn / read
/ keys / kill` against that host then confirmed the full path through
the normal commands.

## Pane identity and the pid-reuse defense

A Unix pane is a tmux id like `%3`. A Windows pane is
`win:<pid>:<creation-filetime>` — the console-owning pid plus its
process-creation timestamp. Windows recycles small-integer pids quickly,
so a bare pid would let capture/keys/kill race onto an unrelated process
after the original exits. Two guards close this:

1. **Timestamp check.** Every op re-opens the pid and confirms the
   creation FILETIME still equals the stamp in the pane id before it
   attaches or kills. A reused pid fails the check (used e.g. after a
   daemon restart, where no handle was held across the gap).
2. **Handle pinning.** The op holds the `OpenProcess` handle across the
   whole operation. Windows will not recycle a pid while a handle to its
   process object is open, so the pid cannot be reused mid-op — even
   `taskkill /PID`, which re-resolves by pid, is safe.

`ProcStartEpoch` (the registry's per-pid liveness stamp) uses the same
creation FILETIME, formatted `ft:<n>`.

## Concurrency

Two helpers may legally attach to one console at once and interleave
input or tear a read. tmux serializes through its server; the Windows
backend serializes with a per-pane mutex in the daemon around every
console op and kill.

## Deployment notes

- **Non-persist daemon** (`Start-Process`): the daemon may own a hidden
  console. Note that Windows OpenSSH terminates a session's process tree
  on disconnect, so a daemon started directly inside an ssh command does
  not outlive the session — start it detached (a scheduled task, or
  `Win32_Process.Create`) or use `--persist`.
- **`--persist` daemon** (`schtasks ONSTART` as SYSTEM): runs in
  **Session 0**. Control (attach/read/write/kill) works there — it is
  session-internal — but console **windows are never user-visible**, so
  `--headed` is meaningless. Because the daemon runs as SYSTEM, spawned
  children run as SYSTEM too; this is an escalation to weigh (the Unix
  `--persist` path likewise runs children as root under a system-wide
  unit). Prefer a dedicated low-privilege service account for untrusted
  meshes.
- **`--headed`** is decided at spawn time (`CREATE_NEW_CONSOLE`): a
  `CREATE_NO_WINDOW` console has no window to reveal later. It requires
  the daemon in the interactive user session.

## Known limitations (v1)

- **No bracketed paste — multi-line text submits line by line.** conhost
  has no bracketed-paste mode, so `Paste` normalizes newlines to `\r`
  and types line by line; each `\r` is a submission. This affects the
  *primary* driving path: a multi-line prompt sent to a TUI agent (e.g.
  Claude Code) submits its first line before the rest is typed, where
  tmux would deliver the whole block as one paste event. Prefer
  single-line `keys` to Windows agents, or send the block in one line.
- **Command is exec'd directly, not shell-interpreted.** On Unix a spawn
  command runs under tmux's implicit `sh -c`, so `spawn x -- "foo | bar"`
  and globs/redirects work. On Windows the command is resolved with
  `LookPath` and run via `CreateProcess` with no shell, so shell
  metacharacters are literal. Pass an explicit shell (`cmd /c ...`) if
  you need one. This suits hive's single-binary agent case but is a
  semantic split behind one interface.
- **Breakaway grandchildren.** `taskkill /T` walks a parent→child
  snapshot; a descendant that broke away from the tree (its own job, or
  `DETACHED_PROCESS`) can survive a kill. If the console-owning process
  exits while such descendants keep the console alive, the pane reports
  dead while work continues — divergent from tmux, where a pane lives as
  long as any process holds it. A Job Object per session is the fix and
  is deferred.
- **Non-atomic capture.** Rows are read one at a time; a child scrolling
  mid-read can tear the snapshot (tmux's capture is atomic).
- **Raw-mode TUIs.** Typed characters carry `UnicodeChar` (and
  `VK_RETURN` for Enter) but no other virtual-key codes, so a TUI that
  dispatches on VK for Backspace/Tab/Esc/arrows may not respond. There
  is no control-key/interrupt injection path.
- **Exec-per-op latency.** Each console op spawns a helper (~tens of ms);
  a readiness wait that polls capture spawns several. A persistent helper
  is a future optimization.

## Strategic note

The classic-conhost path is legacy. **ConPTY** (`CreatePseudoConsole`)
would let the daemon hold the pseudo-console handles directly — no pid
reuse, a real VT stream (clean framing, atomic reads), single owner (no
interleave). The reason to stay on classic conhost is `--headed`: ConPTY
has no revealable window. A hybrid — ConPTY when headless,
`CREATE_NEW_CONSOLE` only when headed — is the natural next step.
