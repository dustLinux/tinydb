# План: tinydb — tiny SQLite DB + REST API + шифрование (финальное состояние)

Проект: `/data/data/com.termux/files/home/web-db`
Все пункты плана выполнены; чек-лист статусов — в `TASK.md`.

## Лимиты и фактические замеры

| Лимит | Замерено | Где проверяется |
|---|---|---|
| RAM ≤ 10 МБ | свежий сервер ~9.7 МБ; после полного smoke `rss_kb` = 9976–10116 | smoke-проверка `RSS ≤ 10240` |
| Диск ≤ 100 МБ | квота `PRAGMA max_page_count` (`-max-size`) | сервер |
| Бинарь ≤ 10 МБ | **4046256 байт (3.86 МиБ)** | `make build` (size-check) |
| C-либа ≤ 512 КиБ | `libwebdb.a` 24696, `libwebdb.so` 26072 | `make size-check` |
| Тесты | smoke **44/44**, client-go **ok**, C-example **PASS** | `make check` |

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
- **Клиенты**: `client-go` (чистый net/http, `APIError`), `client-c`
  (свой HTTP/1.1 поверх TCP, `.a`/`.so`, ≤512 КиБ).
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
   `PRAGMA shrink_memory` → `mallopt(M_PURGE_ALL)` (cgo; bionic прячет
   `mallopt` за availability-guard, прототип объявлен вручную) →
   `debug.FreeOSMemory()` → `evictCode()` (`madvise(DONTNEED)` по r-x
   участкам собственного exe, найденным по inode в `/proc/self/maps`;
   чистые file-backed страницы fault-назад по demand).

### HTTP-ядро

`internal/httpx`: raw-сокеты + runtime poller (deadline'ы без OS-потоков),
парсер запросов, chunked >64 КиБ, `Expect: 100-continue`, свой разбор
query (`net/url` заменён). **Нет DNS** — `-addr` принимает IP/localhost.

## Приёмка (все пункты выполнены)

- [x] `tests/smoke.sh` → fail=0 (44/44), RSS ≤ 10240 kB.
- [x] Мутации видны сразу, на диск пишутся лениво; shutdown создаёт валидный `.enc`.
- [x] `go vet`/`gofmt` чисто; `client-go` и `client-c` проходят тесты.
- [x] Документация покрывает все эндпоинты и флаги.
- [x] `make check` — полная верификация с проверкой размеров.
