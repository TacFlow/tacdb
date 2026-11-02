package tacdb

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

var bg = context.Background()

var errFail = errors.New("the function failed")

// options are the defaults with small log segments, which a memory
// database allocates in full.
func options() Options {
	o := DefaultOptions()
	o.WALSegmentBytes = 256 << 10
	o.CacheBytes = 1 << 20
	return o
}

func memory(t *testing.T) *DB {
	t.Helper()
	db, err := OpenMemory(options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dumpOf returns the canonical dump of the latest commit.
func dumpOf(t *testing.T, db *DB) string {
	t.Helper()
	var b bytes.Buffer
	err := db.View(bg, func(tx *Tx) error { return db.g.Dump(bg, tx.r, &b) })
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func code(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// A file database keeps the graph across Close and Open.
func TestOpenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.tacdb")
	db, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var a Node
	var e Edge
	err = db.Update(bg, func(tx *Tx) error {
		var err error
		if a, err = tx.CreateNode(CreateNodeOptions{Labels: []string{"Person", "Person", "Admin"},
			Properties: map[string]value.Value{"name": value.String("ana")}}); err != nil {
			return err
		}
		b, err := tx.CreateNode(CreateNodeOptions{})
		if err != nil {
			return err
		}
		e, err = tx.CreateEdge(a.ID, b.ID, "KNOWS", map[string]value.Value{"since": value.Int(2020)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.Labels, []string{"Person", "Admin"}) || e.From != a.ID || e.Type != "KNOWS" {
		t.Fatalf("returned %+v %+v", a, e)
	}
	before := dumpOf(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.Create = false
	db, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if after := dumpOf(t, db); after != before {
		t.Fatalf("before:\n%s\nafter:\n%s", before, after)
	}
	err = db.View(bg, func(tx *Tx) error {
		n, err := tx.GetNode(a.ID)
		if err != nil || !slices.Equal(n.Labels, a.Labels) || !value.Identical(n.Properties["name"], value.String("ana")) {
			t.Fatalf("GetNode: %+v, %v", n, err)
		}
		got, err := tx.GetEdge(e.ID)
		if err != nil || got.From != e.From || got.To != e.To || got.Type != "KNOWS" || len(got.Properties) != 1 {
			t.Fatalf("GetEdge: %+v, %v", got, err)
		}
		v, ok, err := tx.GetEdgeProperty(e.ID, "since")
		if !ok || err != nil || !value.Identical(v, value.Int(2020)) {
			t.Fatalf("GetEdgeProperty: %v %v %v", v, ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.tacdb"), opts); code(err) != CodeNotFound {
		t.Fatalf("Open without Create: %v", err)
	}
}

// Update commits only when fn returns nil; an error or a panic undoes it,
// and the panic goes on. Inside View and Update the transaction cannot be
// ended by fn.
func TestUpdate(t *testing.T) {
	db := memory(t)
	create := func(tx *Tx) error { _, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"X"}}); return err }
	if err := db.Update(bg, func(tx *Tx) error { create(tx); return errFail }); err != errFail {
		t.Fatalf("an error of fn: %v", err)
	}
	func() {
		defer func() {
			if recover() != "boom" {
				t.Fatal("the panic did not go on")
			}
		}()
		db.Update(bg, func(tx *Tx) error { create(tx); panic("boom") })
	}()
	if d := dumpOf(t, db); d != "" {
		t.Fatalf("a failed Update left %q", d)
	}
	err := db.Update(bg, func(tx *Tx) error {
		if _, err := tx.Commit(bg); code(err) != CodeInvalidArgument {
			t.Fatalf("Commit inside Update: %v", err)
		}
		if err := tx.Rollback(); code(err) != CodeInvalidArgument {
			t.Fatalf("Rollback inside Update: %v", err)
		}
		return create(tx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := dumpOf(t, db); d != "node 3 [\"X\"] {}\n" {
		t.Fatalf("after Update: %q", d)
	}
	db.View(bg, func(tx *Tx) error {
		if _, err := tx.Commit(bg); code(err) != CodeInvalidArgument {
			t.Fatalf("Commit inside View: %v", err)
		}
		if err := tx.SetProperty(3, "p", value.Int(1)); code(err) != CodeReadOnly {
			t.Fatalf("a write inside View: %v", err)
		}
		return nil
	})
}

// A read transaction refuses writes, commits with a zero token and is
// closed after; a write transaction gives a token the handle knows.
func TestTransactions(t *testing.T) {
	db := memory(t)
	w, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	if w.ReadOnly() || w.Snapshot() != 0 {
		t.Fatalf("write transaction: read-only %v snapshot %d", w.ReadOnly(), w.Snapshot())
	}
	n, _ := w.CreateNode(CreateNodeOptions{})
	rc, err := w.Commit(bg)
	if err != nil || rc.ID != 1 || rc.Token == (CommitToken{}) || !rc.Durable {
		t.Fatalf("commit: %+v, %v", rc, err)
	}
	if st, err := db.CommitStatus(rc.Token); err != nil || st != CommitDurable {
		t.Fatalf("CommitStatus: %v, %v", st, err)
	}
	if _, err := w.CreateNode(CreateNodeOptions{}); code(err) != CodeTxClosed {
		t.Fatalf("a committed transaction: %v", err)
	}
	r, _ := db.BeginRead(bg)
	if !r.ReadOnly() || r.Snapshot() != 1 {
		t.Fatalf("read transaction: read-only %v snapshot %d", r.ReadOnly(), r.Snapshot())
	}
	if _, err := r.CreateNode(CreateNodeOptions{}); code(err) != CodeReadOnly {
		t.Fatalf("a write in a read transaction: %v", err)
	}
	// A later commit stays out of the snapshot.
	db.Update(bg, func(tx *Tx) error { return tx.SetProperty(n.ID, "p", value.Int(1)) })
	if _, ok, err := r.GetProperty(n.ID, "p"); ok || err != nil {
		t.Fatalf("a read transaction sees a later commit: %v %v", ok, err)
	}
	rc, err = r.Commit(bg)
	if err != nil || rc.ID != 1 || rc.Token != (CommitToken{}) {
		t.Fatalf("commit of a read transaction: %+v, %v", rc, err)
	}
	if _, err := r.Commit(bg); code(err) != CodeTxClosed {
		t.Fatalf("a second commit: %v", err)
	}
	if err := r.Rollback(); err != nil {
		t.Fatalf("Rollback after Commit: %v", err)
	}
	if _, err := r.GetNode(n.ID); code(err) != CodeTxClosed {
		t.Fatalf("a closed read transaction: %v", err)
	}
	if _, err := db.TryBeginWrite(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TryBeginWrite(bg); code(err) != CodeDBBusy {
		t.Fatalf("TryBeginWrite while a writer holds the slot: %v", err)
	}
}

// A call while another call of the same transaction runs fails with
// TX_IN_USE, without a data race.
func TestTxInUse(t *testing.T) {
	db := memory(t)
	var id NodeID
	db.Update(bg, func(tx *Tx) error {
		n, err := tx.CreateNode(CreateNodeOptions{Properties: map[string]value.Value{"p": value.Int(1)}})
		id = n.ID
		return err
	})
	tx, _ := db.BeginRead(bg)
	defer tx.Rollback()
	tx.busy.Store(true)
	if _, err := tx.GetNode(id); code(err) != CodeTxInUse {
		t.Fatalf("a busy transaction: %v", err)
	}
	c := &EdgeCursor{tx: tx}
	if c.Next() || code(c.Err()) != CodeTxInUse {
		t.Fatalf("a cursor of a busy transaction: %v", c.Err())
	}
	tx.busy.Store(false)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[Code]int{}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, _, err := tx.GetProperty(id, "p")
				mu.Lock()
				seen[code(err)]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) > 2 || seen[""] == 0 || (len(seen) == 2 && seen[CodeTxInUse] == 0) {
		t.Fatalf("outcomes %v", seen)
	}
}

// Close with a transaction open fails with BUSY and keeps the handle;
// once it succeeds, it is idempotent and the handle is closed.
func TestClose(t *testing.T) {
	db, err := OpenMemory(options())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(bg)
	var e *Error
	if err := db.Close(); code(err) != CodeBusy || !errors.As(err, &e) || e.Details["active"] != 1 {
		t.Fatalf("Close with a transaction open: %v", err)
	}
	if _, err := r.GetNode(1); code(err) != CodeNotFound {
		t.Fatalf("the handle closed under its reader: %v", err)
	}
	r.Rollback()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if _, err := db.BeginRead(bg); code(err) != CodeClosed {
		t.Fatalf("BeginRead after Close: %v", err)
	}
	if _, err := db.CommitStatus(CommitToken{}); code(err) != CodeClosed {
		t.Fatalf("CommitStatus after Close: %v", err)
	}
}

func TestOptions(t *testing.T) {
	for name, change := range map[string]func(o *Options){
		"symbols":    func(o *Options) { o.MaxSymbolsPerNamespace = 0 },
		"labels":     func(o *Options) { o.MaxLabelsPerNode = 0 },
		"properties": func(o *Options) { o.MaxPropertiesPerEntity = -1 },
		"values":     func(o *Options) { o.ValueLimits.MaxDepth = 0 },
		"engine":     func(o *Options) { o.MaxTxBytes = 0 },
	} {
		o := options()
		change(&o)
		if _, err := OpenMemory(o); code(err) != CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	o := options()
	o.MaxTxBytes = 64 << 10
	small, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	err = small.Update(bg, func(tx *Tx) error {
		_, err := tx.CreateNode(CreateNodeOptions{Properties: map[string]value.Value{"big": value.Bytes(make([]byte, 100<<10))}})
		return err
	})
	if code(err) != CodeLimitExceeded {
		t.Fatalf("MaxTxBytes: %v", err)
	}
	small.Close()
	o = options()
	o.MaxLabelsPerNode = 1
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(bg, func(tx *Tx) error {
		_, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"A", "B"}})
		return err
	})
	if code(err) != CodeLimitExceeded {
		t.Fatalf("MaxLabelsPerNode: %v", err)
	}
}

// Values given and returned belong to their owners: changing the caller's
// map after the call changes nothing stored, nor what was returned.
func TestOwnership(t *testing.T) {
	db := memory(t)
	props := map[string]value.Value{"a": value.Int(1)}
	labels := []string{"L"}
	var n Node
	db.Update(bg, func(tx *Tx) error {
		var err error
		n, err = tx.CreateNode(CreateNodeOptions{Labels: labels, Properties: props})
		return err
	})
	props["a"], props["b"], labels[0] = value.Int(2), value.Int(3), "M"
	if len(n.Properties) != 1 || !value.Identical(n.Properties["a"], value.Int(1)) || n.Labels[0] != "L" {
		t.Fatalf("the returned node follows the caller: %+v", n)
	}
	db.View(bg, func(tx *Tx) error {
		got, _ := tx.GetNode(n.ID)
		if len(got.Properties) != 1 || got.Labels[0] != "L" {
			t.Fatalf("stored %+v", got)
		}
		return nil
	})
}

// The public cursor walks the edges of a node in the directions asked and
// ends with its transaction.
func TestEdgeCursor(t *testing.T) {
	db := memory(t)
	var a, b Node
	var loop, out Edge
	db.Update(bg, func(tx *Tx) error {
		a, _ = tx.CreateNode(CreateNodeOptions{})
		b, _ = tx.CreateNode(CreateNodeOptions{})
		out, _ = tx.CreateEdge(a.ID, b.ID, "R", nil)
		loop, _ = tx.CreateEdge(a.ID, a.ID, "S", nil)
		_, err := tx.CreateEdge(b.ID, a.ID, "R", nil)
		return err
	})
	tx, _ := db.BeginRead(bg)
	defer tx.Rollback()
	walk := func(o EdgeScanOptions) []EdgeRef {
		c, err := tx.Edges(a.ID, o)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var out []EdgeRef
		for c.Next() {
			out = append(out, c.Edge())
		}
		if c.Err() != nil {
			t.Fatal(c.Err())
		}
		return out
	}
	if got := walk(EdgeScanOptions{}); len(got) != 3 {
		t.Fatalf("Both: %v", got)
	}
	if got := walk(EdgeScanOptions{Direction: Outgoing, Types: []string{"R"}}); !slices.Equal(got, []EdgeRef{{out.ID, a.ID, b.ID, "R"}}) {
		t.Fatalf("Outgoing R: %v", got)
	}
	if got := walk(EdgeScanOptions{Direction: Incoming, Types: []string{"S"}}); !slices.Equal(got, []EdgeRef{{loop.ID, a.ID, a.ID, "S"}}) {
		t.Fatalf("Incoming S: %v", got)
	}
	if _, err := tx.Edges(a.ID, EdgeScanOptions{Direction: 9}); code(err) != CodeInvalidArgument {
		t.Fatalf("a bad direction: %v", err)
	}
	if _, err := tx.Edges(99, EdgeScanOptions{}); code(err) != CodeNotFound {
		t.Fatalf("Edges of a missing node: %v", err)
	}
	c, _ := tx.Edges(a.ID, EdgeScanOptions{})
	tx.Rollback()
	if c.Next() || code(c.Err()) != CodeTxClosed {
		t.Fatalf("a cursor after its transaction: %v", c.Err())
	}
	err := db.Update(bg, func(tx *Tx) error {
		if err := tx.DeleteNode(a.ID, false); code(err) != CodeInvalidArgument {
			t.Fatalf("DeleteNode with edges: %v", err)
		}
		if err := tx.DeleteEdge(out.ID); err != nil {
			return err
		}
		if err := tx.RemoveEdgeProperty(loop.ID, "x"); err != nil {
			return err
		}
		if err := tx.SetEdgeProperty(loop.ID, "x", value.Null()); err != nil {
			return err
		}
		if v, ok, err := tx.GetEdgeProperty(loop.ID, "x"); !ok || err != nil || !v.IsNull() {
			t.Fatalf("a Null edge property: %v %v %v", v, ok, err)
		}
		if err := tx.AddLabel(a.ID, "L"); err != nil {
			return err
		}
		if err := tx.RemoveLabel(a.ID, "L"); err != nil {
			return err
		}
		if err := tx.RemoveProperty(a.ID, "x"); err != nil {
			return err
		}
		return tx.DeleteNode(a.ID, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := dumpOf(t, db); d != "node 2 [] {}\n" {
		t.Fatalf("after detach: %q", d)
	}
}

// AUD-01: ending read transactions, and write transactions that wrote
// nothing, never appends to the log nor syncs.
func TestReadsNeverSync(t *testing.T) {
	db := memory(t)
	db.Update(bg, func(tx *Tx) error { _, err := tx.CreateNode(CreateNodeOptions{}); return err })
	before := db.e.Stats()
	for i := range 200 {
		switch i % 4 {
		case 0:
			tx, _ := db.BeginRead(bg)
			tx.GetNode(1)
			tx.Commit(bg)
		case 1:
			tx, _ := db.BeginReadDurable(bg)
			tx.Rollback()
		case 2:
			db.View(bg, func(tx *Tx) error { _, err := tx.GetNode(1); return err })
		default:
			if err := db.Update(bg, func(tx *Tx) error { _, err := tx.GetNode(1); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if after := db.e.Stats(); after.Appends != before.Appends || after.Syncs != before.Syncs {
		t.Fatalf("200 reads: appends %d -> %d, syncs %d -> %d", before.Appends, after.Appends, before.Syncs, after.Syncs)
	}
}

// AUD-05: the transaction past MaxActiveTransactions fails with
// TOO_MANY_TRANSACTIONS and its details, a limit above 1024 holds, and
// the count goes back to zero.
func TestTransactionLimit(t *testing.T) {
	for _, limit := range []int{1024, 1500} {
		o := options()
		o.MaxActiveTransactions = limit
		db, err := OpenMemory(o)
		if err != nil {
			t.Fatal(err)
		}
		var txs []*Tx
		for range limit {
			tx, err := db.BeginRead(bg)
			if err != nil {
				t.Fatalf("limit %d: transaction %d: %v", limit, len(txs)+1, err)
			}
			txs = append(txs, tx)
		}
		var e *Error
		_, err = db.BeginRead(bg)
		if code(err) != CodeTooManyTransactions || !errors.As(err, &e) || e.Details["limit"] != int64(limit) {
			t.Fatalf("transaction %d: %v", limit+1, err)
		}
		if _, err := db.BeginWrite(bg); code(err) != CodeTooManyTransactions {
			t.Fatalf("a writer past the limit: %v", err)
		}
		for _, tx := range txs {
			tx.Rollback()
		}
		if n := db.e.Active(); n != 0 {
			t.Fatalf("%d transactions counted after closing all", n)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
