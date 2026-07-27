#!/usr/bin/env bash
# One-command Silo demo: stack -> project -> daemon -> write and read memory.
#
# Every step echoes the real command before running it, so this stays a
# demonstration of how Silo works rather than a black box. Anything it does can
# be typed by hand — see docs/QUICKSTART.md for the same sequence manually.
#
# DEV ONLY. Uses the throwaway credentials from deploy/bootstrap-dev.sh against
# a local stack with no TLS.
#
# Usage:
#   deploy/demo.sh            provision and run the demo
#   deploy/demo.sh --down     tear the demo project down and stop the daemon
set -euo pipefail

cd "$(dirname "$0")/.."

PROJECT="${SILO_DEMO_PROJECT:-demo}"
TOKEN="${SILO_DEMO_TOKEN:-demo-token}"
PORT="${SILO_DEMO_PORT:-8500}"
DASH_PORT="${SILO_DEMO_DASHBOARD_PORT:-8600}"
API="http://127.0.0.1:${PORT}"
COMPOSE="deploy/docker-compose.yaml"
INSTALL_URL="https://raw.githubusercontent.com/ToolTropolis/silo/main/docs/install.sh"
RUN_DIR=".silo-demo"
NOTE='Prefers tabs. Always run gofmt before committing.'

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
run()  { printf '    \033[2m$ %s\033[0m\n' "$*"; eval "$@"; }
note() { printf '    %s\n' "$*"; }

# Stop the daemon/dashboard this script started. Only ever kills PIDs it
# recorded, so an unrelated silod a user is running by hand is left alone.
stop_recorded() {
  local name pidfile pid
  for name in silod dashboard; do
    pidfile="${RUN_DIR}/${name}.pid"
    [ -f "$pidfile" ] || continue
    pid="$(cat "$pidfile")"
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      note "stopped ${name} (pid ${pid})"
    fi
    rm -f "$pidfile"
  done
}

if [ "${1:-}" = "--down" ]; then
  bold "Tearing down the Silo demo"
  stop_recorded

  step "Removing project \"${PROJECT}\" (four confirmed layers)"
  if ./bin/siloctl status 2>/dev/null | grep -q "^${PROJECT}[[:space:]]"; then
    for s in revoke-credential revoke-key delete-bucket deregister; do
      run "./bin/siloctl teardown --project='${PROJECT}' --step='${s}' --yes"
    done
  else
    note "project \"${PROJECT}\" is not registered — nothing to remove"
  fi

  printf '\n'
  bold "Done. The docker stack is still running."
  note "Stop it with:  docker compose -f ${COMPOSE} down"
  exit 0
fi

bold "Silo demo — from nothing to stored, versioned agent memory"
mkdir -p "$RUN_DIR"

# Refuse to trample a daemon that is already on the port. Silently attaching to
# someone else's process is how a demo appears to work while doing nothing.
if lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
  if [ -f "${RUN_DIR}/silod.pid" ] && kill -0 "$(cat "${RUN_DIR}/silod.pid")" 2>/dev/null; then
    note "a previous demo daemon is still running; restarting it"
    stop_recorded
    sleep 1
  else
    echo "ERROR: something is already listening on port ${PORT}." >&2
    echo "       Stop it, or re-run with SILO_DEMO_PORT=<other port>." >&2
    exit 1
  fi
fi

step "1/6  Starting the stack (SeaweedFS, rqlite x3, Vault)"
run "docker compose -f ${COMPOSE} up -d"

printf '    waiting for containers to accept connections'
for _ in $(seq 1 30); do
  if docker exec deploy-seaweedfs-1 true 2>/dev/null; then break; fi
  printf '.'; sleep 1
done
printf '\n'

step "2/6  Bootstrapping Vault and the S3 identities"
note "Vault seals on restart, so this runs every time. It is idempotent."
run "deploy/bootstrap-dev.sh >/dev/null 2>&1"
note "vault unsealed; silo-admin and silo-runtime provisioned"

# Build from source when there's a toolchain and sources present; otherwise fall
# back to the released binaries. That second path is what makes the demo work
# from a bare download of these three scripts, with no clone and no Go.
if command -v go >/dev/null 2>&1 && [ -d ./cmd/siloctl ]; then
  step "3/6  Building the binaries"
  run "go build -o ./bin/siloctl ./cmd/siloctl"
  run "go build -o ./bin/silod ./cmd/silod"
  run "go build -o ./bin/silo-dashboard ./cmd/silo-dashboard"
  # silo-mcp is what actually connects a repo to Silo, and silo-admin is the
  # operator console. A demo that ships neither leaves the user with storage
  # they cannot reach from an agent.
  run "go build -o ./bin/silo-mcp ./cmd/silo-mcp"
  run "go build -o ./bin/silo-admin ./cmd/silo-admin"
else
  step "3/6  Fetching the released binaries"
  if [ -d ./cmd/siloctl ]; then
    note "no Go toolchain found — using the published release instead"
  else
    note "no sources here — using the published release"
  fi
  mkdir -p ./bin
  # The installer verifies checksums (and the cosign signature when available),
  # so this path is no less trustworthy than building from source.
  # INSTALL_DIR and BIN_DIR must differ: the installer symlinks from the latter
  # to the former, and pointing both at ./bin makes every binary a link to
  # itself ("Too many levels of symbolic links").
  printf '    \033[2m$ curl -fsSL .../docs/install.sh | SILO_INSTALL_DIR=./bin sh\033[0m\n'
  if ! curl -fsSL "${INSTALL_URL}" |
       SILO_INSTALL_DIR="$PWD/${RUN_DIR}/dist" SILO_BIN_DIR="$PWD/bin" sh >/dev/null 2>&1; then
    echo "ERROR: could not fetch the released binaries." >&2
    echo "       Install Go and re-run, or see docs/installing.md" >&2
    exit 1
  fi
  note "4 binaries in ./bin"
fi

step "4/6  Creating project \"${PROJECT}\""
note "its own bucket, its own encryption key, its own scoped credential"
if ./bin/siloctl status 2>/dev/null | grep -q "^${PROJECT}[[:space:]]"; then
  note "project \"${PROJECT}\" already exists — reusing it"
else
  run "./bin/siloctl onboard --project='${PROJECT}'"
fi

step "5/6  Starting the daemon"
note "runs as silo-runtime: object access only, cannot create or delete buckets"
run "./bin/silod --listen '127.0.0.1:${PORT}' --tokens '${TOKEN}=${PROJECT}' > '${RUN_DIR}/silod.log' 2>&1 &"
echo $! > "${RUN_DIR}/silod.pid"

# Wait for readiness rather than assuming; a fixed sleep races on a cold cache.
for _ in $(seq 1 40); do
  if curl -sf "${API}/v1/health" >/dev/null 2>&1; then break; fi
  sleep 0.25
done
if ! curl -sf "${API}/v1/health" >/dev/null 2>&1; then
  echo "ERROR: the daemon did not become healthy. Log:" >&2
  tail -20 "${RUN_DIR}/silod.log" >&2
  exit 1
fi
note "listening on ${API}"

step "6/6  Writing and reading a memory file"
run "curl -s -X POST '${API}/v1/write' \\
      -H 'Authorization: Bearer ${TOKEN}' -H 'Content-Type: application/json' \\
      -d \"{\\\"path\\\":\\\"memory/notes.md\\\",\\\"content\\\":\\\"\$(printf '%s' '${NOTE}' | base64)\\\"}\" >/dev/null"

GOT="$(curl -s "${API}/v1/read?path=memory/notes.md" \
       -H "Authorization: Bearer ${TOKEN}" | sed 's/.*"content":"//;s/".*//' | base64 -d)"

printf '\n    read back: \033[1;32m%s\033[0m\n' "$GOT"
if [ "$GOT" != "$NOTE" ]; then
  echo "ERROR: what came back does not match what was written." >&2
  exit 1
fi

# Write again so the dashboard has a version history to show — "nothing is
# overwritten in place" is the claim worth demonstrating, not just stating.
curl -s -X POST "${API}/v1/write" \
  -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
  -d "{\"path\":\"memory/notes.md\",\"content\":\"$(printf '%s Also: wrap errors with %%w.' "$NOTE" | base64)\"}" >/dev/null

./bin/silo-dashboard --listen "127.0.0.1:${DASH_PORT}" > "${RUN_DIR}/dashboard.log" 2>&1 &
echo $! > "${RUN_DIR}/dashboard.pid"

printf '\n'
bold "Silo is running."
printf '\n'
note "Dashboard   http://127.0.0.1:${DASH_PORT}   (memory/notes.md now has 2 versions)"
note "API         ${API}"
note "Logs        ${RUN_DIR}/silod.log"
printf '\n'
note "Try it:"
printf '      curl -s "%s/v1/list?prefix=memory/" -H "Authorization: Bearer %s"\n' "$API" "$TOKEN"
printf '      curl -s "%s/v1/search?prefix=memory/&q=gofmt" -H "Authorization: Bearer %s"\n' "$API" "$TOKEN"
printf '\n'
note "Tear down:  deploy/demo.sh --down"
