#!/data/data/com.termux/files/usr/bin/bash
# Тесты консоли `webdb shell` (в духе sqlite3) против живого сервера.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/webdb"
# Консоль — подкоманда того же бинаря.
SHELL_BIN="$ROOT/bin/webdb"
BASE="http://127.0.0.1:8095"
DDIR="$ROOT/data-shell"
LOG="$ROOT/shell-server.log"

pass=0; fail=0
chk() {
  if [[ "$3" == *"$2"* ]]; then pass=$((pass+1)); echo "  ok   $1";
  else fail=$((fail+1)); echo "  FAIL $1: expected '$2' got: ${3:0:300}"; fi
}
chk_not() {
  if [[ "$3" != *"$2"* ]]; then pass=$((pass+1)); echo "  ok   $1";
  else fail=$((fail+1)); echo "  FAIL $1: forbidden '$2' present: ${3:0:300}"; fi
}
sh_() { printf '%s\n' "$@" | "$SHELL_BIN" shell -addr 127.0.0.1:8095 -data "$DDIR" 2>&1; }

SRVPID=""
cleanup() {
  [ -n "$SRVPID" ] && kill -TERM "$SRVPID" 2>/dev/null
  wait "$SRVPID" 2>/dev/null
  rm -rf "$DDIR" "$LOG"
}
trap cleanup EXIT

[ -x "$BIN" ] || { echo "FAIL: нет $BIN — make build"; exit 1; }
[ -x "$SHELL_BIN" ] || { echo "FAIL: нет $SHELL_BIN — make build"; exit 1; }
if curl -s --max-time 1 "$BASE/health" >/dev/null 2>&1; then
  echo "FAIL: порт 8095 занят посторонним сервером"; exit 1
fi

rm -rf "$DDIR"; mkdir -p "$DDIR"
"$BIN" -addr 127.0.0.1:8095 -data "$DDIR" -writeback 10s -autosave 0 > "$LOG" 2>&1 &
SRVPID=$!
ok=0
for _ in $(seq 1 40); do curl -s "$BASE/health" >/dev/null 2>&1 && { ok=1; break; }; sleep 0.25; done
[ "$ok" = 1 ] || { echo "FAIL: сервер не поднялся"; tail -5 "$LOG"; exit 1; }

echo "== запись данных через консоль"
out=$(sh_ '.put items it1 {"title":"гайка","price":10}' \
           '.put items it2 {"title":"болт","price":3}' \
           '.flush')
chk_not put-no-error "error:" "$out"

echo "== чтение"
out=$(sh_ '.tables')
chk tables-items "items" "$out"
chk tables-count "2 доков" "$out"
out=$(sh_ 'SELECT count(*) AS n FROM docs;')
chk sql-count "2" "$out"
out=$(sh_ '.ls items')
chk ls-has-it1 "it1" "$out"
chk ls-total "(2 всего)" "$out"
out=$(sh_ '.headers on' 'SELECT id, data FROM docs;')
chk headers-on "id" "$out"
chk headers-sep "---" "$out"

echo "== вывод в разных форматах"
out=$(sh_ '.mode json' 'SELECT id FROM docs;')
chk mode-json "it1" "$out"
chk mode-json-brace '"id"' "$out"
out=$(sh_ '.mode list' 'SELECT id, data FROM docs;')
chk mode-list "it1" "$out"

echo "== индексы и схема"
out=$(sh_ '.index add items price')
chk_not index-add-no-error "error:" "$out"
out=$(sh_ '.index items')
chk index-listed "ix_items_price" "$out"
out=$(sh_ '.schema items')
chk schema-create-table "CREATE TABLE items" "$out"
chk schema-index "ix_items_price" "$out"
out=$(sh_ '.index del items price')
chk_not index-del "error:" "$out"

echo "== правка и удаление"
out=$(sh_ '.get items it1')
chk get-json '"title": "гайка"' "$out"
out=$(sh_ '.put items it1 {"title":"гайка M6","price":11}')
chk_not put-replace "error:" "$out"
out=$(sh_ '.get items it1')
chk put-replace-applied "гайка M6" "$out"
out=$(sh_ '.rm items it2')
chk_not rm-no-error "error:" "$out"
out=$(sh_ 'SELECT count(*) AS n FROM docs;')
chk rm-applied "1" "$out"

echo "== защита от записи через SQL"
out=$(sh_ 'DELETE FROM docs;')
chk sql-delete-refused "только чтение" "$out"
out=$(sh_ 'INSERT INTO docs(collection,id,data) VALUES("x","y","{}");')
chk sql-insert-refused "только чтение" "$out"
out=$(sh_ 'SELECT COUNT(*) AS n FROM docs;')
chk data-intact-after-refused "1" "$out"

echo "== справка, версия, ошибки"
out=$(sh_ '.help')
chk help-nonempty "SQL" "$out"
out=$("$SHELL_BIN" shell -version)
chk version "webdb shell" "$out"
out=$(sh_ 'выдумка')
chk unknown-command "неизвестная команда" "$out"
out=$(sh_ '.ls items 999' '.quit')
chk quit-exits "" "$(echo "$out" | tail -1)"

echo "== размер бинаря (консоль внутри, лимит прежний)"
sz=$(wc -c < "$SHELL_BIN")
echo "  info webdb (server+shell) = $sz байт"
if [ "$sz" -le 10485760 ]; then pass=$((pass+1)); echo "  ok   binary-le10mib";
else fail=$((fail+1)); echo "  FAIL binary-le10mib: $sz > 10MiB"; fi

echo
echo "RESULT: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
