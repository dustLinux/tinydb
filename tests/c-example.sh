#!/data/data/com.termux/files/usr/bin/bash
# Прогон C-примера против живого сервера: поднимает сервер при необходимости.
set -u
. "$(dirname "$0")/lib.sh"
webdb_ensure_server || exit 1
trap webdb_stop_server EXIT
TOKEN=$(tr -d '\n' < "$WEBDB_ROOT/data/token")
"$WEBDB_ROOT/client-c/example" "$WEBDB_BASE" "$TOKEN"
rc=$?
exit $rc
