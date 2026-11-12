#!/usr/bin/env bash
# End-to-end backup and restore against a real cinc-server-ng.
#
# Starts a source server loaded with a fixture (--state), backs it up, verifies
# the backup, restores it into a fresh server, backs that up, and diffs the two
# backups.
#
# Then the dangling-reference cycle: a copy of the backup is made to name a
# user and a client it doesn't hold (as an erchef backup does after someone
# was deleted; ng removes the names, so its backups never have them). Restore
# into another fresh server must refuse it with --dangling fail and write
# nothing, then with the default (drop) report exactly the injected names and
# produce the same server as the clean backup. Restore checks its own result,
# and verify --server checks it again: both exit 0 for the clean target and 3
# (everything but the dropped names) for this one.
#
# Exits non-zero on the first failure, including any difference.
#
# Usage: .github/scripts/e2e-ng.sh <fixture-dir>
#   NG_IMAGE  container image to run, e.g. ghcr.io/cinc-project/cinc-server-ng:0.20.0
#   NG_BIN    or: path to a cinc-server-ng binary (used when NG_IMAGE is unset)
#   WORKDIR   where keys, logs and backups go (default: a new temp dir)
set -euo pipefail

fixture=$(cd "${1:?usage: $0 <fixture-dir>}" && pwd)
work=${WORKDIR:-$(mktemp -d)}
mkdir -p "$work/keys"
src_port=18889
dst_port=18890
dangle_port=18891
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

repo=$(cd "$(dirname "$0")/../.." && pwd)
csb="$work/cinc-server-backup"
(cd "$repo" && go build -o "$csb" ./cmd/cinc-server-backup)
(cd "$repo/integration" && go build -o "$work/dangle" ./cmd/dangle)

echo "== starting the source (loaded from $fixture) and an empty target"
start src "$src_port" --state @FIXTURE@
start dst "$dst_port" --orgs unused

echo "== backing up the source"
"$csb" backup --server "http://127.0.0.1:$src_port" --key "$work/keys/src.pem" -o "$work/source.csb"
"$csb" verify "$work/source.csb"
"$csb" inspect "$work/source.csb"

echo "== planning the restore"
# The restore below refuses a target that already has the backup's orgs or
# users, so it also proves the plan changed nothing.
"$csb" restore "$work/source.csb" --server "http://127.0.0.1:$dst_port" --key "$work/keys/dst.pem" \
  --plan --plan-out "$work/plan.json"

echo "== restoring into the target"
"$csb" restore "$work/source.csb" --server "http://127.0.0.1:$dst_port" --key "$work/keys/dst.pem"

echo "== verify --server: the target must hold everything (exit 0)"
"$csb" verify "$work/source.csb" --server "http://127.0.0.1:$dst_port" --key "$work/keys/dst.pem"

echo "== backing up the target and comparing"
"$csb" backup --server "http://127.0.0.1:$dst_port" --key "$work/keys/dst.pem" -o "$work/target.csb"
"$csb" diff "$work/source.csb" "$work/target.csb"

echo "== dangling references: making a copy of the backup name a deleted user and client"
"$work/dangle" "$work/source.csb" "$work/dangling.csb" | sort >"$work/dangling.want"
cat "$work/dangling.want"
"$csb" verify "$work/dangling.csb"
start dangle "$dangle_port" --orgs unused

echo "== --dangling fail must refuse it and write nothing"
if "$csb" restore "$work/dangling.csb" --server "http://127.0.0.1:$dangle_port" --key "$work/keys/dangle.pem" \
  --dangling fail >"$work/fail.out" 2>&1; then
  cat "$work/fail.out"
  echo "restore --dangling fail succeeded; want a refusal" >&2
  exit 1
fi
cat "$work/fail.out"
grep -q "nothing was restored" "$work/fail.out"

echo "== the default (drop) must leave out exactly the injected names, and exit 3"
set +e
"$csb" restore "$work/dangling.csb" --server "http://127.0.0.1:$dangle_port" --key "$work/keys/dangle.pem" \
  | tee "$work/drop.out"
code=${PIPESTATUS[0]}
set -e
if [[ $code -ne 3 ]]; then
  echo "restore exited $code; want 3 (everything but the dropped names)" >&2
  exit 1
fi
awk '/dangling_dropped/ {print $1, $2, $3, $4, $5}' "$work/drop.out" | sed 's/ *$//' | sort >"$work/dangling.got"
diff -u "$work/dangling.want" "$work/dangling.got"

echo "== verify --server: everything but the dropped names (exit 3)"
set +e
"$csb" verify "$work/dangling.csb" --server "http://127.0.0.1:$dangle_port" --key "$work/keys/dangle.pem" | tee "$work/verify.out"
code=${PIPESTATUS[0]}
set -e
if [[ $code -ne 3 ]]; then
  echo "verify exited $code; want 3" >&2
  exit 1
fi
grep -Eq 'dropped +3 ' "$work/verify.out"

echo "== the result must match the clean backup"
"$csb" backup --server "http://127.0.0.1:$dangle_port" --key "$work/keys/dangle.pem" -o "$work/dangle-target.csb"
"$csb" diff "$work/source.csb" "$work/dangle-target.csb"
