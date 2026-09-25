#!/data/data/com.termux/files/usr/bin/bash
# Тесты JWT-аутентификации tinydb: выпуск токена → сервер принимает/отвергает.
#
#   A. выпуск токена (webdb token): валидный, без секрета, короткий секрет
#   B. приём: верный JWT, статический токен по-прежнему работает
#   C. отказ: просроченный, nbf в будущем, чужой iss/aud, подделка подписи,
#      alg=none, alg=RS256 с HS-подписью, мусор, пустой bearer
#   D. X-API-Key с JWT-токеном
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/webdb"
BASE="http://127.0.0.1:8096"
DDIR="$ROOT/data-jwt"
LOG="$ROOT/jwt-server.log"
SECRET='jwt-test-secret-32-bytes-long!!!'

pass=0; fail=0
chk() {
  if [[ "$3" == *"$2"* ]]; then pass=$((pass+1)); echo "  ok   $1";
  else fail=$((fail+1)); echo "  FAIL $1: expected '$2' got: ${3:0:200}"; fi
}
code() { # code <URL> <TOKEN> -> HTTP-код
  curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $2" "$1"
}
SRVPID=""
cleanup() {
  [ -n "$SRVPID" ] && kill -TERM "$SRVPID" 2>/dev/null
  wait "$SRVPID" 2>/dev/null
  rm -rf "$DDIR" "$LOG"
}
trap cleanup EXIT

[ -x "$BIN" ] || { echo "FAIL: нет $BIN — make build"; exit 1; }
if curl -s --max-time 1 "$BASE/health" >/dev/null 2>&1; then
  echo "FAIL: порт 8096 занят посторонним сервером"; exit 1
fi

echo "== A. выпуск токена"
export WEBDB_JWT_SECRET="$SECRET"
TOK=$("$BIN" token -sub alice -ttl 1h -iss tinydb -aud api 2>/dev/null)
chk token-issued "${TOK:0:8}" "$(echo "$TOK" | cut -d. -f1)"
parts=$(echo "$TOK" | awk -F. '{print NF}')
chk token-three-parts "3" "$parts"
# токен без секрета — отказ
out=$(env -u WEBDB_JWT_SECRET "$BIN" token -sub x -ttl 1h 2>&1); rc=$?
chk token-no-secret-rc "1" "$rc"
chk token-no-secret-msg "нужен секрет" "$out"
# короткий секрет — предупреждение как отказ
out=$(WEBDB_JWT_SECRET=short "$BIN" token -sub x -ttl 1h 2>&1); rc=$?
chk token-short-secret-rc "1" "$rc"

echo "== B/C/D. сервер проверяет"
rm -rf "$DDIR"; mkdir -p "$DDIR"
WEBDB_JWT_SECRET="$SECRET" "$BIN" -addr 127.0.0.1:8096 -data "$DDIR" \
  -jwt-issuer tinydb -jwt-audience api -writeback 10s > "$LOG" 2>&1 &
SRVPID=$!
ok=0
for _ in $(seq 1 40); do curl -s "$BASE/health" >/dev/null 2>&1 && { ok=1; break; }; sleep 0.25; done
if [ "$ok" != 1 ]; then echo "FAIL: сервер не поднялся"; tail -5 "$LOG"; exit 1; fi
STATIC=$(tr -d '\n' < "$DDIR/token")

chk valid-jwt-200 "200" "$(code "$BASE/v1/stats" "$TOK")"
chk static-token-200 "200" "$(code "$BASE/v1/stats" "$STATIC")"
chk no-token-401 "401" "$(code "$BASE/v1/stats" "")"
chk health-open-200 "200" "$(code "$BASE/health" "")"

expired=$("$BIN" token -sub bob -ttl -1h -iss tinydb -aud api)
chk expired-401 "401" "$(code "$BASE/v1/stats" "$expired")"
future=$("$BIN" token -sub bob -ttl 1h -nbf 2h -iss tinydb -aud api)
chk nbf-future-401 "401" "$(code "$BASE/v1/stats" "$future")"
badiss=$("$BIN" token -sub bob -ttl 1h -iss other -aud api)
chk wrong-iss-401 "401" "$(code "$BASE/v1/stats" "$badiss")"
badaud=$("$BIN" token -sub bob -ttl 1h -iss tinydb -aud other)
chk wrong-aud-401 "401" "$(code "$BASE/v1/stats" "$badaud")"
noexp=$("$BIN" token -sub bob -ttl 0 -iss tinydb -aud api)
chk no-exp-accepted "200" "$(code "$BASE/v1/stats" "$noexp")"

# подделка: меняем последний символ подписи
forged="${TOK%?}X"
chk forged-sig-401 "401" "$(code "$BASE/v1/stats" "$forged")"
# чужой секрет
other=$(WEBDB_JWT_SECRET='another-secret-32-bytes-long!!' "$BIN" token -sub bob -ttl 1h -iss tinydb -aud api)
chk wrong-secret-401 "401" "$(code "$BASE/v1/stats" "$other")"
# alg=none
none=$(printf '%s' '{"alg":"none","typ":"JWT"}' | base64 -w0 | tr -d '=')
body=$(printf '%s' '{"sub":"attacker","exp":99999999999}' | base64 -w0 | tr -d '=')
chk alg-none-401 "401" "$(code "$BASE/v1/stats" "$none.$body.")"
# подмена алгоритма RS256 с корректной HS-подписью
rshead=$(printf '%s' '{"alg":"RS256","typ":"JWT"}' | base64 -w0 | tr -d '=')
si="$rshead.$body"
sig=$(python3 -c '
import base64,hmac,hashlib,sys
si, secret = sys.argv[1].encode(), sys.argv[2].encode()
mac = hmac.new(secret, si, hashlib.sha256).digest()
print(base64.urlsafe_b64encode(mac).decode().rstrip("="))
' "$si" "$SECRET")
if [ ${#sig} -lt 40 ]; then
  fail=$((fail+1)); echo "  FAIL alg-swap-prep: не удалось посчитать HMAC (sig=${#sig})"
else
  chk alg-swap-401 "401" "$(code "$BASE/v1/stats" "$si.$sig")"
fi
# мусор
chk garbage-401 "401" "$(code "$BASE/v1/stats" "abc.def.ghi")"
chk single-part-401 "401" "$(code "$BASE/v1/stats" "notatoken")"
# X-API-Key с JWT
xkey=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "X-API-Key: $TOK" "$BASE/v1/stats")
chk x-api-key-jwt-200 "200" "$xkey"

# токен не должен светиться в логе
if grep -qF "$TOK" "$LOG" 2>/dev/null; then
  fail=$((fail+1)); echo "  FAIL jwt-not-in-log: токен найден в логе"
else
  pass=$((pass+1)); echo "  ok   jwt-not-in-log"
fi

echo
echo "RESULT: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
