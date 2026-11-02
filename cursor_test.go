package tacdb

import (
	"sync"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

// hubWith creates a node with n self-loops and returns it.
func hubWith(t *testing.T, db *DB, n int) NodeID {
	t.Helper()
	var hub NodeID
	err := db.Update(bg, func(tx *Tx) error {
		h, err := tx.CreateNode(CreateNodeOptions{})
		if err != nil {
			return err
		}
		hub = h.ID
		for range n {
			if _, err := tx.CreateEdge(hub, hub, "L", nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

// A cursor whose read transaction ended fails with TX_CLOSED, even inside
// a batch it already read (SPEC §6: calls after the end return a stable
// error).
func TestCursorAfterReadTxEnd(t *testing.T) {
	db := memory(t)
	hub := hubWith(t, db, 3)
	tx, _ := db.BeginRead(bg)
	c, err := tx.Edges(hub, EdgeScanOptions{Direction: Outgoing})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Next() {
		t.Fatal(c.Err())
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for c.Next() {
		n++
	}
	if n != 0 || code(c.Err()) != CodeTxClosed {
		t.Fatalf("after Rollback the cursor gave %d more edges, then %v; want 0, then TX_CLOSED", n, c.Err())
	}
}

// After a write transaction rolls back, its cursor gives none of the edges
// that never committed.
func TestCursorAfterWriteRollback(t *testing.T) {
	db := memory(t)
	tx, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := tx.CreateNode(CreateNodeOptions{})
	for range 3 {
		if _, err := tx.CreateEdge(a.ID, a.ID, "GHOST", nil); err != nil {
			t.Fatal(err)
		}
	}
	c, err := tx.Edges(a.ID, EdgeScanOptions{Direction: Outgoing})
	if err != nil {
		t.Fatal(err)
	}
	c.Next()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var ghosts []EdgeRef
	for c.Next() {
		ghosts = append(ghosts, c.Edge())
	}
	if len(ghosts) != 0 || code(c.Err()) != CodeTxClosed {
		t.Fatalf("after Rollback the cursor gave rolled-back edges %v, then %v; want TX_CLOSED", ghosts, c.Err())
	}
}

// Closing a cursor while another goroutine is inside Next gives TX_IN_USE
// or closes it, never a data race (run with -race).
func TestCursorCloseRace(t *testing.T) {
	db := memory(t)
	hub := hubWith(t, db, 2000)
	tx, _ := db.BeginRead(bg)
	defer tx.Rollback()
	c, err := tx.Edges(hub, EdgeScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for c.Next() {
		}
	}()
	for c.Close() != nil {
	}
	wg.Wait()
}

// A call that fn left running when it returned ends before Update
// commits; the later ones get TX_IN_USE or TX_CLOSED, never a data race
// (run with -race).
func TestUpdateWaitsForLeftCalls(t *testing.T) {
	db := memory(t)
	hub := hubWith(t, db, 1)
	var wg sync.WaitGroup
	start := make(chan struct{})
	err := db.Update(bg, func(tx *Tx) error {
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(start)
			for range 200 {
				if err := tx.SetProperty(hub, "p", value.Int(1)); err != nil && code(err) != CodeTxInUse && code(err) != CodeTxClosed {
					t.Error(err)
				}
			}
		}()
		<-start
		return nil
	})
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
}

// In a write transaction, a cursor gives the edges that existed when it
// opened, whatever the batch size: a loop that adds one edge per edge
// visited ends after those.
func TestCursorIgnoresNewEdges(t *testing.T) {
	db := memory(t)
	for _, n := range []int{3, 65} {
		tx, _ := db.BeginWrite(bg)
		hub, _ := tx.CreateNode(CreateNodeOptions{})
		for range n {
			tx.CreateEdge(hub.ID, hub.ID, "L", nil)
		}
		c, _ := tx.Edges(hub.ID, EdgeScanOptions{Direction: Outgoing})
		seen := 0
		for c.Next() && seen < 5000 {
			seen++
			if _, err := tx.CreateEdge(hub.ID, hub.ID, "L", nil); err != nil {
				t.Fatal(err)
			}
		}
		tx.Rollback()
		if seen != n {
			t.Errorf("%d edges at the start: the loop visited %d", n, seen)
		}
	}
}

// In a write transaction, an edge deleted after the cursor read its batch
// does not come out of it.
func TestCursorSkipsDeletedEdges(t *testing.T) {
	db := memory(t)
	tx, _ := db.BeginWrite(bg)
	defer tx.Rollback()
	hub, _ := tx.CreateNode(CreateNodeOptions{})
	var ids []EdgeID
	for range 10 {
		e, _ := tx.CreateEdge(hub.ID, hub.ID, "L", nil)
		ids = append(ids, e.ID)
	}
	c, _ := tx.Edges(hub.ID, EdgeScanOptions{Direction: Outgoing})
	c.Next()
	for _, id := range ids {
		if err := tx.DeleteEdge(id); err != nil {
			t.Fatal(err)
		}
	}
	var ghosts []EdgeID
	for c.Next() {
		ghosts = append(ghosts, c.Edge().ID)
	}
	if len(ghosts) > 0 {
		_, err := tx.GetEdge(ghosts[0])
		t.Fatalf("deleted edges came out of the cursor: %v; GetEdge on the first: %v", ghosts, err)
	}
}

// A TX_IN_USE collision ends one Next; once the other call is over, the
// cursor goes on.
func TestCursorAfterCollision(t *testing.T) {
	db := memory(t)
	hub := hubWith(t, db, 3)
	tx, _ := db.BeginRead(bg)
	defer tx.Rollback()
	c, _ := tx.Edges(hub, EdgeScanOptions{Direction: Outgoing})
	tx.busy.Store(true)
	if c.Next() || code(c.Err()) != CodeTxInUse {
		t.Fatalf("a collision: %v", c.Err())
	}
	if err := c.Close(); code(err) != CodeTxInUse {
		t.Fatalf("Close during a collision: %v", err)
	}
	tx.busy.Store(false)
	n := 0
	for c.Next() {
		n++
	}
	if n != 3 || c.Err() != nil {
		t.Fatalf("after the collision: %d edges, %v", n, c.Err())
	}
}

// Close never returns nil while a transaction that began during it stays
// open: either the transaction is refused, or Close returns BUSY and the
// handle keeps working.
func TestCloseRacesBegin(t *testing.T) {
	for i := range 300 {
		db, err := OpenMemory(options())
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var txs []*Tx
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if tx, err := db.BeginRead(bg); err == nil {
					mu.Lock()
					txs = append(txs, tx)
					mu.Unlock()
				} else if c := code(err); c != CodeClosed && c != CodeShuttingDown {
					t.Error(err)
				}
			}()
		}
		close(start)
		cerr := db.Close()
		wg.Wait()
		switch {
		case cerr == nil && len(txs) > 0:
			t.Fatalf("iteration %d: Close returned nil with %d transactions open", i, len(txs))
		case cerr != nil && code(cerr) != CodeBusy:
			t.Fatalf("iteration %d: %v", i, cerr)
		case cerr != nil && len(txs) > 0: // a refused Begin may also be counted for a moment
			if _, err := txs[0].GetNode(1); code(err) != CodeNotFound {
				t.Fatalf("iteration %d: a reader after BUSY: %v", i, err)
			}
		}
		for _, tx := range txs {
			tx.Rollback()
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
