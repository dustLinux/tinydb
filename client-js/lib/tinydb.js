'use strict';
/*
 * tinydb-client — JS/TS-клиент tinydb REST API (v1).
 *
 * Ноль зависимостей, работает на Node.js ≥18 (глобальный fetch), в браузере,
 * Deno и Bun. CommonJS-ядро + ESM-обёртка (lib/tinydb.mjs), типы — в
 * lib/tinydb.d.ts.
 *
 * Использование (ESM):
 *   import { Client, APIError } from 'tinydb-client';
 *   const c = new Client('http://127.0.0.1:8099', token);
 *   const doc = await c.insert('users', { name: 'alice' });
 *
 * Использование (CJS):
 *   const { Client } = require('tinydb-client');
 */

// Таймаут одного запроса по умолчанию, мс (эндпоинты-стримы — без него).
const DEFAULT_TIMEOUT_MS = 30000;
const USER_AGENT = 'tinydb-js/1.0';

/** Ошибка не-2xx ответа сервера. Аналог webdb.APIError в Go-клиенте. */
class APIError extends Error {
  constructor(status, message) {
    super(`tinydb: HTTP ${status}: ${message}`);
    this.name = 'APIError';
    this.status = status;
    this.message = message;
  }
}

/** Проверяет, что ошибка — APIError с указанным статусом. */
function isStatus(err, status) {
  return err instanceof APIError && err.status === status;
}

/** Клиент tinydb-сервера. */
class Client {
  /**
   * @param {string} baseURL адрес сервера, напр. 'http://127.0.0.1:8099'
   * @param {string} [token] токен из <data>/token; пусто, только если сервер с -no-auth
   * @param {{fetch?: typeof fetch, timeoutMs?: number}} [opts]
   */
  constructor(baseURL, token = '', opts = {}) {
    if (!baseURL) throw new TypeError('tinydb: baseURL обязателен');
    this.base = String(baseURL).replace(/\/+$/, '');
    this.token = token || '';
    this.fetchImpl = opts.fetch || globalThis.fetch;
    if (typeof this.fetchImpl !== 'function') {
      throw new TypeError('tinydb: fetch недоступен (нужен Node.js ≥18 или polyfill)');
    }
    this.timeoutMs = opts.timeoutMs === undefined ? DEFAULT_TIMEOUT_MS : opts.timeoutMs;
  }

  // ---- внутреннее ----

  #url(path, query) {
    let u = this.base + path;
    if (query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(query)) {
        if (v === undefined || v === null || v === '') continue;
        qs.set(k, String(v));
      }
      const s = qs.toString();
      if (s) u += '?' + s;
    }
    return u;
  }

  #headers(hasBody) {
    const h = { 'User-Agent': USER_AGENT };
    if (this.token) h.Authorization = 'Bearer ' + this.token;
    if (hasBody) h['Content-Type'] = 'application/json';
    return h;
  }

  async #send(method, path, { query, body, timeoutMs, raw } = {}) {
    const hasBody = body !== undefined && body !== null;
    // Стримы (export/backup) могут быть большими — для них таймаут по умолчанию.
    const tmo = timeoutMs === undefined ? this.timeoutMs : timeoutMs;
    const ctrl = tmo > 0 ? new AbortController() : null;
    const timer = ctrl ? setTimeout(() => ctrl.abort(), tmo) : null;
    let res;
    try {
      res = await this.fetchImpl(this.#url(path, query), {
        method,
        headers: this.#headers(hasBody),
        body: hasBody ? (typeof body === 'string' || body instanceof Uint8Array ? body : JSON.stringify(body)) : undefined,
        signal: ctrl ? ctrl.signal : undefined,
      });
    } catch (e) {
      if (e && (e.name === 'AbortError' || ctrl?.signal.aborted)) {
        throw new Error(`tinydb: таймаут ${tmo}ms на ${method} ${path}`);
      }
      throw new Error(`tinydb: ${method} ${path}: ${e && e.message ? e.message : e}`);
    } finally {
      if (timer) clearTimeout(timer);
    }
    if (res.status >= 400) {
      let text = '';
      try {
        text = await res.text();
      } catch { /* тело могло быть непрочитано — не критично */ }
      let msg = text.trim();
      try {
        const j = JSON.parse(text);
        if (j && typeof j.error === 'string') msg = j.error;
      } catch { /* не JSON — оставляем текст */ }
      throw new APIError(res.status, msg || res.statusText || 'без тела');
    }
    if (raw) return res;
    if (res.status === 204 || res.headers.get('content-length') === '0') return null;
    const text = await res.text();
    if (!text) return null;
    try {
      return JSON.parse(text);
    } catch (e) {
      throw new Error(`tinydb: не-JSON ответ на ${method} ${path}: ${text.slice(0, 200)}`);
    }
  }

  // ---- мета ----

  /** GET /health — работает без токена. */
  async health() {
    const out = await this.#send('GET', '/health', { timeoutMs: 5000 });
    return { status: out.status, version: out.version };
  }

  /** GET /v1/stats. */
  async stats() {
    return this.#send('GET', '/v1/stats');
  }

  /** POST /v1/flush — дренировать буфер и записать снапшот. */
  async flush() {
    await this.#send('POST', '/v1/flush');
  }

  // ---- коллекции ----

  /** POST /v1/collections */
  async createCollection(name) {
    await this.#send('POST', '/v1/collections', { body: { name } });
  }

  /** GET /v1/collections */
  async listCollections() {
    const out = await this.#send('GET', '/v1/collections');
    return out.collections || [];
  }

  /** GET /v1/collections/{name} */
  async getCollection(name) {
    return this.#send('GET', '/v1/collections/' + enc(name));
  }

  /** DELETE /v1/collections/{name} */
  async deleteCollection(name) {
    await this.#send('DELETE', '/v1/collections/' + enc(name));
  }

  // ---- документы ----

  #docsPath(coll, id) {
    let p = '/v1/collections/' + enc(coll) + '/docs';
    if (id) p += '/' + enc(id);
    return p;
  }

  /** POST /v1/collections/{coll}/docs — вставка одного документа. */
  async insert(coll, doc) {
    return this.#send('POST', this.#docsPath(coll, ''), { body: doc });
  }

  /** POST /v1/collections/{coll}/docs — bulk: массив документов. */
  async bulkInsert(coll, docs) {
    const out = await this.#send('POST', this.#docsPath(coll, ''), { body: docs });
    return (out && out.docs) || [];
  }

  /** GET /v1/collections/{coll}/docs/{id} */
  async get(coll, id) {
    return this.#send('GET', this.#docsPath(coll, id));
  }

  /**
   * GET /v1/collections/{coll}/docs
   * @param {string} coll
   * @param {{limit?:number, offset?:number, order?:string, filters?:Record<string,any>}} [opts]
   *        limit 0 — серверный дефолт (50); order — 'price:desc,name:asc';
   *        filters — равенство по верхнеуровневым полям.
   */
  async list(coll, opts = {}) {
    const query = { ...(opts.filters || {}) };
    if (opts.limit) query.limit = opts.limit;
    if (opts.offset) query.offset = opts.offset;
    if (opts.order) query.order = opts.order;
    return this.#send('GET', this.#docsPath(coll, ''), { query });
  }

  /** PUT /v1/collections/{coll}/docs/{id} — заменить (404, если нет). */
  async put(coll, id, doc) {
    return this.#send('PUT', this.#docsPath(coll, id), { body: doc });
  }

  /** PUT ?upsert=true — создать, если нет. */
  async upsert(coll, id, doc) {
    return this.#send('PUT', this.#docsPath(coll, id), { query: { upsert: 'true' }, body: doc });
  }

  /** PATCH /v1/collections/{coll}/docs/{id} — слияние верхнего уровня. */
  async patch(coll, id, doc) {
    return this.#send('PATCH', this.#docsPath(coll, id), { body: doc });
  }

  /** DELETE /v1/collections/{coll}/docs/{id} */
  async delete(coll, id) {
    await this.#send('DELETE', this.#docsPath(coll, id));
  }

  // ---- SQL ----

  /** POST /v1/query — read-only SQL с параметрами. */
  async sql(query, ...args) {
    const body = { sql: query };
    if (args.length) body.args = args;
    return this.#send('POST', '/v1/query', { body });
  }

  // ---- индексы ----

  #idxPath(coll) {
    return '/v1/collections/' + enc(coll) + '/indexes';
  }

  /** POST /v1/collections/{coll}/indexes?field=f */
  async createIndex(coll, field) {
    await this.#send('POST', this.#idxPath(coll), { query: { field } });
  }

  /** GET /v1/collections/{coll}/indexes */
  async listIndexes(coll) {
    const out = await this.#send('GET', this.#idxPath(coll));
    return out.indexes || [];
  }

  /** DELETE /v1/collections/{coll}/indexes/{field} */
  async dropIndex(coll, field) {
    await this.#send('DELETE', this.#idxPath(coll) + '/' + enc(field));
  }

  // ---- экспорт / импорт / бэкап ----

  /**
   * GET /v1/export — сырой Response (стрим). Таймаут по умолчанию отключён.
   * Для обычных случаев пользуйтесь exportText()/exportJson().
   */
  async exportResponse() {
    return this.#send('GET', '/v1/export', { raw: true, timeoutMs: 0 });
  }

  /** GET /v1/export как текст JSON. */
  async exportText() {
    return (await this.exportResponse()).text();
  }

  /** GET /v1/export как разобранный JSON. */
  async exportJson() {
    return JSON.parse(await this.exportText());
  }

  /** GET /v1/backup — сырой Response (потоковый .enc). */
  async backupResponse() {
    return this.#send('GET', '/v1/backup', { raw: true, timeoutMs: 0 });
  }

  /** GET /v1/backup как Uint8Array (удобно для fs.writeFile). */
  async backupBytes() {
    const res = await this.backupResponse();
    return new Uint8Array(await res.arrayBuffer());
  }

  /**
   * POST /v1/import?mode=replace|merge
   * @param {object|string|Uint8Array} payload экспорт из exportText() или разобранный объект
   */
  async import(payload, mode = 'replace') {
    const m = mode || 'replace';
    return this.#send('POST', '/v1/import', {
      query: { mode: m },
      body: typeof payload === 'string' || payload instanceof Uint8Array ? payload : JSON.stringify(payload),
    });
  }
}

/** Экранирование сегмента пути (аналог url.PathEscape в Go-клиенте). */
function enc(seg) {
  return encodeURIComponent(String(seg));
}

module.exports = { Client, APIError, isStatus, DEFAULT_TIMEOUT_MS };
