#!/usr/bin/env bash
# two-node-demo.sh — two hive hubs on one machine, full mesh tour.
#
# Simulates two hosts ("hosta", "hostb") with separate HIVE_HOME state
# dirs, separate ports, and a dedicated tmux socket. Walks: net create/join,
# lazy MCP enrollment without tmux, MCP messaging (send/recv/ask/answer/
# broadcast), then optional managed spawn/keys/read/nudge/kill and audit. The
# tmux worker demonstrates terminal control; messaging itself has no tmux
# dependency and is driven through `hive mcp`.
#
# Needs: go, curl, python3, and tmux for the managed-control half.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/hive-demo.XXXXXX")
export HIVE_TMUX_SOCKET=hive-demo
PORT_A=${PORT_A:-7801}
PORT_B=${PORT_B:-7802}
BIN=$ROOT/hive
DAEMONS=()

cleanup() {
  for pid in "${DAEMONS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  tmux -L "$HIVE_TMUX_SOCKET" kill-server 2>/dev/null || true
  rm -rf "$ROOT"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
run()  { printf '\033[36m$ %s\033[0m\n' "$*"; "$@"; }

# rpc IDENTITY_FN TOOL ARGS_JSON — perform one hive_* MCP tool call as the
# given identity and print the tool's text result. This is exactly what a
# spawned agent does: `hive mcp` is its stdio MCP server, reading its HIVE_*
# env. Messaging has no CLI; these tools are the interface.
rpc() {
  local identity_fn=$1 tool=$2 args=$3 line result
  local matched=0 rpc_status=0
  coproc HIVE_MCP { "$identity_fn"; }
  local in_fd=${HIVE_MCP[1]} out_fd=${HIVE_MCP[0]} mcp_pid=$HIVE_MCP_PID

  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}' \
    "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}" \
    >&"$in_fd"

  # Keep stdin open until the tool result arrives. EOF owns the MCP session's
  # cancellation boundary, so the old one-way pipeline correctly (but too
  # early) cancelled its own in-flight HTTP request.
  while IFS= read -r line <&"$out_fd"; do
    if result=$(python3 -c '
import sys, json
m = json.loads(sys.stdin.read())
if m.get("id") != 2:
    raise SystemExit(1)
r = m["result"]
text = "".join(c.get("text", "") for c in r.get("content", []))
sys.stdout.write(("ERROR: " if r.get("isError") else "") + text)' <<<"$line"); then
      matched=1
      printf '%s\n' "$result"
      [[ $result != ERROR:* ]] || rpc_status=1
      break
    fi
  done

  exec {in_fd}>&-
  wait "$mcp_pid" || rpc_status=1
  exec {out_fd}>&-
  (( matched == 1 )) || rpc_status=1
  return "$rpc_status"
}

step "build"
go build -o "$BIN" ./cmd/hive
echo "built $BIN"

step "start two hubs (hosta:$PORT_A, hostb:$PORT_B)"
for h in a b; do
  home=$ROOT/home-$h
  mkdir -p "$home"
  port=$PORT_A; [ "$h" = b ] && port=$PORT_B
  printf '{"host_name":"host%s","bind":"127.0.0.1","port":%d}\n' "$h" "$port" > "$home/config.json"
  HIVE_HOME=$home "$BIN" daemon > "$ROOT/daemon-$h.log" 2>&1 &
  DAEMONS+=($!)
done
for port in $PORT_A $PORT_B; do
  for _ in $(seq 1 100); do
    curl -sf "http://127.0.0.1:$port/v1/health" >/dev/null 2>&1 && break
    sleep 0.05
  done
  curl -sf "http://127.0.0.1:$port/v1/health"; echo
done

A() { HIVE_HOME=$ROOT/home-a "$BIN" "$@"; }   # CLI on hosta (holds control)
B() { HIVE_HOME=$ROOT/home-b "$BIN" "$@"; }   # CLI on hostb

step "create network 'dev' on hosta"
run A net create dev
MSG_TOK=$(A net show dev | awk '/msg token:/{print $3}')
CTL_TOK=$(A net show dev | awk '/control token:/{print $3}')

step "hostb joins (learns hosta's name from /health); hosta learns hostb"
run B net join dev --hub 127.0.0.1:$PORT_A --msg-token "$MSG_TOK" --control-token "$CTL_TOK"
run A hosts add hostb 127.0.0.1:$PORT_B
run A hosts list

anonymous_mcp() { env HIVE_HOME=$ROOT/home-a HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET "$BIN" mcp; }
alice_mcp()     { env HIVE_HOME=$ROOT/home-a HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET "$BIN" mcp --name alice; }
alice_tools()   { env HIVE_HOME=$ROOT/home-a HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET "$BIN" mcp --name alice --list; }

step "MCP joins the mesh without tmux (generated disposable identity)"
run rpc anonymous_mcp hive_agents '{}'

step "choose stable MCP identity 'alice' (named mailbox survives offline)"
run rpc alice_mcp hive_agents '{}'

step "spawn 'worker' on hostb, driven from hosta (control goes direct)"
run A spawn --host hostb --wait --nudge worker -- sh

step "mesh-wide discovery"
run A agents

step "type into worker's pane from hosta, read its screen back"
run A keys --enter worker@hostb "printf 'hello from hosta\\n'"
sleep 0.4
run A read worker@hostb

step "alice sends mail via her hive_send tool; the opted-in idle pane gets a fixed nudge notice"
run rpc alice_mcp hive_send '{"to":"worker@hostb","body":"psst — status report please"}'
sleep 1.2
run A read worker@hostb

step "worker reads its mail via hive_recv (as the spawned agent would: its own HIVE_* env)"
WENV=$(tmux -L "$HIVE_TMUX_SOCKET" show-environment -t hive-dev-worker 2>/dev/null | grep '^HIVE_' || true)
worker_mcp() { env HIVE_HOME=$ROOT/empty HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET $WENV "$BIN" mcp; }
run rpc worker_mcp hive_recv '{}'

step "the messaging interface is MCP — worker's offered tools:"
env HIVE_HOME=$ROOT/empty HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET $WENV "$BIN" mcp --list

step "ask/answer over MCP: alice asks (blocks), worker answers, alice gets it"
( ASK=""
  for _ in $(seq 1 50); do
    ASK=$(rpc worker_mcp hive_asks '{}' | python3 -c 'import sys,json
a=json.load(sys.stdin); print(a[0]["ask_id"] if a else "")' 2>/dev/null || true)
    [ -n "$ASK" ] && break
    sleep 0.2
  done
  rpc worker_mcp hive_answer "{\"ask_id\":\"$ASK\",\"body\":\"all systems nominal\"}" >/dev/null
) &
ANSWERER=$!
run rpc alice_mcp hive_ask '{"to":"worker@hostb","question":"status?","timeout":30}'
wait "$ANSWERER"

step "broadcast from alice"
run rpc alice_mcp hive_send '{"to":"@all","body":"stand-up in 5"}'
run rpc worker_mcp hive_recv '{}'

step "layer enforcement: Alice's MSG-only MCP surface omits control tools"
ALICE_TOOLS=$(alice_tools)
printf '%s\n' "$ALICE_TOOLS"
if printf '%s\n' "$ALICE_TOOLS" | grep -q '^hive_read$'; then
  echo "UNEXPECTED: msg-layer MCP exposed a control tool"; exit 1
fi

step "kill worker from hosta"
run A kill worker@hostb
run A agents

step "audit trail on hostb (where the control actions happened)"
sed 's/^/  /' "$ROOT/home-b/nets/dev/audit.log"

step "demo complete — cleaning up"
