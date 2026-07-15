#!/usr/bin/env bash
# sshshim.sh — an `ssh` stand-in for the SSH-hosts e2e. The "remote" is this
# same machine's loopback, so remote commands run locally and -O port-forwards
# are realized with socat (localhost->localhost). Enough of the ssh surface for
# internal/hub/sshhosts.go to drive a real transient daemon over "tunnels".
#
# Invoked as $HIVE_SSH_BIN. Tracks socat pids under $HIVE_SHIM_DIR for -O exit.
set -u
dir="${HIVE_SHIM_DIR:?}"
mkdir -p "$dir"

args=("$@")
# Strip ssh options: -o KEY=VAL (2 tokens), -i KEY (2 tokens), lone flags.
rest=()
i=0
octl=""      # -O value (forward|exit)
fwdL=""; fwdR=""
while [ $i -lt ${#args[@]} ]; do
  a="${args[$i]}"
  case "$a" in
    -o) i=$((i+2)); continue ;;
    -i) i=$((i+2)); continue ;;
    -O) octl="${args[$((i+1))]}"; i=$((i+2)); continue ;;
    -L) fwdL="${args[$((i+1))]}"; i=$((i+2)); continue ;;
    -R) fwdR="${args[$((i+1))]}"; i=$((i+2)); continue ;;
    -q) i=$((i+1)); continue ;;
    *) rest+=("$a"); i=$((i+1)) ;;
  esac
done

# rest[0] = target; rest[1..] = the remote command (single string in our use).
target="${rest[0]:-}"
cmd="${rest[*]:1}"

if [ "$octl" = "exit" ]; then
  if [ -f "$dir/socat.pids" ]; then
    while read -r p; do kill "$p" 2>/dev/null; done < "$dir/socat.pids"
    rm -f "$dir/socat.pids"
  fi
  exit 0
fi

if [ "$octl" = "forward" ]; then
  # socat MUST redirect all fds: a backgrounded child that inherits our stdout
  # (a pipe back to the caller) keeps that pipe open, hanging the caller's read.
  if [ -n "$fwdL" ]; then
    # 127.0.0.1:LP:127.0.0.1:RP  -> proxy LP to RP
    lp="$(echo "$fwdL" | cut -d: -f2)"; rp="$(echo "$fwdL" | cut -d: -f4)"
    socat "TCP-LISTEN:$lp,bind=127.0.0.1,reuseaddr,fork" "TCP:127.0.0.1:$rp" >/dev/null 2>&1 &
    echo $! >> "$dir/socat.pids"
    exit 0
  fi
  if [ -n "$fwdR" ]; then
    # 127.0.0.1:0:127.0.0.1:OP -> allocate a port, proxy it to OP, print it
    op="$(echo "$fwdR" | cut -d: -f4)"
    ap="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
    socat "TCP-LISTEN:$ap,bind=127.0.0.1,reuseaddr,fork" "TCP:127.0.0.1:$op" >/dev/null 2>&1 &
    echo $! >> "$dir/socat.pids"
    echo "Allocated port $ap for remote forward"
    exit 0
  fi
  exit 0
fi

# A remote command: run it locally, passing stdin through (WriteRemote pipes
# it). Redirect nothing here EXCEPT: the caller reads our stdout, so any
# backgrounded grandchild (the daemon) must self-redirect — which it does.
exec sh -c "$cmd"
