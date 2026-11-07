package index

import (
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func startBuild(t *testing.T, tx *engine.WriteTx, unique bool) *Index {
	t.Helper()
	var ix *Index
	do(t, tx, func(s *engine.Stmt) (err error) {
		ix, err = StartBuild(ctx, s, Definition{Name: "b", Entity: Nodes, Scope: 1, Property: 1, Unique: unique}, 896, 7)
		return err
	})
	return ix
}

// Writers keep a BUILDING index and count their entries in the build; a
// removal of an entity the batches have not reached is no corruption; the
// batches skip what writers inserted; the last phase checks the counts.
func TestBuildBookkeeping(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	base := tx.Base()
	ix := startBuild(t, tx, false)
	st, ok, err := ReadBuild(ctx, tx, ix)
	if err != nil || !ok || st.Started != base.CommitID+1 || st.Estimated != 7 || ix.State != Building || tx.State().SchemaEpoch != base.SchemaEpoch+1 {
		t.Fatalf("after StartBuild: %+v %v %v, %+v", st, ok, err, ix)
	}
	for e := uint64(1); e <= 6; e++ {
		do(t, tx, func(s *engine.Stmt) error { putBase(t, s, ix, e, value.Int(int64(e))); return nil })
	}
	// A writer changes entity 5 before the batches reach it: the old entry
	// is missing, which is fine; the new one counts for the writers.
	do(t, tx, func(s *engine.Stmt) error {
		if err := Remove(ctx, s, ix, 5, value.Int(5)); err != nil {
			return err
		}
		putBase(t, s, ix, 5, value.Int(50))
		return Insert(ctx, s, ix, 5, value.Int(50))
	})
	// A batch reaches entities 1 to 5; entity 5 is there already.
	do(t, tx, func(s *engine.Stmt) error {
		st, _ := build(ctx, s, ix)
		for e := uint64(1); e <= 5; e++ {
			v := value.Int(int64(e))
			if e == 5 {
				v = value.Int(50)
			}
			inserted, err := Fill(ctx, s, ix, &st, e, v)
			if err != nil || inserted == (e == 5) {
				t.Fatalf("Fill %d: %v %v", e, inserted, err)
			}
		}
		return Advance(ctx, s, ix, st, 5)
	})
	// Behind the cursor, a missing entry is corruption again.
	if err := tx.Statement("t", func(s *engine.Stmt) error { return Remove(ctx, s, ix, 4, value.Int(99)) }); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a missing entry behind the cursor: %v", err)
	}
	do(t, tx, func(s *engine.Stmt) error { return Remove(ctx, s, ix, 1, value.Int(1)) })
	st, _, _ = ReadBuild(ctx, tx, ix)
	if n, _ := Entries(ctx, tx, ix); st.Cursor != 5 || st.Scanned != 4 || st.WriterDelta != 0 || n != 4 {
		t.Fatalf("build %+v, %d entries", st, n)
	}
	epoch := tx.State().SchemaEpoch
	var state State
	do(t, tx, func(s *engine.Stmt) (err error) { state, err = Finish(ctx, s, ix); return err })
	set, _ := load(ctx, tx)
	if got, _ := set.Named("b"); state != Online || got.State != Online || tx.State().SchemaEpoch != epoch {
		t.Fatalf("Finish: %v, declared %v; the epoch moved again in the same commit: %d", state, got.State, tx.State().SchemaEpoch)
	}
}

// The last phase fails the build on a mismatch of counts.
func TestBuildInconsistent(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	ix := startBuild(t, tx, false)
	do(t, tx, func(s *engine.Stmt) error { return addEntries(ctx, s, ix, 1) }) // an entry nobody counted
	var state State
	do(t, tx, func(s *engine.Stmt) (err error) { state, err = Finish(ctx, s, ix); return err })
	st, _, _ := ReadBuild(ctx, tx, ix)
	if purges, _ := Purges(ctx, tx); state != Failed || st.Failure != string(errs.CodeIndexInconsistent) || len(purges) != 1 {
		t.Fatalf("a mismatch: %v, %+v, purges %v", state, st, purges)
	}
	failed := *ix
	failed.State = Failed
	if _, err := Seek(ctx, tx, &failed, Query{Op: All}); errs.CodeOf(err) != errs.CodeIndexBuildFailed {
		t.Fatalf("a seek on a FAILED index: %v", err)
	}
	if _, err := Seek(ctx, tx, ix, Query{Op: All}); errs.CodeOf(err) != errs.CodeIndexBuilding {
		t.Fatalf("a seek on a BUILDING index: %v", err)
	}
}

// In a BUILDING unique index, writers and batches record duplicates
// instead of failing; the build fails at its end with the first pairs.
func TestBuildConflicts(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	ix := startBuild(t, tx, true)
	do(t, tx, func(s *engine.Stmt) error {
		for e := uint64(1); e <= 3; e++ {
			putBase(t, s, ix, e, value.Int(1))
		}
		putBase(t, s, ix, 9, value.Float(1))
		return Insert(ctx, s, ix, 9, value.Float(1)) // a writer, ahead of the batches
	})
	do(t, tx, func(s *engine.Stmt) error {
		st, _ := build(ctx, s, ix)
		for e := uint64(1); e <= 3; e++ {
			if _, err := Fill(ctx, s, ix, &st, e, value.Int(1)); err != nil {
				return err
			}
		}
		return Advance(ctx, s, ix, st, 9)
	})
	// A writer duplicating behind the cursor records a conflict too.
	do(t, tx, func(s *engine.Stmt) error {
		putBase(t, s, ix, 10, value.Int(1))
		return Insert(ctx, s, ix, 10, value.Int(1))
	})
	st, _, _ := ReadBuild(ctx, tx, ix)
	if len(st.Conflicts) != 8 || st.Conflicts[0] != 9 || st.Conflicts[1] != 1 || st.Conflicts[7] != 10 {
		t.Fatalf("conflicts %v", st.Conflicts)
	}
	var state State
	do(t, tx, func(s *engine.Stmt) (err error) { state, err = Finish(ctx, s, ix); return err })
	st, _, _ = ReadBuild(ctx, tx, ix)
	if state != Failed || st.Failure != string(errs.CodeUniqueViolation) || st.Message == "" {
		t.Fatalf("a build with duplicates: %v, %+v", state, st)
	}
	// Many duplicates keep only the first pairs.
	st = Build{}
	for i := uint64(1); i <= 20; i++ {
		conflict(&st, i, i+100)
	}
	if len(st.Conflicts) != 2*MaxConflicts {
		t.Fatalf("%d conflicts kept", len(st.Conflicts))
	}
}

// Damaged build and purge records are corruption.
func TestBuildRecordDamage(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	ix := startBuild(t, tx, false)
	for name, fields := range map[string]map[string]value.Value{
		"negative cursor":  {"cursor": value.Int(-1)},
		"odd conflicts":    {"conflicts": value.List(value.Int(1))},
		"zero conflict":    {"conflicts": value.List(value.Int(0), value.Int(1))},
		"text conflict":    {"conflicts": value.List(value.String("a"), value.Int(1))},
		"too many pairs":   {"conflicts": value.List(make([]value.Value, 2*MaxConflicts+2)...)},
		"negative started": {"started": value.Int(-3)},
	} {
		rec := map[string]value.Value{"cursor": value.Int(0), "scanned": value.Int(0), "writerDelta": value.Int(0),
			"estimated": value.Int(0), "started": value.Int(1), "conflicts": value.List()}
		for k, v := range fields {
			rec[k] = v
		}
		b, err := buildRecord.Encode(rec)
		if err != nil {
			t.Fatal(err)
		}
		do(t, tx, func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotMaintenance, maintenanceKey(prefixBuild, ix.ID), b)
		})
		if _, _, err := ReadBuild(ctx, tx, ix); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("%s: %v", name, err)
		}
	}
	b, _ := purgeRecord.Encode(map[string]value.Value{"entity": value.Int(2)})
	do(t, tx, func(s *engine.Stmt) error {
		return s.Put(ctx, catalog.SlotMaintenance, maintenanceKey(prefixPurge, 4), b)
	})
	if _, err := Purges(ctx, tx); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a purge over entity 2: %v", err)
	}
	do(t, tx, func(s *engine.Stmt) error {
		return s.Put(ctx, catalog.SlotMaintenance, maintenanceKey(prefixPurge, 0), b)
	})
	if _, err := Purges(ctx, tx); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a purge of index 0: %v", err)
	}
}

// Drop takes the build record and schedules the purge; a FAILED index is
// purged already.
func TestDropBuild(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	ix := startBuild(t, tx, false)
	do(t, tx, func(s *engine.Stmt) error { return Drop(ctx, s, ix) })
	if _, ok, _ := ReadBuild(ctx, tx, ix); ok {
		t.Fatal("the build record stays after Drop")
	}
	if ps, _ := Purges(ctx, tx); len(ps) != 1 {
		t.Fatalf("purges %v", ps)
	}
}
