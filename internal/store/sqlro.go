package store

// Настоящий read-only режим для POST /v1/query.
//
// Лексическая проверка «начинается с SELECT/WITH» недостаточна: SQLite
// допускает DML-CTE (WITH x AS (SELECT 1) DELETE FROM docs), и такой запрос
// проходил фильтр, удалял данные в обход overlay, квоты и write-back.
//
// Решение: пользовательский SQL исполняется на том же соединении, но на
// время запроса включаются два уровня защиты:
//   - PRAGMA query_only=1 — SQLite сам отклоняет любую запись в БД;
//   - authorizer, запрещающий все операции, кроме чтения (в том числе PRAGMA
//     и ATTACH, которые query_only не покрывает), плюс denylist опасных
//     функций (файлы, load_extension, BLOB-аллокации).
//
// Отдельный пул соединений сознательно НЕ используется: второй sqlite3
// handle стоил ~0.6 МиB резидентной памяти (свежий сервер уходил с 9.7 в
// 10.3 МиB, то есть за пределы бюджета RAM). Временно защищать общее
// соединение безопасно: s.db имеет MaxOpenConns(1), соединение остаётся
// «занятым» до Close(), а write-операции идут под s.mu (Lock) и не могут
// пересечься с SQL (RLock).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// Функции, которые читают/пишут файлы, грузят расширения или позволяют
// выделить огромный BLOB в память. Остальные (count/sum/json_*/date/…) доступны.
var sqlDeniedFuncs = map[string]bool{
	"load_extension": true,
	"readfile":       true,
	"writefile":      true,
	"edit":           true,
	"fts3_tokenizer": true,
	"zeroblob":       true,
	"randomblob":     true,
}

// readOnlyAuthorizer разрешает только чтение: SELECT, чтение таблиц/колонок и
// скалярные функции (кроме опасного списка).
func readOnlyAuthorizer(op int, a1, a2, a3 string) int {
	switch op {
	case sqlite3.SQLITE_SELECT, sqlite3.SQLITE_READ:
		return sqlite3.SQLITE_OK
	case sqlite3.SQLITE_PRAGMA:
		// query_only разрешён: им включается и проверяется read-only-режим
		// соединения (выполняется под тем же authorizer'ом). Остальные PRAGMA
		// (journal_mode, foreign_keys, mmap_size, …) запрещены.
		if strings.EqualFold(a1, "query_only") {
			return sqlite3.SQLITE_OK
		}
		return sqlite3.SQLITE_DENY
	case sqlite3.SQLITE_FUNCTION:
		if sqlDeniedFuncs[strings.ToLower(a2)] {
			return sqlite3.SQLITE_DENY
		}
		return sqlite3.SQLITE_OK
	default:
		// INSERT/UPDATE/DELETE/CREATE/DROP/ALTER/ATTACH/DETACH/PRAGMA/
		// TRANSACTION/SAVEPOINT/ANALYZE/REINDEX/COPY и всё прочее — запрещено.
		return sqlite3.SQLITE_DENY
	}
}

// withReadOnly выполняет fn на соединении, временно защищённом authorizer'ом и
// PRAGMA query_only=1. Защита снимается в defer при любом исходе (включая
// панику), так что следующий пользователь соединения не увидит read-only.
func (s *Store) withReadOnly(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.Raw(func(dc any) error {
		c, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("неожиданный драйвер %T", dc)
		}
		c.RegisterAuthorizer(readOnlyAuthorizer)
		return nil
	}); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "PRAGMA query_only=0")
		_ = conn.Raw(func(dc any) error {
			if c, ok := dc.(*sqlite3.SQLiteConn); ok {
				c.RegisterAuthorizer(nil)
			}
			return nil
		})
	}()

	// query_only включается уже под authorizer'ом, поэтому authorizer и
	// разрешает ровно эту одну PRAGMA (см. readOnlyAuthorizer).
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=1"); err != nil {
		return fmt.Errorf("включить query_only: %w", err)
	}
	return fn(conn)
}
