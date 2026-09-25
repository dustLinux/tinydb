# client-c — C-клиент tinydb

Один `.h` + один `.c`, без сторонних зависимостей: чистый HTTP/1.1 по TCP
(`getaddrinfo`), Bearer-токен, `Content-Length` в запросе, декодирование
ответов с `Content-Length` / chunked / close-delimited.

Артефакты (`make`):

| Файл | Размер (замерено) | Лимит |
|---|---|---|
| `libwebdb.a` | 24696 байт | 524288 (512 КиБ) |
| `libwebdb.so` | 26072 байт | 524288 (512 КиБ) |

Лимит принудительно проверяется таргетом `size-check` (`WEBDB_LIB_MAX_BYTES`
в `webdb.h`). Сборка: `make` (нужен `cc`), варианты — `libwebdb.a`,
`libwebdb.so`, `example`.

## Типы

```c
typedef struct {
    char *host;      /* разбрано из base URL */
    char *port;
    char *base_path; /* "" или "/prefix" (без завершающего /) */
    char *token;     /* bearer token или NULL (-no-auth) */
} webdb_client;

typedef struct {
    int    status; /* HTTP-статус, -1 = транспортная ошибка */
    char  *body;   /* тело, NUL-терминированный JSON (может быть "") */
    size_t len;    /* длина тела без NUL */
    char  *error;  /* сообщение транспортной ошибки или NULL */
} webdb_response;
```

Ответы — просто строки JSON: парсите их своей библиотекой (jansson/cJSON)
или проверяйте подстроками (`strstr`) как это делает `example.c`.

## Жизненный цикл

```c
webdb_client c;
webdb_response r;

webdb_client_init(&c, "http://127.0.0.1:8080", token); /* 0 = ок */
/* ... запросы ... */
webdb_response_free(&r);   /* освободить тело каждого ответа */
webdb_client_free(&c);     /* освободить сам клиент */

const char *webdb_last_error(void); /* текст ошибки последнего вызова */
```

Все функции возвращают `0`, если получен ответ **любого** статуса (в том
числе 400/401/404 — это уже бизнес-ответ, а не сбой), и `-1` при
транспортной ошибке (`r.status == -1`, заполнен `r.error`).

## Общий запрос

```c
int webdb_request(const webdb_client *c, const char *method,
                  const char *path, const char *json_body,
                  webdb_response *r);
```

`path` — путь с опциональным query (`"/v1/stats"`,
`"/v1/collections/items/docs?limit=10"`), `json_body` — `NULL` или тело.
Через этот метод доступен любой эндпоинт, включая необёрнутый
`POST /v1/query`, `GET /v1/export`, `GET /v1/backup`.

## Обёртки

### Служебные

```c
int webdb_health(const webdb_client *c, webdb_response *r);
int webdb_stats (const webdb_client *c, webdb_response *r);
int webdb_flush (const webdb_client *c, webdb_response *r);
```

### Коллекции

```c
int webdb_create_collection(const webdb_client *c, const char *name, webdb_response *r);
int webdb_list_collections (const webdb_client *c, webdb_response *r);
int webdb_delete_collection(const webdb_client *c, const char *name, webdb_response *r);
```

### Документы

```c
int webdb_insert(const webdb_client *c, const char *collection,
                 const char *json_doc, webdb_response *r);      /* POST (auto-id) */
int webdb_bulk_insert(const webdb_client *c, const char *collection,
                      const char *json_array, webdb_response *r); /* POST — массив */
int webdb_get(const webdb_client *c, const char *collection,
              const char *id, webdb_response *r);
int webdb_list(const webdb_client *c, const char *collection,
                const char *query, webdb_response *r);
int webdb_put(const webdb_client *c, const char *collection,
              const char *id, const char *json_doc, int upsert,
              webdb_response *r);
int webdb_patch(const webdb_client *c, const char *collection,
                const char *id, const char *json_patch, webdb_response *r);
int webdb_delete(const webdb_client *c, const char *collection,
                 const char *id, webdb_response *r);
```

`webdb_list` принимает `query` **без** ведущего `?`, `NULL` допустим:

```c
webdb_list(&c, "items", "price=5&order=price:desc&limit=10", &r);
/* фильтры — отдельные параметры (равенство), order через ':', limit≤500 */
```

`upsert` != 0 добавляет `?upsert=true` (создать или заменить; без него
занятый `_id` даёт 409).

### SQL (только чтение)

```c
int webdb_sql(const webdb_client *c, const char *sql,
              const char *args_json, webdb_response *r);
/* args_json: NULL или JSON-массив аргументов для '?' — "[42,\"x\"]" */
```

### Индексы

```c
int webdb_create_index(const webdb_client *c, const char *collection,
                       const char *field, webdb_response *r);
int webdb_list_indexes (const webdb_client *c, const char *collection,
                        webdb_response *r);
int webdb_drop_index(const webdb_client *c, const char *collection,
                     const char *field, webdb_response *r);
```

## Пример

```c
#define _POSIX_C_SOURCE 200809L
#include "webdb.h"
#include <stdio.h>
#include <string.h>

int main(void) {
    webdb_client c;
    webdb_response r;

    if (webdb_client_init(&c, "http://127.0.0.1:8080", getenv("WEBDB_TOKEN")) != 0) {
        fprintf(stderr, "init: %s\n", webdb_last_error());
        return 1;
    }

    webdb_create_collection(&c, "items", &r);      /* 200 или 409 — оба ок */
    webdb_response_free(&r);

    webdb_put(&c, "items", "it1",
              "{\"title\":\"widget\",\"price\":10}", 1, &r);
    printf("put status=%d flushed-hdr-done\n", r.status);
    webdb_response_free(&r);

    webdb_list(&c, "items", "order=price:desc&limit=10", &r);
    if (r.status == 200 && strstr(r.body, "\"total\""))
        printf("list: %.*s\n", (int)(r.len > 200 ? 200 : r.len), r.body);
    webdb_response_free(&r);

    webdb_flush(&c, &r);                          /* драйн write-back + .enc */
    webdb_response_free(&r);

    webdb_client_free(&c);
    return 0;
}
```

Полный рабочий пример со всеми проверками — `client-c/example.c`.

## Запуск теста

Сервер должен быть запущен:

```sh
cd client-c
make test BASE=http://127.0.0.1:8099 TOKEN=$(cat ../data/token)
# → size-check + прогон example против живого сервера
```

`make test` без `BASE`/`TOKEN` печатает usage и падает — это ожидаемо.

## Write-back глазами C-клиента

Ответы мутаций несут заголовок `X-Webdb-Flushed: false`, пока мутация
только в RAM-буфере сервера (поля `"flushed": false` в теле — у
envelope-ответов: create/bulk/delete/index). Это нормальный lazy-режим.
Чтения (`webdb_get`, `webdb_list`, `webdb_sql`) всегда видят свежие
записи. Гарантированно добить на диск:

```c
webdb_flush(&c, &r);
webdb_response_free(&r);
```

При SIGTERM сервер дренирует буфер сам; риск потери — только SIGKILL
(см. DESIGN.md → «Долговечность»).

## Размер и лимит 512 КиБ

Клиент на чистом POSIX-сокете и посчитанном вручную HTTP/1.1 — поэтому
`libwebdb.a` (24 КБ) и `.so` (26 КБ) на порядок ниже лимита. Лимит
зашит в `WEBDB_LIB_MAX_BYTES` и проверяется при каждой сборке
(`make size-check`), чтобы регрессия не прошла молча.
