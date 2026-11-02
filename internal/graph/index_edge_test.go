package graph

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// A backfill over MaxTxBytes with fewer entities than one periodic check
// still fails with the CreateIndex error that points to BuildIndex (SPEC
// §12.2).
func TestCreateIndexTooLargeFewEntities(t *testing.T) {
	o := options()
	o.MaxTxBytes = 256 << 10
	db := openDB(t, vfs.NewMemFS(), o)
	defer db.Close()
	g := newGraph()
	for i := range 200 {
		tx := begin(t, db)
		b := []byte(strings.Repeat("x", 3000))
		b[0] = byte('a' + i%26)
		b[1] = byte('a' + i/26)
		node(t, g, tx, []string{"L"}, props("s", value.String(string(b))))
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	g.IndexPrefixBytes = 980
	tx := begin(t, db)
	defer tx.Rollback()
	err := tx.Statement("t", func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "s", Scope: "L", Property: "s"})
	})
	var e *errs.Error
	if errs.CodeOf(err) != errs.CodeLimitExceeded || !errors.As(err, &e) {
		t.Fatalf("a creation beyond MaxTxBytes: %v", err)
	}
	if !strings.Contains(e.Message, "BuildIndex") {
		t.Fatalf("LIMIT_EXCEEDED without the BuildIndex hint: %v", err)
	}
}

// An index must not make a node undeletable: deleting a node, or setting
// or removing an indexed property, works whatever the handle's ValueLimits
// are, as it does without the index.
func TestIndexedValueBeyondHandleLimits(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openDB(t, fs, options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "i", Scope: "A", Property: "p"})
	})
	big := value.String(strings.Repeat("v", 5000))
	a := node(t, g, tx, []string{"A"}, props("p", big))
	b := node(t, g, tx, []string{"B"}, props("p", big)) // same value, no index
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A handle that accepts smaller values now (say, after a reopen with
	// tighter ValueLimits).
	g2 := newGraph()
	g2.Limits.MaxValueBytes = 1024
	tx = begin(t, db)
	defer tx.Rollback()
	if err := tx.Statement("t", func(s *engine.Stmt) error { return g2.DeleteNode(ctx, s, b, true) }); err != nil {
		t.Fatalf("delete of the unindexed node: %v", err)
	}
	if err := tx.Statement("t", func(s *engine.Stmt) error { return g2.DeleteNode(ctx, s, a, true) }); err != nil {
		t.Fatalf("delete of the indexed node fails where the unindexed one succeeded: %v", err)
	}
}

// Uniqueness edge cases through the graph: the same entity rewriting a
// long (truncated) value, two long values that share their prefix, equal
// long values, lists with Int and Float, maps.
func TestUniqueEdgeCases(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	g.IndexPrefixBytes = 16
	tx := begin(t, db)
	defer tx.Rollback()
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "u", Scope: "U", Property: "k", Unique: true})
	})
	long := strings.Repeat("p", 40)
	a := node(t, g, tx, []string{"U"}, props("k", value.String(long+"1")))
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, a, "k", value.String(long+"1")) })
	node(t, g, tx, []string{"U"}, props("k", value.String(long+"2")))
	node(t, g, tx, []string{"U"}, props("k", value.List(value.Int(3))))
	node(t, g, tx, []string{"U"}, props("k", value.MapOf(map[string]value.Value{"a": value.Int(0)})))
	node(t, g, tx, []string{"U"}, props("k", value.Int(0)))
	node(t, g, tx, []string{"U"}, props("k", value.Int(1<<62)))
	for name, v := range map[string]value.Value{
		"equal long":    value.String(long + "1"),
		"list int/flt":  value.List(value.Float(3)),
		"map int/flt":   value.MapOf(map[string]value.Value{"a": value.Float(math.Copysign(0, -1))}),
		"neg zero":      value.Float(math.Copysign(0, -1)),
		"big int float": value.Float(1 << 62),
	} {
		err := tx.Statement("t", func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, []string{"U"}, props("k", v))
			return err
		})
		if errs.CodeOf(err) != errs.CodeUniqueViolation {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Distinct values that must not collide.
	for name, v := range map[string]value.Value{
		"2^62+1":       value.Int(1<<62 + 1),
		"list 3.5":     value.List(value.Float(3.5)),
		"long prefix3": value.String(long + "3"),
		"prefix only":  value.String(long),
	} {
		if err := tx.Statement("t", func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, []string{"U"}, props("k", v))
			return err
		}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	verify(t, g, tx)
}

// The index cache across savepoints, statements that fail after
// declaring, and readers of older snapshots: maintenance and listings must
// always follow the declarations the reader sees.
func TestCacheAcrossSavepointsAndSnapshots(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	n := node(t, g, tx, []string{"A"}, props("p", value.Int(1)))
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	tx = begin(t, db)
	sp, _ := tx.Savepoint()
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "i", Scope: "A", Property: "p"})
	})
	if defs, _ := g.ListIndexes(ctx, old); len(defs) != 0 {
		t.Fatalf("an older snapshot sees %v", defs)
	}
	if err := tx.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	if defs, _ := g.ListIndexes(ctx, tx); len(defs) != 0 {
		t.Fatalf("after the rollback the writer sees %v", defs)
	}
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, n, "p", value.Int(2)) })
	verify(t, g, tx)
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "i", Scope: "A", Property: "p", Unique: true})
	})
	node(t, g, tx, []string{"A"}, props("p", value.Int(3)))
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if defs, _ := g.ListIndexes(ctx, old); len(defs) != 0 {
		t.Fatalf("an older snapshot sees %v after the commit", defs)
	}
	tx = begin(t, db)
	defer tx.Rollback()
	verify(t, g, tx)
	if err := tx.Statement("t", func(s *engine.Stmt) error {
		_, err := g.CreateNode(ctx, s, []string{"A"}, props("p", value.Float(3)))
		return err
	}); errs.CodeOf(err) != errs.CodeUniqueViolation {
		t.Fatalf("the committed unique index is not enforced: %v", err)
	}
	if err := g.Verify(ctx, old); err != nil {
		t.Fatalf("the older snapshot: %v", err)
	}
}
