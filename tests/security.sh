#!/data/data/com.termux/files/usr/bin/bash
# Тесты безопасности tinydb (e2e, свой сервер на :8098 в ./data-sec):
#   A. аутентификация (401/нужен X-API-Key/health открыт)
#   B. инъекции и валидации SQL (SELECT-only, без стека, параметризация)
#   C. валидации имён (плохие, резервные, длинные, path traversal)
#   D. лимиты (413, битый JSON, битый order)
#   E. файловые права (0700/0600), токена нет в логе, CORS выключен
#   F. целостность после SIGTERM: нет plaintext, есть WEBDBENC-snapshot
#   G. отрицательные старты: битый ключ, чужой ключ, подделанный .enc
#   H. режим passphrase: нет db.key, верный пароль — старт, неверный — отказ
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/webdb"
BASE="http://127.0.0.1:8098"
PPORT=8097
DDIR="$ROOT/data-sec"
PDIR="$ROOT/data-sec-pass"
SLOG="$ROOT/sec-server.log"    # вне data-sec: переживает удаление каталога
PSLOG="$ROOT/sec-pass.log"
BIG="$ROOT/sec-big.tmp"

pass=0; fail=0
chk() { # chk <name> <expected-substr> <actual>
  if [[ "$3" == *"$2"* ]]; then pass=$((pass+1)); echo "  ok   $1";
  else fail=$((fail+1)); echo "  FAIL $1: expected '$2' got: ${3:0:300}"; fi
}
chk_not() { # chk_not <name> <forbidden-substr> <actual>
  if [[ "$3" != *"$2"* ]]; then pass=$((pass+1)); echo "  ok   $1";
  else fail=$((fail+1)); echo "  FAIL $1: forbidden '$2' present: ${3:0:300}"; fi
}
j() { curl -s "$@"; }

[ -x "$BIN" ] || { echo "FAIL: нет $BIN — сделай make build"; exit 1; }

SRVPID=""
cleanup() {
  if [ -n "$SRVPID" ]; then kill -TERM "$SRVPID" 2>/dev/null; wait "$SRVPID" 2>/dev/null; fi
  rm -f "$BIG"
}
trap cleanup EXIT

# Пред-проверка: чужой сервер на наших портах ломает весь прогон
# (все запросы получают 401 от чужого токена — маскирует реальные результаты).
for prt in 8098 8097; do
  if curl -s --max-time 1 "http://127.0.0.1:$prt/health" >/dev/null 2>&1; then
    echo "FAIL: порт $prt занят посторонним сервером — остановите его и повторите"
    exit 1
  fi
done

echo "== подготовка (свежий data-sec, сервер :8098)"
rm -rf "$DDIR" "$PDIR" "$SLOG" "$PSLOG"
mkdir -p "$DDIR"
# -max-import 1: лимит импорта1MiB — проверяем, что5MiB-загрузка отбрасывается.
"$BIN" -addr 127.0.0.1:8098 -data "$DDIR" -writeback 10s -autosave 0 -max-import 1 \
  > "$SLOG" 2>&1 &
SRVPID=$!
for _ in $(seq 1 40); do j "$BASE/health" >/dev/null 2>&1 && break; sleep 0.25; done
if ! kill -0 "$SRVPID" 2>/dev/null; then
  echo "FAIL: сервер умер при старте:"; tail -5 "$SLOG"; exit 1
fi
if ! j "$BASE/health" >/dev/null 2>&1; then echo "FAIL: сервер не поднялся"; tail -5 "$SLOG"; exit 1; fi
TOKEN=$(tr -d '\n' < "$DDIR/token")
HA=(-H "Authorization: Bearer $TOKEN")

echo "== A. аутентификация"
chk auth-stats401 '"status":401' "$(j $BASE/v1/stats)"
chk auth-wrong401 '"status":401' "$(j -H 'Authorization: Bearer wrong-token' $BASE/v1/stats)"
chk auth-post401 '"status":401' "$(j -X POST -d '{"name":"evil"}' $BASE/v1/collections)"
chk auth-x-api-key200 '"rss_kb"' "$(j -H "X-API-Key: $TOKEN" $BASE/v1/stats)"
chk health-open '"status":"ok"' "$(j $BASE/health)"

echo "== B. SQL: только чтение, без стека, параметризация"
chk sql-insert400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"INSERT INTO docs (collection,id,data) VALUES (\"c\",\"i\",\"{}\")"}' $BASE/v1/query)"
chk sql-stack400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"SELECT COUNT(*) FROM docs; DROP TABLE docs"}' $BASE/v1/query)"
chk sql-pragma400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"PRAGMA journal_mode=DELETE"}' $BASE/v1/query)"
chk sql-attach400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"ATTACH DATABASE \"/etc/passwd\" AS p"}' $BASE/v1/query)"
# вредоносный аргумент уходит параметром, а не в SQL — таблица должна выжить
chk sql-arg-ok '"rows"' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"SELECT COUNT(*) AS n FROM docs WHERE id=?","args":["'"'"'; DROP TABLE docs; --"]}' $BASE/v1/query)"
chk sql-table-alive '"n"' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"SELECT COUNT(*) AS n FROM docs"}' $BASE/v1/query)"

echo "== C. валидации имён и path traversal"
chk name-bad400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST -d '{"name":"bad name!"}' $BASE/v1/collections)"
chk name-reserved400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST -d '{"name":"_collections"}' $BASE/v1/collections)"
chk name-meta400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST -d '{"name":"_meta"}' $BASE/v1/collections)"
chk name-long400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST \
  -d "{\"name\":\"$(printf 'a%.0s' $(seq 1 65))\"}" $BASE/v1/collections)"
TRAV_CODE=$(j -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/collections/..%2F..%2Fetc%2Fpasswd/docs")
if [ "$TRAV_CODE" -ge 400 ] && [ "$TRAV_CODE" -lt 500 ]; then pass=$((pass+1)); echo "  ok   traversal-4xx ($TRAV_CODE)";
else fail=$((fail+1)); echo "  FAIL traversal: status $TRAV_CODE"; fi
chk_not traversal-no-leak 'root:' "$(j -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/collections/..%2F..%2Fetc%2Fpasswd/docs?x=1")"
chk order-bad-field400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/collections/nonexist/docs?order=bad%20field:asc")"

echo "== D. лимиты"
chk json-garbage400 '"status":400' "$(j -H "Authorization: Bearer $TOKEN" -X POST -d '{не json' $BASE/v1/collections)"
head -c 5242884 /dev/zero | tr '\0' 'a' > "$BIG"   # ~5MiB > -max-body 4MiB
BIGCODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  -X POST --data-binary @"$BIG" "$BASE/v1/collections" --max-time 15 || true)
if [ "$BIGCODE" = "413" ]; then pass=$((pass+1)); echo "  ok   body-over-limit413 (обычный эндпоинт)";
else fail=$((fail+1)); echo "  FAIL body-over-limit: got '$BIGCODE'"; fi
IMPCODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  -X POST --data-binary @"$BIG" "$BASE/v1/import" --max-time 15 || true)
if [ "$IMPCODE" = "413" ]; then pass=$((pass+1)); echo "  ok   import-over-limit413 (-max-import 1)";
else fail=$((fail+1)); echo "  FAIL import-over-limit: got '$IMPCODE'"; fi
rm -f "$BIG"

echo "== E. файловые права, логи, CORS"
chk perm-dir700 '700' "$(stat -c %a "$DDIR")"
chk perm-key600 '600' "$(stat -c %a "$DDIR/db.key")"
chk perm-token600 '600' "$(stat -c %a "$DDIR/token")"
chk perm-plain600 '600' "$(stat -c %a "$DDIR/db.sqlite")"
chk key-len32 '32' "$(wc -c < "$DDIR/db.key" | tr -d ' ')"
chk_not log-no-token "$TOKEN" "$(cat "$SLOG")"
chk_not cors-off 'Access-Control-Allow-Origin' "$(curl -s -D - -o /dev/null $BASE/health)"
chk log-auth-enabled 'auth=true' "$(cat "$SLOG")"

echo "== F. SIGTERM: plaintext убран, снапшот WEBDBENC цел"
kill -TERM "$SRVPID"; wait "$SRVPID" 2>/dev/null; SRVPID=""
if [ ! -f "$DDIR/db.sqlite" ]; then pass=$((pass+1)); echo "  ok   plain-removed";
else fail=$((fail+1)); echo "  FAIL plain-removed: db.sqlite остался"; fi
chk enc-exists '' "$(ls "$DDIR/db.sqlite.enc" 2>/dev/null)"
chk enc-magic 'WEBDBENC' "$(head -c 8 "$DDIR/db.sqlite.enc" 2>/dev/null)"
chk shutdown-clean 'bye (encrypted snapshot saved)' "$(cat "$SLOG")"

echo "== G. отрицательные старты (битый/чужой ключ, подделанный .enc)"
cp "$DDIR/db.key" "$DDIR/db.key.bak"
cp "$DDIR/db.sqlite.enc" "$DDIR/db.sqlite.enc.bak"

printf '0123456789' > "$DDIR/db.key"   # 10 байт вместо 32
timeout 10 "$BIN" -addr 127.0.0.1:8098 -data "$DDIR" > "$ROOT/sec-neg.log" 2>&1
rc=$?
if [ "$rc" -ne 0 ] && [ "$rc" -ne 124 ]; then pass=$((pass+1)); echo "  ok   short-key-refused (exit $rc)";
else fail=$((fail+1)); echo "  FAIL short-key-refused: exit $rc"; fi
chk short-key-msg 'must be exactly 32 bytes' "$(cat "$ROOT/sec-neg.log")"

head -c 32 /dev/urandom > "$DDIR/db.key"   # валидный размер, чужой ключ
timeout 10 "$BIN" -addr 127.0.0.1:8098 -data "$DDIR" > "$ROOT/sec-neg.log" 2>&1
rc=$?
if [ "$rc" -ne 0 ] && [ "$rc" -ne 124 ]; then pass=$((pass+1)); echo "  ok   wrong-key-refused (exit $rc)";
else fail=$((fail+1)); echo "  FAIL wrong-key-refused: exit $rc"; fi
chk wrong-key-msg 'decrypt' "$(cat "$ROOT/sec-neg.log")"

cp "$DDIR/db.key.bak" "$DDIR/db.key"      # теперь правильный ключ, битый файл
ENCLEN=$(wc -c < "$DDIR/db.sqlite.enc" | tr -d ' ')
printf '\xff' | dd of="$DDIR/db.sqlite.enc" bs=1 seek=$((ENCLEN / 2)) conv=notrunc status=none
timeout 10 "$BIN" -addr 127.0.0.1:8098 -data "$DDIR" > "$ROOT/sec-neg.log" 2>&1
rc=$?
if [ "$rc" -ne 0 ] && [ "$rc" -ne 124 ]; then pass=$((pass+1)); echo "  ok   tampered-enc-refused (exit $rc)";
else fail=$((fail+1)); echo "  FAIL tampered-enc-refused: exit $rc"; fi
chk tampered-msg 'decrypt' "$(cat "$ROOT/sec-neg.log")"
cp "$DDIR/db.sqlite.enc.bak" "$DDIR/db.sqlite.enc"  # восстановить валидный снапшот
rm -f "$DDIR/db.key.bak" "$DDIR/db.sqlite.enc.bak" "$ROOT/sec-neg.log"

echo "== H. passphrase-режим (отдельный каталог, :$PPORT)"
mkdir -p "$PDIR"
WEBDB_PASSPHRASE='correct horse battery staple' \
  "$BIN" -addr 127.0.0.1:$PPORT -data "$PDIR" -autosave 0 > "$PSLOG" 2>&1 &
PPID_=$!
for _ in $(seq 1 40); do j "http://127.0.0.1:$PPORT/health" >/dev/null 2>&1 && break; sleep 0.25; done
chk pass-start '"status":"ok"' "$(j http://127.0.0.1:$PPORT/health)"
j -H "Authorization: Bearer $(tr -d '\n' < "$PDIR/token")" -X POST \
  -d '{"name":"penc"}' "http://127.0.0.1:$PPORT/v1/collections" >/dev/null
kill -TERM "$PPID_"; wait "$PPID_" 2>/dev/null
if [ ! -f "$PDIR/db.key" ]; then pass=$((pass+1)); echo "  ok   pass-no-keyfile";
else fail=$((fail+1)); echo "  FAIL pass-no-keyfile: db.key создан"; fi
chk pass-enc-magic 'WEBDBENC' "$(head -c 8 "$PDIR/db.sqlite.enc" 2>/dev/null)"
rm -f "$PDIR/db.sqlite"
WEBDB_PASSPHRASE='correct horse battery staple' \
  "$BIN" -addr 127.0.0.1:$PPORT -data "$PDIR" -autosave 0 > "$PSLOG" 2>&1 &
PPID_=$!
ok=0
for _ in $(seq 1 40); do j "http://127.0.0.1:$PPORT/health" >/dev/null 2>&1 && { ok=1; break; }; sleep 0.25; done
kill -TERM "$PPID_" 2>/dev/null; wait "$PPID_" 2>/dev/null
if [ "$ok" = 1 ]; then pass=$((pass+1)); echo "  ok   pass-restart-ok";
else fail=$((fail+1)); echo "  FAIL pass-restart-ok"; fi
WEBDB_PASSPHRASE='wrong password' \
  timeout 10 "$BIN" -addr 127.0.0.1:$PPORT -data "$PDIR" > "$ROOT/sec-neg.log" 2>&1
rc=$?
if [ "$rc" -ne 0 ] && [ "$rc" -ne 124 ]; then pass=$((pass+1)); echo "  ok   wrong-pass-refused (exit $rc)";
else fail=$((fail+1)); echo "  FAIL wrong-pass-refused: exit $rc"; fi
chk wrong-pass-msg 'decrypt' "$(cat "$ROOT/sec-neg.log")"
rm -f "$ROOT/sec-neg.log"

echo
echo "RESULT: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
