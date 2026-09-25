# tinydb REST API v1

Базовый URL: `http://127.0.0.1:8080` (по умолчанию `:8080`).

## Аутентификация

Все `/v1/*`, кроме явно отмеченных, требуют токен:

```
Authorization: Bearer <token>
```

или альтернативно `X-API-Key: <token>`. Токен лежит в `<data>/token`
(создаётся при первом старте), задаётся флагом `-token`/`-token-file`
или env `WEBDB_TOKEN`. Без токена → `401`. `-no-auth` отключает проверку
(только локальная разработка).

`GET /health` и `GET /` токена не требуют.

## Формат ответов

- Успех: JSON-объект с полями результата; `200 OK` (создание — `200` с
  `"created": true`).
- Ошибка: `{"status": <code>, "error": "<message>"}` с соответствующим
  HTTP-кодом.
- Заголовок `X-Webdb-Flushed: true|false` ставится на ответы мутаций:
  `false` означает, что мутация принята в RAM-буфер write-back и ещё не
  в SQLite (дублируется полем `"flushed"` в теле).

Коды ошибок: `400` — невалидный запрос/имя/JSON; `401` — нет токена;
`404` — нет коллекции/дока; `409` — конфликт (коллекция существует);
`413` — тело больше `-max-body`, импорт больше `-max-import`; `429`/`507` — переполнение квоты;
`500` — внутренняя ошибка.

## Эндпоинты

### Служебные

#### `GET /health` (без токена)

```json
{"status":"ok"}
```

#### `GET /v1/stats`

Снимок состояния: число коллекций/доков, байты, квота, состояние
write-back буфера, RSS и куча Go.

```sh
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/v1/stats
```

```json
{
  "collections": 1, "docs": 305, "data_bytes": 153136,
  "plain_bytes": 1855488, "enc_bytes": 1856459,
  "max_bytes": 104857600, "encrypted": true, "dirty": false,
  "flushes": 4, "last_flush_unix": 1790262807,
  "buffer_items": 0, "buffer_bytes": 0, "buffer_applies": 6,
  "buffer_max_bytes": 1048576,
  "rss_kb": 9976, "heap_alloc_kb": 664, "heap_sys_kb": 7168,
  "heap_idle_kb": 5768,
  "schema_version": "1", "server_version": "1.0.0"
}
```

- `buffer_*` — текущее состояние RAM-буфера write-back (`buffer_items > 0`
  значит мутации ещё не в SQLite); `buffer_applies` — сколько раз буфер
  применялся.
- `rss_kb` — VmRSS процесса; проверка бюджета RAM — `≤ 10240`.
- Эндпоинт **не** является read-barrier (буфер не дренирует).

#### `POST /v1/flush` (мутация)

Дренирует write-back буфер в SQLite одной транзакцией, синхронизирует
`db.sqlite`, пишет шифрованный снапшот `db.sqlite.enc` и возвращает
память ОС. Ответ:

```json
{"flushed": true, "path": "data/db.sqlite.enc"}
```

### Коллекции

#### `POST /v1/collections` — создать

Тело: `{"name": "items"}`. Имя: 1–64 символа, буквы/цифры/`_`/`-`,
первый символ — буква или `_`. Имена, начинающиеся с `_`, зарезервированы
за системными таблицами (`_collections`, `_meta`) → `400`.

```sh
curl -s -H "Authorization: Bearer $TOKEN" -X POST \
  -H 'Content-Type: application/json' -d '{"name":"items"}' \
  http://127.0.0.1:8080/v1/collections
# {"name":"items","created":true,"flushed":false}
```

Существующая → `409`. Ошибка в имени → `400`.

#### `GET /v1/collections` — список (read-barrier)

```json
{"collections":[{"name":"items","docs":4,"created_ms":...}],"count":1}
```

#### `GET /v1/collections/{coll}` — метаданные (read-barrier)

Нет коллекции → `404`.

#### `DELETE /v1/collections/{coll}` — удалить коллекцию и все её доки

Применяет буфер перед удалением, ответ всегда `"flushed": true`.

```json
{"dropped":"items","flushed":true}
```

### Документы

Документ — произвольный JSON-объект. Идентификатор `_id` задаётся в URL;
`_created_ms`/`_updated_ms` сервер управляет сам.

#### `PUT /v1/collections/{coll}/docs/{id}?upsert=true` — вставить/заменить

```sh
curl -s -H "Authorization: Bearer $TOKEN" -X PUT \
  -d '{"title":"widget","price":10}' \
  'http://127.0.0.1:8080/v1/collections/items/docs/it1?upsert=true'
# {"_id":"it1","collection":"items","created":...,"updated":...,
#  "data":{"title":"widget","price":10}}
# + заголовок X-Webdb-Flushed: false (мутация в RAM-буфере)
```

Без `upsert=true` существующий `_id` → `409`. Нет коллекции → `404`.

#### `POST /v1/collections/{coll}/docs` — bulk-вставка

Тело — массив объектов. Ответ: `{"inserted":3,"docs":[...],"flushed":false}`.

#### `GET /v1/collections/{coll}/docs` — список (read-barrier)

Параметры query:

| Параметр | Пример | Описание |
|---|---|---|
| `limit` | `50` | максимум доков; дефолт 50, максимум 500 |
| `offset` | `100` | смещение (≥0) |
| `order` | `price:desc,name:asc` | сортировка через `:`; без части — asc |
| *любое другое имя* | `price=5` | фильтр: **равенство** по полю дока. Значение парсится как JSON (`true`, `10`, `"x"`), иначе трактуется как строка. Имя поля — `A-Za-z0-9_-`, иначе `400` |

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:8080/v1/collections/items/docs?price=5&order=price:desc&limit=10'
```

Ответ: `{"items":[{"_id":"it1","collection":"items","created":...,
"updated":...,"data":{...}}], "total":4, "truncated":false}` — `total`
всех доков под фильтром (до limit/offset), `truncated=true`, если
скан-лимит (2 МиБ) исчерпан. Невалидное имя поля → `400`.

Несколько фильтров складываются по `AND`. Диапазоны/операторы (`$gte`,
`$like`…) в API нет — для них есть SQL: `POST /v1/query` с
`json_extract`.

#### `GET /v1/collections/{coll}/docs/{id}` — получить (overlay→SQLite)

Нет дока → `404`.

#### `PATCH /v1/collections/{coll}/docs/{id}` — точечное обновление

Тело — объект изменений (мелкий merge верхнего уровня).

#### `DELETE /v1/collections/{coll}/docs/{id}`

```json
{"deleted":"it1","flushed":false}
```

### SQL (read-only)

#### `POST /v1/query`

```json
{"sql":"SELECT COUNT(*) AS n FROM docs WHERE collection=?","args":[42]}
```

```sh
curl -s -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"sql":"SELECT COUNT(*) AS n FROM docs"}' \
  http://127.0.0.1:8080/v1/query
# {"columns":["n"],"rows":[[305]]}
```

Только `SELECT`/`WITH` на чтение. Попытка записи (`INSERT`,
`UPDATE`, `DELETE`, `DROP`…) → `400`. Стек из более одного запроса
в `sql` → `400`. `args` — массив скаляров (`?`-плейсхолдеры).
`collection`/`docs` — таблицы: `docs(collection TEXT, id TEXT,
data TEXT, ...)`, `_collections(name TEXT, ...)`.

### Индексы

Индекс — выражение по полю дока (JSON-функции SQLite).

#### `POST /v1/collections/{coll}/indexes?field=price`

```json
{"field":"price","indexed":true,"flushed":false}
```

#### `GET /v1/collections/{coll}/indexes` (read-barrier)

```json
{"indexes":["idx_price"]}
```

#### `DELETE /v1/collections/{coll}/indexes/{field}`

```json
{"dropped":"price","flushed":true}
```

### Экспорт / импорт / бэкап

#### `GET /v1/export` (read-barrier)

Весь датасет одним JSON-потоком (незашифрованный, для переноса).

#### `POST /v1/import?mode=replace|merge`

Тело — JSON из `/v1/export`. `replace` (по умолчанию) перезаписывает
коллекции, `merge` — вливает (upsert по `_id`). Импорт стримится (не
буферизуется в RAM), но ограничен `-max-import` (64 МиБ по умолчанию) →
`413` при превышении; превышение откатывается транзакцией, без частичного
применения.

```sh
curl -s -H "Authorization: Bearer $TOKEN" "$B/v1/export" > db.json
curl -s -H "Authorization: Bearer $TOKEN" -X POST \
  --data-binary @db.json "$B/v1/import?mode=replace"
```

#### `GET /v1/backup`

Шифрованный снапшот `db.sqlite.enc` как есть (`Content-Disposition:
attachment`). Предварительно вызывает flush. Метод безопасен для
целостности: читается готовый файл.

### CORS

При `-cors` на все ответы добавляются `Access-Control-Allow-Origin: *`
и обрабатываются preflight `OPTIONS`.

## Read-barrier и write-back

Эндпоинты, дренирующие RAM-буфер в SQLite перед работой (чтение обязано
видеть все принятые мутации): `GET /v1/collections`,
`GET /v1/collections/{coll}`, `GET .../docs` (список),
`GET .../indexes`, `POST /v1/query`, `GET /v1/export`,
`DELETE /v1/collections/{coll}`. `GET /v1/stats`, `GET .../docs/{id}`
барьер **не** ставят: overlay в `Get()` гарантирует свежесть одной
записи, а статистика отражает буфер отдельными полями `buffer_*`.
