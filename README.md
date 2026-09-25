# tinydb

[![ci](https://github.com/dustlinux/tinydb/actions/workflows/ci.yml/badge.svg)](https://github.com/dustlinux/tinydb/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dustlinux/tinydb/client-go.svg)](https://pkg.go.dev/github.com/dustlinux/tinydb/client-go)
[![license](https://img.shields.io/badge/license-BSD--3--Clause-green.svg)](LICENSE)

Крошечная встраиваемая база данных с REST API (ранее — `web-db`): SQLite на
бэкенде, шифрование at-rest по умолчанию, сервер на Go (без `net/http` и
`net`), клиентские библиотеки для Go и C.

Ориентировочный бюджет (Termux/Android arm64, замерено):

| Метрика | Значение |
|---|---|
| RSS сервера под нагрузкой | ≤ 10 МБ (`rss_kb` ≤ 10240, smoke-проверка) |
| Бинарь `webdb` | ~4.0 МБ (`-ldflags "-s -w"`, `-gcflags=all=-B`) |
| Диск (квота) | 100 МБ (`-max-size`) |
| C-клиент: `libwebdb.a` / `libwebdb.so` | 24696 / 26072 байт (лимит 512 КиБ) |

## Установка

### Go-клиент

```sh
go get github.com/dustlinux/tinydb/client-go
```

```go
package main

import (
	"fmt"

	webdb "github.com/dustlinux/tinydb/client-go"
)

func main() {
	c := webdb.New("http://127.0.0.1:8080", "токен-из-data/token")
	h, err := c.Health()
	if err != nil {
		panic(err)
	}
	fmt.Println(h.Status)
}
```

Клиент — только stdlib (`net/http`, `encoding/json`), никаких зависимостей.
Подробнее — [docs/CLIENT-GO.md](docs/CLIENT-GO.md); для C —
[docs/CLIENT-C.md](docs/CLIENT-C.md).

### Сервер

- **Бинарники под все архитектуры** собирает CI и публикует в
  [Releases](https://github.com/dustlinux/tinydb/releases) (см. таблицу ниже).
- **Из исходников**: `make build` (Termux: `pkg install clang go`, системный
  SQLite) или `go build -o webdb ./cmd/webdb` (bundled SQLite, без cgo-зависимостей
  от системы — так собирает CI).

## Quickstart

```sh
# запуск (токен автогенерируется в data/token)
make run
# эквивалент:
./bin/webdb -addr 127.0.0.1:8099 -data ./data -writeback 1s

TOKEN=$(cat data/token)

curl -s -H "Authorization: Bearer $TOKEN" -X POST \
  -H 'Content-Type: application/json' \
  -d '{"name":"items"}' http://127.0.0.1:8099/v1/collections

curl -s -H "Authorization: Bearer $TOKEN" -X PUT \
  -d '{"title":"widget","price":10}' \
  'http://127.0.0.1:8099/v1/collections/items/docs/it1?upsert=true'

curl -s -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:8099/v1/collections/items/docs?order=price:desc&limit=10'
```

Полный список эндпоинтов — [docs/API.md](docs/API.md).

## Архитектуры (собирает CI)

| GOOS | GOARCH |
|---|---|
| `linux` | `amd64`, `386`, `arm64`, `armv7`, `riscv64`, `ppc64le`, `ppc64`, `s390x`, `mips64le`, `mips64`, `mipsle`, `mips`, `loong64`* |
| `darwin` | `arm64`, `amd64` (macOS) |
| `android` | `arm64`, `armv7` — то, что нужно Termux (aarch64 / armv7) |

Сборки cgo: Linux — кросс-gcc из apt, macOS — родной `clang` (плюс `-arch
x86_64`), Android — NDK clang. `loong64` помечен как экспериментальный
( зависит от наличия кросс-тулчейна в runner'е). Windows намеренно не
поддерживается.

## Как это работает

- **Хранение** — один SQLite-файл `data/db.sqlite` (plaintext, 0600) во время
  работы. На старте сервер расшифровывает рядом лежащий `data/db.sqlite.enc`;
  при flush/autosave/shutdown записывает новый зашифрованный снапшот и
  (по умолчанию) удаляет plaintext. Формат контейнера — `WEBDBENC`
  (AES-256-GCM, потоковое шифрование по 64 КиБ-чанкам), см.
  [docs/SECURITY.md](docs/SECURITY.md).
- **Write-back (lazy)** — мутации сначала попадают в RAM-буфер и применяются
  в SQLite пачками: фоновым тикером (`-writeback`), при чтениях (read
  barrier), при переполнении буфера, по `POST /v1/flush` и при штатном
  завершении. Чтения видят свои записи всегда: `Get()` смотрит в overlay
  первым. **SIGKILL может потерять буфер** (до интервала тикера) —
  SIGINT/SIGTERM дренируют его обязательно. Подробнее —
  [docs/DESIGN.md](docs/DESIGN.md).
- **Транспорт** — собственный минимальный HTTP/1.1 на raw-сокетах
  (`internal/httpx`): без `net/http`, `net`, `net/url`, `regexp`, `log/slog`.
  Это осознанный выбор ради RAM/размера бинаря, а не амбиция —
  см. [docs/DESIGN.md](docs/DESIGN.md).

## Флаги

| Флаг | По умолчанию | Назначение |
|---|---|---|
| `-addr` | `:8080` | адрес listen (хост — только IP-литерал или `localhost`, DNS нет) |
| `-data` | `./data` | каталог данных (`db.sqlite`, `db.sqlite.enc`, `db.key`, `token`) |
| `-max-size` | `100` | квота на диске, МиБ |
| `-cache-kb` | `64` | кэш страниц SQLite в КиБ **на соединение** (RAM-бюджет) |
| `-go-memlimit` | `2` | лимит Go-кучи, МиБ (`GOMEMLIMIT`), 0 = выкл. |
| `-autosave` | `60s` | писать шифрованный снапшот каждые N (`0` = только flush/shutdown) |
| `-writeback` | `1s` | применять RAM-буфер в SQLite каждые N (`0` = только барьеры/лимиты/flush/shutdown) |
| `-buffer-bytes` | `1048576` | лимит буфера write-back, байт |
| `-buffer-items` | `10000` | лимит буфера write-back, число мутаций |
| `-keep-plain` | `false` | оставить `db.sqlite` после завершения |
| `-max-body` | `4` | максимум тела запроса, МиБ |
| `-max-import` | `64` | максимум `/v1/import`, МиБ (стримится, не буферизуется в RAM) |
| `-cors` | `false` | permissive CORS (для отладки) |
| `-token` / `-token-file` | — | API-токен; по умолчанию генерируется в `<data>/token`; также env `WEBDB_TOKEN` |
| `-no-auth` | `false` | отключить auth (только локальная разработка!) |
| `-key-file` | `<data>/db.key` | 32-байтовый ключ шифрования |
| `-passphrase` / `-passphrase-file` | — | шифровать паролем вместо keyfile (лучше env `WEBDB_PASSPHRASE`) |

## Лимиты и гарантии

- **RAM ≤ 10 МБ** — RSS держится в этом коридоре: кэш SQLite 64 КиБ×2
  соединения, `GOGC=30` + `GOMEMLIMIT=2MiB`, после каждой фазы бёрста
  `Flush()` возвращает память ОС (`shrink_memory`, `mallopt(M_PURGE_ALL)`,
  `FreeOSMemory`, `madvise(DONTNEED)` по r-x бинаря). Метрика —
  `GET /v1/stats → rss_kb`.
- **Диск ≤ 100 МБ** — `PRAGMA max_page_count` под квоту.
- **Долговечность** — штатное завершение (SIGINT/SIGTERM) гарантирует
  драйн write-back буфера + шифрованный снапшот. SIGKILL = потеря
  неприменённых мутаций (до интервала `-writeback`).

## Тесты

```sh
make test    # gofmt + vet + smoke (44) + security (45) + клиенты Go/C
make check   # то же + контроль размеров (бинарь ≤10 МиБ, C-клиент ≤512 КиБ)
```

- `tests/smoke.sh` — e2e по API, включая write-back, экспорт/импорт, RSS.
- `tests/security.sh` — **тесты безопасности**: auth (401/`X-API-Key`),
  SQL-инъекции (SELECT-only, без стека, параметризация), валидации имён и
  path traversal, лимиты тел (`413`), права файлов (`0700`/`0600`), отсутствие
  токена в логах, CORS выключен, негативные старты (битый/чужой ключ,
  подделанный `db.sqlite.enc`, неверный passphrase) и режим passphrase.
- `tests/c-example.sh` — C-клиент против живого сервера.

CI (GitHub Actions) гоняет `make check`-набор на Ubuntu, дополнительно
проверяет запуск бинарника на macOS и собирает все архитектуры из таблицы
выше (артефакты — в Actions, релизные тарболы — под тегом `v*`).

## Структура

```
├── cmd/webdb/           точка входа: флаги, re-exec (GODEBUG), сигналы
├── internal/
│   ├── httpx/           минимальный HTTP/1.1: сокеты, парсер, чанки, query
│   ├── logx/            ключ=значение логгер (замена log/slog)
│   ├── cryptobox/       потоковое AES-256-GCM, контейнер WEBDBENC, KDF
│   ├── store/           движок: SQLite + write-back overlay + flush/evict
│   └── server/          REST-хендлеры v1, auth, stats
├── client-go/           Go-клиент (github.com/dustlinux/tinydb/client-go)
├── client-c/            C-клиент (libwebdb.a/.so) + example
├── tests/               smoke.sh, security.sh, c-example.sh, lib.sh
├── .github/workflows/   CI: тесты + сборки всех архитектур + releases
├── docs/                API, DESIGN, SECURITY, CLIENT-GO, CLIENT-C
├── PLAN.md, TASK.md     архитектура и чек-лист
└── Makefile             build / test / run / clients / check / clean
```

## Документация

- [docs/API.md](docs/API.md) — все эндпоинты, параметры, коды ошибок, curl.
- [docs/DESIGN.md](docs/DESIGN.md) — архитектура, write-back, RAM-бюджет.
- [docs/SECURITY.md](docs/SECURITY.md) — шифрование, ключи, threat model.
- [docs/CLIENT-GO.md](docs/CLIENT-GO.md), [docs/CLIENT-C.md](docs/CLIENT-C.md) — клиенты.

## Лицензия

BSD 3-Clause — см. [LICENSE](LICENSE).
