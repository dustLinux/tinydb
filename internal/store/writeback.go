// Write-back (write-behind) buffer.
//
// Mutations (Insert/Put/Patch/Delete/CreateCollection/...) are applied to an
// in-RAM overlay first and land in SQLite lazily, in batches. This is the
// "сначала в RAM, потом запись" processing mode:
//
//   - mutations return immediately after touching the overlay;
//   - the overlay is applied to SQLite by: the background writeback ticker,
//     read barriers (List/SQL/Export/... must see the pending writes),
//     buffer limits (-buffer-bytes / -buffer-items), POST /v1/flush,
//     autosave ticks and graceful shutdown;
//   - Get() reads the overlay before falling back to SQLite, so a client
//     always sees its own writes;
//   - the overlay is bounded (1 MiB / 10000 ops by default); on overflow the
//     buffer is applied synchronously.
//
// Durability: a SIGKILL can lose writes that are still in the overlay (up to
// -writeback interval). Graceful shutdown (SIGINT/SIGTERM) drains the buffer.
package store

import (
	"fmt"
	"time"
)

type opKind uint8

const (
	opPutDoc     opKind = iota // upsert a document
	opDelDoc                   // delete a document
	opCreateColl               // create a collection
	opCreateIdx                // create an expression index
	opDropIdx                  // drop an expression index
)

// pendingOp is one queued mutation. Ops are applied in FIFO order.
type pendingOp struct {
	kind    opKind
	coll    string // collection name
	id      string // document id / index field
	data    string // JSON document body (opPutDoc)
	created int64  // document or collection creation time (ms)
	updated int64  // last modification time (ms)
}

func (op pendingOp) key() string {
	return string(rune('0'+op.kind)) + "\x00" + op.coll + "\x00" + op.id
}

func (op pendingOp) size() int {
	return len(op.data) + len(op.coll) + len(op.id) + 96
}

// enqueue appends an op to the write-behind buffer. s.mu must be held.
// Applies the buffer synchronously when a limit is exceeded (apply errors are
// logged and retried on the next barrier; the op stays buffered).
func (s *Store) enqueue(op *pendingOp) {
	s.pend = append(s.pend, op)
	s.pendIdx[op.key()] = len(s.pend) - 1
	s.pendBytes += op.size()
	s.pendN.Store(int32(len(s.pend)))
	s.dirty.Store(true)
	if len(s.pend) >= s.cfg.BufferItems || s.pendBytes >= s.cfg.BufferBytes {
		if err := s.applyLocked(); err != nil {
			s.log.Error("write-back apply (buffer full)", "err", err)
		}
	}
}

// applyLocked applies every buffered op to SQLite in one transaction.
// s.mu must be held. On error the buffer is kept for a later retry.
func (s *Store) applyLocked() error {
	if len(s.pend) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback()
	for _, op := range s.pend {
		switch op.kind {
		case opPutDoc:
			if err := ensureCollection(tx, op.coll); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO docs(collection,id,data,created,updated) VALUES(?,?,?,?,?)
				ON CONFLICT(collection,id) DO UPDATE SET data=excluded.data, updated=excluded.updated`,
				op.coll, op.id, op.data, op.created, op.updated)
			if err != nil {
				return mapErr(err)
			}
		case opDelDoc:
			if _, err := tx.Exec(`DELETE FROM docs WHERE collection=? AND id=?`,
				op.coll, op.id); err != nil {
				return mapErr(err)
			}
		case opCreateColl:
			_, err := tx.Exec(`INSERT OR IGNORE INTO _collections(name,created) VALUES(?,?)`,
				op.coll, op.created)
			if err != nil {
				return mapErr(err)
			}
		case opCreateIdx:
			stmt := fmt.Sprintf(
				`CREATE INDEX IF NOT EXISTS %s ON docs(collection, json_extract(data, '$.%s'))`,
				indexName(op.coll, op.id), op.id)
			if _, err := tx.Exec(stmt); err != nil {
				return mapErr(err)
			}
		case opDropIdx:
			if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + indexName(op.coll, op.id)); err != nil {
				return mapErr(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return mapErr(err)
	}
	s.pend = s.pend[:0]
	s.pendIdx = make(map[string]int)
	s.pendBytes = 0
	s.pendN.Store(0)
	s.applies.Add(1)
	s.dirty.Store(true)
	return nil
}

// ApplyBuffer drains the write-behind buffer into SQLite. Read endpoints call
// it automatically (read barrier); POST /v1/flush and the ticker call it too.
func (s *Store) ApplyBuffer() error {
	if s.pendN.Load() == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked()
}

// Writeback periodically applies the buffer until stop is closed.
func (s *Store) Writeback(every time.Duration, stop <-chan struct{}) {
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
			if err := s.ApplyBuffer(); err != nil {
				s.log.Error("write-back apply", "err", err)
			}
		}
	}
}

// Buffered reports how many ops are waiting in the write-behind buffer.
func (s *Store) Buffered() int { return int(s.pendN.Load()) }

// lookupDocLocked resolves a document against the overlay first, then SQLite.
// s.mu must be held (read or write).
func (s *Store) lookupDocLocked(collection, id string) (Doc, bool) {
	putKey := pendingOp{kind: opPutDoc, coll: collection, id: id}.key()
	delKey := pendingOp{kind: opDelDoc, coll: collection, id: id}.key()
	pi, hasPut := s.pendIdx[putKey]
	di, hasDel := s.pendIdx[delKey]
	// The op with the larger position is the later mutation (FIFO order).
	switch {
	case hasDel && (!hasPut || di > pi):
		return Doc{}, false
	case hasPut:
		op := s.pend[pi]
		return Doc{ID: id, Collection: collection, Created: op.created,
			Updated: op.updated, Data: []byte(op.data)}, true
	}
	var d Doc
	var data string
	err := s.db.QueryRow(
		`SELECT id, data, created, updated FROM docs WHERE collection=? AND id=?`,
		collection, id).Scan(&d.ID, &data, &d.Created, &d.Updated)
	if err != nil {
		return Doc{}, false
	}
	d.Collection = collection
	d.Data = []byte(data)
	return d, true
}
