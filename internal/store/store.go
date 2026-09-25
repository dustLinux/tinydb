// Package store implements the SQLite-backed storage engine of tinydb.
//
// Design constraints (see docs/DESIGN.md):
//   - RAM budget: <= 10 MiB. The database lives on disk (never loaded into
//     memory); PRAGMA cache_size and GOMEMLIMIT keep RSS bounded. Encryption
//     and decryption are streaming (64 KiB chunks), constant memory.
//   - Disk quota: 100 MiB by default, enforced with PRAGMA max_page_count.
//   - Encryption at rest: the plaintext SQLite file exists only while the
//     server runs; the durable artifact is the encrypted container
//     (AES-256-GCM, see internal/cryptobox). A snapshot is written on every
//     autosave tick, on demand, and on graceful shutdown.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dustlinux/tinydb/internal/logx"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/dustlinux/tinydb/internal/cryptobox"
)

// Config configures a Store.
type Config struct {
	// Dir is the data directory (db.sqlite, db.sqlite.enc, db.key live here).
	Dir string
	// MaxBytes is the disk quota for the database (default 100 MiB).
	MaxBytes int64
	// Key is a raw 32-byte key. If nil, the passphrase is used; if both are
	// empty, a key file is created in Dir.
	Key []byte
	// Passphrase, when non-empty, encrypts with a stretched password instead
	// of a key file.
	Passphrase string
	// KeepPlain leaves the plaintext SQLite file on disk after shutdown.
	KeepPlain bool
	// CacheKiB is the SQLite page cache size per connection (default 64 KiB).
	CacheKiB int
	// BufferBytes caps the write-behind overlay (default 1 MiB). When the
	// buffer exceeds the cap it is applied to SQLite synchronously.
	BufferBytes int
	// BufferItems caps the number of buffered mutations (default 10000).
	BufferItems int
	// GoMemLimitMiB bounds the Go heap via debug.SetMemoryLimit (0 = off).
	GoMemLimitMiB int
	Logger        *logx.Logger
}

// DefaultMaxBytes is the default storage quota: 100 MiB.
const DefaultMaxBytes = 100 << 20

// Doc is a stored document with metadata.
type Doc struct {
	ID         string          `json:"_id"`
	Collection string          `json:"collection"`
	Created    int64           `json:"created"`
	Updated    int64           `json:"updated"`
	Data       json.RawMessage `json:"data"`
}

// CollectionInfo describes a collection.
type CollectionInfo struct {
	Name      string `json:"name"`
	Created   int64  `json:"created"`
	Docs      int64  `json:"docs"`
	DataBytes int64  `json:"data_bytes"`
}

// ListOptions controls document listing.
type ListOptions struct {
	Limit   int               // 0 = default 50, capped at 500
	Offset  int               // >= 0
	Order   []Order           // up to 2 keys
	Filters map[string]string // top-level field = raw query value
	MaxScan int64             // stop scanning after this many data bytes (0 = 8 MiB)
}

// Order is a sort key on a top-level JSON field.
type Order struct {
	Field string
	Desc  bool
}

// ListResult is the outcome of a listing.
type ListResult struct {
	Items     []Doc `json:"items"`
	Total     int64 `json:"total"`
	Truncated bool  `json:"truncated,omitempty"`
}

// Stats reports engine statistics.
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
	// Write-behind buffer state.
	BufferItems    int   `json:"buffer_items"`
	BufferBytes    int   `json:"buffer_bytes"`
	BufferMaxBytes int   `json:"buffer_max_bytes"`
	BufferApplies  int64 `json:"buffer_applies"`
}

// Sentinel errors.
var (
	ErrNotFound    = errors.New("store: not found")
	ErrExists      = errors.New("store: already exists")
	ErrInvalid     = errors.New("store: invalid argument")
	ErrQuota       = errors.New("store: disk quota exceeded")
	ErrClosed      = errors.New("store: closed")
	ErrReadOnlySQL = errors.New("store: only read-only queries (SELECT/WITH) are allowed")
)

// Name patterns (previously regexp; hand-rolled to keep the binary/RSS small —
// regexp alone costs ~270 KiB of code pages):
//
//	collection: ^[A-Za-z_][A-Za-z0-9_\-]{0,63}$
//	field:      ^[A-Za-z_][A-Za-z0-9_]{0,63}$
const (
	collNamePat  = `^[A-Za-z_][A-Za-z0-9_\-]{0,63}$`
	fieldNamePat = `^[A-Za-z_][A-Za-z0-9_]{0,63}$`
)

func nameOK(s string, allowDash bool) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	c := s[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c = s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' ||
			(allowDash && c == '-') {
			continue
		}
		return false
	}
	return true
}

const (
	plainName = "db.sqlite"
	encName   = "db.sqlite.enc"
	keyName   = "db.key"
	schemaVer = "1"
)

// Store is the storage engine. All exported methods are safe for concurrent
// use.
type Store struct {
	cfg                         Config
	db                          *sql.DB
	log                         *logx.Logger
	mu                          sync.RWMutex // serialises writers and snapshots
	key                         []byte
	encPath, plainPath, keyPath string

	dirty     atomic.Bool
	flushes   atomic.Int64
	lastFlush atomic.Int64
	closed    atomic.Bool
	saveSeq   atomic.Int64
	applies   atomic.Int64

	// write-behind overlay (guarded by mu; see writeback.go)
	pend      []*pendingOp
	pendIdx   map[string]int
	pendBytes int
	pendN     atomic.Int32

	closeMu sync.Mutex // serialises Close
}

// Open opens (or creates) the store in cfg.Dir.
func Open(cfg Config) (*Store, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("%w: empty data dir", ErrInvalid)
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.CacheKiB <= 0 {
		cfg.CacheKiB = 64
	}
	if cfg.BufferBytes <= 0 {
		cfg.BufferBytes = 1 << 20 // 1 MiB write-behind overlay
	}
	if cfg.BufferItems <= 0 {
		cfg.BufferItems = 10000
	}
	if cfg.Logger == nil {
		cfg.Logger = logx.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	// Tighten permissions in case the dir existed with a wider mode.
	_ = os.Chmod(cfg.Dir, 0o700)

	s := &Store{
		cfg:       cfg,
		log:       cfg.Logger,
		encPath:   filepath.Join(cfg.Dir, encName),
		plainPath: filepath.Join(cfg.Dir, plainName),
		keyPath:   filepath.Join(cfg.Dir, keyName),
		pendIdx:   make(map[string]int),
	}

	// Resolve the key before touching any plaintext.
	if len(cfg.Key) > 0 {
		s.key = cfg.Key
	} else if cfg.Passphrase == "" {
		k, err := cryptobox.LoadOrCreateKeyFile(s.keyPath)
		if err != nil {
			return nil, err
		}
		s.key = k
	}

	// Materialise the plaintext working file if we only have the encrypted one.
	hadPlain := false
	if st, err := os.Stat(s.plainPath); err == nil && st.Size() > 0 {
		hadPlain = true
		s.log.Info("resuming from existing plaintext working file", "path", s.plainPath, "bytes", st.Size())
	} else if st, err := os.Stat(s.encPath); err == nil && st.Size() > 0 {
		s.log.Info("decrypting database", "path", s.encPath, "bytes", st.Size())
		if err := s.decryptTo(s.encPath, s.plainPath); err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", s.encPath, err)
		}
		hadPlain = true
	}
	_ = hadPlain

	dsn := s.plainPath + "?_busy_timeout=5000&_journal_mode=DELETE&_synchronous=FULL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// Two pooled connections with a small per-connection page cache: SQLite's
	// cache_size is per connection, so 4 conns × 256 KiB would be a 1 MiB
	// anon-RSS resident budget by itself.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	s.db = db

	// Keep resident memory small: bounded page cache, no mmap.
	pragmas := []string{
		"PRAGMA page_size=4096",
		"PRAGMA journal_mode=DELETE",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		fmt.Sprintf("PRAGMA cache_size=-%d", cfg.CacheKiB),
		"PRAGMA mmap_size=0",
		"PRAGMA temp_store=FILE",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	// SQLite creates the plaintext working file with the process umask
	// (0644 under a typical umask); it must be owner-only like the rest
	// of the data directory.
	_ = os.Chmod(s.plainPath, 0o600)
	// Enforce the disk quota in pages (page_size is fixed at 4096 above).
	maxPages := cfg.MaxBytes / 4096
	if maxPages < 64 {
		maxPages = 64
	}
	if _, err := db.Exec("PRAGMA max_page_count=" + strconv.FormatInt(maxPages, 10)); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	// A hot journal (crash) is recovered by the first statement above; verify.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// The .enc snapshot may be stale after a crash — refresh it soon.
	if hadPlain {
		s.dirty.Store(true)
	}
	return s, nil
}

func (s *Store) initSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS _meta(
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS _collections(
			name TEXT PRIMARY KEY,
			created INTEGER NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS docs(
			collection TEXT NOT NULL,
			id TEXT NOT NULL,
			data TEXT NOT NULL,
			created INTEGER NOT NULL,
			updated INTEGER NOT NULL,
			PRIMARY KEY(collection, id)
		) WITHOUT ROWID`,
		`INSERT OR IGNORE INTO _meta(k,v) VALUES('schema_version','1')`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// ---- key / encryption plumbing ----

func (s *Store) decryptTo(encPath, dstPath string) error {
	src, err := os.Open(encPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := dstPath + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := cryptobox.Decrypt(src, dst, s.key, s.cfg.Passphrase); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dstPath)
}

// Flush applies the write-behind buffer and writes an encrypted snapshot of
// the database to <dir>/db.sqlite.enc (atomic replace). It is safe to call
// concurrently with reads; writers are blocked for the duration.
//
// After a successful snapshot the Go runtime is asked to return freed pages
// to the OS (RSS stays close to the idle baseline instead of the high-water
// mark of the last burst of writes).
func (s *Store) Flush() error {
	if s.closed.Load() {
		return ErrClosed
	}
	// Drain the RAM overlay first so the snapshot contains every mutation.
	s.mu.Lock()
	err := s.applyLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.mu.RLock() // wait for in-flight writers/readers to leave statements
	defer s.mu.RUnlock()

	src, err := os.Open(s.plainPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if st, err := src.Stat(); err == nil && st.Size() > s.cfg.MaxBytes {
		s.log.Warn("plaintext exceeds quota", "bytes", st.Size(), "max", s.cfg.MaxBytes)
	}

	dir := filepath.Dir(s.encPath)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", encName, s.saveSeq.Add(1)))
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := cryptobox.Encrypt(src, dst, s.key, s.cfg.Passphrase, 0); err != nil {
		dst.Close()
		os.Remove(tmp)
		return fmt.Errorf("encrypt: %w", err)
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.encPath); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	s.dirty.Store(false)
	s.flushes.Add(1)
	s.lastFlush.Store(time.Now().Unix())
	// Release unused heap pages now that the burst is over (RAM budget).
	// Order matters: first shrink SQLite page/lookaside caches, then hand
	// cached free pages of the C allocator back to the OS, scavenge the Go
	// heap, and finally evict our own clean code pages (they refault from
	// the page cache on demand, so this is free apart from minor faults).
	_, _ = s.db.Exec(`PRAGMA shrink_memory`)
	purgeNative()
	debug.FreeOSMemory()
	if n, errno := evictCode(); errno != 0 {
		s.log.Warn("evict code failed", "ranges", n, "errno", errno.Error())
	} else {
		s.log.Debug("evicted code pages", "ranges", n)
	}
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// MarkDirty flags the encrypted snapshot as stale.
func (s *Store) MarkDirty() { s.dirty.Store(true) }

// Dirty reports whether the encrypted snapshot is stale.
func (s *Store) Dirty() bool { return s.dirty.Load() }

// Autosave periodically flushes dirty state until stop is closed.
func (s *Store) Autosave(every time.Duration, stop <-chan struct{}) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if s.dirty.Load() {
				if err := s.Flush(); err != nil {
					s.log.Error("autosave failed", "err", err)
				} else {
					s.log.Debug("autosave ok")
				}
			}
		}
	}
}

// Close drains the write-behind buffer, flushes (if needed) and releases
// resources. The plaintext file is removed unless KeepPlain is set or the
// flush failed (never destroy data).
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed.Load() {
		return nil
	}
	var ferr error
	// Drain the write-behind overlay before the final snapshot.
	s.mu.Lock()
	if err := s.applyLocked(); err != nil {
		ferr = err
		s.log.Error("draining write-back buffer", "err", err)
	}
	s.mu.Unlock()
	if ferr == nil && (s.dirty.Load() || !fileExists(s.encPath)) {
		ferr = s.Flush()
	}
	s.closed.Store(true)
	err := s.db.Close()
	if ferr == nil && !s.cfg.KeepPlain {
		os.Remove(s.plainPath)
		os.Remove(s.plainPath + "-journal")
		os.Remove(s.plainPath + ".new")
		syncDir(filepath.Dir(s.plainPath))
	} else if ferr != nil {
		s.log.Error("keeping plaintext file: encrypted flush failed", "err", ferr)
	}
	if err != nil {
		return err
	}
	return ferr
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// EncPath returns the path of the encrypted snapshot.
func (s *Store) EncPath() string { return s.encPath }

// PlainPath returns the path of the working plaintext file.
func (s *Store) PlainPath() string { return s.plainPath }

// ---- validation helpers ----

func validCollection(name string) error {
	// The '_' prefix is reserved for system tables (_collections, _meta);
	// keeping it free makes index names and future SQL surface unambiguous.
	if strings.HasPrefix(name, "_") {
		return fmt.Errorf("%w: collection names must not start with '_' (reserved)", ErrInvalid)
	}
	if !nameOK(name, true) {
		return fmt.Errorf("%w: collection name must match %s", ErrInvalid, collNamePat)
	}
	return nil
}

func validField(name string) error {
	if !nameOK(name, false) {
		return fmt.Errorf("%w: field name must match %s", ErrInvalid, fieldNamePat)
	}
	return nil
}

func normalizeJSON(raw []byte) (string, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// Reject trailing garbage.
	if dec.More() {
		return "", fmt.Errorf("%w: trailing data after JSON value", ErrInvalid)
	}
	if _, ok := v.(map[string]any); !ok {
		return "", fmt.Errorf("%w: document must be a JSON object", ErrInvalid)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// fallback: time-based
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 32)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "database or disk is full"),
		strings.Contains(msg, "disk I/O error"),
		strings.Contains(msg, "SQLITE_FULL"):
		return fmt.Errorf("%w: %v", ErrQuota, err)
	}
	return err
}

func (s *Store) begin() (*sql.Tx, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, mapErr(err)
	}
	return tx, nil
}

// ---- collections ----

// CreateCollection queues creation of an empty collection in the write-behind
// buffer; the row is inserted lazily (see writeback.go).
func (s *Store) CreateCollection(name string) error {
	if err := validCollection(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Duplicate check: applied row or pending creation (index 0 is valid!).
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM _collections WHERE name=?`, name).Scan(&one)
	pendKey := pendingOp{kind: opCreateColl, coll: name}.key()
	_, pendingExists := s.pendIdx[pendKey]
	switch {
	case pendingExists:
		return fmt.Errorf("%w: collection %q", ErrExists, name)
	case err == nil:
		return fmt.Errorf("%w: collection %q", ErrExists, name)
	case errors.Is(err, sql.ErrNoRows):
		// ok — proceed
	default:
		return mapErr(err)
	}
	s.enqueue(&pendingOp{kind: opCreateColl, coll: name, created: time.Now().UnixMilli()})
	return nil
}

// DropCollection removes a collection and all of its documents and indexes.
// The write-behind buffer is applied first (read barrier), so no pending
// writes survive the drop.
func (s *Store) DropCollection(name string) error {
	if err := validCollection(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyLocked(); err != nil {
		return err
	}
	tx, err := s.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM _collections WHERE name=?`, name)
	if err != nil {
		return mapErr(err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM docs WHERE collection=?`, name); err != nil {
		return mapErr(err)
	}
	// Drop expression indexes belonging to this collection.
	idx, err := collectIndexes(tx, name)
	if err != nil {
		return err
	}
	for _, in := range idx {
		if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + in); err != nil {
			return mapErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return fmt.Errorf("%w: collection %q", ErrNotFound, name)
	}
	s.dirty.Store(true)
	return nil
}

// ListCollections returns all collections with counts (read barrier: pending
// writes are applied first).
func (s *Store) ListCollections() ([]CollectionInfo, error) {
	if err := s.ApplyBuffer(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`
		SELECT c.name, c.created,
		       COALESCE(d.n, 0), COALESCE(d.b, 0)
		FROM _collections c
		LEFT JOIN (
			SELECT collection, COUNT(*) AS n, SUM(length(data)) AS b
			FROM docs GROUP BY collection
		) d ON d.collection = c.name
		ORDER BY c.name`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []CollectionInfo{}
	for rows.Next() {
		var ci CollectionInfo
		if err := rows.Scan(&ci.Name, &ci.Created, &ci.Docs, &ci.DataBytes); err != nil {
			return nil, err
		}
		out = append(out, ci)
	}
	return out, rows.Err()
}

// Collection returns one collection or ErrNotFound (read barrier applied).
func (s *Store) Collection(name string) (CollectionInfo, error) {
	if err := validCollection(name); err != nil {
		return CollectionInfo{}, err
	}
	if err := s.ApplyBuffer(); err != nil {
		return CollectionInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ci CollectionInfo
	err := s.db.QueryRow(`
		SELECT c.name, c.created,
		       COALESCE((SELECT COUNT(*) FROM docs WHERE collection=c.name),0),
		       COALESCE((SELECT SUM(length(data)) FROM docs WHERE collection=c.name),0)
		FROM _collections c WHERE c.name=?`, name).
		Scan(&ci.Name, &ci.Created, &ci.Docs, &ci.DataBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionInfo{}, fmt.Errorf("%w: collection %q", ErrNotFound, name)
	}
	return ci, err
}

// ---- documents ----

// Insert queues a document into the write-behind buffer, generating its _id.
// The response returns as soon as the RAM overlay is updated; SQLite sees the
// document after the next apply (ticker / read barrier / limits / flush).
func (s *Store) Insert(collection string, raw []byte) (Doc, error) {
	if err := validCollection(collection); err != nil {
		return Doc{}, err
	}
	data, err := normalizeJSON(raw)
	if err != nil {
		return Doc{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Doc{}, ErrClosed
	}
	now := time.Now().UnixMilli()
	doc := Doc{ID: newID(), Collection: collection, Created: now, Updated: now, Data: json.RawMessage(data)}
	s.enqueue(&pendingOp{kind: opPutDoc, coll: collection, id: doc.ID,
		data: data, created: now, updated: now})
	return doc, nil
}

func ensureCollection(tx *sql.Tx, name string) error {
	var created int64
	err := tx.QueryRow(`SELECT created FROM _collections WHERE name=?`, name).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.Exec(`INSERT INTO _collections(name,created) VALUES(?,?)`,
			name, time.Now().UnixMilli()); err != nil {
			return mapErr(err)
		}
		return nil
	}
	return err
}

// BulkInsert queues many documents into the write-behind buffer.
func (s *Store) BulkInsert(collection string, raws [][]byte) ([]Doc, error) {
	if err := validCollection(collection); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrClosed
	}
	now := time.Now().UnixMilli()
	out := make([]Doc, 0, len(raws))
	for _, raw := range raws {
		data, err := normalizeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", len(out), err)
		}
		doc := Doc{ID: newID(), Collection: collection, Created: now, Updated: now, Data: json.RawMessage(data)}
		s.enqueue(&pendingOp{kind: opPutDoc, coll: collection, id: doc.ID,
			data: data, created: now, updated: now})
		out = append(out, doc)
	}
	return out, nil
}

// Get fetches a single document: the write-behind overlay first, then SQLite.
func (s *Store) Get(collection, id string) (Doc, error) {
	if err := validCollection(collection); err != nil {
		return Doc{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d, ok := s.lookupDocLocked(collection, id); ok {
		return d, nil
	}
	return Doc{}, fmt.Errorf("%w: doc %q", ErrNotFound, id)
}

// Put fully replaces a document (upsert=false: missing doc is an error).
func (s *Store) Put(collection, id string, raw []byte) (Doc, error) {
	return s.writeDoc(collection, id, raw, false, false)
}

// Patch merges top-level keys of raw into the existing document.
func (s *Store) Patch(collection, id string, raw []byte) (Doc, error) {
	return s.writeDoc(collection, id, raw, true, false)
}

// Upsert writes a document with an explicit or generated id, creating if absent.
func (s *Store) Upsert(collection, id string, raw []byte) (Doc, error) {
	if id == "" {
		id = newID()
	}
	return s.writeDoc(collection, id, raw, false, true)
}

func (s *Store) writeDoc(collection, id string, raw []byte, merge, upsert bool) (Doc, error) {
	if err := validCollection(collection); err != nil {
		return Doc{}, err
	}
	if id == "" || len(id) > 64 {
		return Doc{}, fmt.Errorf("%w: bad id", ErrInvalid)
	}
	patch, err := normalizeJSON(raw)
	if err != nil {
		return Doc{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Doc{}, ErrClosed
	}
	base, exists := s.lookupDocLocked(collection, id)
	if !exists && !upsert {
		return Doc{}, fmt.Errorf("%w: doc %q", ErrNotFound, id)
	}
	data := patch
	if merge && exists {
		var bm, ov map[string]any
		if json.Unmarshal(base.Data, &bm) != nil {
			bm = map[string]any{}
		}
		if json.Unmarshal([]byte(patch), &ov) != nil {
			return Doc{}, fmt.Errorf("%w: patch must be a JSON object", ErrInvalid)
		}
		for k, v := range ov {
			bm[k] = v
		}
		b, err := json.Marshal(bm)
		if err != nil {
			return Doc{}, err
		}
		data = string(b)
	}
	now := time.Now().UnixMilli()
	created := base.Created
	if !exists {
		created = now
	}
	s.enqueue(&pendingOp{kind: opPutDoc, coll: collection, id: id,
		data: data, created: created, updated: now})
	return Doc{ID: id, Collection: collection, Created: created, Updated: now, Data: json.RawMessage(data)}, nil
}

// Delete queues removal of a document in the write-behind buffer.
func (s *Store) Delete(collection, id string) error {
	if err := validCollection(collection); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if _, exists := s.lookupDocLocked(collection, id); !exists {
		return fmt.Errorf("%w: doc %q", ErrNotFound, id)
	}
	s.enqueue(&pendingOp{kind: opDelDoc, coll: collection, id: id})
	return nil
}

// ---- listing & filtering ----

func buildWhere(collection string, lo ListOptions) (string, []any) {
	where := []string{"collection = ?"}
	args := []any{collection}
	fields := make([]string, 0, len(lo.Filters))
	for f := range lo.Filters {
		fields = append(fields, f)
	}
	sort.Strings(fields) // deterministic SQL
	for _, f := range fields {
		raw := lo.Filters[f]
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil || raw == "" {
			v = raw // treat as string
		}
		if v == nil {
			where = append(where, "json_extract(data, ?) IS NULL")
			args = append(args, "$."+f)
			continue
		}
		switch t := v.(type) {
		case bool:
			v = map[bool]int{true: 1, false: 0}[t]
		}
		where = append(where, "json_extract(data, ?) = ?")
		args = append(args, "$."+f, v)
	}
	return strings.Join(where, " AND "), args
}

// List returns a page of documents.
func (s *Store) List(collection string, lo ListOptions) (ListResult, error) {
	if err := validCollection(collection); err != nil {
		return ListResult{}, err
	}
	if lo.Limit <= 0 {
		lo.Limit = 50
	}
	if lo.Limit > 500 {
		lo.Limit = 500
	}
	if lo.Offset < 0 {
		lo.Offset = 0
	}
	if lo.MaxScan <= 0 {
		lo.MaxScan = 2 << 20 // keep listing allocations inside the RAM budget
	}
	for _, o := range lo.Order {
		if err := validField(o.Field); err != nil {
			return ListResult{}, err
		}
	}
	for f := range lo.Filters {
		if err := validField(f); err != nil {
			return ListResult{}, err
		}
	}
	// Read barrier: listings must include buffered writes.
	if err := s.ApplyBuffer(); err != nil {
		return ListResult{}, err
	}
	where, args := buildWhere(collection, lo)

	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM docs WHERE "+where, args...).Scan(&total); err != nil {
		return ListResult{}, mapErr(err)
	}

	order := ""
	if len(lo.Order) > 0 {
		parts := make([]string, len(lo.Order))
		for i, o := range lo.Order {
			dir := "ASC"
			if o.Desc {
				dir = "DESC"
			}
			parts[i] = "json_extract(data, ?) " + dir
			args = append(args, "$."+o.Field)
		}
		order = " ORDER BY " + strings.Join(parts, ", ")
	}

	q := "SELECT id, data, created, updated FROM docs WHERE " + where + order +
		" LIMIT ? OFFSET ?"
	qargs := append(append([]any{}, args...), lo.Limit, lo.Offset)
	rows, err := s.db.Query(q, qargs...)
	if err != nil {
		return ListResult{}, mapErr(err)
	}
	defer rows.Close()

	res := ListResult{Items: []Doc{}, Total: total}
	var scanned int64
	for rows.Next() {
		var d Doc
		var data string
		if err := rows.Scan(&d.ID, &data, &d.Created, &d.Updated); err != nil {
			return ListResult{}, err
		}
		d.Collection = collection
		d.Data = json.RawMessage(data)
		scanned += int64(len(data))
		if scanned > lo.MaxScan {
			res.Truncated = true
			break
		}
		res.Items = append(res.Items, d)
	}
	return res, rows.Err()
}

// ---- indexes ----

func indexName(collection, field string) string {
	return fmt.Sprintf("ix_%s_%s", collection, field)
}

// CreateIndex queues creation of an expression index on a top-level field
// (applied lazily with the rest of the buffer).
func (s *Store) CreateIndex(collection, field string) error {
	if err := validCollection(collection); err != nil {
		return err
	}
	if err := validField(field); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	s.enqueue(&pendingOp{kind: opCreateIdx, coll: collection, id: field})
	return nil
}

// DropIndex queues removal of an expression index.
func (s *Store) DropIndex(collection, field string) error {
	if err := validCollection(collection); err != nil {
		return err
	}
	if err := validField(field); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	s.enqueue(&pendingOp{kind: opDropIdx, coll: collection, id: field})
	return nil
}

// ListIndexes returns index names for a collection (read barrier applied).
func (s *Store) ListIndexes(collection string) ([]string, error) {
	if err := validCollection(collection); err != nil {
		return nil, err
	}
	if err := s.ApplyBuffer(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return collectIndexes(s.db, collection)
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// collectIndexes returns index names for the expression indexes of a collection.
func collectIndexes(q querier, collection string) ([]string, error) {
	rows, err := q.Query(`
		SELECT name FROM sqlite_master
		WHERE type='index' AND tbl_name='docs'
		  AND name LIKE ? ESCAPE '\'
		ORDER BY name`, escapeLike("ix_"+collection+"_")+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	// Escape _ and % so the LIKE pattern matches only our index prefix.
	var b strings.Builder
	for _, c := range s {
		if c == '_' || c == '%' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ---- read-only SQL ----

// SQL runs a single read-only statement (SELECT or WITH...) with optional
// positional args and returns column names plus rows as raw JSON values.
// Results are capped at 1000 rows / 2 MiB (truncated flag reports a cut).
func (s *Store) SQL(query string, args []any) ([]string, [][]json.RawMessage, bool, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil, false, fmt.Errorf("%w: empty query", ErrInvalid)
	}
	upper := strings.ToUpper(strings.TrimLeft(q, " \t\r\n("))
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return nil, nil, false, ErrReadOnlySQL
	}
	// Reject stacked statements (allow a single trailing ';').
	t := strings.TrimRight(strings.TrimSpace(q), "; \t\r\n")
	if strings.Contains(t, ";") {
		return nil, nil, false, fmt.Errorf("%w: multiple statements are not allowed", ErrInvalid)
	}
	// Read barrier: SQL must see buffered writes.
	if err := s.ApplyBuffer(); err != nil {
		return nil, nil, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, nil, false, mapErr(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	var out [][]json.RawMessage
	const maxRows = 1000
	const maxBytes = 2 << 20
	var total int64
	truncated := false
	for rows.Next() && len(out) < maxRows {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, false, err
		}
		rec := make([]json.RawMessage, len(cols))
		rowBytes := int64(0)
		for i, v := range vals {
			rec[i] = scanToJSON(v)
			rowBytes += int64(len(rec[i]))
		}
		total += rowBytes
		if total > maxBytes {
			truncated = true
			break
		}
		out = append(out, rec)
	}
	if rows.Next() {
		truncated = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, mapErr(err)
	}
	return cols, out, truncated, nil
}

func scanToJSON(v any) json.RawMessage {
	switch t := v.(type) {
	case nil:
		return json.RawMessage("null")
	case []byte:
		return mustJSON(string(t))
	case string:
		return mustJSON(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return json.RawMessage("null")
		}
		return b
	}
}

func mustJSON(s string) json.RawMessage {
	var probe any
	if err := json.Unmarshal([]byte(s), &probe); err == nil {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// ---- export / import ----

// Export streams the whole database as JSON into w (constant memory).
// Read barrier: buffered writes are applied first.
func (s *Store) Export(w io.Writer) error {
	if err := s.ApplyBuffer(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	write := func(s string) error {
		_, err := w.Write([]byte(s))
		return err
	}
	if err := write(`{"schema_version":"` + schemaVer + `","collections":[`); err != nil {
		return err
	}
	// NB: direct query — ListCollections would take the lock recursively.
	rows0, err := s.db.Query(`SELECT name, created,
		COALESCE((SELECT COUNT(*) FROM docs WHERE collection=_collections.name),0),
		COALESCE((SELECT SUM(length(data)) FROM docs WHERE collection=_collections.name),0)
		FROM _collections ORDER BY name`)
	if err != nil {
		return mapErr(err)
	}
	cols := []CollectionInfo{}
	for rows0.Next() {
		var ci CollectionInfo
		if err := rows0.Scan(&ci.Name, &ci.Created, &ci.Docs, &ci.DataBytes); err != nil {
			rows0.Close()
			return err
		}
		cols = append(cols, ci)
	}
	if err := rows0.Err(); err != nil {
		rows0.Close()
		return err
	}
	rows0.Close()
	for i, c := range cols {
		if i > 0 {
			if err := write(","); err != nil {
				return err
			}
		}
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := write(string(b)); err != nil {
			return err
		}
	}
	if err := write(`],"docs":[`); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT collection, id, data, created, updated FROM docs ORDER BY collection, id`)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var coll, id, data string
		var created, updated int64
		if err := rows.Scan(&coll, &id, &data, &created, &updated); err != nil {
			return err
		}
		if !first {
			if err := write(","); err != nil {
				return err
			}
		}
		first = false
		rec := Doc{ID: id, Collection: coll, Created: created, Updated: updated, Data: json.RawMessage(data)}
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if err := write(string(b)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return write(`]}`)
}

// ImportResult reports what an import did.
type ImportResult struct {
	Collections int    `json:"collections"`
	Docs        int    `json:"docs"`
	Mode        string `json:"mode"`
}

// Import restores an export. mode = "replace" (wipe first) or "merge".
func (s *Store) Import(r io.Reader, mode string) (ImportResult, error) {
	if mode != "replace" && mode != "merge" {
		return ImportResult{}, fmt.Errorf("%w: mode must be replace|merge", ErrInvalid)
	}
	dec := json.NewDecoder(r)
	res := ImportResult{Mode: mode}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Drain buffered writes so replace/merge operates on the full data set.
	if err := s.applyLocked(); err != nil {
		return res, err
	}
	tx, err := s.begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// Decode the top-level object token by token (streaming).
	tok, err := dec.Token()
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return res, fmt.Errorf("%w: expected JSON object", ErrInvalid)
	}
	if mode == "replace" {
		if _, err := tx.Exec(`DELETE FROM docs`); err != nil {
			return res, mapErr(err)
		}
		if _, err := tx.Exec(`DELETE FROM _collections`); err != nil {
			return res, mapErr(err)
		}
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return res, err
		}
		key, _ := keyTok.(string)
		switch key {
		case "schema_version":
			var v string
			if err := dec.Decode(&v); err != nil {
				return res, err
			}
		case "collections":
			t, err := dec.Token()
			if err != nil {
				return res, err
			}
			if d, ok := t.(json.Delim); !ok || d != '[' {
				return res, fmt.Errorf("%w: collections must be an array", ErrInvalid)
			}
			for dec.More() {
				var ci CollectionInfo
				if err := dec.Decode(&ci); err != nil {
					return res, err
				}
				if err := validCollection(ci.Name); err != nil {
					return res, err
				}
				if err := ensureCollection(tx, ci.Name); err != nil {
					return res, err
				}
				res.Collections++
			}
			if _, err := dec.Token(); err != nil { // ']'
				return res, err
			}
		case "docs":
			t, err := dec.Token()
			if err != nil {
				return res, err
			}
			if d, ok := t.(json.Delim); !ok || d != '[' {
				return res, fmt.Errorf("%w: docs must be an array", ErrInvalid)
			}
			for dec.More() {
				var d Doc
				if err := dec.Decode(&d); err != nil {
					return res, err
				}
				if err := validCollection(d.Collection); err != nil {
					return res, err
				}
				if d.ID == "" || len(d.ID) > 64 {
					return res, fmt.Errorf("%w: bad _id in import", ErrInvalid)
				}
				data, err := normalizeJSON([]byte(d.Data))
				if err != nil {
					return res, err
				}
				if d.Created == 0 {
					d.Created = time.Now().UnixMilli()
				}
				if d.Updated == 0 {
					d.Updated = d.Created
				}
				if err := ensureCollection(tx, d.Collection); err != nil {
					return res, err
				}
				if mode == "replace" {
					_, err = tx.Exec(`INSERT INTO docs(collection,id,data,created,updated) VALUES(?,?,?,?,?)`,
						d.Collection, d.ID, data, d.Created, d.Updated)
				} else {
					_, err = tx.Exec(`INSERT INTO docs(collection,id,data,created,updated) VALUES(?,?,?,?,?)
						ON CONFLICT(collection,id) DO UPDATE SET data=excluded.data, updated=excluded.updated`,
						d.Collection, d.ID, data, d.Created, d.Updated)
				}
				if err != nil {
					return res, mapErr(err)
				}
				res.Docs++
			}
			if _, err := dec.Token(); err != nil { // ']'
				return res, err
			}
		default:
			// skip unknown value
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return res, err
			}
		}
	}
	if _, err := dec.Token(); err != nil { // '}'
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, mapErr(err)
	}
	s.dirty.Store(true)
	return res, nil
}

// Stats returns engine statistics.
//
// Doc/collection counters reflect the applied (SQLite) state; mutations that
// are still waiting in the write-behind buffer are reported separately as
// buffer_items / buffer_bytes (and are included by List/SQL/Export thanks to
// the read barrier).
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st Stats
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM _collections`).Scan(&st.Collections)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM docs`).Scan(&st.Docs)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(length(data)),0) FROM docs`).Scan(&st.DataBytes)
	var pages, pageSize int64
	_ = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	_ = s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	st.PlainBytes = pages * pageSize
	if fi, err := os.Stat(s.encPath); err == nil {
		st.EncBytes = fi.Size()
	}
	st.MaxBytes = s.cfg.MaxBytes
	st.Encrypted = true
	st.Dirty = s.dirty.Load()
	st.Flushes = s.flushes.Load()
	st.LastFlushUnix = s.lastFlush.Load()
	st.BufferItems = len(s.pend)
	st.BufferBytes = s.pendBytes
	st.BufferApplies = s.applies.Load()
	st.BufferMaxBytes = s.cfg.BufferBytes
	ver := ""
	_ = s.db.QueryRow(`SELECT v FROM _meta WHERE k='schema_version'`).Scan(&ver)
	st.SchemaVersion = ver
	return st
}
