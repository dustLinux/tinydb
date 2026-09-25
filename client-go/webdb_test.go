package webdb

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServer launches bin/webdb on a free-ish port with a temp data dir.
func startServer(t *testing.T) (base, token string) {
	t.Helper()
	bin := filepath.Join("..", "bin", "webdb")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("server binary ../bin/webdb not built")
	}
	dir := t.TempDir()
	port := "18099"
	cmd := exec.Command(bin, "-addr", "127.0.0.1:"+port, "-data", dir)
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=6MiB")
	var logBuf bytes.Buffer
	cmd.Stdout = &logBuf
	cmd.Stderr = &logBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	base = "http://127.0.0.1:" + port
	// wait for /health
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := New(base, "")
		if _, _, err := c.Health(); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	tb, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("token file: %v (log: %s)", err, logBuf.String())
	}
	return base, strings.TrimSpace(string(tb))
}

func TestIntegration(t *testing.T) {
	base, token := startServer(t)
	c := New(base, token)

	// health
	st, ver, err := c.Health()
	if err != nil || st != "ok" {
		t.Fatalf("health: %q %q err=%v", st, ver, err)
	}

	// auth: wrong token -> 401 APIError
	bad := New(base, "wrong-token")
	if _, err := bad.Stats(); !IsStatus(err, 401) {
		t.Fatalf("want 401, got %v", err)
	}

	// collections
	if err := c.CreateCollection("users"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.CreateCollection("users"); !IsStatus(err, 409) {
		t.Fatalf("want 409, got %v", err)
	}
	cols, err := c.ListCollections()
	if err != nil || len(cols) != 1 || cols[0].Name != "users" {
		t.Fatalf("list collections: %+v err=%v", cols, err)
	}

	// docs CRUD
	d, err := c.Insert("users", map[string]any{"name": "alice", "age": 30})
	if err != nil || d.ID == "" {
		t.Fatalf("insert: %+v err=%v", d, err)
	}
	d2, err := c.Insert("users", map[string]any{"name": "bob", "age": 25})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("users", d.ID)
	if err != nil || got.Collection != "users" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	// list + filters + order
	lr, err := c.List("users", ListOptions{
		Limit:   10,
		Order:   "age:asc",
		Filters: map[string]string{"name": "bob"},
	})
	if err != nil || lr.Total != 1 || lr.Items[0].ID != d2.ID {
		t.Fatalf("list: total=%d items=%+v err=%v", lr.Total, lr.Items, err)
	}
	// patch
	pd, err := c.Patch("users", d.ID, map[string]any{"age": 31})
	if err != nil || !bytes.Contains(pd.Data, []byte(`"age":31`)) {
		t.Fatalf("patch: %s err=%v", pd.Data, err)
	}
	// put 404 then upsert
	if _, err := c.Put("users", "nope", map[string]any{"x": 1}); !IsStatus(err, 404) {
		t.Fatalf("want 404, got %v", err)
	}
	if _, err := c.Upsert("users", "k1", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	// get 404
	if _, err := c.Get("users", "missing"); !IsStatus(err, 404) {
		t.Fatalf("want 404, got %v", err)
	}
	// delete
	if err := c.Delete("users", "k1"); err != nil {
		t.Fatal(err)
	}

	// bulk
	docs := make([]any, 0, 5)
	for i := 0; i < 5; i++ {
		docs = append(docs, map[string]any{"n": i})
	}
	ins, err := c.BulkInsert("users", docs)
	if err != nil || len(ins) != 5 {
		t.Fatalf("bulk: %d err=%v", len(ins), err)
	}

	// sql
	qr, err := c.SQL("SELECT COUNT(*) AS n FROM docs WHERE collection = ?", "users")
	if err != nil || len(qr.Rows) != 1 {
		t.Fatalf("sql: %+v err=%v", qr, err)
	}
	if n, _ := qr.Rows[0]["n"].(float64); int(n) != 7 { // alice+bob+5
		t.Fatalf("sql count = %v", qr.Rows[0]["n"])
	}

	// sql write forbidden
	if _, err := c.SQL("DELETE FROM docs"); !IsStatus(err, 400) {
		t.Fatalf("want 400 for write sql, got %v", err)
	}

	// indexes
	if err := c.CreateIndex("users", "name"); err != nil {
		t.Fatal(err)
	}
	idxs, err := c.ListIndexes("users")
	if err != nil || len(idxs) != 1 || idxs[0] != "ix_users_name" {
		t.Fatalf("indexes: %v err=%v", idxs, err)
	}
	if err := c.DropIndex("users", "name"); err != nil {
		t.Fatal(err)
	}

	// export/import roundtrip
	exp, err := c.ExportBytes()
	if err != nil || !bytes.HasPrefix(exp, []byte(`{"schema_version"`)) {
		t.Fatalf("export: err=%v head=%.60s", err, exp)
	}
	ir, err := c.Import(bytes.NewReader(exp), "merge")
	if err != nil || ir.Docs == 0 {
		t.Fatalf("import: %+v err=%v", ir, err)
	}

	// stats + flush
	if err := c.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	s, err := c.Stats()
	if err != nil || s.Docs == 0 || s.Encrypted != true {
		t.Fatalf("stats: %+v err=%v", s, err)
	}

	// bulk insert large enough to be interesting (and check json validity)
	var big []map[string]any
	for i := 0; i < 300; i++ {
		big = append(big, map[string]any{"i": i, "pad": strings.Repeat("x", 500)})
	}
	bigAny := make([]any, len(big))
	for i := range big {
		bigAny[i] = big[i]
	}
	ins, err = c.BulkInsert("big", bigAny)
	if err != nil || len(ins) != 300 {
		t.Fatalf("big bulk: %d err=%v", len(ins), err)
	}
	lr, err = c.List("big", ListOptions{Limit: 500})
	if err != nil || lr.Total != 300 {
		t.Fatalf("big list: total=%d err=%v", lr.Total, err)
	}
	var _ json.RawMessage = lr.Items[0].Data
}
