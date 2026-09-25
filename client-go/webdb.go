// Package webdb is a tiny Go client for the tinydb REST API.
//
// Zero third-party dependencies (stdlib net/http only). The whole library
// adds well under 512 KiB to a consumer binary (see docs/CLIENT-GO.md for a
// measured size report).
//
// Basic usage:
//
//	c := webdb.New("http://127.0.0.1:8080", "token-from-data/token")
//	doc, err := c.Insert("users", map[string]any{"name": "alice"})
//	if err != nil { var ae *webdb.APIError; if errors.As(err, &ae) { ... } }
package webdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout is the default per-request timeout.
const DefaultTimeout = 30 * time.Second

// Client talks to a tinydb server.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New creates a client for baseURL (e.g. "http://127.0.0.1:8080").
// token may be empty only when the server runs with -no-auth.
func New(baseURL, token string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		hc:    &http.Client{Timeout: DefaultTimeout},
	}
}

// SetHTTPClient overrides the underlying *http.Client (connection pooling, etc).
func (c *Client) SetHTTPClient(hc *http.Client) { c.hc = hc }

// APIError is a non-2xx response from the server.
type APIError struct {
	Status  int    `json:"status"`
	Message string `json:"error"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("webdb: HTTP %d: %s", e.Status, e.Message)
}

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Status == status
}

// ---- generic plumbing ----

func (c *Client) do(method, path string, query url.Values, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("webdb: marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "webdb-go/1.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("webdb: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("webdb: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		ae := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, ae) != nil || ae.Message == "" {
			ae.Message = strings.TrimSpace(string(data))
		}
		return ae
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("webdb: decode response: %w (body %q)", err, truncate(data, 200))
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

func (c *Client) doRaw(method, path string, query url.Values, body io.Reader) (io.ReadCloser, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// Streaming endpoints (export/backup) may be large: no global timeout.
	hc := c.hc
	if hc.Timeout > 0 {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdb: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
		return nil, ae
	}
	return resp.Body, nil
}

// ---- types ----

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

// ListResult is a page of documents.
type ListResult struct {
	Items     []Doc `json:"items"`
	Total     int64 `json:"total"`
	Truncated bool  `json:"truncated"`
}

// Stats mirrors GET /v1/stats.
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
	// Write-behind buffer state (lazy write-back).
	BufferItems    int   `json:"buffer_items"`
	BufferBytes    int   `json:"buffer_bytes"`
	BufferMaxBytes int   `json:"buffer_max_bytes"`
	BufferApplies  int64 `json:"buffer_applies"`
	// Go heap diagnostics.
	HeapAllocKB int64 `json:"heap_alloc_kb"`
	HeapSysKB   int64 `json:"heap_sys_kb"`
	HeapIdleKB  int64 `json:"heap_idle_kb"`
}

// QueryResult is the output of POST /v1/query.
type QueryResult struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated"`
}

// ImportResult reports what an import did.
type ImportResult struct {
	Collections int    `json:"collections"`
	Docs        int    `json:"docs"`
	Mode        string `json:"mode"`
}

// ListOptions controls List.
type ListOptions struct {
	Limit   int               // 0 = server default (50)
	Offset  int               // >= 0
	Order   string            // e.g. "price:desc,name:asc"
	Filters map[string]string // top-level field equality filters
}

func (o ListOptions) values() url.Values {
	q := url.Values{}
	if o.Limit > 0 {
		q.Set("limit", fmt.Sprint(o.Limit))
	}
	if o.Offset > 0 {
		q.Set("offset", fmt.Sprint(o.Offset))
	}
	if o.Order != "" {
		q.Set("order", o.Order)
	}
	for k, v := range o.Filters {
		q.Set(k, v)
	}
	return q
}

// ---- meta ----

// Health calls GET /health (works without a token).
func (c *Client) Health() (status string, version string, err error) {
	var out struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	// Health is auth-exempt; keep the request plain for simplicity.
	req, err := http.NewRequest(http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("webdb: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", &APIError{Status: resp.StatusCode, Message: "health failed"}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Status, out.Version, nil
}

// Stats calls GET /v1/stats.
func (c *Client) Stats() (*Stats, error) {
	var s Stats
	if err := c.do(http.MethodGet, "/v1/stats", nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Flush forces buffer+snapshot flush: POST /v1/flush.
func (c *Client) Flush() error {
	return c.do(http.MethodPost, "/v1/flush", nil, nil, nil)
}

// ---- collections ----

// CreateCollection: POST /v1/collections.
func (c *Client) CreateCollection(name string) error {
	return c.do(http.MethodPost, "/v1/collections", nil,
		map[string]string{"name": name}, nil)
}

// ListCollections: GET /v1/collections.
func (c *Client) ListCollections() ([]CollectionInfo, error) {
	var out struct {
		Collections []CollectionInfo `json:"collections"`
	}
	if err := c.do(http.MethodGet, "/v1/collections", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Collections, nil
}

// GetCollection: GET /v1/collections/{name}.
func (c *Client) GetCollection(name string) (*CollectionInfo, error) {
	var ci CollectionInfo
	if err := c.do(http.MethodGet, "/v1/collections/"+url.PathEscape(name), nil, nil, &ci); err != nil {
		return nil, err
	}
	return &ci, nil
}

// DeleteCollection: DELETE /v1/collections/{name}.
func (c *Client) DeleteCollection(name string) error {
	return c.do(http.MethodDelete, "/v1/collections/"+url.PathEscape(name), nil, nil, nil)
}

// ---- documents ----

func docsPath(coll, id string) string {
	p := "/v1/collections/" + url.PathEscape(coll) + "/docs"
	if id != "" {
		p += "/" + url.PathEscape(id)
	}
	return p
}

// Insert: POST /v1/collections/{coll}/docs (single JSON object).
func (c *Client) Insert(coll string, doc any) (*Doc, error) {
	var d Doc
	if err := c.do(http.MethodPost, docsPath(coll, ""), nil, doc, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// BulkInsert: POST with a JSON array body. Returns inserted docs.
func (c *Client) BulkInsert(coll string, docs []any) ([]Doc, error) {
	var out struct {
		Inserted int   `json:"inserted"`
		Docs     []Doc `json:"docs"`
	}
	if err := c.do(http.MethodPost, docsPath(coll, ""), nil, docs, &out); err != nil {
		return nil, err
	}
	return out.Docs, nil
}

// Get: GET /v1/collections/{coll}/docs/{id}.
func (c *Client) Get(coll, id string) (*Doc, error) {
	var d Doc
	if err := c.do(http.MethodGet, docsPath(coll, id), nil, nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// List: GET /v1/collections/{coll}/docs with options.
func (c *Client) List(coll string, opts ListOptions) (*ListResult, error) {
	var lr ListResult
	if err := c.do(http.MethodGet, docsPath(coll, ""), opts.values(), nil, &lr); err != nil {
		return nil, err
	}
	return &lr, nil
}

// Put replaces a document (404 if missing): PUT.
func (c *Client) Put(coll, id string, doc any) (*Doc, error) {
	var d Doc
	if err := c.do(http.MethodPut, docsPath(coll, id), nil, doc, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Upsert writes a document, creating it if missing (PUT ?upsert=true).
func (c *Client) Upsert(coll, id string, doc any) (*Doc, error) {
	var d Doc
	q := url.Values{"upsert": {"true"}}
	if err := c.do(http.MethodPut, docsPath(coll, id), q, doc, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Patch merges top-level keys into a document: PATCH.
func (c *Client) Patch(coll, id string, doc any) (*Doc, error) {
	var d Doc
	if err := c.do(http.MethodPatch, docsPath(coll, id), nil, doc, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Delete removes a document: DELETE.
func (c *Client) Delete(coll, id string) error {
	return c.do(http.MethodDelete, docsPath(coll, id), nil, nil, nil)
}

// ---- SQL ----

// SQL runs a read-only query: POST /v1/query {"sql":..., "args":[...]}.
func (c *Client) SQL(query string, args ...any) (*QueryResult, error) {
	body := map[string]any{"sql": query}
	if len(args) > 0 {
		body["args"] = args
	}
	var qr QueryResult
	if err := c.do(http.MethodPost, "/v1/query", nil, body, &qr); err != nil {
		return nil, err
	}
	return &qr, nil
}

// ---- indexes ----

func idxPath(coll string) string { return "/v1/collections/" + url.PathEscape(coll) + "/indexes" }

// CreateIndex: POST /v1/collections/{coll}/indexes?field=f.
func (c *Client) CreateIndex(coll, field string) error {
	q := url.Values{"field": {field}}
	return c.do(http.MethodPost, idxPath(coll), q, nil, nil)
}

// ListIndexes: GET /v1/collections/{coll}/indexes.
func (c *Client) ListIndexes(coll string) ([]string, error) {
	var out struct {
		Indexes []string `json:"indexes"`
	}
	if err := c.do(http.MethodGet, idxPath(coll), nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Indexes, nil
}

// DropIndex: DELETE /v1/collections/{coll}/indexes/{field}.
func (c *Client) DropIndex(coll, field string) error {
	return c.do(http.MethodDelete, idxPath(coll)+"/"+url.PathEscape(field), nil, nil, nil)
}

// ---- backup / export / import ----

// Export streams a JSON export (caller closes the reader).
func (c *Client) Export() (io.ReadCloser, error) {
	return c.doRaw(http.MethodGet, "/v1/export", nil, nil)
}

// ExportBytes reads the whole export into memory (small DBs).
func (c *Client) ExportBytes() ([]byte, error) {
	rc, err := c.Export()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Backup streams the encrypted snapshot (caller closes the reader).
func (c *Client) Backup() (io.ReadCloser, error) {
	return c.doRaw(http.MethodGet, "/v1/backup", nil, nil)
}

// Import restores an export from r. mode = "replace" (default) or "merge".
func (c *Client) Import(r io.Reader, mode string) (*ImportResult, error) {
	if mode == "" {
		mode = "replace"
	}
	q := url.Values{"mode": {mode}}
	// Body must be JSON passthrough: use raw request with unknown length.
	u := c.base + "/v1/import?" + q.Encode()
	req, err := http.NewRequest(http.MethodPost, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdb: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		ae := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, ae)
		if ae.Message == "" {
			ae.Message = string(data)
		}
		return nil, ae
	}
	var ir ImportResult
	if err := json.Unmarshal(data, &ir); err != nil {
		return nil, err
	}
	return &ir, nil
}
