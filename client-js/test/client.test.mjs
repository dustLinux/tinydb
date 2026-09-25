// Интеграционные тесты tinydb-client против живого bin/webdb.
// Запуск: node --test test/   (или npm test) из каталога client-js.
// Сервер поднимается автоматически, если ../bin/webdb собран.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { Client, APIError, isStatus } from '../lib/tinydb.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const BIN = path.join(here, '..', '..', 'bin', 'webdb');
const PORT = 18097;
const BASE = `http://127.0.0.1:${PORT}`;

let proc = null;
let dir = '';
let c = null;

before(async () => {
  try {
    await readFile(BIN);
  } catch {
    // нет бинаря — тесты не запускаем (как t.Skip в Go-клиенте)
    return;
  }
  dir = await mkdtemp(path.join(tmpdir(), 'tinydb-js-'));
  proc = spawn(BIN, ['-addr', `127.0.0.1:${PORT}`, '-data', dir], {
    env: { ...process.env, GOMEMLIMIT: '6MiB' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`${BASE}/health`);
      if (r.ok) break;
    } catch { /* ещё не поднялся */ }
    await new Promise((res) => setTimeout(res, 50));
  }
  const token = (await readFile(path.join(dir, 'token'), 'utf8')).trim();
  c = new Client(BASE, token);
});

after(async () => {
  if (proc) proc.kill('SIGTERM');
  if (dir) await rm(dir, { recursive: true, force: true });
});

const need = (ok) => { if (!ok) throw new Error('сервер ../bin/webdb не собран — тесты пропущены?'); };

test('health без токена', async () => {
  need(proc);
  const h = await c.health();
  assert.equal(h.status, 'ok');
  assert.ok(h.version);
});

test('неверный токен → 401 APIError', async () => {
  need(proc);
  const bad = new Client(BASE, 'wrong-token');
  await assert.rejects(() => bad.stats(), (e) => isStatus(e, 401) && e instanceof APIError);
});

test('коллекции: create/list/get/delete', async () => {
  need(proc);
  await c.createCollection('users');
  const list = await c.listCollections();
  assert.ok(list.some((x) => x.name === 'users'));
  const one = await c.getCollection('users');
  assert.equal(one.name, 'users');
  assert.equal(one.docs, 0);
  await c.deleteCollection('users');
  assert.ok(!(await c.listCollections()).some((x) => x.name === 'users'));
});

test('документы: insert/get/list/put/patch/delete + bulk', async () => {
  need(proc);
  await c.createCollection('items');

  const d = await c.insert('items', { name: 'widget', price: 10 });
  assert.ok(d._id);
  assert.equal(d.data.name, 'widget');

  const got = await c.get('items', d._id);
  assert.equal(got.data.price, 10);

  const bulk = await c.bulkInsert('items', [
    { name: 'a', price: 5 },
    { name: 'b', price: 15 },
    { name: 'c', price: 25 },
  ]);
  assert.equal(bulk.length, 3);

  const page = await c.list('items', { limit: 2, order: 'price:desc' });
  assert.equal(page.items.length, 2);
  assert.equal(page.items[0].data.price, 25);
  assert.ok(page.total >= 4);

  const filtered = await c.list('items', { filters: { name: 'a' } });
  assert.equal(filtered.items.length, 1);
  assert.equal(filtered.items[0].data.name, 'a');

  await c.put('items', d._id, { name: 'widget2', price: 11 });
  assert.equal((await c.get('items', d._id)).data.name, 'widget2');

  const up = await c.upsert('items', 'fixed-id', { k: 'v' });
  assert.equal(up._id, 'fixed-id');

  const patched = await c.patch('items', d._id, { extra: true });
  assert.equal(patched.data.extra, true);
  assert.equal(patched.data.name, 'widget2', 'patch не затирает поля');

  await c.delete('items', d._id);
  await assert.rejects(() => c.get('items', d._id), (e) => isStatus(e, 404));
});

test('SQL read-only с параметрами; запись запрещена', async () => {
  need(proc);
  const qr = await c.sql('SELECT COUNT(*) AS n FROM docs WHERE collection = ?', 'items');
  assert.equal(qr.rows.length, 1);
  assert.ok(qr.rows[0].n > 0);
  assert.ok(qr.columns.includes('n'));
  await assert.rejects(() => c.sql('DELETE FROM docs'), (e) => isStatus(e, 400));
  await assert.rejects(() => c.sql('SELECT 1; SELECT 2'), (e) => isStatus(e, 400));
});

test('индексы: create/list/drop', async () => {
  need(proc);
  await c.createIndex('items', 'price');
  assert.ok((await c.listIndexes('items')).includes('ix_items_price'));
  await c.dropIndex('items', 'price');
  assert.ok(!(await c.listIndexes('items')).includes('ix_items_price'));
});

test('stats и flush', async () => {
  need(proc);
  const s = await c.stats();
  assert.ok(s.docs > 0);
  assert.equal(typeof s.encrypted, 'boolean');
  await c.flush();
});

test('export/import round-trip', async () => {
  need(proc);
  const dump = await c.exportJson();
  assert.ok(dump.collections.length > 0);

  await c.createCollection('tobedropped');
  await c.insert('tobedropped', { tmp: 1 });

  // replace — полная замена состояния: tobedropped исчезнет
  const res = await c.import(dump, 'replace');
  assert.ok(res.docs > 0);
  assert.ok(!(await c.listCollections()).some((x) => x.name === 'tobedropped'));
  // вернём items, который дропнулся вместе с дампом? нет — он был в дампе
  assert.ok((await c.listCollections()).some((x) => x.name === 'items'));
});

test('backup отдаёт WEBDBENC-контейнер', async () => {
  need(proc);
  const bytes = await c.backupBytes();
  assert.ok(bytes.length > 0);
  const magic = new TextDecoder().decode(bytes.slice(0, 8));
  assert.equal(magic, 'WEBDBENC');
});

test('валидация имён и traversal → 400', async () => {
  need(proc);
  await assert.rejects(() => c.createCollection('bad name'), (e) => isStatus(e, 400));
  await assert.rejects(() => c.createCollection('_meta'), (e) => isStatus(e, 400));
  await assert.rejects(() => c.get('items', '../../etc/passwd'), (e) => isStatus(e, 400) || isStatus(e, 404));
});

test('таймаут запроса отрабатывает', async () => {
  need(proc);
  const slow = new Client(BASE, c.token, { timeoutMs: 1 });
  await assert.rejects(() => slow.stats(), /таймаут/);
});
