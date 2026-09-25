#!/data/data/com.termux/files/usr/bin/bash
# End-to-end smoke test for tinydb REST API.
# Поднимает сервер сам, если он не запущен (tests/lib.sh).
set -u
. "$(dirname "$0")/lib.sh"
BASE="$WEBDB_BASE"
DDIR="$WEBDB_ROOT/data"                         # web-db/data
webdb_ensure_server || exit 1
trap webdb_stop_server EXIT
TOKEN=$(tr -d '\n' < "$DDIR/token")
H=(-H "Authorization: Bearer $TOKEN")
pass=0; fail=0
chk() { # chk <name> <expected-substr> <actual>
  if [[ "$3" == *"$2"* ]]; then pass=$((pass+1)); echo "  ok   $1";
  else fail=$((fail+1)); echo "  FAIL $1: expected '$2' got: ${3:0:300}"; fi
}
j() { curl -s "${H[@]}" "$@"; }

echo "== reset state (чистая БД для воспроизводимости)"
for c in $(j $BASE/v1/collections | grep -o '"name":"[^"]*"' | cut -d'"' -f4); do
  j -X DELETE "$BASE/v1/collections/$c" >/dev/null
done

echo "== health (без токена)"
chk health '"status":"ok"' "$(curl -s $BASE/health)"

echo "== auth"
chk auth401 '"status":401' "$(curl -s $BASE/v1/stats)"

echo "== collections"
CR=$(j -X POST -H 'Content-Type: application/json' -d '{"name":"items"}' $BASE/v1/collections)
chk coll-create '"created":true' "$CR"
chk coll-create-buffered '"flushed":false' "$CR"
chk coll-exists409 '"status":409' "$(j -X POST -d '{"name":"items"}' $BASE/v1/collections)"
chk coll-list '"name":"items"' "$(j $BASE/v1/collections)"
# coll-list — read barrier: он обязан дренировать RAM-буфер в SQLite
chk wb-barrier-drained '"buffer_items":0' "$(j $BASE/v1/stats)"
chk coll-get '"name":"items"' "$(j $BASE/v1/collections/items)"
chk coll-bad-name '"status":400' "$(j -X POST -d '{"name":"bad name!"}' $BASE/v1/collections)"

echo "== docs CRUD"
chk insert1 '"_id"' "$(j -X PUT -d '{"title":"widget","price":10}' "$BASE/v1/collections/items/docs/it1?upsert=true")"
BK=$(j -X POST -d '[{"title":"gizmo","price":5},{"title":"gadget","price":25},{"title":"doohickey","price":7}]' $BASE/v1/collections/items/docs)
chk bulk '"inserted":3' "$BK"
chk bulk-buffered '"flushed":false' "$BK"
chk list-total '"total":4' "$(j "$BASE/v1/collections/items/docs")"
chk get-doc '"title":"widget"' "$(j $BASE/v1/collections/items/docs/it1)"
chk patch '"price":99' "$(j -X PATCH -d '{"price":99}' $BASE/v1/collections/items/docs/it1)"
chk put404 '"status":404' "$(j -X PUT -d '{"x":1}' $BASE/v1/collections/items/docs/nonexistent)"
chk upsert '"title":"new-upsert"' "$(j -X PUT -d '{"title":"new-upsert"}' "$BASE/v1/collections/items/docs/upkey1?upsert=true")"
chk delete '"deleted"' "$(j -X DELETE $BASE/v1/collections/items/docs/upkey1)"
chk get404 '"status":404' "$(j $BASE/v1/collections/items/docs/upkey1)"

echo "== фильтры/сортировка"
chk filter-price '"total":1' "$(j "$BASE/v1/collections/items/docs?price=5")"
chk filter-str '"total":1' "$(j "$BASE/v1/collections/items/docs?title=gizmo")"
chk filter-bool '"total"' "$(j "$BASE/v1/collections/items/docs?admin=true")"
chk order-desc '"widget"' "$(j "$BASE/v1/collections/items/docs?order=price:desc&limit=1")"
chk limit '"total":4' "$(j "$BASE/v1/collections/items/docs?limit=1")"
chk bad-field400 '"status":400' "$(j "$BASE/v1/collections/items/docs?bad%20field=1")"

echo "== SQL"
chk sql-ok '"n":4' "$(j -X POST -d '{"sql":"SELECT COUNT(*) AS n FROM docs WHERE collection='"'"'items'"'"'"}' $BASE/v1/query)"
chk sql-write400 '"status":400' "$(j -X POST -d '{"sql":"DELETE FROM docs"}' $BASE/v1/query)"
chk sql-stack400 '"status":400' "$(j -X POST -d '{"sql":"SELECT 1; SELECT 2"}' $BASE/v1/query)"
chk sql-arg '"x":30' "$(j -X POST -d '{"sql":"SELECT ? AS x","args":[30]}' $BASE/v1/query)"

echo "== индексы"
chk idx-create '"indexed":true' "$(j -X POST "$BASE/v1/collections/items/indexes?field=price")"
chk idx-list 'ix_items_price' "$(j $BASE/v1/collections/items/indexes)"
chk idx-drop '"dropped"' "$(j -X DELETE $BASE/v1/collections/items/indexes/price)"

echo "== большой ответ (chunked >64KB)"
BIG='['
for i in $(seq 1 300); do
  if [ "$i" -gt 1 ]; then BIG+=','; fi
  BIG+="{\"pad\":\"$(printf 'x%.0s' {1..500})\"}"
done
BIG+=']'
printf '%s' "$BIG" > "$(dirname "$0")/tmp-big.json"
chk bulk-big '"inserted":300' "$(curl -s "${H[@]}" -H 'Content-Type: application/json' --data-binary @"$(dirname "$0")/tmp-big.json" $BASE/v1/collections/items/docs)"
LISTRESP=$(j "$BASE/v1/collections/items/docs?limit=500")
if [ "${#LISTRESP}" -gt 65536 ]; then pass=$((pass+1)); echo "  ok   large response streamed (${#LISTRESP} bytes)";
else fail=$((fail+1)); echo "  FAIL large response too small: ${#LISTRESP}"; fi

echo "== экспорт/импорт"
EXP=$(j $BASE/v1/export)
chk export '"docs":' "$EXP"
if [[ "$EXP" != '{"schema_version":'* ]]; then
  echo "  debug export head: ${EXP:0:120}"
fi
chk import-replace '"mode":"replace"' "$(echo "$EXP" | curl -s "${H[@]}" -X POST --data-binary @- "$BASE/v1/import?mode=replace")"
chk count-after-import '"n":304' "$(j -X POST -d '{"sql":"SELECT COUNT(*) AS n FROM docs WHERE collection='"'"'items'"'"'"}' $BASE/v1/query)"

echo "== flush/backup"
# мутация прямо перед flush: буфер должен быть непустым, flush — дренировать его
j -X PUT -d '{"title":"wb-probe"}' "$BASE/v1/collections/items/docs/wb1?upsert=true" >/dev/null
BI=$(j $BASE/v1/stats | grep -o '"buffer_items":[0-9]*' | cut -d: -f2)
BA0=$(j $BASE/v1/stats | grep -o '"buffer_applies":[0-9]*' | cut -d: -f2)
if [ -n "$BI" ] && [ "$BI" -ge 1 ]; then pass=$((pass+1)); echo "  ok   write-back buffered in RAM (buffer_items=$BI)"
else fail=$((fail+1)); echo "  FAIL write-back: buffer_items=$BI, expected >=1 после мутации"; fi
chk flush '"flushed":true' "$(j -X POST $BASE/v1/flush)"
chk flush-drained '"buffer_items":0' "$(j $BASE/v1/stats)"
BA1=$(j $BASE/v1/stats | grep -o '"buffer_applies":[0-9]*' | cut -d: -f2)
if [ -n "$BA0" ] && [ -n "$BA1" ] && [ "$BA1" -gt "$BA0" ]; then pass=$((pass+1)); echo "  ok   flush применил буфер (applies $BA0 -> $BA1)"
else fail=$((fail+1)); echo "  FAIL flush applies: $BA0 -> $BA1"; fi
curl -s "${H[@]}" -o "$DDIR/backup-test.enc" $BASE/v1/backup
sz=$(stat -c %s "$DDIR/backup-test.enc")
if [ "$sz" -gt 100 ]; then pass=$((pass+1)); echo "  ok   backup ($sz bytes)"; else fail=$((fail+1)); echo "  FAIL backup size=$sz"; fi
hdr=$(head -c 8 "$DDIR/backup-test.enc")
if [ "$hdr" = "WEBDBENC" ]; then pass=$((pass+1)); echo "  ok   backup encrypted header"; else fail=$((fail+1)); echo "  FAIL backup header: $hdr"; fi

echo "== stats/RSS"
# Порог RSS настраивается (RSS_MAX_KB); дефолт — бюджет 10 МиB из спецификации.
# CI ставит больший порог: portable-сборка с bundled SQLite и x86-64 дают иной базовый RSS.
# Замер: берём минимальный из3 (устоявшийся RSS) — разовые выборки дёргаются
# на страничном кэше общих библиотек (±200 КиБ), бюджет при этом строгий.
RSS_MAX_KB="${RSS_MAX_KB:-10240}"
ST=""; RSS=""
for _try in 1 2 3; do
  ST=$(j $BASE/v1/stats)
  R=$(echo "$ST" | grep -o '"rss_kb":[0-9]*' | cut -d: -f2)
  if [ -n "$R" ]; then
    if [ -z "$RSS" ] || [ "$R" -lt "$RSS" ]; then RSS=$R; fi
  fi
  sleep 1
done
echo "  $ST"
if [ -n "$RSS" ] && [ "$RSS" -le "$RSS_MAX_KB" ]; then pass=$((pass+1)); echo "  ok   RSS=${RSS}kB <= ${RSS_MAX_KB}kB (min of3)";
else fail=$((fail+1)); echo "  FAIL RSS=${RSS}kB > ${RSS_MAX_KB}kB"; fi

echo
echo "RESULT: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
