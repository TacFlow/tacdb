package graph

import (
	"encoding/binary"
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// Verify finds a BUILDING index that lacks the entry of an entity its
// batches already passed, even when its counters agree.
func TestVerifyFindsHoleBelowCursor(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	a := node(t, g, tx, []string{"P"}, props("p", value.Int(1)))
	node(t, g, tx, []string{"P"}, props("p", value.Int(2)))
	var ix *index.Index
	do(t, tx, func(s *engine.Stmt) (err error) {
		ix, err = g.StartBuild(ctx, s, IndexDef{Name: "p", Scope: "P", Property: "p"})
		return err
	})
	do(t, tx, func(s *engine.Stmt) error { _, _, err := g.BuildBatch(ctx, s, ix.ID, 1<<20); return err })
	// Damage: the entry of a goes, with consistent counters.
	do(t, tx, func(s *engine.Stmt) error {
		k, err := index.EntryKey(ix, a, value.Int(1))
		if err != nil {
			return err
		}
		if _, err := s.Delete(ctx, catalog.SlotNodeProperty, k); err != nil {
			return err
		}
		ck := codec.AppendKeyUint64(codec.AppendKeyUint8(nil, 1), ix.ID)
		if err := s.Put(ctx, catalog.SlotStats, ck, binary.LittleEndian.AppendUint64(nil, 1)); err != nil {
			return err
		}
		st, _, err := index.ReadBuild(ctx, s, ix)
		if err != nil {
			return err
		}
		st.Scanned--
		return index.Advance(ctx, s, ix, st, st.Cursor)
	})
	if err := g.Verify(ctx, tx); err == nil {
		do(t, tx, func(s *engine.Stmt) error { _, err := g.FinishBuild(ctx, s, ix.ID); return err })
		t.Fatalf("Verify accepts a BUILDING index with a hole below its cursor; after Finish it says: %v", g.Verify(ctx, tx))
	}
}
