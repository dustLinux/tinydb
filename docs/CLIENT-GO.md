# Go-клиент tinydb

Go-клиент нужен, когда приложение на Go хочет обращаться к локальному tinydb по REST, не подключая SQLite напрямую. В пакете есть CRUD для документов, read-only SQL, индексы, экспорт и импорт, а также принудительное применение write-back буфера. Это обычный пакет `github.com/dustlinux/tinydb/client-go` без сторонних зависимостей: используется только стандартная библиотека Go. Для приложений на JS/TS есть [tinydb-client](../client-js/README.md), а для C — [tinydb C-клиент](CLIENT-C.md).

## Быстрый старт

Пакет лежит в дереве проекта, поэтому его можно импортировать как есть. Если модуль ещё не опубликован, добавьте в свой `go.mod` директиву `replace`, указывающую на `client-go`; в самом `client-go/go.mod` указан Go 1.27.

```go
import "github.com/dustlinux/tinydb/client-go"
```

Минимальная программа создаёт коллекцию, записывает документ и читает его обратно. Повторный запуск не считается ошибкой: `CreateCollection` для существующей коллекции возвращает `APIError` со статусом 409, и такой ответ можно спокойно пропустить.

```go
package main

import (
	"fmt"
	"log"
	"os"

	webdb "github.com/dustlinux/tinydb/client-go"
)

func main() {
	c := webdb.New("http://127.0.0.1:8080", os.Getenv("WEBDB_TOKEN"))

	if err := c.CreateCollection("items"); err != nil && !webdb.IsStatus(err, 409) {
		log.Fatal(err)
	}

	doc, err := c.Upsert("items", "it1", map[string]any{
		"title": "widget",
		"price": 10,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("saved %s in %s\n", doc.ID, doc.Collection)

	page, err := c.List("items", webdb.ListOptions{
		Order:   "price:desc",
		Limit:   10,
		Filters: map[string]string{"admin": "true"},
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, item := range page.Items {
		fmt.Printf("%s %s\n", item.ID, item.Data)
	}
}
```

Токен берётся из `WEBDB_TOKEN`. Пустой токен допустим только при запуске сервера с `-no-auth`; для обычного запуска передайте содержимое `<data>/token`. `Upsert` создаёт документ с заданным `_id` или заменяет существующий, а `List` сортирует результат и применяет фильтры равенства к верхнеуровневым полям.

## API

Все сетевые вызовы выполняет один `*webdb.Client`. Конструктор принимает базовый URL и Bearer-токен, а `SetHTTPClient` позволяет заменить стандартный HTTP-клиент, если нужны другой транспорт или таймаут.

```go
const DefaultTimeout = 30 * time.Second

func New(baseURL, token string) *Client
func (c *Client) SetHTTPClient(hc *http.Client)
```

```go
c := webdb.New(baseURL, token)
c.SetHTTPClient(&http.Client{Timeout: 10 * time.Second})
```

`New` убирает завершающие `/` из базового URL и добавляет `Authorization: Bearer <token>`, если токен непустой. По умолчанию один запрос живёт `DefaultTimeout` — 30 секунд; потоковые `Export` и `Backup` намеренно выполняются без глобального таймаута.

### Служебные методы

| Метод | Что делает | HTTP-запрос |
|---|---|---|
| `Health() (status string, version string, err error)` | Возвращает состояние и версию сервера; токен не нужен | `GET /health` |
| `Stats() (*Stats, error)` | Собирает статистику базы, буфера и процесса | `GET /v1/stats` |
| `Flush() error` | Применяет буфер и сохраняет зашифрованный снапшот | `POST /v1/flush` |

`Stats` — не просто счётчик документов. В нём есть размеры данных и квоты, состояние шифрования, счётчики flush, поля write-back буфера (`BufferItems`, `BufferBytes` и другие), RSS и диагностика Go-кучи.

```go
type Stats struct {
	Collections   int64  `json:"collections"`
	Docs          int64  `json:"docs"`
	DataBytes     int64  `json:"data_bytes"`
	PlainBytes    int64  `json:"plain_bytes"`
	EncBytes      int64  `json:"enc_bytes"`
	MaxBytes      int64  `json:"max_bytes"`
	Encrypted     bool   `json:"encrypted"`
	Dirty         bool   `json:"dirty"`
	Flushes       int64  `json:"flushes"`
	LastFlushUnix int64  `json:"last_flush_unix"`
	SchemaVersion string `json:"schema_version"`
	ServerVersion string `json:"server_version"`
	RSSKB         int64  `json:"rss_kb"`

	BufferItems    int   `json:"buffer_items"`
	BufferBytes    int   `json:"buffer_bytes"`
	BufferMaxBytes int   `json:"buffer_max_bytes"`
	BufferApplies  int64 `json:"buffer_applies"`

	HeapAllocKB int64 `json:"heap_alloc_kb"`
	HeapSysKB   int64 `json:"heap_sys_kb"`
	HeapIdleKB  int64 `json:"heap_idle_kb"`
}
```

### Коллекции

Коллекция — это именованное множество документов. `ListCollections` и `GetCollection` возвращают описание с именем, временем создания, числом документов и размером данных; удаление коллекции удаляет также её документы.

| Метод | Результат |
|---|---|
| `CreateCollection(name string) error` | Создаёт коллекцию; существующая даёт `APIError` 409 |
| `ListCollections() ([]CollectionInfo, error)` | Возвращает список описаний |
| `GetCollection(name string) (*CollectionInfo, error)` | Возвращает описание одной коллекции |
| `DeleteCollection(name string) error` | Удаляет коллекцию и её документы |

```go
type CollectionInfo struct {
	Name      string `json:"name"`
	Created   int64  `json:"created"`
	Docs      int64  `json:"docs"`
	DataBytes int64  `json:"data_bytes"`
}
```

### Документы

Для документа принимается любой JSON-объект, а ответ всегда приходит как `*Doc`. Это удобно: сервер добавляет `_id`, время создания и обновления, а исходное тело можно прочитать как `json.RawMessage` или разобрать отдельным вызовом.

| Метод | Поведение |
|---|---|
| `Insert(coll string, doc any) (*Doc, error)` | Вставляет объект с автоматически созданным ID |
| `BulkInsert(coll string, docs []any) ([]Doc, error)` | Вставляет JSON-массив документов |
| `Get(coll, id string) (*Doc, error)` | Возвращает документ; сначала проверяется write-back overlay |
| `List(coll string, opts ListOptions) (*ListResult, error)` | Возвращает страницу документов |
| `Put(coll, id string, doc any) (*Doc, error)` | Полностью заменяет документ; для отсутствующего ID сервер отвечает 404 |
| `Upsert(coll, id string, doc any) (*Doc, error)` | Создаёт документ, если его нет, иначе заменяет его |
| `Patch(coll, id string, doc any) (*Doc, error)` | Сливает переданные ключи верхнего уровня |
| `Delete(coll, id string) error` | Удаляет документ |

`Put` и `Upsert` выглядят похоже, но различаются поведением при отсутствии документа: `Upsert` нужен именно для create-or-replace. `Patch` не удаляет поля, которых нет в запросе, — он обновляет только переданные верхнеуровневые ключи.

```go
type Doc struct {
	ID         string          `json:"_id"`
	Collection string          `json:"collection"`
	Created    int64           `json:"created"` // миллисекунды
	Updated    int64           `json:"updated"` // миллисекунды
	Data       json.RawMessage `json:"data"`
}

type ListResult struct {
	Items     []Doc `json:"items"`
	Total     int64 `json:"total"`
	Truncated bool  `json:"truncated"`
}
```

`Total` считает все совпадения до `limit` и `offset`, а `Truncated` означает, что сервер достиг лимита сканирования. Параметры `List` собираются в `ListOptions`. Фильтры — это отдельные query-параметры и равенство по полю; несколько фильтров соединяются по `AND`. Значение фильтра сначала трактуется как JSON, поэтому `true`, `10` и `"x"` имеют разные типы, а нестроковое значение остаётся строкой.

```go
type ListOptions struct {
	Limit   int               // 0 — дефолт сервера (50), максимум 500
	Offset  int               // >= 0
	Order   string            // например, "price:desc,name:asc"
	Filters map[string]string // верхнеуровневые поля, например {"price": "5"}
}
```

Операторов диапазона или `$gte` в фильтрах списка нет. Когда нужно выражение сложнее равенства, удобнее перейти к read-only SQL с параметрами.

### SQL, индексы и обмен данными

`SQL` принимает запрос и вариативные аргументы для `?`-плейсхолдеров. Сервер разрешает только чтение; попытка изменить данные через SQL возвращает `APIError` со статусом 400.

```go
func (c *Client) SQL(query string, args ...any) (*QueryResult, error)
```

```go
func countItems(c *webdb.Client) error {
	result, err := c.SQL(
		"SELECT COUNT(*) AS n FROM docs WHERE collection = ?",
		"items",
	)
	if err != nil {
		return err
	}
	fmt.Println(result.Columns, result.Rows, result.Count)
	return nil
}
```

`QueryResult` содержит имена колонок, строки в виде `[]map[string]any`, количество строк и признак усечения ответа.

```go
type QueryResult struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated"`
}
```

Индекс создаётся по полю документа, а имя, возвращаемое сервером, имеет вид `ix_<collection>_<field>`.

```go
func (c *Client) CreateIndex(coll, field string) error
func (c *Client) ListIndexes(coll string) ([]string, error)
func (c *Client) DropIndex(coll, field string) error
```

```go
func refreshIndex(c *webdb.Client) error {
	if err := c.CreateIndex("items", "price"); err != nil {
		return err
	}
	indexes, err := c.ListIndexes("items") // например, []string{"ix_items_price"}
	if err != nil {
		return err
	}
	fmt.Println(indexes)
	return c.DropIndex("items", "price")
}
```

Для переноса данных есть три разных пути. `Export` возвращает JSON-поток, `ExportBytes` читает его целиком в `[]byte`, а `Import` принимает `io.Reader` и возвращает статистику операции. `Backup` отдаёт зашифрованный снапшот `data/db.sqlite.enc` как есть; вызывающий код обязан закрыть возвращённый `io.ReadCloser`.

| Метод | Результат |
|---|---|
| `Export() (io.ReadCloser, error)` | JSON-поток экспорта; reader закрывает вызывающий код |
| `ExportBytes() ([]byte, error)` | Весь экспорт в памяти |
| `Import(r io.Reader, mode string) (*ImportResult, error)` | `replace` или `merge`; пустой режим означает `replace` |
| `Backup() (io.ReadCloser, error)` | Зашифрованный snapshot; reader закрывает вызывающий код |

```go
func transfer(c *webdb.Client) error {
	dump, err := c.Export()
	if err != nil {
		return err
	}
	defer dump.Close()

	backup, err := c.Backup()
	if err != nil {
		return err
	}
	defer backup.Close()

	dumpBytes, err := c.ExportBytes()
	if err != nil {
		return err
	}
	imported, err := c.Import(bytes.NewReader(dumpBytes), "merge")
	if err != nil {
		return err
	}
	fmt.Println(imported.Collections, imported.Docs, imported.Mode)
	return nil
}
```

```go
type ImportResult struct {
	Collections int    `json:"collections"`
	Docs        int    `json:"docs"`
	Mode        string `json:"mode"`
}
```

Режим импорта — `replace` или `merge`; пустая строка трактуется как `replace`. Для большой базы не превращайте дамп в `ExportBytes`: передавайте в `Import` файл или другой `io.Reader`, чтобы не держать весь экспорт в памяти. Это особенно важно при серверном бюджете RAM 10 МБ.

## Ошибки

Любой HTTP-ответ с кодом 400 или больше превращается в `*webdb.APIError`. В нём есть номер статуса и текст сервера, поэтому обычные проверки удобно делать без разбора строк ошибки.

```go
type APIError struct {
	Status  int    `json:"status"`
	Message string `json:"error"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("webdb: HTTP %d: %s", e.Status, e.Message)
}

func IsStatus(err error, status int) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Status == status
}
```

```go
var apiErr *webdb.APIError
if errors.As(err, &apiErr) {
	fmt.Println(apiErr.Status, apiErr.Message)
}
if webdb.IsStatus(err, http.StatusConflict) {
	// 409: например, коллекция уже существует
}
```

`IsStatus(err, code)` — короткая проверка именно API-ошибки. Ошибки транспорта (сервер не запущен, соединение оборвалось, истёк таймаут) остаются обычными `error` из `net/http` и не превращаются в `APIError`.

Сервер использует следующие основные коды: 400 для невалидного запроса, имени или JSON, 401 для неверного токена, 404 для отсутствующей коллекции или документа, 409 для конфликта при создании коллекции, 413 для слишком большого тела или импорта, 429 или 507 для переполнения квоты и 500 для внутренней ошибки. `Health` не требует авторизации; остальные вызовы клиент отправляет с Bearer-токеном.

## Примеры

### Обновление документа и обработка 404

Если документ мог исчезнуть между чтением и обновлением, проверяйте статус до повторной операции. `Get` и `Patch` возвращают полноценный `Doc`, поэтому обновлённое тело можно сразу передать дальше.

```go
func markSeen(c *webdb.Client) error {
	doc, err := c.Get("items", "it1")
	if webdb.IsStatus(err, 404) {
		log.Printf("документ не найден")
		return nil
	}
	if err != nil {
		return err
	}

	updated, err := c.Patch("items", doc.ID, map[string]any{
		"seen": true,
	})
	if err != nil {
		return err
	}
	fmt.Println(updated.Data)
	return nil
}
```

### Фильтры, сортировка и SQL

Сначала берите страницу через `List`, когда достаточно равенства и сортировки. Для составных условий и агрегатов используйте `SQL`; аргументы передаются отдельно от текста запроса.

```go
func inspectItems(c *webdb.Client) error {
	page, err := c.List("items", webdb.ListOptions{
		Limit:  50,
		Offset: 100,
		Order:  "price:desc,name:asc",
		Filters: map[string]string{
			"admin": "true",
		},
	})
	if err != nil {
		return err
	}
	fmt.Println("all matching:", page.Total, "returned:", len(page.Items))

	query, err := c.SQL(
		"SELECT id, data FROM docs WHERE collection = ? AND json_extract(data, '$.active') = 1",
		"items",
	)
	if err != nil {
		return err
	}
	fmt.Println(query.Columns, query.Rows)
	return nil
}
```

### Большой экспорт и импорт

`Export` и `Backup` возвращают поток, который нужно закрыть. Ниже показана запись экспорта в файл без промежуточного `[]byte`; `Import` затем читает этот файл отдельным запросом.

```go
func saveExport(c *webdb.Client, path string) error {
	reader, err := c.Export()
	if err != nil {
		return err
	}
	defer reader.Close()

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, reader); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func restore(c *webdb.Client, path string) (*webdb.ImportResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return c.Import(file, "replace")
}
```

`ExportBytes` удобен для маленького дампа или теста, но `io.Reader` оставляет память под контролем на больших базах. `Backup` и `Export` не надо смешивать: первый возвращает зашифрованный snapshot, второй — JSON для переноса.

## Мелочи и советы

Для локального tinydb передавайте URL с IP и портом, например `http://127.0.0.1:8080`. Серверный HTTP-слой не делает DNS; `-addr` понимает IP-литерал или специальный `localhost`, но клиенту надёжнее обращаться по `127.0.0.1`. Доменное имя для адреса самого сервера не подходит.

Мутации сначала попадают в RAM-буфер write-back. В HTTP-ответе это отражает заголовок `X-Webdb-Flushed: false`, а в envelope-ответе — поле `"flushed": false`; это нормальный lazy-режим, а не ошибка. `Flush` принудительно доводит буфер до SQLite и сохраняет зашифрованный snapshot. `Get` сначала смотрит в overlay, а `List` и `SQL` ставят read-barrier, поэтому чтения видят свежие записи. При `SIGTERM` сервер дренирует буфер сам; потерять незаписанный хвост можно только при аварийном `SIGKILL` (см. [DESIGN.md](DESIGN.md), раздел «Долговечность»).

Интеграционный тест запускает настоящий `../bin/webdb` на `127.0.0.1:18099`, создаёт временный каталог данных, передаёт процессу `GOMEMLIMIT=6MiB`, ждёт `/health` и берёт токен из файла. Если бинарь не собран, тест пропускается; заранее поднимать сервер не нужно.

```sh
cd client-go
go test ./...
```

Проверяются health и неверный токен (401), CRUD, фильтры и сортировка, `Put` с отсутствующим ID (404), `Upsert`, bulk-вставка, read-only SQL и отказ записи через SQL (400), индексы, экспорт/импорт, `Stats` и `Flush`. В тесте также есть большая bulk-вставка на 300 документов и список с `Limit: 500`.

Артефакт Go-клиента — исходники пакета, а не отдельная тяжёлая библиотека. Он использует `net/http` и `encoding/json` из стандартной библиотеки, поэтому не добавляет сторонних зависимостей; вклад в бинарь потребителя остаётся небольшим в пределах лимита 512 КиБ.

Если язык приложения другой, не нужно искать отдельный протокол: у проекта есть [JS/TS-клиент](../client-js/README.md) и [C-клиент](CLIENT-C.md).
