#!/usr/bin/env bash
# two-node-demo.sh — two hive hubs on one machine, full mesh tour.
#
# Simulates two hosts ("hosta", "hostb") with separate HIVE_HOME state
# dirs, separate ports, and a dedicated tmux socket. Walks: net create/
# join, register, send/recv, ask/answer, broadcast, spawn/keys/read,
# nudge, layer enforcement, kill, audit log. Cleans up after itself.
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

step "register alice on hosta (message-only external agent)"
REG=$(A register --name alice --pane "")
echo "$REG"
alice() {  # act as alice: only her exports, no local net.json fallback
  env HIVE_HOME=$ROOT/empty HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET \
    $(echo "$REG" | sed 's/^export //') "$BIN" "$@"
}

step "spawn 'worker' on hostb, driven from hosta (control goes direct)"
run A spawn --host hostb --wait worker -- cat

step "mesh-wide discovery"
run A agents

step "type into worker's pane from hosta, read its screen back"
run A keys --enter worker@hostb "hello from hosta"
sleep 0.4
run A read worker@hostb

step "alice sends mail to worker; the idle pane gets nudged (body never injected)"
run alice send worker@hostb "psst — status report please"
sleep 1.2
run A read worker@hostb

step "worker checks mail (as the spawned agent would: its own HIVE_* env)"
WENV=$(tmux -L "$HIVE_TMUX_SOCKET" show-environment -t hive-dev-worker 2>/dev/null | grep '^HIVE_' || true)
worker() { env HIVE_HOME=$ROOT/empty HIVE_TMUX_SOCKET=$HIVE_TMUX_SOCKET $WENV "$BIN" "$@"; }
run worker recv

step "ask/answer: alice asks worker, worker answers, alice gets it (blocking)"
( ASK_ID=""
  for _ in $(seq 1 50); do
    ASK_ID=$(worker asks | awk '/from alice@hosta/{print $1; exit}')
    [ -n "$ASK_ID" ] && break
    sleep 0.2
  done
  worker answer "$ASK_ID" "all systems nominal" >/dev/null
) &
ANSWERER=$!
run alice ask --timeout 30 worker@hostb "status?"
wait "$ANSWERER"

step "broadcast from alice"
run alice send @all "stand-up in 5"
run worker recv

step "layer enforcement: alice (MSG) cannot control anyone"
if alice read worker@hostb 2>&1; then
  echo "UNEXPECTED: msg-layer agent controlled a pane"; exit 1
fi

step "kill worker from hosta"
run A kill worker@hostb
run A agents

step "audit trail on hostb (where the control actions happened)"
sed 's/^/  /' "$ROOT/home-b/nets/dev/audit.log"

step "demo complete — cleaning up"
