// Типы tinydb-client (CommonJS + ESM, без зависимостей).

/** Не-2xx ответ сервера. */
export declare class APIError extends Error {
  readonly name: 'APIError';
  /** HTTP-статус ответа. */
  readonly status: number;
  /** Сообщение сервера (поле error или текст тела). */
  readonly message: string;
  constructor(status: number, message: string);
}

/** Проверяет, что ошибка — APIError с указанным статусом. */
export declare function isStatus(err: unknown, status: number): boolean;

export declare const DEFAULT_TIMEOUT_MS: number;

/** Хранимый документ с метаданными. */
export interface Doc {
  _id: string;
  collection: string;
  /** Unix-время, секунды. */
  created: number;
  updated: number;
  data: unknown;
}

/** Описание коллекции. */
export interface CollectionInfo {
  name: string;
  created: number;
  docs: number;
  data_bytes: number;
}

/** Страница документов. */
export interface ListResult<T = unknown> {
  items: Doc[];
  total: number;
  truncated: boolean;
}

/** Ответ GET /v1/stats. */
export interface Stats {
  collections: number;
  docs: number;
  data_bytes: number;
  plain_bytes: number;
  enc_bytes: number;
  max_bytes: number;
  encrypted: boolean;
  dirty: boolean;
  flushes: number;
  last_flush_unix: number;
  schema_version: string;
  server_version: string;
  /** RSS процесса, КиБ (0 на платформах без /proc). */
  rss_kb: number;
  buffer_items: number;
  buffer_bytes: number;
  buffer_max_bytes: number;
  buffer_applies: number;
  heap_alloc_kb: number;
  heap_sys_kb: number;
  heap_idle_kb: number;
}

/** Результат read-only SQL-запроса. */
export interface QueryResult {
  columns: string[];
  rows: Record<string, unknown>[];
  count: number;
  truncated: boolean;
}

/** Что сделал импорт. */
export interface ImportResult {
  collections: number;
  docs: number;
  mode: string;
}

/** Опции list(). */
export interface ListOptions {
  /** 0 — серверный дефолт (50). */
  limit?: number;
  offset?: number;
  /** Например, 'price:desc,name:asc'. */
  order?: string;
  /** Равенство по верхнеуровневым полям документа. */
  filters?: Record<string, string | number | boolean>;
}

export interface ClientOptions {
  /** Альтернатная реализация fetch (Node <18, тесты, прокси). */
  fetch?: typeof fetch;
  /** Таймаут запроса, мс; 0 — без таймаута (стримы). */
  timeoutMs?: number;
}

export declare class Client {
  constructor(baseURL: string, token?: string, opts?: ClientOptions);
  readonly base: string;
  readonly token: string;
  readonly timeoutMs: number;

  health(): Promise<{ status: string; version: string }>;
  stats(): Promise<Stats>;
  flush(): Promise<void>;

  createCollection(name: string): Promise<void>;
  listCollections(): Promise<CollectionInfo[]>;
  getCollection(name: string): Promise<CollectionInfo>;
  deleteCollection(name: string): Promise<void>;

  insert(coll: string, doc: unknown): Promise<Doc>;
  bulkInsert(coll: string, docs: unknown[]): Promise<Doc[]>;
  get(coll: string, id: string): Promise<Doc>;
  list(coll: string, opts?: ListOptions): Promise<ListResult>;
  put(coll: string, id: string, doc: unknown): Promise<Doc>;
  upsert(coll: string, id: string, doc: unknown): Promise<Doc>;
  patch(coll: string, id: string, doc: unknown): Promise<Doc>;
  delete(coll: string, id: string): Promise<void>;

  sql(query: string, ...args: unknown[]): Promise<QueryResult>;

  createIndex(coll: string, field: string): Promise<void>;
  listIndexes(coll: string): Promise<string[]>;
  dropIndex(coll: string, field: string): Promise<void>;

  exportResponse(): Promise<Response>;
  exportText(): Promise<string>;
  exportJson(): Promise<unknown>;
  backupResponse(): Promise<Response>;
  backupBytes(): Promise<Uint8Array>;
  import(payload: unknown | string | Uint8Array, mode?: 'replace' | 'merge'): Promise<ImportResult>;
}
