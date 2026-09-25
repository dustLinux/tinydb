---
name: tinydb
description: Как работать с tinydb — встраиваемой SQLite-базой с REST API и шифрованием at-rest. Используй, когда задача связана с tinydb: запуск сервера, консоль webdb-shell, REST-запросы, коллекции и документы, индексы, read-only SQL, экспорт/импорт, бэкап, авторизация (статический токен или JWT), тесты и сборка. Триггеры: «tinydb», «webdb», «webdb-shell», «tinydb REST», «tinydb консоль», «база с шифрованием tinydb».
---

# tinydb для ИИ-агента

tinydb — это сервер на Go поверх SQLite: наружу REST API, на диске только
шифротекст (AES-256-GCM), процесс ест меньше 10 МБ RAM. Клиенты есть на Go, JS/TS
и C, плюс консоль `webdb-shell` в духе sqlite3.

Ниже — только то, что нужно, чтобы начать работать. Подробности в
`docs/API.md` (эндпоинты), `docs/SECURITY.md` (ключи и угрозы), `docs/DESIGN.md`
(write-back и память).

## 1. Поднять сервер

```sh
make build                 # собрать сервер (нужны Go и clang)
make run                   # запустить на 127.0.0.1:8099, данные в ./data
# или напрямую:
./bin/webdb -addr 127.0.0.1:8099 -data ./data -writeback 1s
```

Токен генерируется сам при первом старте: `cat ./data/token`. Он же нужен для
всех запросов. `-addr` по умолчанию слушает только loopback — для доступа из
сети нужно указать `-addr :8099` явно (и тогда ставить TLS-прокси).

## 2. Консоль — самый быстрый способ потрогать данные

```sh
make shell                                        # собрать консоль
./bin/webdb-shell -addr 127.0.0.1:8099 -data ./data
```

```
tinydb> .help                 справка по командам
tinydb> .tables               коллекции и число документов
tinydb> .schema items         поля и индексы коллекции
tinydb> .put items it1 {"title":"гайка","price":10}
tinydb> .ls items             документы коллекции
tinydb> SELECT count(*) AS n FROM docs;
tinydb> .index add items price
tinydb> .stats                состояние: буфер, размеры, RSS
tinydb> .quit
```

Полезно: `.headers on|off`, `.mode column|list|json`, `.export`, `.flush`,
`.get COLL ID`, `.rm COLL ID`. Незакрытая кавычка или скобка включает
многострочный ввод. Однострочный запуск: `webdb-shell -cmd ".tables"`.

## 3. REST API — если нужен код

| Что | Запрос |
|---|---|
| создать коллекцию | `POST /v1/collections` `{"name":"items"}` |
| список коллекций | `GET /v1/collections` |
| вставить документ | `POST /v1/collections/{c}/docs` `{"title":"x"}` |
| вставить с id | `PUT /v1/collections/{c}/docs/{id}?upsert=true` |
| прочитать | `GET /v1/collections/{c}/docs/{id}` |
| список с фильтром | `GET /v1/collections/{c}/docs?limit=10&order=price:desc&color=red` |
| bulk | `POST /v1/collections/{c}/docs` `[{},{}]` |
| частичное обновление | `PATCH /v1/collections/{c}/docs/{id}` `{"a":1}` |
| удалить | `DELETE /v1/collections/{c}/docs/{id}` |
| индекс | `POST /v1/collections/{c}/indexes?field=price` |
| read-only SQL | `POST /v1/query` `{"sql":"SELECT ...","args":[...]}` |
| экспорт / импорт | `GET /v1/export`, `POST /v1/import?mode=merge` |
| бэкап (шифротекст) | `GET /v1/backup` |
| дренировать буфер | `POST /v1/flush` |
| метрики | `GET /v1/stats` → `rss_kb`, `buffer_items` |

Заголовок `Authorization: Bearer <token>` обязателен (кроме `/health`).

## 4. Особенности, о которые легко споткнуться

- **SQL только для чтения.** `SELECT`/`WITH` работают, любые изменения — `400`.
  Это не фильтр по словам: запрос исполняется под `PRAGMA query_only` и
  SQLite-authorizer'ом, так что `WITH x AS (SELECT 1) DELETE FROM docs` тоже
  отвергается. Для изменений есть эндпоинты выше.
- **Записи сначала в память.** Мутация отвечает сразу, в SQLite попадает позже
  (тикер, чтение, переполнение буфера, `POST /v1/flush`, завершение). Чтения
  всегда видят свои записи. `SIGKILL` может потерять буфер; `SIGINT`/`SIGTERM`
  — нет.
- **Имена ограничены:** `A-Za-z0-9_`, до 64 символов, префикс `_` зарезервирован
  под системные таблицы. Дефис в имени коллекции допустим, но в SQL
  идентификаторы оборачиваются в кавычки автоматически.
- **Лимиты по умолчанию:** квота диска 100 МБ, тело запроса 4 МБ, импорт 64 МБ.
  Превышение → `413` или понятная ошибка квоты.
- **JWT (если включён):** сервер принимает и статический токен, и HS256-JWT
  (`-jwt-secret`, `-jwt-issuer`, `-jwt-audience`). Выпустить токен:
  `WEBDB_JWT_SECRET=... ./bin/webdb token -sub alice -ttl 24h -iss tinydb -aud api`.
- **На диске:** `db.sqlite` (plaintext, только пока сервер работает),
  `db.sqlite.enc` (WEBDBENC, шифротекст), `db.key` (или пароль вместо него),
  `token`. Права: каталог `0700`, файлы `0600`.

## 5. Клиенты

- Go: `go get github.com/dustlinux/tinydb/client-go` (только stdlib).
- JS/TS: `npm install tinydb-client` (Node ≥18 / браузер / Deno / Bun, типы).
- C: `client-c/` → `libwebdb.a` / `libwebdb.so`, ~25 КиБ.

## 6. Проверить изменения

```sh
make check     # gofmt, vet, smoke (44), безопасность (69), JWT, shell, Go/JS/C-клиенты
```

Точечно: `make smoke`, `make security`, `bash tests/shell.sh`,
`bash tests/jwt.sh`, `go test ./internal/...`.

## 7. Если что-то не работает

- `401` — токен: `cat ./data/token`, проверить заголовок `Authorization`.
- `500` c `near "...": syntax error` при flush — в буфере осталась операция,
  которую SQLite не смог выполнить; смотри лог сервера (там `write-back op failed`).
- Сервер не стартует и пишет про `decrypt` — не тот ключ или повреждён `.enc`.
  Восстановление: положить `.enc` и `db.key` из резервной копии рядом.
- Данные есть, а `SELECT` возвращает 0 — скорее всего, сервер запущен на другом
  каталоге `-data`.
