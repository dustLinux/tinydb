# tinydb

**Русский** | [English](README.en.md)

Маленькая база данных, которая живёт рядом с программой: SQLite внутри, наружу —
REST API. Данные на диске зашифрованы, процесс ест меньше 10 МБ RAM, а бинарь
собирается под все Linux-архитектуры, Termux и macOS.

Придумана не «на замену Postgres», а для конкретной задачи: поднять базу на
телефоне или в контейнере, где нет ни памяти, ни желания ставить Postgres.

```sh
./webdb -data ./data          # токен сгенерируется в ./data/token
```

Нужен Go (для сборки) или готовый бинарь из
[релизов](https://github.com/dustLinux/tinydb/releases/tag/v0.1.0).

[![ci](https://github.com/dustlinux/tinydb/actions/workflows/ci.yml/badge.svg)](https://github.com/dustlinux/tinydb/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dustlinux/tinydb/client-go.svg)](https://pkg.go.dev/github.com/dustlinux/tinydb/client-go)
[![license](https://img.shields.io/badge/license-BSD--3--Clause-green.svg)](LICENSE)

---

## За минуту

Подняли сервер, создали коллекцию, положили документ, прочитали обратно:

```sh
make run                          # или ./bin/webdb -addr 127.0.0.1:8099 -data ./data
TOKEN=$(cat data/token)           # токен автогенерируется при первом старте

curl -s -X POST -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"name":"items"}' \
     http://127.0.0.1:8099/v1/collections

curl -s -X PUT -H "Authorization: Bearer $TOKEN" \
     -d '{"title":"гайка","price":10}' \
     'http://127.0.0.1:8099/v1/collections/items/docs/1?upsert=true'

curl -s -H "Authorization: Bearer $TOKEN" \
     'http://127.0.0.1:8099/v1/collections/items/docs?order=price:desc&limit=10'
```

Всё, что умеет сервер, — в [docs/API.md](docs/API.md): коллекции, документы,
bulk-загрузки, индексы, фильтры, read-only SQL, экспорт/импорт, бэкап.

## Консоль

Если хочется не писать curl, есть `webdb shell` — консоль в духе sqlite3,
живущая внутри того же бинаря:

```sh
./bin/webdb shell -addr 127.0.0.1:8099 -data ./data
```

```
tinydb> .tables
tinydb> .put items it1 {"title":"гайка","price":10}
tinydb> SELECT count(*) AS n FROM docs;
tinydb> .schema items
tinydb> .stats
```

Понимает `.help`, `.tables`, `.schema`, `.ls`, `.get`, `.put`, `.rm`, `.index`,
`.headers on|off`, `.mode column|list|json`, `.stats`, `.flush`, `.export` и
read-only SQL. Одна команда вместо сессии: `webdb shell -cmd ".tables"`.
Подробности — [docs/SHELL.md](docs/SHELL.md).

## Клиенты

Три языка, ни одного лишнего байта зависимостей.

```go
// Go — только стандартная библиотека
c := webdb.New("http://127.0.0.1:8080", token)
doc, _ := c.Insert("users", map[string]any{"name": "alice"})
```

```js
// JS/TS — Node.js ≥18, браузер, Deno, Bun. Без сборки, типы из коробки
import { Client } from 'tinydb-client';
const c = new Client('http://127.0.0.1:8080', token);
const doc = await c.insert('users', { name: 'alice' });
```

```c
/* C — своя реализация HTTP/1.1, .a/.so весят меньше 30 КиБ */
webdb_client c;
webdb_client_init(&c, "http://127.0.0.1:8080", token);
```

| | установка | подробности |
|---|---|---|
| Go | `go get github.com/dustlinux/tinydb/client-go` | [docs/CLIENT-GO.md](docs/CLIENT-GO.md) |
| JS/TS | `npm install tinydb-client` | [client-js/README.md](client-js/README.md) |
| C | собирается из `client-c/` | [docs/CLIENT-C.md](docs/CLIENT-C.md) |

## Авторизация

По умолчанию — статический токен: он генерируется при первом старте, лежит в
`data/token` (права `0600`) и один на всё. Если приложению нужны разные
права, срок жизни или отзыв отдельных токенов — включите JWT:

```sh
export WEBDB_JWT_SECRET=$(openssl rand -hex 32)   # секрет на сервере и при выдаче
./webdb -data ./data -jwt-issuer tinydb -jwt-audience api

./webdb token -sub alice -ttl 24h -scp "read write" -iss tinydb -aud api
```

Сервер проверяет подпись HS256, `exp`/`nbf` и, если заданы, `iss`/`aud`.
Алгоритм берётся из конфига сервера, а не из заголовка токена, так что
`alg: none` и подмена RS/HS не проходят. Статический токен при этом продолжает
работать — JWT его дополняет, а не заменяет.

## Что внутри и почему

**SQLite, но свой HTTP-слой.** Сервер не берёт `net/http` — он разговаривает с
сокетом сам (`internal/httpx`). Звучит странно, но `net/http` с его пулами,
keep-alive и контекстами стоит заметно больше, чем весь остальной проект
вместе. Консоль наоборот использует тот же минимальный клиент и весит 3.5 МБ
вместо 6.7 МБ с `net/http`.

**Записи сначала в память.** Мутация возвращает ответ сразу, а в SQLite
попадает пачками — по таймеру, при чтении, при переполнении буфера или при
`POST /v1/flush`. На чтениях «свои» записи всегда видны, так что приложение не
замечает буферизации. Цена — `SIGKILL` может съесть последние записи (не больше,
чем за интервал `-writeback`); `Ctrl-C` и `SIGTERM` буфер дренируют всегда.

**На диске лежит только шифротекст.** При работе база расшифрована в
`data/db.sqlite` (права `0600`), но при каждом flush/autosave сервер пишет
`data/db.sqlite.enc` — потоковый AES-256-GCM с уникальным nonce на каждый
чанк — и удаляет plaintext. Ключ либо лежит рядом (`db.key`), либо его заменяет
пароль (`-passphrase`, KDF со 100k итерациями). Оговорка честная: если ключ лежит
рядом с базой, копия каталога = база; для настоящего разделения нужен
passphrase или внешний KMS.

**Память экономят на каждом шаге.** Кэш SQLite ограничен, Go-куча живёт под
`GOMEMLIMIT=2MiB`, а после каждого бёрста сервер отдаёт память операционной
системе: `shrink_memory`, `mallopt(M_PURGE_ALL)`, `FreeOSMemory` и `madvise`
по собственному коду бинаря. На Termux/arm64 сервер под нагрузкой — около
9–10 МБ RSS, и это проверяется автоматически (`GET /v1/stats → rss_kb`).

Подробности, включая формат контейнера и модель угроз: [docs/DESIGN.md](docs/DESIGN.md),
[docs/SECURITY.md](docs/SECURITY.md).

## Скачать

Готовые бинари — в [Releases](https://github.com/dustlinux/tinydb/releases),
вместе с `SHA256SUMS`. Собрано в каждом пуше, поэтому актуальная версия всегда
в [Actions](https://github.com/dustLinux/tinydb/actions).

| Платформа | Архитектуры |
|---|---|
| Linux | `amd64`, `386`, `arm64`, `armv7`, `riscv64`, `ppc64le`, `ppc64`, `s390x`, `mips64`, `mips64le`, `mips`, `mipsle`, `loong64` |
| Termux (Android) | `arm64`, `armv7` |
| macOS | `arm64` (Apple Silicon), `amd64` (Intel) |

Консоль — часть того же бинаря, так что она есть везде, где есть сервер.
В релиз также кладётся **скилл для ИИ-агентов**:
[`tinydb-agent-skill.tar.gz`](skills/tinydb/SKILL.md) — короткая инструкция
(на русском) о том, как поднять сервер, пользоваться консолью и API.

Windows не поддерживается и не планируется. Там, где в дистрибутивах нет
подходящего кросс-компилятора (`ppc64` с его ELFv2, `loong64`, mips с
hard-float ABI), сборка идёт через `zig cc` — версия и SHA-256 зафиксированы в
[ci.yml](.github/workflows/ci.yml).

Собрать самому:

```sh
make build      # Termux: pkg install clang go
go build -o webdb ./cmd/webdb   # SQLite вшит в бинарь, без зависимостей cgo
```

## Флаги

По умолчанию всё безопасное: сервер слушает только `127.0.0.1`, токен
генерируется сам, auth включён.

| Флаг | По умолчанию | Зачем |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | адрес; публичный доступ — только явно: `-addr :8080` |
| `-data` | `./data` | каталог данных (`db.sqlite`, `.enc`, `db.key`, `token`) |
| `-max-size` | `100` | квота на диск, МБ |
| `-autosave` | `60s` | как часто писать зашифрованный снапшот; `0` — только flush и завершение |
| `-writeback` | `1s` | как часто сливать RAM-буфер в SQLite; `0` — только по требованию |
| `-cache-kb` | `64` | кэш страниц SQLite на соединение, КиБ |
| `-go-memlimit` | `2` | лимит Go-кучи, МБ (`GOMEMLIMIT`) |
| `-max-body` | `4` | максимум тела запроса, МБ |
| `-max-import` | `64` | максимум для `/v1/import`, МБ (стримится, не буферизуется) |
| `-buffer-bytes` / `-buffer-items` | `1 MiB` / `10000` | пределы RAM-буфера |
| `-key-file` | `<data>/db.key` | ключ шифрования (32 байта) |
| `-passphrase` | — | шифровать паролем вместо файла ключа; лучше `WEBDB_PASSPHRASE` |
| `-token` / `-token-file` | авто | свой API-токен вместо сгенерированного; также `WEBDB_TOKEN` |
| `-jwt-secret` / `-jwt-secret-file` | — | принимать HS256-JWT; также `WEBDB_JWT_SECRET` |
| `-jwt-issuer` / `-jwt-audience` | — | требовать эти claim'ы в JWT |
| `-no-auth` | выкл. | выключить авторизацию. Только для локальной отладки |
| `-keep-plain` | выкл. | не удалять `db.sqlite` после завершения |
| `-cors` | выкл. | разрешить CORS (для отладки из браузера) |

## Про безопасность

Авторизация обязательна: `Authorization: Bearer <token>` или `X-API-Key`, всё
кроме `/health` закрыто. Токен генерируется криптослучайно, лежит в `0600` и
никогда не пишется в логи.

SQL в `/v1/query` действительно read-only, а не «словами»: запрос исполняется с
`PRAGMA query_only=1` и SQLite-authorizer'ом, который запрещает всё, кроме
чтения. Поэтому `WITH x AS (SELECT 1) DELETE FROM docs` получает `400` и
ничего не удаляет (такой баг был — поймали активным тестированием, теперь
закрыт тестом).

Проверено атаками на живом сервере, а не только чтением кода: обход авторизации
15 способами, path traversal, request smuggling, DoS, подделка `.enc`, права
файлов, попытки писать через SQL, подделки JWT. Из этого выросли 69 проверок в
`tests/security.sh` и 22 в `tests/jwt.sh`.

Чего нет: TLS (ставится за nginx/Caddy), rate limit, защита от локального
root — кто читает файл ключа, тот читает базу. Модель угроз целиком:
[docs/SECURITY.md](docs/SECURITY.md).

## Разработка

```sh
make check   # gofmt, vet, e2e (44), безопасность (69), JWT (22), shell (29), клиенты
```

- `tests/smoke.sh` — сквозной тест API, write-back, экспорт/импорт, RSS.
- `tests/security.sh` — 69 проверок безопасности.
- `tests/jwt.sh` — выпуск и проверка JWT, включая `alg: none` и подмену подписи.
- `tests/shell.sh` — сценарии консоли против живого сервера.
- `client-go`, `client-js`, `client-c` — у каждого свои тесты против живого сервера.

CI гоняет всё это на Ubuntu, поднимает бинарь на macOS и собирает все
архитектуры из таблицы выше; по тегу `v*` собирает релиз с тарболами,
`SHA256SUMS` и скиллом для агентов.

## Для ИИ-агентов

В репозитории лежит [skills/tinydb/SKILL.md](skills/tinydb/SKILL.md) — короткая
инструкция: как поднять сервер, что умеет консоль, таблица эндпоинтов, типичные
грабли (read-only SQL, write-back, лимиты, токен) и команды проверки. В релизе
доступна как `tinydb-agent-skill.tar.gz`.

## Ещё почитать

- [docs/API.md](docs/API.md) — все эндпоинты с примерами.
- [docs/DESIGN.md](docs/DESIGN.md) — как устроены write-back, шифротекст и RAM-бюджет.
- [docs/SECURITY.md](docs/SECURITY.md) — ключи, контейнер `WEBDBENC`, модель угроз.
- [PLAN.md](PLAN.md) — архитектура и решения с обоснованием.

## Лицензия

BSD 3-Clause, см. [LICENSE](LICENSE).
