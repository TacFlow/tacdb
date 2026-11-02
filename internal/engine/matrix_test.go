package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/vfs"
)

// run is a deterministic workload over every path that writes files:
// groups, segment rolls with their spares, identifier reservations,
// checkpoints with scrub, removal of segments and the last checkpoint of
// Close. It stops at the first error, as a process that lost its disk.
type run struct {
	states []map[string]string // states[i]: the state after i commits
	tokens [][16]byte          // tokens[i]: the token of commit i+1
	acked  int                 // commits that returned success
	synced int                 // commits known durable: all acked, or the last Flush in Async
}

func workload(db *DB, mode Durability) *run {
	r := &run{states: []map[string]string{{}}}
	model := map[string]string{}
	for i := 0; i < 40; i++ {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			return r
		}
		k := fmt.Sprintf("k%02d", i%25)
		v := strings.Repeat(string(rune('a'+i%26)), 2000+i*397%3000)
		next := maps.Clone(model)
		next[k] = v
		if err := tx.Put(ctx, 0, []byte(k), []byte(v)); err != nil {
			tx.Rollback()
			return r
		}
		if i%7 == 6 { // frees pages for the scrub
			d := fmt.Sprintf("k%02d", (i+3)%25)
			if _, err := tx.Delete(ctx, 0, []byte(d)); err != nil {
				tx.Rollback()
				return r
			}
			delete(next, d)
		}
		for range 2 {
			if _, err := tx.NextID(ctx, KindNode); err != nil {
				tx.Rollback()
				return r
			}
		}
		r.states = append(r.states, next) // what a crash may recover from here on
		rc, err := tx.Commit(ctx)
		r.tokens = append(r.tokens, rc.Token)
		if err != nil {
			return r
		}
		model = next
		r.acked++
		if mode != Async {
			r.synced = r.acked
		}
		switch {
		case mode == Async && i%5 == 4:
			if db.Flush(ctx) != nil {
				return r
			}
			r.synced = r.acked
		case i == 15 || i == 30:
			if db.Checkpoint(ctx) != nil {
				return r
			}
		}
	}
	if db.Close() == nil {
		r.synced = r.acked
	}
	return r
}

// verify reopens fs and checks the recovered state: one of the states the
// workload went through, never before the last durable one and never past
// the commit in flight; tokens of lost commits Unknown; the structure
// sound, and the token of a recovered commit in flight Durable. It returns
// the recovered state. Then one more group and a clean crash: what recovery decided
// stays, and a discarded tail never comes back.
func verify(t *testing.T, fs *vfs.MemFS, o Options, r *run, label string) int {
	t.Helper()
	db, err := Open(ctx, fs, name, o)
	if err != nil {
		t.Fatalf("%s: reopen: %v", label, err)
	}
	got := read(t, db)
	j := -1
	for i := r.synced; i <= min(r.acked+1, len(r.states)-1); i++ {
		if maps.Equal(got, r.states[i]) {
			j = i
		}
	}
	if j < 0 {
		db.Close()
		t.Fatalf("%s: the recovered state (%d keys) is none of states %d to %d", label, len(got), r.synced, r.acked+1)
	}
	for i := j; i < len(r.tokens); i++ {
		if s := db.CommitStatus(r.tokens[i]); s != Unknown {
			t.Fatalf("%s: commit %d was not recovered, yet its token is %v", label, i+1, s)
		}
	}
	if j > r.acked { // the commit in flight was recovered whole
		if s := db.CommitStatus(r.tokens[j-1]); s != Durable {
			t.Fatalf("%s: commit %d was recovered, yet its token is %v", label, j, s)
		}
	}
	structural(t, db)
	put(t, db, "after", label)
	if err := db.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = open(t, fs, o)
	want := maps.Clone(r.states[j])
	want["after"] = label
	same(t, read(t, db), want)
	structural(t, db)
	db.Close()
	return j
}

// The matrix of SPEC §11.3: a power loss at every call that changes a file
// during the workload, before and after it, with unsynced writes lost,
// torn by sector subsets or applied out of order. The recovered state is
// always a state the workload went through: the previous one or the
// complete group, never part of one.
func TestCrashMatrix(t *testing.T) {
	ops := []vfs.Op{vfs.OpCreate, vfs.OpWrite, vfs.OpSync, vfs.OpSyncDir, vfs.OpRename, vfs.OpRemove, vfs.OpTrunc, vfs.OpPrealloc}
	for _, mode := range []Durability{GroupSync, Async} {
		o := quiet()
		o.Durability = mode
		o.AsyncSyncInterval = time.Hour // Syncs only by Flush, room and bytes
		o.IDReservationInitial, o.IDReservationMax = 64, 64
		points, whole := 0, 0
		for _, op := range ops {
			for n := 1; ; n++ {
				fired := false
				for _, after := range []bool{false, true} {
					opts := []vfs.CrashOptions{{}, {Seed: uint64(n), TornWrites: true, SectorSubset: true},
						{Seed: uint64(n), TornWrites: true, Reorder: true},
						{Seed: uint64(n), Reorder: true}}[(n+boolInt(after))%4] // the last keeps every unsynced write
					fs := vfs.NewMemFS()
					db := open(t, fs, o) // the creation is covered by TestOrphanLog
					fs.InjectCrash(vfs.CrashPoint{Op: op, Nth: n, After: after, Options: opts})
					r := workload(db, mode)
					db.Close()
					if fs.Crashes() == 0 {
						continue
					}
					fired = true
					points++
					fs.Restart()
					if verify(t, fs, o, r, fmt.Sprintf("%v %s #%d after=%v", mode, op, n, after)) > r.acked {
						whole++
					}
				}
				if !fired {
					break
				}
			}
		}
		if mode == GroupSync && whole == 0 {
			t.Fatal("no crash recovered the commit in flight whole: the matrix misses that case")
		}
		if points < 200 {
			t.Fatalf("%v: only %d crash points fired", mode, points)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A crash that truncates the active segment anywhere after its durable
// end keeps the durable commits and a prefix of the unsynced ones, each
// whole.
func TestCrashTruncatedSegment(t *testing.T) {
	o := quiet()
	o.Durability, o.AsyncSyncInterval = Async, time.Hour
	kept := map[int]bool{}
	for cut := int64(0); cut < 8; cut++ {
		fs := vfs.NewMemFS()
		db := open(t, fs, o)
		model := map[string]string{}
		fill(t, db, model, 0, 20)
		if err := db.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		states := []map[string]string{maps.Clone(model)}
		seq, end := db.co.seq.Load(), db.co.off.Load()
		for i := 100; i < 105; i++ { // appended, not synced: new keys
			fill(t, db, model, i, 1)
			states = append(states, maps.Clone(model))
		}
		seg := fmt.Sprintf("%s-wal/%016x.wal", name, seq)
		fs.Crash(vfs.CrashOptions{Seed: uint64(cut), Reorder: true, // every unsynced write survives, cut by the truncation
			Truncate: map[string]int64{seg: end + cut*5000}})
		db.Close()
		db = open(t, fs, o)
		got := read(t, db)
		i := slices.IndexFunc(states, func(s map[string]string) bool { return maps.Equal(s, got) })
		if i < 0 {
			t.Fatalf("cut %d: the recovered state is no prefix of the commits", cut)
		}
		kept[i] = true
		structural(t, db)
		db.Close()
	}
	if len(kept) < 2 {
		t.Fatalf("every cut recovered the same prefix %v: the cuts miss the unsynced commits", kept)
	}
}

// kill ends a handle as a process that dies: no Sync, no checkpoint; the
// files keep what the operating system received.
func kill(db *DB) {
	db.mt.stop()
	db.co.stop()
	db.w.Close()
	db.release()
}

// After a process dies, recovery reads unsynced appends from the operating
// system and syncs them before relying on them: a power loss after the
// reopen keeps every recovered commit.
func TestProcessDeathThenPowerLoss(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	o.Durability, o.AsyncSyncInterval = Async, time.Hour
	db := open(t, fs, o)
	model := map[string]string{}
	fill(t, db, model, 0, 30) // appended, never synced
	kill(db)
	db = open(t, fs, o)
	same(t, read(t, db), model)
	// The open itself made them durable: a power loss right after it,
	// before any commit, keeps them.
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = open(t, fs, o)
	same(t, read(t, db), model)
	put(t, db, "x", "y")
	model["x"] = "y"
	if err := db.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	fs.Crash(vfs.CrashOptions{Seed: 1, TornWrites: true, Reorder: true})
	db.Close()
	db = open(t, fs, o)
	same(t, read(t, db), model)
	structural(t, db)
	db.Close()
}

// A tail discarded by recovery never comes back: a group of the same size
// written after it, then a crash, recovers the new group alone.
func TestDiscardedTailStaysDiscarded(t *testing.T) {
	o := quiet()
	o.Durability, o.AsyncSyncInterval = Async, time.Hour
	discarded := 0
	for seed := uint64(0); seed < 30; seed++ {
		fs := vfs.NewMemFS()
		db := open(t, fs, o)
		model := map[string]string{}
		fill(t, db, model, 0, 10)
		db.Flush(ctx)
		put(t, db, "tail", strings.Repeat("o", 6000)) // unsynced
		fs.Crash(vfs.CrashOptions{Seed: seed, TornWrites: true, SectorSubset: true})
		db.Close()
		db = open(t, fs, o)
		if _, ok := read(t, db)["tail"]; ok {
			db.Close()
			continue
		}
		discarded++
		put(t, db, "tail", strings.Repeat("n", 6000)) // unsynced too, over the same place
		fs.Crash(vfs.CrashOptions{Seed: seed + 100, TornWrites: true, SectorSubset: true})
		db.Close()
		db = open(t, fs, o)
		got := read(t, db)
		if v, ok := got["tail"]; ok && v != strings.Repeat("n", 6000) {
			t.Fatalf("seed %d: the discarded tail came back", seed)
		}
		delete(got, "tail")
		same(t, got, model)
		db.Close()
	}
	if discarded == 0 {
		t.Fatal("no seed discarded the tail")
	}
}

// A checkpoint whose target is the last record of its segment, with the
// next segment already in use: the floor is the next segment, and the
// commits after T survive a crash.
func TestCheckpointAtSegmentEnd(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	model := map[string]string{}
	var r *ReadTx
	for i := 0; r == nil; i++ {
		if i > 500 {
			t.Fatal("no roll")
		}
		before := db.co.seq.Load()
		h, _ := db.BeginRead(ctx) // the state before this commit
		fill(t, db, model, i, 1)
		if db.co.seq.Load() != before {
			r = h // its commit ended the previous segment
		} else {
			h.Close()
		}
	}
	fill(t, db, model, 1000, 30)
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	segsNow, _ := db.segments() // the checkpoint removed the segment T ends
	if db.sb.CommitID != r.State().CommitID || segsNow[0].seq != db.sb.WALFloorSegment ||
		segsNow[0].first != db.sb.CheckpointLSN+1 {
		t.Fatalf("checkpoint at %d (reader %d), floor %d, segments %v, checkpoint_lsn %d",
			db.sb.CommitID, r.State().CommitID, db.sb.WALFloorSegment, segsNow, db.sb.CheckpointLSN)
	}
	r.Close()
	fill(t, db, model, 2000, 10)
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = open(t, fs, quiet())
	same(t, read(t, db), model)
	structural(t, db)
	db.Close()
}
