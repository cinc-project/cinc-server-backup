#!/usr/bin/env bash
# End-to-end backup and restore against a real cinc-server-ng.
#
# Starts a source server loaded with a fixture (--state), backs it up, verifies
# the backup, restores it into a fresh server, backs that up, and diffs the two
# backups. Exits non-zero on the first failure, including any difference.
#
# Usage: scripts/e2e-ng.sh <fixture-dir>
#   NG_IMAGE  container image to run, e.g. ghcr.io/cinc-project/cinc-server-ng:0.20.0
#   NG_BIN    or: path to a cinc-server-ng binary (used when NG_IMAGE is unset)
#   WORKDIR   where keys, logs and backups go (default: a new temp dir)
set -euo pipefail

fixture=$(cd "${1:?usage: $0 <fixture-dir>}" && pwd)
work=${WORKDIR:-$(mktemp -d)}
mkdir -p "$work/keys"
src_port=18889
dst_port=18890
pids=()
containers=()

cleanup() {
  for c in "${containers[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

# start <name> <port> <args...>: run a server listening on 127.0.0.1:<port>.
start() {
  local name=$1 port=$2
  shift 2
  if [[ -n "${NG_IMAGE:-}" ]]; then
    # Run as the invoking user so the admin key it writes is readable here.
    docker run -d --name "e2e-$name" --user "$(id -u):$(id -g)" \
      -p "127.0.0.1:$port:8889" -v "$work/keys:/keys" -v "$fixture:/fixture:ro" \
      "$NG_IMAGE" --addr 0.0.0.0:8889 --key-out "/keys/$name.pem" "${@//@FIXTURE@//fixture}" >/dev/null
    containers+=("e2e-$name")
  else
    "${NG_BIN:?set NG_IMAGE or NG_BIN}" --addr "127.0.0.1:$port" --key-out "$work/keys/$name.pem" \
      "${@//@FIXTURE@/$fixture}" >"$work/$name.log" 2>&1 &
    pids+=($!)
  fi
  for _ in $(seq 60); do
    if curl -fsS "http://127.0.0.1:$port/_status" >/dev/null 2>&1 && [[ -s "$work/keys/$name.pem" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "server $name did not come up" >&2
  if [[ -n "${NG_IMAGE:-}" ]]; then docker logs "e2e-$name" >&2; else cat "$work/$name.log" >&2; fi
  return 1
}

repo=$(cd "$(dirname "$0")/.." && pwd)
csb="$work/cinc-server-backup"
(cd "$repo" && go build -o "$csb" ./cmd/cinc-server-backup)

echo "== starting the source (loaded from $fixture) and an empty target"
start src "$src_port" --state @FIXTURE@
start dst "$dst_port" --orgs unused

echo "== backing up the source"
"$csb" backup --server "http://127.0.0.1:$src_port" --key "$work/keys/src.pem" -o "$work/source.csb"
"$csb" verify "$work/source.csb"
"$csb" inspect "$work/source.csb"

echo "== restoring into the target"
"$csb" restore "$work/source.csb" --server "http://127.0.0.1:$dst_port" --key "$work/keys/dst.pem"

echo "== backing up the target and comparing"
"$csb" backup --server "http://127.0.0.1:$dst_port" --key "$work/keys/dst.pem" -o "$work/target.csb"
"$csb" diff "$work/source.csb" "$work/target.csb"
