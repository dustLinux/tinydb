# tinydb-client (JS/TS)

JS/TS-клиент **tinydb** REST API. Ноль зависимостей, работает на Node.js ≥18
(глобальный `fetch`), в браузере, Deno и Bun.

- Ядро CommonJS (`lib/tinydb.js`) + ESM-обёртка (`lib/tinydb.mjs`) — без сборки,
  без `node_modules`.
- Типы TypeScript поставляются (`lib/tinydb.d.ts`).
- Размер: **10.5 КиБ** ядра (3.6 КиБ gzip), типы — 4.2 КиБ.

## Установка

```sh
npm install tinydb-client
# или из репозитория:
npm install github:dustlinux/tinydb#client-js
```

## Быстрый старт

```js
import { Client, isStatus } from 'tinydb-client';
// CommonJS: const { Client, isStatus } = require('tinydb-client');

const c = new Client('http://127.0.0.1:8099', process.env.TINYDB_TOKEN);

// мета
const { status, version } = await c.health();
const stats = await c.stats();          // rss_kb, docs, encrypted, ...
await c.flush();                        // дренировать буфер + снапшот

// коллекции
await c.createCollection('users');
const collections = await c.listCollections();

// документы
const doc = await c.insert('users', { name: 'alice', age: 30 });
await c.get('users', doc._id);
await c.upsert('users', 'u-42', { name: 'bob' });
await c.patch('users', doc._id, { age: 31 });
const page = await c.list('users', { limit: 20, order: 'age:desc' });
await c.bulkInsert('users', [{ name: 'c' }, { name: 'd' }]);
await c.delete('users', doc._id);

// read-only SQL с параметрами
const { rows } = await c.sql('SELECT COUNT(*) AS n FROM docs WHERE collection = ?', 'users');

// индексы
await c.createIndex('users', 'age');
await c.listIndexes('users');           // ['ix_users_age']
await c.dropIndex('users', 'age');

// экспорт / импорт / бэкап
const dump = await c.exportJson();
await c.import(dump, 'merge');          // 'replace' (по умолчанию) | 'merge'
const enc = await c.backupBytes();      // Uint8Array, начинается с 'WEBDBENC'
```

## Обработка ошибок

Не-2xx ответы бросают `APIError` со свойствами `status` и `message`:

```js
import { APIError, isStatus } from 'tinydb-client';

try {
  await c.get('users', 'missing');
} catch (e) {
  if (isStatus(e, 404)) { /* нет документа */ }
  if (e instanceof APIError) console.error(e.status, e.message);
}
```

Таймаут запроса — 30 с по умолчанию (`{ timeoutMs }` в конструкторе); для
стримов `exportResponse()`/`backupResponse()` таймаут отключён — их можно
читать потоком:

```js
const res = await c.exportResponse();
for await (const chunk of res.body) process.stdout.write(chunk);
```

## Опции конструктора

| Опция | По умолчанию | Смысл |
|---|---|---|
| `fetch` | `globalThis.fetch` | свой fetch (Node <18, тесты, прокси) |
| `timeoutMs` | `30000` | таймаут запроса; `0` — без таймаута |

Токен можно не передавать, только если сервер запущен с `-no-auth`.

## Тесты

Интеграционные: поднимают настоящий `../bin/webdb` (нужен собранный бинарь),
проверяют весь API, 401, валидации, SQL-инъекции, таймаут, WEBDBENC-бэкап.

```sh
make build          # из корня репозитория — нужен bin/webdb
cd client-js && npm test
```

Результат: **11/11**.

## Соответствие Go-клиенту

API — зеркало `client-go` (те же методы, те же ошибки):

| Go (`client-go`) | JS (`client-js`) |
|---|---|
| `New(base, token)` | `new Client(base, token)` |
| `c.Health()` | `await c.health()` |
| `c.Stats()` | `await c.stats()` |
| `c.Flush()` | `await c.flush()` |
| `c.CreateCollection(n)` | `await c.createCollection(n)` |
| `c.ListCollections()` | `await c.listCollections()` |
| `c.Insert(c, d)` | `await c.insert(c, d)` |
| `c.BulkInsert(c, ds)` | `await c.bulkInsert(c, ds)` |
| `c.List(c, opts)` | `await c.list(c, opts)` |
| `c.Upsert(c, id, d)` | `await c.upsert(c, id, d)` |
| `c.SQL(q, args...)` | `await c.sql(q, ...args)` |
| `c.Export()` / `ExportBytes()` | `c.exportResponse()` / `c.exportText()` |
| `c.Backup()` | `c.backupResponse()` / `c.backupBytes()` |
| `webdb.IsStatus(err, 401)` | `isStatus(err, 401)` |
| `*webdb.APIError` | `APIError` |

Лицензия: BSD-3-Clause (см. `LICENSE` в корне репозитория).
