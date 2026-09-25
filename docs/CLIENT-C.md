# C-клиент tinydb

C-клиент пригодится, когда встраиваемая база нужна программе на C или приложению, которое не хочет зависеть от Go и JSON-библиотеки. Это тонкий слой над REST API: один заголовок, один исходник и POSIX-сокеты, без сторонних зависимостей. Но сам сервер всё равно должен быть запущен, а JSON из ответа клиент разбирает сам. Для Go-приложения рядом есть [Go-клиент](CLIENT-GO.md), а для браузера и Node.js — [JS/TS-клиент](../client-js/README.md).

## Быстрый старт

Сборка делается из каталога `client-c`. По умолчанию Makefile берёт `clang`; если его нет, можно выбрать другой C-компилятор через `CC`.

```sh
cd client-c
make
```

Команда создаёт `libwebdb.a`, `libwebdb.so` и исполняемый `example`. В примере ниже токен читается из `WEBDB_TOKEN`; передача `NULL` допустима, только если сервер запущен без авторизации. После каждого ответа вызывается `webdb_response_free`, а в конце — `webdb_client_free`.

```c
#define _POSIX_C_SOURCE 200809L
#include "webdb.h"

#include <stdio.h>
#include <stdlib.h>

static void report(const char *operation, const webdb_response *r)
{
    if (r->error != NULL) {
        fprintf(stderr, "%s: %s\n", operation, r->error);
        return;
    }
    fprintf(stderr, "%s: HTTP %d: %.*s\n", operation, r->status,
            (int)r->len, r->body != NULL ? r->body : "");
}

int main(void)
{
    webdb_client c;
    webdb_response r = {0};
    const char *token = getenv("WEBDB_TOKEN");
    int exit_code = 1;

    if (webdb_client_init(&c, "http://127.0.0.1:8080", token) != 0) {
        fprintf(stderr, "init: %s\n", webdb_last_error());
        return 1;
    }

    if (webdb_create_collection(&c, "items", &r) != 0) {
        report("create collection", &r);
        goto done;
    }
    if (r.status != 201 && r.status != 409) {
        report("create collection", &r);
        goto done;
    }
    webdb_response_free(&r); /* 409 — коллекция уже была */

    if (webdb_put(&c, "items", "it1",
                  "{\"title\":\"widget\",\"price\":10}", 1, &r) != 0) {
        report("put", &r);
        goto done;
    }
    if (r.status != 200 && r.status != 201) {
        report("put", &r);
        goto done;
    }
    printf("put: HTTP %d\n", r.status);
    webdb_response_free(&r);

    if (webdb_list(&c, "items", "order=price:desc&limit=10", &r) != 0) {
        report("list", &r);
        goto done;
    }
    if (r.status != 200) {
        report("list", &r);
        goto done;
    }
    printf("list: %.*s\n", (int)r.len, r.body);
    webdb_response_free(&r);

    if (webdb_flush(&c, &r) != 0) {
        report("flush", &r);
        goto done;
    }
    if (r.status != 200) {
        report("flush", &r);
        goto done;
    }
    printf("flush: HTTP %d\n", r.status);
    exit_code = 0;

done:
    webdb_response_free(&r); /* безопасно и после goto */
    webdb_client_free(&c);
    return exit_code;
}
```

Возвращаемое значение функций и HTTP-статус — разные вещи. `webdb_*` возвращает `0`, если сервер вообще ответил, даже если ответ имеет код 400 или 401; транспортный сбой обозначается `-1`. В примере статус проверяется отдельно, а тело освобождается независимо от результата.

## API

### Типы и жизненный цикл

`webdb_client` хранит разобранные хост, порт, базовый путь и токен. `webdb_response` содержит сырой ответ: `body` всегда NUL-терминирован, а `len` не считает завершающий NUL, поэтому в `body` можно передавать JSON-библиотеке или использовать `strstr` для простой проверки.

```c
typedef struct {
    char *host;      /* разобран из base URL */
    char *port;
    char *base_path; /* "" или "/prefix" без завершающего / */
    char *token;     /* bearer token или NULL для -no-auth */
} webdb_client;

typedef struct {
    int    status; /* HTTP-статус, -1 при транспортной ошибке */
    char  *body;   /* тело с завершающим NUL, может быть "" */
    size_t len;    /* длина тела без NUL */
    char  *error;  /* сообщение транспортной ошибки или NULL */
} webdb_response;
```

Создание клиента и освобождение ресурсов выглядят так:

```c
int  webdb_client_init(webdb_client *c, const char *base_url, const char *token);
void webdb_client_free(webdb_client *c);
void webdb_response_free(webdb_response *r);

const char *webdb_last_error(void);
```

`webdb_client_init` принимает `http://host:port` или URL с дополнительным путём. HTTPS не поддерживается: попытка передать `https://` сразу возвращает `-1`. При успехе функция возвращает `0`, при плохом URL или нехватке памяти — `-1`; подробность можно получить через `webdb_last_error`. Этот буфер ошибки thread-local. `webdb_client_free` освобождает строки внутри клиента, а `webdb_response_free` — `body` и `error` ответа. Версия клиента опубликована в заголовке как `WEBDB_VERSION` (`1.0.0`).

### Произвольный запрос

Через `webdb_request` доступны не только обёртки, но и любые REST-эндпоинты, включая `POST /v1/query`, `GET /v1/export` и `GET /v1/backup`.

```c
int webdb_request(const webdb_client *c, const char *method, const char *path,
                  const char *json_body, webdb_response *r);
```

`path` может содержать query-строку, а `json_body` — быть `NULL`, если тело не нужно. Если тело есть, клиент отправляет `Content-Type: application/json` и `Content-Length`. В ответе поддерживаются `Content-Length`, chunked и close-delimited framing. Для JSON-эндпоинтов `r.body` остаётся обычной строкой; `/v1/export` тоже возвращает JSON-поток, а `/v1/backup` — сырые зашифрованные байты, поэтому для файла ориентируйтесь на `r.len`.

### Обёртки API

В таблице перечислены реальные функции из `webdb.h`. Они не разбирают JSON: вызывающая сторона получает `r.status`, `r.body` и должна освободить ответ.

| Функция | Что делает |
|---|---|
| `int webdb_health(const webdb_client *c, webdb_response *r)` | `GET /health`; токен не требуется |
| `int webdb_stats(const webdb_client *c, webdb_response *r)` | `GET /v1/stats` |
| `int webdb_flush(const webdb_client *c, webdb_response *r)` | `POST /v1/flush` |
| `int webdb_create_collection(const webdb_client *c, const char *name, webdb_response *r)` | Создаёт коллекцию; повторное создание даёт HTTP 409 |
| `int webdb_list_collections(const webdb_client *c, webdb_response *r)` | `GET /v1/collections` |
| `int webdb_delete_collection(const webdb_client *c, const char *name, webdb_response *r)` | Удаляет коллекцию вместе с документами |
| `int webdb_insert(const webdb_client *c, const char *collection, const char *json_doc, webdb_response *r)` | `POST` одного JSON-объекта; ID назначается сервером |
| `int webdb_bulk_insert(const webdb_client *c, const char *collection, const char *json_array, webdb_response *r)` | `POST` JSON-массива документов |
| `int webdb_get(const webdb_client *c, const char *collection, const char *id, webdb_response *r)` | Получает один документ |
| `int webdb_list(const webdb_client *c, const char *collection, const char *query, webdb_response *r)` | Получает страницу документов |
| `int webdb_put(const webdb_client *c, const char *collection, const char *id, const char *json_doc, int upsert, webdb_response *r)` | Полностью заменяет документ; `upsert != 0` разрешает создать отсутствующий |
| `int webdb_patch(const webdb_client *c, const char *collection, const char *id, const char *json_patch, webdb_response *r)` | Сливает верхнеуровневые ключи |
| `int webdb_delete(const webdb_client *c, const char *collection, const char *id, webdb_response *r)` | Удаляет документ |
| `int webdb_sql(const webdb_client *c, const char *sql, const char *args_json, webdb_response *r)` | Выполняет read-only SQL |
| `int webdb_create_index(const webdb_client *c, const char *collection, const char *field, webdb_response *r)` | Создаёт индекс по полю |
| `int webdb_list_indexes(const webdb_client *c, const char *collection, webdb_response *r)` | Возвращает имена индексов |
| `int webdb_drop_index(const webdb_client *c, const char *collection, const char *field, webdb_response *r)` | Удаляет индекс |

У `webdb_list` аргумент `query` передаётся без ведущего `?`; `NULL` означает запрос без дополнительных параметров. Фильтры идут отдельными параметрами и проверяют равенство, `order` использует разделитель `:`, а сервер по умолчанию берёт 50 документов и ограничивает `limit` значением 500. Значение фильтра трактуется как JSON, а если это не JSON — как строка. `order=price:desc&limit=10` — допустимый вариант.

Аргумент `upsert` в `webdb_put` ненулевой добавляет `?upsert=true`. Без него запрос заменяет существующий документ, а отсутствующий ID даёт 404; с ним отсутствующий документ создаётся. `webdb_sql` принимает `args_json` как JSON-массив аргументов для `?` или `NULL`; запись через SQL запрещена и возвращает HTTP 400. Имя поля индекса должно соответствовать правилам сервера: `[A-Za-z0-9_-]`, не длиннее 64 символов.

## Ошибки

Успешный HTTP-ответ и ошибка бизнес-логики — не одно и то же. Любая из перечисленных ниже функций возвращает `0`, когда сервер прислал ответ, даже если `r.status` равен 400, 401 или 404. При обрыве связи, таймауте или ошибке чтения возвращается `-1`, `r.status` становится `-1`, а `r.error` получает текст.

```c
if (webdb_get(&c, "items", "missing", &r) != 0) {
    fprintf(stderr, "transport: %s\n", r.error != NULL ? r.error : "unknown");
} else if (r.status == 404) {
    fprintf(stderr, "document not found\n");
} else {
    printf("%.*s\n", (int)r.len, r.body);
}
webdb_response_free(&r);
```

Тело HTTP-ошибки не превращается в `r.error`: если нужны сообщения сервера, читайте `r.status` и `r.body`. `webdb_last_error()` нужен для ошибок инициализации и транспорта; он возвращает текст последнего такого сообщения в thread-local буфере. В любом случае освобождайте каждый `webdb_response`, даже если статус был 4xx или вызов завершился с `-1`, и один раз освобождайте клиент.

Основные коды те же, что и у REST API: 400 означает невалидные данные или попытку записи через read-only SQL, 401 — проблему с токеном, 404 — отсутствие объекта, 409 — конфликт создания коллекции, 413 — превышение размера тела или импорта, 429 или 507 — квоту, а 500 — внутреннюю ошибку сервера.

## Примеры

### SQL и индексы

Ошибку транспорта и HTTP-статус проверяйте отдельно. В этом фрагменте каждый ответ освобождается сразу после использования.

```c
if (webdb_sql(&c,
              "SELECT COUNT(*) AS n FROM docs WHERE collection = ?",
              "[\"items\"]", &r) != 0) {
    fprintf(stderr, "sql: %s\n", r.error != NULL ? r.error : "unknown");
} else if (r.status == 200) {
    printf("%.*s\n", (int)r.len, r.body);
} else {
    fprintf(stderr, "sql: HTTP %d\n", r.status);
}
webdb_response_free(&r);

if (webdb_create_index(&c, "items", "price", &r) != 0) {
    fprintf(stderr, "index: %s\n", r.error != NULL ? r.error : "unknown");
}
webdb_response_free(&r);

if (webdb_list_indexes(&c, "items", &r) != 0) {
    fprintf(stderr, "index list: %s\n", r.error != NULL ? r.error : "unknown");
} else {
    printf("%.*s\n", (int)r.len, r.body);
}
webdb_response_free(&r);
```

Индекс обычно возвращается как `ix_<collection>_<field>`, например `ix_items_price`. При удалении индекса используется тот же `field`.

```c
if (webdb_drop_index(&c, "items", "price", &r) != 0) {
    fprintf(stderr, "drop index: %s\n", r.error != NULL ? r.error : "unknown");
}
webdb_response_free(&r);
```

### Сырой export и backup

У C-клиента нет отдельных `webdb_export` и `webdb_backup`, но общий запрос позволяет получить эти данные. Ответ всё равно хранится в `r.body`; export можно сохранить как JSON, а backup — как бинарный файл, используя для обоих случаев `r.len`.

```c
webdb_response r = {0};
FILE *file = fopen("backup.webdbenc", "wb");
if (file == NULL) {
    perror("backup.webdbenc");
} else if (webdb_request(&c, "GET", "/v1/backup", NULL, &r) != 0) {
    fprintf(stderr, "backup: %s\n", r.error != NULL ? r.error : "unknown");
    fclose(file);
} else if (r.status != 200) {
    fprintf(stderr, "backup: HTTP %d\n", r.status);
    fclose(file);
} else {
    fwrite(r.body, 1, r.len, file);
    fclose(file);
}
webdb_response_free(&r);

webdb_client_free(&c);
```

Так же вызывается `GET /v1/export`; разница только в содержимом ответа. `webdb_request` не возвращает HTTP-заголовки, поэтому `X-Webdb-Flushed` через C-обёртку не прочитать — поле `"flushed"` в JSON-envelope остаётся доступным в `r.body`.

Полный пример с проверками health, авторизации, CRUD, bulk, фильтров, SQL, индексов, статистики, flush и большого chunked-ответа лежит в [`client-c/example.c`](../client-c/example.c). Он освобождает ответы после каждой операции и завершает работу через `webdb_client_free`.

## Мелочи и советы

### Сборка и размер

Makefile собирает обёртку как C99 с `-O2 -Wall -Wextra` и добавляет `-fPIC`; базовые флаги можно переопределить через `CFLAGS`, а компилятор — через `CC`. Стандартная цель `all` создаёт обе библиотеки и `example`, `size-check` проверяет размер каждой библиотеки, а `clean` удаляет артефакты.

```sh
make clean
make
make size-check
```

`WEBDB_LIB_MAX_BYTES` в `webdb.h` равен 524288 байт, то есть 512 КиБ. `libwebdb.a` и `libwebdb.so` получаются примерно по 25 КиБ, а `size-check` не позволит одному из них незаметно перерасти лимит. Сторонних библиотек здесь нет: транспорт и разбор HTTP/1.1 написаны вручную поверх POSIX-сокетов.

### Тест против живого сервера

`example` принимает base URL и токен как два аргумента. `make test` передаёт их через `BASE` и `TOKEN`, а перед запуском выполняет `size-check`.

```sh
cd client-c
make test BASE=http://127.0.0.1:8099 TOKEN=$(cat ../data/token)
```

Сервер для этой команды должен быть запущен заранее. Без `BASE` или `TOKEN` `example` печатает usage и завершает работу с ошибкой — это ожидаемое поведение Makefile-теста, а не успешный тест без сервера.

### Write-back и сохранность

Мутация может попасть в RAM-буфер раньше, чем в SQLite. В протоколе это сопровождается `X-Webdb-Flushed: false`, а в JSON-envelope — полем `"flushed": false`; C-клиент сохраняет только тело, поэтому проверяйте поле, если оно есть. `webdb_flush` принудительно применяет буфер и сохраняет зашифрованный snapshot, после чего его ответ нужно освободить.

`webdb_get`, `webdb_list` и `webdb_sql` видят свежие записи: отдельный `Get` смотрит в overlay, а список и SQL ставят read-barrier. При `SIGTERM` сервер дренирует буфер сам; при аварийном `SIGKILL` хвост, оставшийся только в RAM, может потеряться. Если этот риск недопустим, вызывайте `webdb_flush` в важных точках и отдельно настройте режим write-back сервера.

### Адрес и HTTPS

Серверный HTTP-слой не использует DNS, поэтому для tinydb обычно используют `http://127.0.0.1:8080` или другой явно заданный IP-адрес. C-клиент сам умеет разрешать хост через `getaddrinfo`, но HTTPS он не реализует: `webdb_client_init` принимает только plain HTTP. В URL можно указать префикс пути, и запросы будут добавляться к нему автоматически.

Наконец, не забывайте порядок очистки: сначала освободите каждый `webdb_response`, затем `webdb_client`. Для приложений на других языках в репозитории есть [Go-клиент](CLIENT-GO.md) и [JS/TS-клиент](../client-js/README.md).
