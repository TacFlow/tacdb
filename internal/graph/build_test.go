package graph

import (
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// Batches move the cursor by ascending ID, at least one entity each; a
// finished build takes no more batches; the estimate counts the kind.
func TestBuildBatches(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	var ids []uint64
	for i := range 10 {
		ids = append(ids, node(t, g, tx, []string{"P"}, props("p", value.Int(int64(i)))))
	}
	other := node(t, g, tx, nil, nil)
	edge(t, g, tx, other, other, "T")
	var ix *index.Index
	do(t, tx, func(s *engine.Stmt) (err error) {
		ix, err = g.StartBuild(ctx, s, IndexDef{Name: "p", Scope: "P", Property: "p"})
		return err
	})
	st, _, _ := index.ReadBuild(ctx, tx, ix)
	if st.Estimated != 11 || st.Cursor != 0 {
		t.Fatalf("a node build: %+v", st)
	}
	batches := 0
	for done := false; !done; batches++ {
		do(t, tx, func(s *engine.Stmt) (err error) {
			_, done, err = g.BuildBatch(ctx, s, ix.ID, 1)
			return err
		})
		st, _, _ = index.ReadBuild(ctx, tx, ix)
		if want := ids[min(batches, len(ids)-1)]; st.Cursor != want {
			t.Fatalf("batch %d: cursor %d, want %d", batches, st.Cursor, want)
		}
	}
	if batches != len(ids) || st.Scanned != 10 {
		t.Fatalf("%d batches, %d scanned", batches, st.Scanned)
	}
	do(t, tx, func(s *engine.Stmt) error { _, err := g.FinishBuild(ctx, s, ix.ID); return err })
	do(t, tx, func(s *engine.Stmt) error {
		got, done, err := g.BuildBatch(ctx, s, ix.ID, 1)
		if got != nil || !done {
			t.Fatalf("a batch of a finished build: %v, %v", got, done)
		}
		return err
	})
	verify(t, g, tx)
	do(t, tx, func(s *engine.Stmt) (err error) {
		ix, err = g.StartBuild(ctx, s, IndexDef{Name: "e", Entity: EdgeEntity, Scope: "T", Property: "w"})
		return err
	})
	if st, _, _ := index.ReadBuild(ctx, tx, ix); st.Estimated != 1 {
		t.Fatalf("an edge build estimates %d", st.Estimated)
	}
	// A drop takes the build record with the declaration.
	do(t, tx, func(s *engine.Stmt) error { return g.DropIndex(ctx, s, "e") })
	if _, ok, _ := index.ReadBuild(ctx, tx, ix); ok {
		t.Fatal("the build record of a dropped index stays")
	}
}

// Verify finds builds whose counts disagree with their entries, and
// indexes kept and purged at once.
func TestVerifyFindsBuildDamage(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	node(t, g, tx, []string{"P"}, props("p", value.Int(1)))
	var ix *index.Index
	do(t, tx, func(s *engine.Stmt) (err error) {
		ix, err = g.StartBuild(ctx, s, IndexDef{Name: "p", Scope: "P", Property: "p"})
		return err
	})
	do(t, tx, func(s *engine.Stmt) error { _, _, err := g.BuildBatch(ctx, s, ix.ID, 1<<20); return err })
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	verify(t, g, r)
	r.Close()
	for name, damage := range map[string]func(s *engine.Stmt) error{
		"build counts": func(s *engine.Stmt) error {
			st, _, _ := index.ReadBuild(ctx, s, ix)
			st.Scanned++
			return index.Advance(ctx, s, ix, st, st.Cursor)
		},
		"kept and purged": func(s *engine.Stmt) error {
			p, _ := codec.AppendValue(nil, value.MapOf(map[string]value.Value{"entity": value.Int(0)}), value.DefaultLimits())
			return s.Put(ctx, catalog.SlotMaintenance, codec.AppendKeyUint64(codec.AppendKeyUint8(nil, 4), ix.ID), append([]byte{1, 0}, p...))
		},
		"no build record": func(s *engine.Stmt) error {
			_, err := s.Delete(ctx, catalog.SlotMaintenance, codec.AppendKeyUint64(codec.AppendKeyUint8(nil, 1), ix.ID))
			return err
		},
	} {
		tx := begin(t, db)
		do(t, tx, damage)
		if err := g.Verify(ctx, tx); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("%s: Verify says %v", name, err)
		}
		tx.Rollback()
	}
}
