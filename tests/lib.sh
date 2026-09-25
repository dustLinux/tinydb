# shellcheck shell=bash
# Общий хелпер для e2e-тестов: поднять сервер, если он не запущен,
# и убить только того, кого подняли сами.
#
#   source "$(dirname "$0")/lib.sh"
#   webdb_ensure_server || exit 1
#   trap webdb_stop_server EXIT

WEBDB_ROOT="${WEBDB_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
WEBDB_BASE="${WEBDB_BASE:-http://127.0.0.1:8099}"
WEBDB_STARTED=0
WEBDB_SRVPID=""

webdb_ensure_server() {
  if curl -sf "$WEBDB_BASE/health" >/dev/null 2>&1; then
    WEBDB_STARTED=0 # чужой сервер — не трогаем
    return 0
  fi
  if [ ! -x "$WEBDB_ROOT/bin/webdb" ]; then
    echo "FAIL: нет $WEBDB_ROOT/bin/webdb — сделай make build" >&2
    return 1
  fi
  mkdir -p "$WEBDB_ROOT/data"
  # -writeback 10s: тикер не должен успевать применить буфер раньше,
  # чем smoke проверит buffer_items >= 1.
  "$WEBDB_ROOT/bin/webdb" -addr 127.0.0.1:8099 -data "$WEBDB_ROOT/data" \
    -writeback 10s >"$WEBDB_ROOT/data/server.log" 2>&1 &
  WEBDB_SRVPID=$!
  WEBDB_STARTED=1
  local i
  for i in $(seq 1 60); do
    curl -sf "$WEBDB_BASE/health" >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  echo "FAIL: сервер не поднялся, хвост лога:" >&2
  tail -20 "$WEBDB_ROOT/data/server.log" >&2
  return 1
}

webdb_stop_server() {
  [ "${WEBDB_STARTED:-0}" = 1 ] || return 0
  kill -TERM "$WEBDB_SRVPID" 2>/dev/null || true
  wait "$WEBDB_SRVPID" 2>/dev/null || true
  WEBDB_STARTED=0
}
