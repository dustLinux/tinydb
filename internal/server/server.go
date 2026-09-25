// Package server exposes the tinydb REST API.
//
// Handlers run on the dependency-free internal/httpx HTTP/1.1 core (keeps the
// binary small; see docs/DESIGN.md). The API itself is transport-agnostic:
// plain HTTP, token auth, JSON in/out.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dustlinux/tinydb/internal/logx"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dustlinux/tinydb/internal/httpx"
	"github.com/dustlinux/tinydb/internal/store"
)

// Version is the tinydb server version.
const Version = "1.0.0"

// Config configures the API server.
type Config struct {
	Store     *store.Store
	Token     string // empty = auth disabled
	MaxBody   int64  // max request body bytes (default 4 MiB)
	MaxImport int64  // max /v1/import upload bytes, streamed (default 64 MiB)
	CORS      bool
	Logger    *logx.Logger
}

// New builds the HTTP handler.
func New(cfg Config) httpx.Handler {
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 4 << 20
	}
	if cfg.MaxImport <= 0 {
		cfg.MaxImport = 64 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = logx.Default()
	}
	s := &server{cfg: cfg, log: cfg.Logger}
	mux := httpx.NewMux()

	// Liveness (no auth).
	mux.HandleFunc("GET /health", s.health)

	// Meta.
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("POST /v1/flush", s.flush)
	mux.HandleFunc("GET /v1/export", s.export)
	mux.HandleFunc("POST /v1/import", s.importData)
	mux.HandleFunc("GET /v1/backup", s.backup)
	mux.HandleFunc("POST /v1/query", s.sqlQuery)

	// Collections.
	mux.HandleFunc("GET /v1/collections", s.listCollections)
	mux.HandleFunc("POST /v1/collections", s.createCollection)
	mux.HandleFunc("GET /v1/collections/{coll}", s.getCollection)
	mux.HandleFunc("DELETE /v1/collections/{coll}", s.dropCollection)

	// Documents.
	mux.HandleFunc("POST /v1/collections/{coll}/docs", s.insertDocs)
	mux.HandleFunc("GET /v1/collections/{coll}/docs", s.listDocs)
	mux.HandleFunc("GET /v1/collections/{coll}/docs/{id}", s.getDoc)
	mux.HandleFunc("PUT /v1/collections/{coll}/docs/{id}", s.putDoc)
	mux.HandleFunc("PATCH /v1/collections/{coll}/docs/{id}", s.patchDoc)
	mux.HandleFunc("DELETE /v1/collections/{coll}/docs/{id}", s.deleteDoc)

	// Indexes.
	mux.HandleFunc("GET /v1/collections/{coll}/indexes", s.listIndexes)
	mux.HandleFunc("POST /v1/collections/{coll}/indexes", s.createIndex)
	mux.HandleFunc("DELETE /v1/collections/{coll}/indexes/{field}", s.dropIndex)

	// Root info.
	mux.HandleFunc("GET /", s.index)

	var h httpx.Handler = mux.Handler()
	h = s.auth(h)
	h = s.cors(h)
	h = s.logRequests(h)
	h = s.recoverPanic(h)
	return h
}

type server struct {
	cfg Config
	log *logx.Logger
}

// ---- middleware ----

func (s *server) recoverPanic(next httpx.Handler) httpx.Handler {
	return func(w *httpx.Response, r *httpx.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "path", r.Path)
				if !w.HdrWritten() {
					w.Fail(httpx.StatusInternalServerError, "internal error")
				} else {
					w.CloseConnection()
				}
			}
		}()
		next(w, r)
	}
}

func (s *server) logRequests(next httpx.Handler) httpx.Handler {
	return func(w *httpx.Response, r *httpx.Request) {
		start := time.Now()
		next(w, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.Path,
			"status", w.Status(),
			"bytes", w.Written(),
			"dur_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr)
	}
}

func (s *server) cors(next httpx.Handler) httpx.Handler {
	if !s.cfg.CORS {
		return next
	}
	return func(w *httpx.Response, r *httpx.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
		if r.Method == "OPTIONS" {
			w.WriteHeader(httpx.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (s *server) auth(next httpx.Handler) httpx.Handler {
	if s.cfg.Token == "" {
		return next
	}
	want := sha256hex(s.cfg.Token)
	return func(w *httpx.Response, r *httpx.Request) {
		if r.Path == "/health" {
			next(w, r)
			return
		}
		got := ""
		if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
			got = strings.TrimPrefix(ah, "Bearer ")
		} else if k := r.Header.Get("X-API-Key"); k != "" {
			got = k
		}
		if subtle.ConstantTimeCompare(sha256hex(got), want) != 1 {
			w.Fail(httpx.StatusUnauthorized, "missing or invalid token")
			return
		}
		next(w, r)
	}
}

func sha256hex(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// ---- helpers ----

func writeJSON(w *httpx.Response, status int, v any) {
	_ = w.JSON(status, v)
}

func writeErr(w *httpx.Response, status int, msg string) {
	w.Fail(status, msg)
}

// markBuffered reflects the write-behind buffer state into the response:
// header X-Webdb-Flushed plus, for envelope bodies, the "flushed" field.
// Returns true when the mutation already reached SQLite.
func (s *server) markBuffered(w *httpx.Response) bool {
	flushed := s.cfg.Store.Buffered() == 0
	if flushed {
		w.Header().Set("X-Webdb-Flushed", "true")
	} else {
		w.Header().Set("X-Webdb-Flushed", "false")
	}
	return flushed
}

func (s *server) body(w *httpx.Response, r *httpx.Request) []byte {
	b, err := httpx.ReadBody(r, s.cfg.MaxBody)
	if err != nil {
		var tl *httpx.TooLargeError
		if errors.As(err, &tl) {
			writeErr(w, httpx.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", s.cfg.MaxBody))
			return nil
		}
		writeErr(w, httpx.StatusBadRequest, "read body: "+err.Error())
		return nil
	}
	return b
}

// storeErr maps storage errors to HTTP status codes.
func storeErr(w *httpx.Response, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, httpx.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrExists):
		writeErr(w, httpx.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid),
		errors.Is(err, store.ErrReadOnlySQL):
		writeErr(w, httpx.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrQuota):
		writeErr(w, httpx.StatusInsufficientStorage, err.Error())
	case errors.Is(err, store.ErrClosed):
		writeErr(w, httpx.StatusServiceUnavailable, err.Error())
	default:
		writeErr(w, httpx.StatusInternalServerError, err.Error())
	}
}

// ---- handlers ----

func (s *server) health(w *httpx.Response, _ *httpx.Request) {
	writeJSON(w, httpx.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) index(w *httpx.Response, _ *httpx.Request) {
	writeJSON(w, httpx.StatusOK, map[string]any{
		"name":    "tinydb",
		"version": Version,
		"docs":    "docs/API.md",
		"endpoints": []string{
			"GET/POST /v1/collections",
			"GET/DELETE /v1/collections/{coll}",
			"GET/POST /v1/collections/{coll}/docs",
			"GET/PUT/PATCH/DELETE /v1/collections/{coll}/docs/{id}",
			"GET/POST/DELETE /v1/collections/{coll}/indexes",
			"POST /v1/query",
			"GET /v1/stats, /v1/export, /v1/backup",
			"POST /v1/flush, /v1/import",
		},
	})
}

func (s *server) stats(w *httpx.Response, _ *httpx.Request) {
	st := s.cfg.Store.Stats()
	stMap := map[string]any{}
	b, _ := json.Marshal(st)
	_ = json.Unmarshal(b, &stMap)
	stMap["server_version"] = Version
	stMap["rss_kb"] = rssKB()
	// Go heap diagnostics (RAM budget): how much of rss_kb is Go's.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	stMap["heap_alloc_kb"] = (ms.HeapAlloc + 1023) / 1024
	stMap["heap_sys_kb"] = (ms.HeapSys + 1023) / 1024
	stMap["heap_idle_kb"] = (ms.HeapIdle + 1023) / 1024
	writeJSON(w, httpx.StatusOK, stMap)
}

// rssKB reads VmRSS from /proc (Linux/Android); returns 0 when unavailable.
func rssKB() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb int64
			if _, err := fmt.Sscanf(line, "VmRSS: %d kB", &kb); err == nil {
				return kb
			}
		}
	}
	return 0
}

func (s *server) flush(w *httpx.Response, _ *httpx.Request) {
	if err := s.cfg.Store.Flush(); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, httpx.StatusOK, map[string]any{"flushed": true, "path": s.cfg.Store.EncPath()})
}

func (s *server) export(w *httpx.Response, _ *httpx.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="webdb-export.json"`)
	if err := s.cfg.Store.Export(w); err != nil {
		s.log.Error("export failed", "err", err)
	}
}

func (s *server) backup(w *httpx.Response, _ *httpx.Request) {
	if err := s.cfg.Store.Flush(); err != nil {
		storeErr(w, err)
		return
	}
	f, err := os.Open(s.cfg.Store.EncPath())
	if err != nil {
		writeErr(w, httpx.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="db.sqlite.enc"`)
	if st != nil {
		w.SetContentLength(st.Size())
	} else {
		writeErr(w, httpx.StatusInternalServerError, "stat failed")
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		s.log.Error("backup stream failed", "err", err)
	}
}

func (s *server) importData(w *httpx.Response, r *httpx.Request) {
	mode := r.Query().Get("mode")
	if mode == "" {
		mode = "replace"
	}
	// Import is streamed to stay inside the RAM budget, but the upload is
	// bounded by MaxImport: reject early on Content-Length, and guard
	// chunked bodies while reading (rollback keeps this all-or-nothing).
	if cl := r.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64); err == nil && n > s.cfg.MaxImport {
			writeErr(w, httpx.StatusRequestEntityTooLarge,
				fmt.Sprintf("import exceeds %d bytes", s.cfg.MaxImport))
			return
		}
	}
	lr := &limitBody{r: r.Body, left: s.cfg.MaxImport + 1, limit: s.cfg.MaxImport}
	res, err := s.cfg.Store.Import(lr, mode)
	if lr.over {
		writeErr(w, httpx.StatusRequestEntityTooLarge,
			fmt.Sprintf("import exceeds %d bytes", s.cfg.MaxImport))
		return
	}
	if err != nil {
		var tl *httpx.TooLargeError
		if errors.As(err, &tl) {
			writeErr(w, httpx.StatusRequestEntityTooLarge,
				fmt.Sprintf("import exceeds %d bytes", s.cfg.MaxImport))
			return
		}
		storeErr(w, err)
		return
	}
	writeJSON(w, httpx.StatusOK, res)
}

// limitBody reads at most `left` bytes and then fails with TooLargeError,
// so an oversized streamed body cannot be silently consumed. The `over`
// flag survives any error wrapping done by the JSON decoder.
type limitBody struct {
	r     io.Reader
	left  int64
	limit int64
	over  bool
}

func (l *limitBody) Read(p []byte) (int, error) {
	if l.left <= 0 {
		l.over = true
		return 0, &httpx.TooLargeError{Limit: l.limit}
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

func (s *server) listCollections(w *httpx.Response, _ *httpx.Request) {
	cols, err := s.cfg.Store.ListCollections()
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, httpx.StatusOK, map[string]any{"collections": cols, "count": len(cols)})
}

func (s *server) createCollection(w *httpx.Response, r *httpx.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if b := s.body(w, r); b == nil {
		return
	} else if err := json.Unmarshal(b, &req); err != nil {
		writeErr(w, httpx.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.cfg.Store.CreateCollection(req.Name); err != nil {
		storeErr(w, err)
		return
	}
	flushed := s.markBuffered(w)
	writeJSON(w, httpx.StatusCreated, map[string]any{
		"name": req.Name, "created": true, "flushed": flushed})
}

func (s *server) getCollection(w *httpx.Response, r *httpx.Request) {
	ci, err := s.cfg.Store.Collection(r.PathValue("coll"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, httpx.StatusOK, ci)
}

func (s *server) dropCollection(w *httpx.Response, r *httpx.Request) {
	if err := s.cfg.Store.DropCollection(r.PathValue("coll")); err != nil {
		storeErr(w, err)
		return
	}
	// DropCollection applies the buffer, so the response is flushed by design.
	s.markBuffered(w)
	writeJSON(w, httpx.StatusOK, map[string]any{"dropped": r.PathValue("coll"), "flushed": true})
}

func (s *server) insertDocs(w *httpx.Response, r *httpx.Request) {
	b := s.body(w, r)
	if b == nil {
		return
	}
	coll := r.PathValue("coll")
	trimmed := strings.TrimLeft(string(b), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		// Bulk insert.
		var raws []json.RawMessage
		if err := json.Unmarshal(b, &raws); err != nil {
			writeErr(w, httpx.StatusBadRequest, "invalid JSON array: "+err.Error())
			return
		}
		byteSlices := make([][]byte, len(raws))
		for i, rr := range raws {
			byteSlices[i] = rr
		}
		docs, err := s.cfg.Store.BulkInsert(coll, byteSlices)
		if err != nil {
			storeErr(w, err)
			return
		}
		flushed := s.markBuffered(w)
		writeJSON(w, httpx.StatusCreated, map[string]any{
			"inserted": len(docs), "docs": docs, "flushed": flushed})
		return
	}
	doc, err := s.cfg.Store.Insert(coll, b)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.markBuffered(w)
	writeJSON(w, httpx.StatusCreated, doc)
}

var reservedListParams = map[string]bool{
	"limit": true, "offset": true, "order": true,
}

func (s *server) listDocs(w *httpx.Response, r *httpx.Request) {
	q := r.Query()
	lo := store.ListOptions{}
	lo.Limit, _ = strconv.Atoi(q.Get("limit"))
	lo.Offset, _ = strconv.Atoi(q.Get("offset"))
	if o := q.Get("order"); o != "" {
		for _, part := range strings.Split(o, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			field, dir := part, "asc"
			if i := strings.LastIndex(part, ":"); i >= 0 {
				field, dir = part[:i], strings.ToLower(part[i+1:])
			} else if i := strings.LastIndex(part, " "); i >= 0 {
				field, dir = strings.TrimSpace(part[:i]), strings.ToLower(strings.TrimSpace(part[i+1:]))
			}
			lo.Order = append(lo.Order, store.Order{Field: field, Desc: dir == "desc"})
		}
	}
	lo.Filters = map[string]string{}
	for k, vs := range q {
		if reservedListParams[k] || len(vs) == 0 {
			continue
		}
		lo.Filters[k] = vs[0]
	}
	res, err := s.cfg.Store.List(r.PathValue("coll"), lo)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, httpx.StatusOK, res)
}

func (s *server) getDoc(w *httpx.Response, r *httpx.Request) {
	doc, err := s.cfg.Store.Get(r.PathValue("coll"), r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, httpx.StatusOK, doc)
}

func (s *server) putDoc(w *httpx.Response, r *httpx.Request) {
	b := s.body(w, r)
	if b == nil {
		return
	}
	upsert := r.Query().Get("upsert") == "true"
	var (
		doc store.Doc
		err error
	)
	if upsert {
		doc, err = s.cfg.Store.Upsert(r.PathValue("coll"), r.PathValue("id"), b)
	} else {
		doc, err = s.cfg.Store.Put(r.PathValue("coll"), r.PathValue("id"), b)
	}
	if err != nil {
		storeErr(w, err)
		return
	}
	s.markBuffered(w)
	writeJSON(w, httpx.StatusOK, doc)
}

func (s *server) patchDoc(w *httpx.Response, r *httpx.Request) {
	b := s.body(w, r)
	if b == nil {
		return
	}
	doc, err := s.cfg.Store.Patch(r.PathValue("coll"), r.PathValue("id"), b)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.markBuffered(w)
	writeJSON(w, httpx.StatusOK, doc)
}

func (s *server) deleteDoc(w *httpx.Response, r *httpx.Request) {
	if err := s.cfg.Store.Delete(r.PathValue("coll"), r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	flushed := s.markBuffered(w)
	writeJSON(w, httpx.StatusOK, map[string]any{
		"deleted": r.PathValue("id"), "flushed": flushed})
}

func (s *server) sqlQuery(w *httpx.Response, r *httpx.Request) {
	var req struct {
		SQL  string `json:"sql"`
		Args []any  `json:"args"`
	}
	b := s.body(w, r)
	if b == nil {
		return
	}
	if err := json.Unmarshal(b, &req); err != nil {
		writeErr(w, httpx.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	cols, rows, truncated, err := s.cfg.Store.SQL(req.SQL, req.Args)
	if err != nil {
		storeErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			var v any
			_ = json.Unmarshal(row[i], &v)
			m[c] = v
		}
		out = append(out, m)
	}
	writeJSON(w, httpx.StatusOK, map[string]any{
		"columns":   cols,
		"rows":      out,
		"count":     len(out),
		"truncated": truncated,
	})
}

func (s *server) listIndexes(w *httpx.Response, r *httpx.Request) {
	idx, err := s.cfg.Store.ListIndexes(r.PathValue("coll"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if idx == nil {
		idx = []string{}
	}
	writeJSON(w, httpx.StatusOK, map[string]any{"indexes": idx})
}

func (s *server) createIndex(w *httpx.Response, r *httpx.Request) {
	field := r.Query().Get("field")
	if field == "" {
		var req struct {
			Field string `json:"field"`
		}
		b := s.body(w, r)
		if b == nil {
			return
		}
		if err := json.Unmarshal(b, &req); err != nil {
			writeErr(w, httpx.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		field = req.Field
	}
	if err := s.cfg.Store.CreateIndex(r.PathValue("coll"), field); err != nil {
		storeErr(w, err)
		return
	}
	flushed := s.markBuffered(w)
	writeJSON(w, httpx.StatusCreated, map[string]any{
		"field": field, "indexed": true, "flushed": flushed})
}

func (s *server) dropIndex(w *httpx.Response, r *httpx.Request) {
	if err := s.cfg.Store.DropIndex(r.PathValue("coll"), r.PathValue("field")); err != nil {
		storeErr(w, err)
		return
	}
	flushed := s.markBuffered(w)
	writeJSON(w, httpx.StatusOK, map[string]any{
		"dropped": r.PathValue("field"), "flushed": flushed})
}
