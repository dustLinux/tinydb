# TASK.md — tinydb (план задач)

Статусы: `[ ]` не начато, `[~]` в работе, `[x]` готово.
Проект: `/data/data/com.termux/files/home/web-db`

## Этап 0. Текущее состояние

- [x] Каркас проекта: cmd/webdb, internal/{httpx,logx,cryptobox,store,server}
- [x] Шифрование at-rest: AES-256-GCM потоковое, keyfile/passphrase
- [x] REST API v1 (коллекции, доки, bulk, фильтры, SQL RO, индексы,
      экспорт/импорт, backup, flush, stats+rss_kb)
- [x] Auth (Bearer / X-API-Key), автотокен `<data>/token`
- [x] Квота диска 100 МБ (max_page_count), RAM-лимиты (cache 64КБ, GOMEMLIMIT)
- [x] Смоук-тест: **44/44, fail=0** (включая write-back и RSS-метрику)
- [x] Тесты безопасности: **45/45, fail=0** (tests/security.sh)
- [x] CI на GitHub Actions: тесты + сборки всех архитектур + releases

## Этап 1. Багфиксы (P0/P1)

- [x] **P0** chunked >64 КиБ: в `writeChunk` отсутствовал `\r\n` после размера
      чанка + при переходе buffered→chunked терялись накопленные 64 КиБ.
- [x] **P0** «Числовой фильтр» — баг теста (значение уже спатчено).
- [x] **P1** `GET /indexes` пуст — escapeLike экранировал концевой `%`.
- [x] **P1** Правки smoke.sh: reset state, `?upsert=true`, детерминизм.

## Этап 1.5. RSS ≤ 10 МБ

- [x] Замер компонентов RSS (nm text, smaps по регионам, diff до/после нагрузки).
- [x] Урезание: regexp (→ ручные валидаторы), log/slog (→ internal/logx),
      net (→ raw-сокеты в httpx/sock.go), net/url (→ httpx/query.go),
      SQLite-пул 2/1 conns + cache 64КБ, `-gcflags=-B`.
- [x] Re-exec c `GODEBUG=madvdontneed=1` (GOOS=android по умолчанию MADV_FREE)
      и сбросом `LD_PRELOAD` (shim termux-exec ~52 КБ RSS).
- [x] Возврат памяти после бёрстов в `Flush()`: `PRAGMA shrink_memory`,
      `mallopt(M_PURGE_ALL)` (cgo), `debug.FreeOSMemory()`, `evictCode()`
      (madvise DONTNEED на r-x собственного exe; страницы file-backed,
      чистые — возвращаются из page cache по demand).
- [x] `/v1/stats.rss_kb ≤ 10240` в конце smoke: **9976 кБ** (fresh ~9.7 МБ).

## Этап 2. Lazy loading / write-back

- [x] Оверлей в `internal/store/writeback.go`: FIFO мутаций (put/del/coll/idx),
      `Get()` читает overlay→SQLite, read-barriers дренируют буфер.
- [x] Фоновый тикер `Writeback(interval)`; лимиты `-buffer-bytes 1MiB` /
      `-buffer-items 10000` → синхронный apply; `Flush()`, `Close()`.
- [x] Флаги: `-writeback 1s` (0 = только барьеры/лимиты/flush/shutdown).
- [x] Мутации отвечают `flushed:false` + заголовок `X-Webdb-Flushed`;
      `POST /v1/flush` дренирует буфер + SQLite + шифрованный снапшот.
- [x] Shutdown (SIGTERM/SIGINT): drain буфера + encrypt; SIGKILL: потеря
      буфера до интервала тикера — задокументировано.
- [x] Тест: write-back проверки в smoke (buffer_items ≥1 после мутации, 0 после
      read-barrier, flushed:false в bulk, applies растёт от flush);
      тикер `-writeback 500ms` применяет буфер (applies 0→1);
      SIGTERM-drain: док пережил рестарт — проверено.

## Этап 3. Go client lib

- [x] `client-go` (module github.com/dustlinux/tinydb/client-go, пакет webdb): NewClient, Health,
      Stats, коллекции, Insert/Bulk/Get/List/Upsert/Patch/Delete, SQL,
      индексы, Export/Import/Backup/Flush.
- [x] `APIError{Status,Message}` через errors.As.
- [x] `go test ./...` интеграционный — **ok** (без кэша, после переписывания
      транспорта httpx).
- [x] Лимит ≤ 512 КБ: исходники клиента (артефакт — исходный код + .a не
      нужен; размер на диске мал).

## Этап 4. C client lib

- [x] `client-c/webdb.h` + `webdb.c`: URL parse, TCP, HTTP/1.1, Content-Length
      + chunked decode, Bearer.
- [x] Функции: init/free, request, health, stats, create_collection, insert,
      bulk_insert, get, list, put, patch, delete, sql, flush; webdb_response.
- [x] Makefile: libwebdb.a, libwebdb.so, example, size-check (≤524288).
- [x] example прошёл живой тест: **RESULT: PASS**.
- [x] Лимит ≤ 512 КБ: **libwebdb.a 24696, libwebdb.so 26072 байт**
      (включая webdb_drop_index — полный CRUD индексов).

## Этап 5. Документация

- [x] README.md — quickstart, флаги, лимиты, структура, как это работает.
- [x] docs/API.md — все эндпоинты: auth, фильтры-равенства (`?price=5`),
      `order=price:desc`, limit 50/500, ответ `{items,total,truncated}`,
      read-barrier-семантика, коды ошибок, curl.
- [x] docs/DESIGN.md — httpx vs net/http (таблица экономии, no-DNS),
      write-back (6 событий apply, overlay, durability/SIGKILL), RAM-бюджет
      (3 уровня: не растить / re-exec GODEBUG+LD_PRELOAD / отдавать после
      бёрстов: shrink_memory → mallopt(M_PURGE_ALL) → FreeOSMemory →
      evictCode), почему RSS не сливается сам.
- [x] docs/SECURITY.md — threat model, контейнер WEBDBENC (макет, чанки,
      AAD), keyfile vs passphrase (PBKDF2 100k), права, auth, доп. меры,
      таблица рисков.
- [x] docs/CLIENT-GO.md — пример, ListOptions/Doc/Stats по реальным
      типам, обработка APIError, write-back глазами клиента.
- [x] docs/CLIENT-C.md — типы, жизненный цикл, все обёртки, пример,
      лимит 512 КиБ.

## Этап 6. Сборка и верификация

- [x] Makefile: build (проверка ≤10МБ), test (fmt+vet+smoke+клиенты),
      run, clients, check, clean.
- [x] `go vet ./...` без замечаний; gofmt чистый (fmt-check в make test).
- [x] Тесты самодостаточны: tests/lib.sh поднимает сервер, если не
      запущен, и убивает только поднятый сам (trap); Go-тест сам поднимает
      bin/webdb на :18099.
- [x] **Финальный `make check`: ok** — bin/webdb 4046256 байт (≤10МБ),
      smoke 44/44 (RSS=10116 ≤10240), client-go ok, C-либы 24696/26072
      (≤512КиБ), C-example PASS (idx-create/list/drop, идемпотентен).

## Этап 7. Публикация: GitHub, CI/CD, тесты безопасности

- [x] Module path → `github.com/dustlinux/tinydb` (+ `/client-go`);
      репо **dustlinux/tinydb** (public, BSD-3-Clause LICENSE — пользователя),
      git-история: стартовый коммит пользователя + основной коммит проекта.
- [x] `go get github.com/dustlinux/tinydb/client-go` работает через прокси
      (псевдоверсия `v0.0.0-20260925043623-1451052c3051`).
- [x] **Тесты безопасности `tests/security.sh` — 45/45**: auth
      (401/неверный токен/X-API-Key/health открыт), SQL только чтение
      (INSERT/стек/PRAGMA/ATTACH → 400, вредный аргумент — параметр, таблица
      жива), валидации имён (плохие/`_`-резерв/65 символов) и path
      traversal, лимиты тел (`413`: обычный эндпоинт и `-max-import`), права
      0700/0600, токена нет в логе, CORS off, SIGTERM → нет plaintext +
      `WEBDBENC`, негативные старты (ключ 10 байт / чужой ключ / подделанный
      .enc / неверный passphrase → отказ), passphrase-режим (без db.key).
- [x] Найдено и исправлено этим же этапом:
  - токен писался в лог (`use token`) — утечка, удалено;
  - `/v1/import` читал тело в обход `-max-body` — добавлен `-max-import`
    (64 МиБ, стрим с лимитом, `413` c rollback транзакции);
  - резервные имена коллекций не проверялись — запрет префикса `_`;
  - свежий `db.sqlite` создавался 0644 — принудительный chmod 0600.
- [x] CI `.github/workflows/ci.yml`:
  - `test` (ubuntu): `make check` на dev-пути (libsqlite3) + portable-сборка
    (bundled SQLite) + smoke/security; RSS_MAX_KB=14336 для x86-64;
  - `build`: 13 linux-архитектур кросс-gcc (amd64, 386, arm64, armv7,
    riscv64, ppc64le, ppc64, s390x, loong64*, mips64le, mips64, mipsle, mips);
  - `android`: arm64 + armv7 через NDK clang (то, что нужно Termux);
  - `macos`: darwin/arm64 + darwin/amd64, запуск бинарника (health/stats/
    шифрование на выходе);
  - `release` по тегу `v*`: тарболы всех сборок + SHA256SUMS.
- [x] Портируемость, найденная CI/кросс-проверкой и исправленная:
  `C.M_PURGE_ALL` (константа bionic — на glibc/macOS сборка падала;
  теперь no-op/фолбэк), `syscall.SOCK_NONBLOCK` (linux-only → SetNonblock).
- [x] README переоформлен: бейджи (CI/pkg.go.dev/license), установка
      `go get`, матрица архитектур, секция «Тесты», структура с `.github`.
