package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// quiet are testOptions without background checkpoints: the tests call
// Checkpoint.
func quiet() Options {
	o := testOptions()
	o.MaintenanceInterval = time.Hour
	o.CheckpointBytes = 1 << 40
	o.CheckpointSegments = 1 << 30
	return o
}

// clock is a settable Now.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *clock) now() time.Time { return time.Unix(0, c.ns.Load()) }

func (c *clock) add(d time.Duration) { c.ns.Add(int64(d)) }

func (c *clock) options(o *Options) *clock { o.Now = c.now; return c }

// segs lists the segments of the log.
func segs(t *testing.T, fs vfs.FS) []uint64 {
	t.Helper()
	names, err := fs.List(name + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	var out []uint64
	for _, n := range names {
		if seq, ok := wal.ParseSegmentName(n); ok {
			out = append(out, seq)
		}
	}
	return out
}

// fill commits n transactions of values big enough to roll segments, and
// returns the model.
func fill(t *testing.T, db *DB, model map[string]string, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		k, v := fmt.Sprintf("k%03d", i%60), strings.Repeat(string(rune('a'+i%26)), 3000+i%7*100)
		put(t, db, k, v)
		model[k] = v
	}
}

func same(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d keys, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %s: %d bytes, want %d", k, len(got[k]), len(v))
		}
	}
}

func TestCheckpoint(t *testing.T) {
	for _, mode := range []Durability{GroupSync, Sync, Async} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			fs := vfs.NewMemFS()
			o := quiet()
			o.Durability = mode
			db := open(t, fs, o)
			model := map[string]string{}
			fill(t, db, model, 0, 300)
			if err := db.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			before := segs(t, fs)
			if len(before) < 3 {
				t.Fatalf("the test needs several segments: %v", before)
			}
			if err := db.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			d := db.durable.Load()
			if db.b.Base() != d.State.CommitID || db.sb.Generation != 2 || db.sb.CheckpointLSN != d.CommitLSN {
				t.Fatalf("base %d, superblock %+v, durable %d", db.b.Base(), db.sb, d.State.CommitID)
			}
			after := segs(t, fs)
			if after[0] != db.sb.WALFloorSegment || len(after) >= len(before) {
				t.Fatalf("segments %v then %v, floor %d", before, after, db.sb.WALFloorSegment)
			}
			if st := db.Stats(); st.Checkpoints != 1 || st.CheckpointPages == 0 || st.SegmentsRemoved == 0 {
				t.Fatalf("stats %+v", st)
			}
			if b := db.co.walBytes(db); b > int64(len(after))*o.WALSegmentBytes {
				t.Fatalf("MaxWALBytes still counts removed segments: %d bytes in %d segments", b, len(after))
			}
			same(t, read(t, db), model)
			if err := db.Checkpoint(ctx); err != nil || db.Stats().Checkpoints != 1 {
				t.Fatalf("a checkpoint without progress wrote: %v, %+v", err, db.Stats())
			}
			fill(t, db, model, 300, 20)
			if err := db.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			// A power loss: the main file and the log after the floor hold
			// the state.
			fs.Crash(vfs.CrashOptions{})
			db.Close()
			db = open(t, fs, o)
			same(t, read(t, db), model)
			structural(t, db)
			db.Close()
		})
	}
}

// REC-04: a crash at each call that changes a file during a checkpoint
// recovers the durable state; the checkpoint after it completes, and a
// second reopen gives the same state.
func TestCheckpointCrashPoints(t *testing.T) {
	type point struct {
		label string
		p     vfs.CrashPoint
	}
	var points []point
	for n := 1; n <= 40; n++ {
		for _, after := range []bool{false, true} {
			points = append(points, point{fmt.Sprintf("write %d after=%v", n, after),
				vfs.CrashPoint{Op: vfs.OpWrite, Name: name, Nth: n, After: after,
					Options: vfs.CrashOptions{Seed: uint64(n), TornWrites: true, SectorSubset: true}}})
		}
	}
	for n := 1; n <= 2; n++ {
		for _, after := range []bool{false, true} {
			points = append(points, point{fmt.Sprintf("sync %d after=%v", n, after),
				vfs.CrashPoint{Op: vfs.OpSync, Name: name, Nth: n, After: after}})
		}
	}
	points = append(points,
		point{"remove", vfs.CrashPoint{Op: vfs.OpRemove, Name: name + "-wal/*", Nth: 1}},
		point{"remove after", vfs.CrashPoint{Op: vfs.OpRemove, Name: name + "-wal/*", Nth: 1, After: true}},
		point{"syncdir", vfs.CrashPoint{Op: vfs.OpSyncDir, Name: name + "-wal", Nth: 1}},
		point{"syncdir after", vfs.CrashPoint{Op: vfs.OpSyncDir, Name: name + "-wal", Nth: 1, After: true}})
	fired := 0
	for _, pt := range points {
		fs := vfs.NewMemFS()
		db := open(t, fs, quiet())
		model := map[string]string{}
		fill(t, db, model, 0, 120)
		for i := 0; i < 40; i++ { // free pages for the scrub
			tx, _ := db.BeginWrite(ctx)
			tx.Delete(ctx, 0, []byte(fmt.Sprintf("k%03d", i)))
			tx.Commit(ctx)
			delete(model, fmt.Sprintf("k%03d", i))
		}
		fs.InjectCrash(pt.p)
		err := db.Checkpoint(ctx)
		if fs.Crashes() == 0 {
			if err != nil {
				t.Fatalf("%s: %v", pt.label, err)
			}
			db.Close()
			continue
		}
		fired++
		db.Close()
		fs.Restart()
		for round := 0; round < 2; round++ {
			db = open(t, fs, quiet())
			same(t, read(t, db), model)
			structural(t, db)
			for _, s := range segs(t, fs) {
				if s < db.sb.WALFloorSegment {
					t.Fatalf("%s: segment %d below the floor %d after recovery", pt.label, s, db.sb.WALFloorSegment)
				}
			}
			if err := db.Checkpoint(ctx); err != nil {
				t.Fatalf("%s: checkpoint after recovery: %v", pt.label, err)
			}
			cleanFree(t, fs, db)
			fs.Crash(vfs.CrashOptions{})
			db.Close()
		}
	}
	if fired < len(points)/2 {
		t.Fatalf("only %d of %d crash points fired", fired, len(points))
	}
}

// cleanFree checks the raw main file: every page free in the durable
// bitmaps is either never written or a zeroed free page (the scrub).
func cleanFree(t *testing.T, fs *vfs.MemFS, db *DB) {
	t.Helper()
	img, ok := fs.ReadDurable(name)
	if !ok {
		t.Fatal("no main file")
	}
	pc := db.sb.PageCount
	pageAt := func(id page.ID) *page.Buf {
		var b page.Buf
		if off := int64(id) * page.Size; off < int64(len(img)) {
			copy(b[:], img[off:])
		}
		return &b
	}
	for g := uint64(0); uint64(page.BitmapPage(g)) < pc; g++ {
		bm := pageAt(page.BitmapPage(g))
		if page.Verify(bm, page.BitmapPage(g), pc) != nil {
			t.Fatalf("bitmap of group %d is not valid after a checkpoint", g)
		}
		for i := 0; i < page.GroupPages && uint64(page.BitmapPage(g))+1+uint64(i) < pc; i++ {
			if page.BitmapBit(bm, i) {
				continue
			}
			id := page.BitmapPage(g) + 1 + page.ID(i)
			b := pageAt(id)
			if *b == (page.Buf{}) {
				continue
			}
			if page.Verify(b, id, pc) != nil || page.HeaderOf(b).Type() != page.TypeFree {
				t.Fatalf("free page %d holds type %d data", id, page.HeaderOf(b).Type())
			}
		}
	}
}

// BT-05 with the scrub: pages allocated before a checkpoint and freed
// after it, and pages allocated and freed inside one interval, are zeroed
// by the next checkpoint; the raw file keeps no stale data.
func TestScrub(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	model := map[string]string{}
	fill(t, db, model, 0, 60)
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	fill(t, db, model, 1000, 20) // allocated inside the interval
	tx, _ := db.BeginWrite(ctx)
	for i := 0; i < 60; i += 2 {
		k := fmt.Sprintf("k%03d", i)
		if _, err := tx.Delete(ctx, 0, []byte(k)); err != nil {
			t.Fatal(err)
		}
		delete(model, k)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if db.Stats().ScrubbedPages == 0 {
		t.Fatal("nothing was scrubbed")
	}
	cleanFree(t, fs, db)
	structural(t, db)
	db.Close()

	o := quiet()
	o.ScrubFreedPages = false
	db = open(t, fs, o)
	fill(t, db, model, 2000, 10)
	tx, _ = db.BeginWrite(ctx)
	for k := range model {
		tx.Delete(ctx, 0, []byte(k))
		delete(model, k)
	}
	tx.Commit(ctx)
	before := db.Stats().ScrubbedPages
	if err := db.Checkpoint(ctx); err != nil || db.Stats().ScrubbedPages != before {
		t.Fatalf("ScrubFreedPages off: %v, %d scrubbed", err, db.Stats().ScrubbedPages-before)
	}
	same(t, read(t, db), model)
	structural(t, db)
	db.Close()
}

// A checkpoint interrupted in step 2 leaves bitmaps of the base newer than
// the superblock, or torn; the next one zeroes the free pages of their
// groups instead of failing.
func TestScrubAfterInterruptedCheckpoint(t *testing.T) {
	for seed := uint64(0); seed < 24; seed++ {
		fs := vfs.NewMemFS()
		db := open(t, fs, quiet())
		model := map[string]string{}
		fill(t, db, model, 0, 60)
		if err := db.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ { // pages allocated inside the interval
			k, v := fmt.Sprintf("n%03d", i), strings.Repeat("n", 5000)
			put(t, db, k, v)
			model[k] = v
		}
		tx, _ := db.BeginWrite(ctx)
		for i := 0; i < 60; i += 3 {
			k := fmt.Sprintf("k%03d", i)
			tx.Delete(ctx, 0, []byte(k))
			delete(model, k)
		}
		tx.Commit(ctx)
		// Page 2, the bitmap, is the first page written: crash after it,
		// torn or whole, or after the data Sync.
		p := vfs.CrashPoint{Op: vfs.OpWrite, Name: name, Nth: 1, After: true,
			Options: vfs.CrashOptions{Seed: seed, TornWrites: true, SectorSubset: true}}
		switch seed % 3 {
		case 1:
			p = vfs.CrashPoint{Op: vfs.OpSync, Name: name, Nth: 1, After: true}
		case 2: // before the data Sync: any subset of the writes, in any order
			p = vfs.CrashPoint{Op: vfs.OpSync, Name: name, Nth: 1,
				Options: vfs.CrashOptions{Seed: seed, TornWrites: true, Reorder: true}}
		}
		fs.InjectCrash(p)
		if db.Checkpoint(ctx) == nil {
			t.Fatal("the crash point did not fire")
		}
		db.Close()
		fs.Restart()
		db = open(t, fs, quiet())
		same(t, read(t, db), model)
		// Free every page the interrupted checkpoint may have written; a
		// data write may have survived where its bitmap's did not.
		tx, _ = db.BeginWrite(ctx)
		for k := range model {
			tx.Delete(ctx, 0, []byte(k))
			delete(model, k)
		}
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.Checkpoint(ctx); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		cleanFree(t, fs, db)
		structural(t, db)
		db.Close()
	}
}

// Watermarks never go back across checkpoints and a crash: identifiers
// handed out, committed or rolled back, are never handed out again.
func TestWatermarksAcrossCheckpoints(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	o.IDReservationInitial = 64
	o.IDReservationMax = 64
	db := open(t, fs, o)
	var high uint64
	for round := 0; round < 3; round++ {
		for _, commit := range []bool{true, false} {
			tx, err := db.BeginWrite(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 100; i++ {
				id, err := tx.NextID(ctx, KindNode)
				if err != nil {
					t.Fatal(err)
				}
				if id <= high {
					t.Fatalf("identifier %d handed out again (high %d)", id, high)
				}
				high = id
			}
			tx.Put(ctx, 0, []byte("r"), []byte(fmt.Sprint(round)))
			if commit {
				if _, err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				tx.Rollback()
			}
		}
		if err := db.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		fs.Crash(vfs.CrashOptions{})
		db.Close()
		db = open(t, fs, o)
	}
	tx, _ := db.BeginWrite(ctx)
	if id, _ := tx.NextID(ctx, KindNode); id <= high {
		t.Fatalf("after the last crash: %d, handed out up to %d", id, high)
	}
	tx.Rollback()
	db.Close()
}

// CommitStatus after a crash: the tokens after checkpoint_lsn are Durable,
// those the checkpoint covered are forgotten.
func TestCommitStatusAfterCheckpointAndCrash(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	old := put(t, db, "a", "1")
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if db.CommitStatus(old.Token) != Unknown {
		t.Fatal("a checkpointed token is still remembered")
	}
	recent := put(t, db, "b", "2")
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = open(t, fs, quiet())
	defer db.Close()
	if db.CommitStatus(old.Token) != Unknown || db.CommitStatus(recent.Token) != Durable {
		t.Fatalf("after the crash: old %v, recent %v", db.CommitStatus(old.Token), db.CommitStatus(recent.Token))
	}
}

// Opening and closing many times with a small log neither piles up
// segments nor reaches MaxWALBytes.
func TestOpenCloseCycles(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	o.MaxWALBytes = 4 * o.WALSegmentBytes
	model := map[string]string{}
	for i := 0; i < 40; i++ {
		db := open(t, fs, o)
		fill(t, db, model, i*10, 10)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if n := len(segs(t, fs)); n != 1 {
			t.Fatalf("cycle %d: %d segments after a clean close", i, n)
		}
	}
	db := open(t, fs, o)
	same(t, read(t, db), model)
	structural(t, db)
	db.Close()
}

// A failed Sync of the main file fails the handle: the Sync is never
// retried, and the reopen recovers from the log.
func TestCheckpointSyncFailure(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	model := map[string]string{}
	fill(t, db, model, 0, 50)
	fs.Inject(vfs.Fault{Op: vfs.OpSync, Name: name, Nth: 1})
	if err := db.Checkpoint(ctx); err == nil {
		t.Fatal("the failed Sync was not reported")
	}
	deadline := time.Now().Add(5 * time.Second)
	for db.usable("test") == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := db.BeginWrite(ctx); !isCode(err, errs.CodeRecoveryRequired) {
		t.Fatalf("after a failed checkpoint: %v", err)
	}
	db.Close()
	db = open(t, fs, quiet())
	same(t, read(t, db), model)
	db.Close()
}

// A canceled checkpoint changes nothing visible, and the next completes.
func TestCheckpointCanceled(t *testing.T) {
	db := open(t, vfs.NewMemFS(), quiet())
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 50)
	cctx, cancel := context.WithCancel(ctx)
	db.hooks.checkpoint = func(stage string) {
		if stage == "target" {
			cancel()
		}
	}
	if err := db.Checkpoint(cctx); !isCode(err, errs.CodeCanceled) {
		t.Fatalf("canceled checkpoint: %v", err)
	}
	db.hooks.checkpoint = nil
	if db.b.Base() != 0 || db.usable("test") != nil {
		t.Fatal("a canceled checkpoint published, or failed the handle")
	}
	put(t, db, "x", "y")
	model["x"] = "y"
	if err := db.Checkpoint(ctx); err != nil || db.b.Base() == 0 {
		t.Fatalf("the next checkpoint: %v", err)
	}
	same(t, read(t, db), model)
}

// Recovery is idempotent: a crash at any call of a recovery that changes
// files, then a reopen, gives the same state.
func TestRecoveryIdempotent(t *testing.T) {
	ops := []vfs.Op{vfs.OpCreate, vfs.OpWrite, vfs.OpSync, vfs.OpSyncDir, vfs.OpRename, vfs.OpRemove, vfs.OpTrunc}
	for _, op := range ops {
		for n := 1; n <= 3; n++ {
			fs := vfs.NewMemFS()
			db := open(t, fs, quiet())
			model := map[string]string{}
			fill(t, db, model, 0, 80)
			db.Checkpoint(ctx)
			fill(t, db, model, 80, 40)
			db.Close()
			// An unsynced commit, maybe torn, maybe whole: the open that
			// discards a tail rolls a segment.
			o := quiet()
			o.Durability, o.AsyncSyncInterval = Async, time.Hour
			db = open(t, fs, o)
			put(t, db, "lost", strings.Repeat("t", 5000))
			fs.Crash(vfs.CrashOptions{Seed: uint64(n), TornWrites: true, SectorSubset: true})
			db.Close()
			crashes := fs.Crashes()
			fs.InjectCrash(vfs.CrashPoint{Op: op, Nth: n})
			if db, err := Open(ctx, fs, name, quiet()); err == nil {
				db.Close()
			}
			if fs.Crashes() == crashes {
				continue // recovery never made that call
			}
			fs.Restart()
			var first map[string]string
			for round := 0; round < 2; round++ {
				db = open(t, fs, quiet())
				got := read(t, db)
				if round == 0 {
					first = got
				} else {
					same(t, got, first)
				}
				if v, ok := got["lost"]; ok && len(v) != 5000 {
					t.Fatalf("%s %d: a partial commit", op, n)
				}
				delete(got, "lost")
				same(t, got, model)
				structural(t, db)
				fs.Crash(vfs.CrashOptions{})
				db.Close()
			}
		}
	}
}

// The checkpoint and the reads of every kind of snapshot run together;
// the base moves under readers that keep reading the right versions.
func TestCheckpointConcurrentReaders(t *testing.T) {
	o := quiet()
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 60)
	snap := map[string]string{}
	for k, v := range model {
		snap[k] = v
	}
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	fill(t, db, model, 60, 200)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				x, _ := db.BeginRead(ctx)
				got := map[string]string{}
				x.Range(ctx, 0, nil, nil, func(k, v []byte) error { got[string(k)] = string(v); return nil })
				x.Close()
				if len(got) != 60 { // fill writes keys k000 to k059
					t.Errorf("a reader saw %d keys", len(got))
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if err := db.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		fill(t, db, model, 300+i*10, 10)
	}
	wg.Wait()
	same(t, contents(t, r), snap)
	r.Close()
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	same(t, read(t, db), model)
}

// Checkpoint refuses a read-only or closed handle; suspended, it writes
// nothing (the hook of the REC-11 gate).
func TestCheckpointModes(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	model := map[string]string{}
	fill(t, db, model, 0, 20)
	db.mt.suspended.Store(true)
	if err := db.Checkpoint(ctx); err != nil || db.Stats().Checkpoints != 0 {
		t.Fatalf("suspended: %v, %+v", err, db.Stats())
	}
	db.mt.suspended.Store(false)
	db.Close()
	if err := db.Checkpoint(ctx); !isCode(err, errs.CodeClosed) {
		t.Fatalf("closed: %v", err)
	}
	o := quiet()
	o.Create, o.ReadOnly = false, true
	ro := open(t, fs, o)
	defer ro.Close()
	if err := ro.Checkpoint(ctx); !isCode(err, errs.CodeReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
	same(t, read(t, ro), model)
}

// A target in the middle of a segment, with later segments: the floor is
// the segment that holds checkpoint_lsn + 1, so the groups after T in the
// same segment are recovered after a crash.
func TestCheckpointFloorMidSegment(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	model := map[string]string{}
	fill(t, db, model, 0, 30)
	r, _ := db.BeginRead(ctx) // holds T here
	fill(t, db, model, 30, 5) // groups after T, same segment
	fill(t, db, model, 35, 200)
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	ck := db.sb.CheckpointLSN
	segsNow, err := db.segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segsNow) < 3 || db.sb.CommitID != r.State().CommitID {
		t.Fatalf("segments %v, checkpoint at %d, reader at %d", segsNow, db.sb.CommitID, r.State().CommitID)
	}
	i := slices.IndexFunc(segsNow, func(s segInfo) bool { return s.seq == db.sb.WALFloorSegment })
	if i < 0 || segsNow[i].first > ck+1 || i+1 < len(segsNow) && segsNow[i+1].first <= ck+1 {
		t.Fatalf("floor %d does not hold checkpoint_lsn+1 = %d: %v", db.sb.WALFloorSegment, ck+1, segsNow)
	}
	r.Close()
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = open(t, fs, quiet())
	same(t, read(t, db), model)
	structural(t, db)
	db.Close()
}

// A reader in the middle of an operation, and a writer for its life, pin
// the log floor in force when they may resolve a version: a checkpoint
// removes no segment from it on until they end.
func TestOperationPinsSegments(t *testing.T) {
	fs := vfs.NewMemFS()
	db := open(t, fs, quiet())
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 200)
	first := segs(t, fs)[0]
	r, _ := db.BeginRead(ctx)
	err := r.Range(ctx, 0, nil, nil, func(k, v []byte) error {
		if err := db.Checkpoint(ctx); err != nil {
			return err
		}
		// A nested operation neither moves nor drops the pin of the outer.
		if _, _, err := r.Get(ctx, 0, k); err != nil {
			return err
		}
		if err := db.Checkpoint(ctx); err != nil {
			return err
		}
		if db.b.Base() != r.State().CommitID || segs(t, fs)[0] != first {
			return fmt.Errorf("base %d, segments %v: a segment pinned by an operation was removed", db.b.Base(), segs(t, fs))
		}
		return errStop
	})
	if err != errStop {
		t.Fatal(err)
	}
	if err := db.Checkpoint(ctx); err != nil || segs(t, fs)[0] != db.walFloor.Load() {
		t.Fatalf("an idle reader pins no segment: %v, segments %v, floor %d", err, segs(t, fs), db.walFloor.Load())
	}
	r.Close()

	// An old reader keeps the floor low; a writer admitted then pins it.
	old, _ := db.BeginRead(ctx)
	fill(t, db, model, 200, 200)
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ := db.BeginWrite(ctx)
	pin := tx.snap.seg.Load()
	old.Close()
	if err := db.Checkpoint(ctx); err != nil || db.walFloor.Load() <= pin || segs(t, fs)[0] != pin {
		t.Fatalf("the writer's pin %d: %v, floor %d, segments %v", pin, err, db.walFloor.Load(), segs(t, fs))
	}
	tx.Rollback()
	if err := db.Checkpoint(ctx); err != nil || segs(t, fs)[0] <= pin {
		t.Fatalf("once the writer ended: %v, segments %v", err, segs(t, fs))
	}
}

var errStop = fmt.Errorf("stop")

// A reservation appended before the group of T and made durable by its
// Sync is skipped by the scan after a checkpoint at T, but its block may
// already be handed out: the superblock carries its watermark.
func TestWatermarkOfEarlierReservation(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	o.IDReservationInitial, o.IDReservationMax = 64, 64
	db := open(t, fs, o)
	tx, _ := db.BeginWrite(ctx)
	for i := 0; i < 40; i++ { // past half of 1..64: 65..128 is requested
		tx.NextID(ctx, KindNode)
	}
	tx.Put(ctx, 0, []byte("a"), []byte("1"))
	if _, err := tx.Commit(ctx); err != nil { // its Sync makes 65..128 durable
		t.Fatal(err)
	}
	tx, _ = db.BeginWrite(ctx)
	var high uint64
	for i := 0; i < 56; i++ { // up to 96, below the half of 65..128
		high, _ = tx.NextID(ctx, KindNode)
	}
	tx.Rollback()
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = open(t, fs, o)
	defer db.Close()
	tx, _ = db.BeginWrite(ctx)
	defer tx.Rollback()
	if id, _ := tx.NextID(ctx, KindNode); id <= high {
		t.Fatalf("identifier %d handed out again (up to %d before the crash)", id, high)
	}
}
