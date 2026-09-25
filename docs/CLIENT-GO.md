# client-go — Go-клиент tinydb

Модуль: `github.com/dustlinux/tinydb/client-go`, пакет `webdb`. Только stdlib (`net/http`,
`encoding/json`) — зависимостей у клиента нет.

## Установка

Клиент лежит в дереве проекта. Импортируйте как есть (replace в вашем
`go.mod`, если модуль ещё не опубликован):

```go
import "github.com/dustlinux/tinydb/client-go"
```

## Быстрый старт

```go
package main

import (
    "fmt"
    "log"

    webdb "github.com/dustlinux/tinydb/client-go"
)

func main() {
    c := webdb.New("http://127.0.0.1:8080", os.Getenv("WEBDB_TOKEN"))

    if err := c.CreateCollection("items"); err != nil && !webdb.IsStatus(err, 409) {
        log.Fatal(err) // 409 = уже существует, это нормально при рестарте
    }

    doc, err := c.Upsert("items", "it1", map[string]any{
        "title": "widget", "price": 10,
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("id:", doc.ID, "collection:", doc.Collection)

    res, err := c.List("items", webdb.ListOptions{
        Order:   "price:desc",
        Limit:   10,
        Filters: map[string]string{"admin": "true"}, // равенство; значение — JSON или строка
    })
    if err != nil {
        log.Fatal(err)
    }
    for _, d := range res.Items {
        fmt.Println(d.ID, string(d.Data))
    }
}
```

## Конструктор и транспорт

```go
c := webdb.New(baseURL, token)          // Authorization: Bearer <token>
c.SetHTTPClient(&http.Client{           // опционально: таймауты/транспорт
    Timeout: 10 * time.Second,
})
```

`New` принимает baseURL **с портом и по IP** — сервер не умеет DNS
(см. DESIGN.md): `http://127.0.0.1:8080`, не `http://localhost:8080`
(если только `localhost` не резолвится у вас локально) и не доменное имя.

## Обработка ошибок

```go
var apiErr *webdb.APIError
if errors.As(err, &apiErr) {
    fmt.Println(apiErr.Status, apiErr.Message)
}
if webdb.IsStatus(err, http.StatusConflict) { // удобный хелпер
    // 409
}
```

`APIError{Status int; Message string}` — для любого HTTP-ответа с
`status >= 400`. Транспортные ошибки (нет сервера, таймаут) — обычные
`error` из `net/http`.

## Справочник методов

### Служебные

| Метод | Описание |
|---|---|
| `Health() (status, version string, err error)` | `GET /health` |
| `Stats() (*Stats, err error)` | `GET /v1/stats` — включает `RSSKB`, `BufferItems`, `Docs`… |
| `Flush() error` | `POST /v1/flush` — драйн write-back + шифрованный снапшот |

### Коллекции

```go
c.CreateCollection("items")            // уже существует → APIError 409
c.ListCollections()                    // []CollectionInfo
c.GetCollection("items")               // *CollectionInfo
c.DeleteCollection("items")            // удаляет вместе с доками
```

### Документы

```go
c.Insert(coll, doc)                    // auto-id           → *Doc
c.BulkInsert(coll, []any{d1, d2})      // []Doc
c.Get(coll, id)                        // *Doc (видит overlay)
c.List(coll, webdb.ListOptions{...})   // *ListResult{Items, Total, Truncated}
c.Put(coll, id, doc)                   // 409 если id занят
c.Upsert(coll, id, doc)                // create-or-replace
c.Patch(coll, id, map[string]any{...}) // точечное обновление
c.Delete(coll, id)
```

`ListOptions` (фильтры — отдельными query-параметрами, равенство по полю;
значение — JSON, иначе строка):

```go
type ListOptions struct {
    Limit   int               // 0 = дефолт сервера (50, максимум 500)
    Offset  int               // >= 0
    Order   string            // "price:desc,name:asc"
    Filters map[string]string // {"price": "5", "admin": "true"}
}
```

Документ:

```go
type Doc struct {
    ID         string          `json:"_id"`
    Collection string          `json:"collection"`
    Created    int64           `json:"created"` // ms
    Updated    int64           `json:"updated"` // ms
    Data       json.RawMessage `json:"data"`    // тело дока как есть
}
```

### SQL (только чтение)

```go
res, err := c.SQL("SELECT COUNT(*) AS n FROM docs WHERE collection=?", "items")
// res.Columns []string, res.Rows []map[string]any, res.Count int
```

Запись через SQL запрещена сервером → `APIError 400`.

### Индексы

```go
c.CreateIndex("items", "price")   // индекс по полю дока
c.ListIndexes("items")            // []string{"idx_price", ...}
c.DropIndex("items", "price")
```

### Экспорт / импорт / бэкап

```go
rc, err := c.Export()             // io.Reader с JSON (не загружает в память)
defer rc.Close()
c.ExportBytes()                   // вариант целиком в []byte

c.Import(reader, "replace")       // или "merge" → *ImportResult

rc, err = c.Backup()              // io.Reader: data/db.sqlite.enc как есть
```

Импорт большой базы: передавайте `io.Reader` (например `os.File`), а не
`ExportBytes` — сервер и клиент не будут держать всё в RAM
(бюджет 10 МБ).

## Write-back глазами клиента

Заголовок ответа `X-Webdb-Flushed: false` (у envelope-ответов — поле
`"flushed": false`) означает: мутация принята в RAM-буфер сервера и ещё
не в SQLite. Это норма (lazy-режим). Чтобы добить на диск сейчас:

```go
if err := c.Flush(); err != nil { ... }
```

Чтения (`Get`, `List`, `SQL`) всегда видят свежие записи — сервер сам
ставит read-barrier или читает overlay. Терять записи «просто так» нельзя:
при SIGTERM сервер дренирует буфер; риск только при SIGKILL (см.
DESIGN.md → «Долговечность»).

## Тест

```sh
# сервер должен быть запущен
cd client-go && go test ./...
```

Интеграционный тест гоняет полный цикл (коллекции → CRUD → фильтры →
SQL → индексы → экспорт/импорт → flush) против живого сервера.

## Размер

Артефакт клиента — исходники пакета. Потребитель получает только
`net/http`+`encoding/json`, которые в его бинарь уже входят, поэтому
вклад в размер — единицы килобайт (в пределах лимита 512 КиБ).
