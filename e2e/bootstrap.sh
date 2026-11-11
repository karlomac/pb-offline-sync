#!/usr/bin/env bash
# Bootstrap and run the end-to-end replication test plan on a blank Ubuntu 22.04 VPS.
#
#   git clone -b staging https://github.com/karlomac/pb-offline-sync.git
#   cd pb-offline-sync
#   sudo ./e2e/bootstrap.sh            # full run (installs toolchain, builds, tests, load)
#
# Options:
#   --skip-install        use the toolchain already on PATH (no apt/go/node download)
#   --skip-load           skip the k6 load phase
#   --skip-vuln           skip govulncheck (needs network; slow)
#   --race                also run `go test -race`
#   --load-duration 2m    k6 duration for push/pull (default 2m)
#   --soak-seconds 0      extra soak duration in seconds (0 = none)
#   --load-users 50       users for the load phase (default 50)
#   --port 8090           loopback port (first free in 8090..8099 is used)
#   --remote URL          run against an already-deployed instance instead of
#                         building/starting a local one. Skips install/L0/L1,
#                         provisions by default (see --no-provision) and runs
#                         the live suites (and k6 if not --skip-load).
#                         Admin creds come from E2E_ADMIN_EMAIL/E2E_ADMIN_PASSWORD.
#   --no-provision        with --remote: do not touch the remote schema/users
#   --skip-auth           with --remote: skip the auth-gate suite (it needs a
#                         Turnstile verifier reachable from the server over https)
#   --dir PATH            work dir (default /tmp/pb-e2e)
#   --keep                keep the provisioned data dir after the run
#   --dev                 enable PocketBase dev mode (SQL logging) for debugging;
#                         off by default because it distorts load measurements
#
# Exit code: 0 only when every executed phase passed. Artifacts are written to
# <dir>/artifacts/<timestamp>/ (summary.txt, phase logs, server logs, versions).
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
while IFS= read -r v; do unset "$v"; done < <(env | sed -nE 's/^((SYNC|AUTH)_[A-Z0-9_]+)=.*/\1/p')
WORK="${E2E_WORK_DIR:-/tmp/pb-e2e}"
PORT=8090
SKIP_INSTALL=0
SKIP_LOAD=0
SKIP_VULN=0
RACE=0
LOAD_DURATION="2m"
SOAK_SECONDS=0
LOAD_USERS=50
KEEP=0
DEV=0
REMOTE_URL=""
NO_PROVISION=0
SKIP_AUTH=0

while [ $# -gt 0 ]; do
  case "$1" in
    --skip-install) SKIP_INSTALL=1 ;;
    --skip-load) SKIP_LOAD=1 ;;
    --skip-vuln) SKIP_VULN=1 ;;
    --race) RACE=1 ;;
    --load-duration) LOAD_DURATION="$2"; shift ;;
    --soak-seconds) SOAK_SECONDS="$2"; shift ;;
    --load-users) LOAD_USERS="$2"; shift ;;
    --port) PORT="$2"; shift ;;
    --dir) WORK="$2"; shift ;;
    --keep) KEEP=1 ;;
    --dev) DEV=1 ;;
    --remote) REMOTE_URL="${2%/}"; shift ;;
    --no-provision) NO_PROVISION=1 ;;
    --skip-auth) SKIP_AUTH=1 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

DATA="$WORK/pb_data"
BIN="$WORK/pb-offline-sync"
ART="$WORK/artifacts/$(date +%Y%m%d-%H%M%S)"
SUMMARY="$ART/summary.txt"
BASE="http://127.0.0.1:$PORT"
ADMIN_EMAIL="e2e-admin@example.com"
ADMIN_PASSWORD="e2e-admin-password"
FAILED=0
SERVER_PID=""
STUB_PID=""

# All phases write under $WORK: create the work dir, artifact dir and data dir
# up front. The first log line writes $SUMMARY, and the install phase runs before
# anything else on a fresh host, so this must happen before any logging.
if ! mkdir -p "$WORK" "$ART" "$DATA"; then
  echo "bootstrap: cannot create work directories under $WORK" >&2
  exit 1
fi
if [ ! -w "$ART" ] || [ ! -w "$DATA" ]; then
  echo "bootstrap: work directories under $WORK are not writable" >&2
  exit 1
fi
export WORK ART DATA SUMMARY

log()  { printf '\n=== %s\n' "$*" | tee -a "$SUMMARY"; }
pass() { printf 'PASS  %s\n' "$*" | tee -a "$SUMMARY"; }
fail() { printf 'FAIL  %s\n' "$*" | tee -a "$SUMMARY"; FAILED=1; }

run_step() {
  local name="$1"; shift
  local logfile="$ART/${name//[^A-Za-z0-9_-]/_}.log"
  if "$@" >"$logfile" 2>&1; then
    pass "$name"
  else
    fail "$name (see $logfile)"
    tail -n 40 "$logfile" | sed 's/^/      /' || true
  fi
}

as_root() {
  if [ "$(id -u)" = "0" ]; then "$@"; elif command -v sudo >/dev/null 2>&1; then sudo -n "$@"; else
    echo "need root or passwordless sudo for: $*" >&2; exit 1; fi
}

cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  if [ "$KEEP" = "0" ] && [ -d "$DATA" ]; then rm -rf "$DATA"; fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- toolchain

install_packages() {
  as_root apt-get update -y
  DEBIAN_FRONTEND=noninteractive as_root apt-get install -y \
    git curl ca-certificates xz-utils sqlite3
}

ensure_go() {
  if command -v go >/dev/null 2>&1 && go version | grep -q 'go1\.27'; then return; fi
  if [ -x "$HOME/tools/go/bin/go" ] && "$HOME/tools/go/bin/go" version | grep -q 'go1\.27'; then
    export PATH="$HOME/tools/go/bin:$PATH"; return
  fi
  local arch; case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
  curl -fsSL "https://go.dev/dl/go1.27.3.linux-${arch}.tar.gz" -o /tmp/go.tgz
  as_root rm -rf /usr/local/go
  as_root tar -C /usr/local -xzf /tmp/go.tgz
  as_root ln -sf /usr/local/go/bin/go /usr/local/bin/go
  as_root ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  export PATH="/usr/local/go/bin:$PATH"
}

ensure_node() {
  if ! command -v node >/dev/null 2>&1 || ! node -v | grep -q '^v22\.'; then
    local arch; case "$(uname -m)" in x86_64) arch=x64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
    curl -fsSL "https://nodejs.org/dist/v22.23.4/node-v22.23.4-linux-${arch}.tar.xz" -o /tmp/node.tar.xz
    as_root tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz
  fi
  if ! command -v pnpm >/dev/null 2>&1; then
    as_root npm install -g pnpm@12
  fi
}

ensure_k6() {
  command -v k6 >/dev/null 2>&1 && return 0
  local arch; case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) return 1 ;; esac
  curl -fsSL "https://github.com/grafana/k6/releases/download/v2.3.0/k6-v2.3.0-linux-${arch}.tar.gz" -o /tmp/k6.tgz || return 1
  tar -C /tmp -xzf /tmp/k6.tgz
  as_root install "/tmp/k6-v2.3.0-linux-${arch}/k6" /usr/local/bin/k6
}

if [ -n "$REMOTE_URL" ]; then
  case "$REMOTE_URL" in
    http://*|https://*) : ;;
    *) echo "bootstrap: --remote must be an http(s) URL" >&2; exit 2 ;;
  esac
  SKIP_INSTALL=1
  BASE="$REMOTE_URL"
fi

if [ "$SKIP_INSTALL" = "0" ]; then
  log "installing base packages"
  run_step "install packages" install_packages
  log "installing Go / Node / pnpm"
  run_step "install go" ensure_go
  run_step "install node" ensure_node
  if [ "$SKIP_LOAD" = "0" ]; then run_step "install k6" ensure_k6; fi
else
  # Use a local Go install if the shell has none (developer sandboxes).
  if ! command -v go >/dev/null 2>&1 && [ -x "$HOME/tools/go/bin/go" ]; then
    export PATH="$HOME/tools/go/bin:$PATH"
  fi
fi

{
  echo "root: $ROOT"
  echo "work: $WORK"
  echo "base: $BASE"
  git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || true
  git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || true
  go version 2>/dev/null || true
  node -v 2>/dev/null || true
  pnpm -v 2>/dev/null || true
  k6 version 2>/dev/null || true
  sqlite3 --version 2>/dev/null || true
} | tee "$ART/versions.txt" >/dev/null

if [ -z "$REMOTE_URL" ]; then
  # Pick the first free loopback port in 8090..8099.
  port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
  for p in $(seq 8090 8099); do if ! port_in_use "$p"; then PORT="$p"; break; fi; done
  BASE="http://127.0.0.1:$PORT"
  echo "port: $PORT" >> "$ART/versions.txt"
else
  echo "remote: $BASE" >> "$ART/versions.txt"
fi

# ---------------------------------------------------------------- L0 / L1

phase_l0() {
  log "L0: build, vet, unit tests"
  ( cd "$ROOT/server" && go build ./... ) || return 1
  ( cd "$ROOT/server" && go vet ./... ) || return 1
  ( cd "$ROOT/server" && go test ./... -count=1 ) || return 1
  if [ "$RACE" = "1" ]; then ( cd "$ROOT/server" && go test -race ./syncplugin/ -count=1 ) || return 1; fi
  if [ "$SKIP_VULN" = "0" ]; then
    ( cd "$ROOT/server" && go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./... ) || return 1
  fi
}

phase_l1() {
  log "L1: client suites"
  local p
  for p in core transport-pocketbase adapter-expo-sqlite; do
    ( cd "$ROOT/client/$p" && pnpm install --frozen-lockfile && pnpm run typecheck && pnpm test ) || return 1
  done
}

# ---------------------------------------------------------------- live rig

wait_health() {
  local i
  for i in $(seq 1 120); do
    curl -fsS "$BASE/api/health" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

start_server() { # $1 = plain|RC-B|RC-C ; logs to $ART/server-$1.log
  local mode="$1" vars=()
  case "$mode" in
    plain) : ;;
    RC-B) vars=(SYNC_COLLECTIONS=notes SYNC_OWNER_FIELD=owner SYNC_PRUNE_CRON=none
                SYNC_MEDIA_COLLECTION=media SYNC_MEDIA_MAX_FILE_BYTES=52428800
                SYNC_MEDIA_PER_USER_QUOTA=104857600) ;;
    RC-C) vars=(SYNC_COLLECTIONS=notes SYNC_OWNER_FIELD=owner SYNC_PRUNE_CRON=none
                SYNC_MEDIA_COLLECTION=media SYNC_MEDIA_MAX_FILE_BYTES=52428800
                SYNC_MEDIA_PER_USER_QUOTA=104857600
                AUTH_SIGNUP_MODE=password AUTH_COLLECTION=users
                AUTH_TURNSTILE_SECRET=e2e-secret AUTH_TURNSTILE_HOSTNAMES=localhost
                AUTH_TURNSTILE_VERIFY_URL=http://127.0.0.1:8787/siteverify
                AUTH_ALLOWED_EMAIL_DOMAINS=example.com
                AUTH_DISPOSABLE_DOMAINS=mailinator.com) ;;
    *) echo "unknown mode $mode" >&2; return 1 ;;
  esac
  local devflag=()
  [ "$DEV" = "1" ] && devflag=(--dev)
  env "${vars[@]}" "$BIN" serve --http="127.0.0.1:${PORT}" --dir="$DATA" "${devflag[@]}" \
    >>"$ART/server-$mode.log" 2>&1 &
  SERVER_PID=$!
  wait_health
}

stop_server() {
  if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""; fi
}

provision() {
  start_server plain || return 1
  E2E_BASE_URL="$BASE" E2E_LOAD_USERS="$LOAD_USERS" node "$ROOT/e2e/provision.mjs" || { stop_server; return 1; }
  stop_server
}

phase_sync_media() {
  log "L3: live replication + media (RC-B)"
  start_server RC-B || return 1
  ( cd "$ROOT/client/transport-pocketbase" && \
    E2E_BASE_URL="$BASE" pnpm vitest run tests/e2e.replication.test.ts ) || { stop_server; return 1; }
  stop_server
}

phase_auth() {
  log "L3: auth signup gate (RC-C)"
  node "$ROOT/e2e/turnstile-stub.mjs" >>"$ART/turnstile.log" 2>&1 &
  STUB_PID=$!
  for _ in $(seq 1 40); do curl -fsS "http://127.0.0.1:8787/mode?value=ok" >/dev/null 2>&1 && break; sleep 0.25; done
  start_server RC-C || { kill "$STUB_PID" 2>/dev/null || true; STUB_PID=""; return 1; }
  ( cd "$ROOT/client/transport-pocketbase" && \
    E2E_BASE_URL="$BASE" \
    E2E_ADMIN_EMAIL="$ADMIN_EMAIL" E2E_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
    pnpm vitest run tests/e2e.auth.test.ts ) || { stop_server; kill "$STUB_PID" 2>/dev/null || true; STUB_PID=""; return 1; }
  stop_server
  kill "$STUB_PID" 2>/dev/null || true; STUB_PID=""
}

# Latency budgets are reported, not fatal: a slow host is not a correctness
# failure (see the plan). Only transport errors/missing results fail the phase.
run_k6() { # $1 = summary file, $2 = duration, $3 = vus
  E2E_BASE_URL="$BASE" E2E_LOAD_USERS="$LOAD_USERS" E2E_DURATION="$2" E2E_VUS="$3" \
    k6 run --summary-export "$1" "$ROOT/e2e/load.k6.js" >>"$ART/k6.log" 2>&1 || true
  [ -f "$1" ] || { echo "k6 produced no summary"; return 1; }
  python3 - "$1" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
m = d.get("metrics", {})
reqs = float(m.get("http_reqs", {}).get("count", 0))
failed = float(m.get("http_req_failed", {}).get("value", 1))
dur = m.get("http_req_duration", {})
push = m.get("http_req_duration{scenario:push}", {})
pull = m.get("http_req_duration{scenario:pull}", {})
print(f"requests={reqs:.0f} failed_rate={failed:.5f} "
      f"p95={dur.get('p(95)')}ms push_p95={push.get('p(95)')}ms pull_p95={pull.get('p(95)')}ms")
sys.exit(0 if reqs > 0 and failed < 0.001 else 1)
PYEOF
}

remote_provision() {
  if [ -z "${E2E_ADMIN_EMAIL:-}" ] || [ -z "${E2E_ADMIN_PASSWORD:-}" ]; then
    echo "set E2E_ADMIN_EMAIL and E2E_ADMIN_PASSWORD for --remote provisioning" >&2
    return 1
  fi
  echo "WARNING: provisioning mutates the remote test instance (creates collections, sets users.createRule/authRule)." >&2
  E2E_BASE_URL="$BASE" E2E_LOAD_USERS="$LOAD_USERS" node "$ROOT/e2e/provision.mjs"
}

remote_sync_media() {
  ( cd "$ROOT/client/transport-pocketbase" && \
    E2E_BASE_URL="$BASE" pnpm vitest run tests/e2e.replication.test.ts )
}

remote_auth() {
  ( cd "$ROOT/client/transport-pocketbase" && \
    E2E_BASE_URL="$BASE" \
    E2E_ADMIN_EMAIL="${E2E_ADMIN_EMAIL:-}" E2E_ADMIN_PASSWORD="${E2E_ADMIN_PASSWORD:-}" \
    pnpm vitest run tests/e2e.auth.test.ts )
}

remote_load() {
  log "L4: remote load (${LOAD_DURATION}) and soak (${SOAK_SECONDS}s) from this host"
  if ! command -v k6 >/dev/null 2>&1; then
    fail "k6 is not installed (install it, or use --skip-load)"
    return 1
  fi
  run_k6 "$ART/k6-summary.json" "$LOAD_DURATION" 25 || return 1
  if [ "$SOAK_SECONDS" -gt 0 ]; then
    run_k6 "$ART/k6-soak-summary.json" "${SOAK_SECONDS}s" 5 || return 1
  fi
  echo "remote run: server DB/RSS metrics unavailable (use a co-located runner for capacity numbers)" >> "$ART/metrics.txt"
}

phase_load() {
  log "L4: load (${LOAD_DURATION}) and soak (${SOAK_SECONDS}s)"
  start_server RC-B || return 1
  if ! command -v k6 >/dev/null 2>&1; then
    fail "k6 is not installed (rerun without --skip-install, or install k6)"
    stop_server; return 1
  fi
  run_k6 "$ART/k6-summary.json" "$LOAD_DURATION" 25 || { stop_server; return 1; }
  if [ "$SOAK_SECONDS" -gt 0 ]; then
    run_k6 "$ART/k6-soak-summary.json" "${SOAK_SECONDS}s" 5 || { stop_server; return 1; }
  fi
  {
    echo "--- server metrics"
    ps -o rss=,pcpu= -p "$SERVER_PID" 2>/dev/null || true
    ls -lh "$DATA"/data.db* 2>/dev/null || true
    echo "--- log sizes"
    ls -lh "$ART"/server-*.log 2>/dev/null || true
    sqlite3 "$DATA/data.db" 'select (select count(*) from sync_changes) as changes, (select count(*) from sync_meta) as meta, (select count(*) from sync_processed) as processed, (select count(*) from sync_conflicts) as conflicts;' 2>/dev/null || true
  } >> "$ART/metrics.txt"
  stop_server
}

# ---------------------------------------------------------------- main

log "bootstrap: $ROOT @ $(git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"

if [ -n "$REMOTE_URL" ]; then
  log "remote mode: $BASE (local build/tests skipped)"
  if [ "$NO_PROVISION" = "0" ]; then
    run_step "provision remote schema" remote_provision
  else
    log "remote provisioning skipped (--no-provision)"
  fi
  run_step "L3 sync+media (remote)" remote_sync_media
  if [ "$SKIP_AUTH" = "0" ]; then
    run_step "L3 auth gate (remote)" remote_auth
  else
    log "remote auth gate skipped (--skip-auth)"
  fi
  if [ "$SKIP_LOAD" = "0" ]; then
    run_step "L4 load (remote, WAN)" remote_load
  else
    log "remote load skipped (--skip-load)"
  fi
  log "summary complete"
  echo
  echo "summary:   $SUMMARY"
  echo "artifacts: $ART"
  if [ "$FAILED" = "0" ]; then echo "RESULT: PASS"; exit 0; else echo "RESULT: FAIL"; exit 1; fi
fi

run_step "L0 build/vet/test" phase_l0
run_step "L1 client suites" phase_l1

log "building server binary"
if ( cd "$ROOT/server" && go build -o "$BIN" ./cmd/pb-offline-sync ); then
  pass "build server binary"
  "$BIN" superuser upsert "$ADMIN_EMAIL" "$ADMIN_PASSWORD" --dir="$DATA" >>"$ART/superuser.log" 2>&1 \
    && pass "superuser bootstrap" || fail "superuser bootstrap"
else
  fail "build server binary"
fi

if [ -x "$BIN" ]; then
  run_step "provision schema" provision
  run_step "L3 sync+media" phase_sync_media
  run_step "L3 auth gate" phase_auth
  if [ "$SKIP_LOAD" = "0" ]; then
    run_step "L4 load/soak" phase_load
  else
    log "L4 load skipped (--skip-load)"
  fi
fi

log "summary complete"
echo
echo "summary:   $SUMMARY"
echo "artifacts: $ART"
if [ "$FAILED" = "0" ]; then echo "RESULT: PASS"; exit 0; else echo "RESULT: FAIL"; exit 1; fi
