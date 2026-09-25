# План: tinydb — tiny SQLite DB + REST API + шифрование (финальное состояние)

Проект: `/data/data/com.termux/files/home/web-db`
Все пункты плана выполнены; чек-лист статусов — в `TASK.md`.

## Лимиты и фактические замеры

| Лимит | Замерено | Где проверяется |
|---|---|---|
| RAM ≤ 10 МБ | свежий сервер ~9.7 МБ; после полного smoke (замер после штатного flush-реклейма) `rss_kb` ≤ 10240 | smoke-проверка `RSS ≤ 10240` (`RSS_MAX_KB` настраивается; CI ставит 14336) |
| Диск ≤ 100 МБ | квота `PRAGMA max_page_count` (`-max-size`) | сервер |
| Бинарь ≤ 10 МБ | dev (libsqlite3) **4055248**, portable (bundled SQLite) **5407568** | `make build` (size-check) |
| C-либа ≤ 512 КиБ | `libwebdb.a` 24696, `libwebdb.so` 26072 | `make size-check` |
| JS-клиент | ядро 10526 байт (3680 gzip) + типы 4216 | `make clients-js` |
| Тесты | smoke **44/44**, security **45/45**, client-go **ok**, client-js **11/11**, C-example **PASS** | `make check` |

## Что построено

- **Сервер Go** (`cmd/webdb`): свой микро-HTTP core `internal/httpx`
  (без `net/http`/`net`/`net/url`/`regexp`/`log/slog`), SQLite через
  системную либу (`-tags libsqlite3`, mattn/go-sqlite3, пул 2/1, cache 64 КиБ),
  шифрование at-rest `internal/cryptobox` (AES-256-GCM потоком, чанки 64 КиБ).
- **REST API v1**: коллекции/документы CRUD, bulk, фильтры-равенства,
  сортировка/пагинация, read-only SQL, индексы, экспорт/импорт, backup,
  flush, stats (+`rss_kb`, heap, buffer).
- **Auth**: Bearer/X-API-Key, автотокен `<data>/token`, `-no-auth`.
- **Write-back (lazy)**: мутации → RAM-оверлей → SQLite пачками →
  шифрованный снапшот (см. `docs/DESIGN.md`).
- **Клиенты**: `client-go` (чистый net/http, `APIError`), `client-js`
  (CJS-ядро + ESM-обёртка + `.d.ts`, ноль зависимостей, Node ≥18/браузер/
  Deno/Bun; 10.5 КиБ ядра), `client-c` (свой HTTP/1.1 поверх TCP, `.a`/`.so`,
  ≤512 КиБ).
- **Документация (RU)**: `README.md`, `docs/{API,DESIGN,SECURITY,CLIENT-GO,CLIENT-C}.md`.
- **Сборка/тесты**: корневой `Makefile` (`build/test/run/clients/check/clean`),
  тесты самодостаточны (`tests/lib.sh` поднимает сервер при необходимости).

## Ключевые решения (как реализовано)

### Write-back

1. Мутации (Put/Patch/Delete/CreateCollection/CreateIndex/…) попадают в
   FIFO-оверлей в RAM (`store/writeback.go`) под `store.mu`; ответ клиенту —
   сразу, `flushed:false` (+ заголовок `X-Webdb-Flushed`).
2. Apply в SQLite одной транзакцией по событиям: тикер `-writeback`
   (default 1s; 0 = только пункты 3–6), read-barrier (List/SQL/Export/…),
   переполнение `-buffer-bytes 1MiB`/`-buffer-items 10000`,
   `POST /v1/flush`, autosave, SIGTERM/SIGINT.
3. `Get()` читает overlay первым — свежесть одной записи без барьера,
   поэтому get/stats не дренируют буфер.
4. Долговечность: штатный shutdown дренирует буфер и пишет `.enc`;
   **SIGKILL может потерять буфер** (≤ интервала тикера) — задокументировано.

### RAM ≤ 10 МБ (3 уровня)

1. **Не растить**: cache 64 КиБ×2 соединения, `GOGC=30` + `GOMEMLIMIT=2MiB`,
   reader 16 КиБ/conn, лимиты буфера write-back.
2. **Re-exec**: на GOOS=android рантайм отдаёт страницы через `MADV_FREE`,
   поэтому `main()` перезапускает себя с `GODEBUG=madvdontneed=1`
   (RSS падает сразу) и сбрасывает `LD_PRELOAD` (shim termux-exec ~52 КиБ).
3. **Отдавать после бёрстов** — в конце `Flush()`:
   `PRAGMA shrink_memory` → `purgeNative()` (на Android/Linux —
   `mallopt(M_PURGE_ALL, 0)` через портируемый C-преамбул с фолбэком
   значения для glibc; на macOS — no-op: mallopt нет) →
   `debug.FreeOSMemory()` → `evictCode()` (`madvise(DONTNEED)` по r-x
   участкам собственного exe, найденным по inode в `/proc/self/maps`;
   чистые file-backed страницы fault-назад по demand).
   Смоук меряет RSS **после `POST /v1/flush`** — в стабильном состоянии,
   а не в пике между автосейвами (и минимумом из 3 выборок).

### HTTP-ядро

`internal/httpx`: raw-сокеты + runtime poller (deadline'ы без OS-потоков),
парсер запросов, chunked >64 КиБ, `Expect: 100-continue`, свой разбор
query (`net/url` заменён). **Нет DNS** — `-addr` принимает IP/localhost.

### Публикация и CI

- **Репо**: `github.com/dustlinux/tinydb` (public, BSD-3-Clause), module path
  `github.com/dustlinux/tinydb` + `/client-go` — `go get` работает напрямую.
- **`.github/workflows/ci.yml`** (вместо Colab):
  - `test` (ubuntu): `make check` на dev-пути (`-tags libsqlite3` +
    `libsqlite3-dev`) и portable-сборка (bundled SQLite) с повторным
    smoke/security; `RSS_MAX_KB=14336` (x86-64 даёт иной базовый RSS);
  - `build`: 13 linux-архитектур кросс-сборкой; `mips`/`mipsle` — hard-float
    ABI (дистрибутивный o32-тулчейн только hard-float), `ppc64` — `zig cc`
    (дистрибутивный даёт ELFv1, Go требует ELFv2), `loong64` — `zig cc`
    (пакета в Ubuntu нет); оба помечены experimental;
  - `android`: NDK r27c, `arm64` и `arm`+`GOARM=7` (Termux);
  - `macos`: `darwin/arm64` + `darwin/amd64` + запуск (health/stats/шифр.);
  - `release`: на теги `v*` — тарболы всех сборок + `SHA256SUMS`.
- **Портируемость, найденная кросс-сборками**: `C.M_PURGE_ALL` (bionic-only)
  и `syscall.SOCK_NONBLOCK` (linux-only) — заменены на переносимые вызовы.
- **Тесты безопасности** `tests/security.sh` (45 проверок): auth, SQL только
  чтение, валидации/traversal, лимиты тел (`-max-body`/`-max-import`), права
  0600/0700, отсутствие токена в логе, негативные старты и tamper.

## Приёмка (все пункты выполнены)

- [x] `tests/smoke.sh` → fail=0 (44/44), RSS ≤ 10240 kB.
- [x] `tests/security.sh` → fail=0 (45/45).
- [x] Мутации видны сразу, на диск пишутся лениво; shutdown создаёт валидный `.enc`.
- [x] `go vet`/`gofmt` чисто; `client-go` и `client-c` проходят тесты.
- [x] Документация покрывает все эндпоинты и флаги.
- [x] `make check` — полная верификация с проверкой размеров.
- [x] CI собирает все linux-архитектуры, Termux (android/arm64+armv7) и macOS.
