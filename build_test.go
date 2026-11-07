package tacdb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// buildOptions build one entity per batch, so that builds take many
// transactions.
func buildOptions() Options {
	o := options()
	o.IndexBuildBatchBytes = 1
	return o
}

func openFS(t *testing.T, fs vfs.FS, o Options) *DB {
	t.Helper()
	db, err := open(fs, "b.tacdb", o)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// seed creates n nodes labeled P with an Int property p, most distinct.
func seed(t *testing.T, db *DB, n int) []NodeID {
	t.Helper()
	var ids []NodeID
	err := db.Update(bg, func(tx *Tx) error {
		for i := range n {
			p := map[string]value.Value{"p": value.Int(int64(i))}
			if i%7 == 0 {
				p = nil // no property: no entry
			}
			nd, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"P"}, Properties: p})
			if err != nil {
				return err
			}
			ids = append(ids, nd.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// verifyDB checks the graph and every index against a rebuild.
func verifyDB(t *testing.T, db *DB) {
	t.Helper()
	if err := db.View(bg, func(tx *Tx) error { return db.g.Verify(bg, tx.r) }); err != nil {
		t.Fatal(err)
	}
}

// waitPurges waits until no purge is pending.
func waitPurges(t *testing.T, db *DB) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		db.View(bg, func(tx *Tx) error {
			ps, err := index.Purges(bg, tx.r)
			n = len(ps)
			return err
		})
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d purges still pending", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func epoch(t *testing.T, db *DB) uint64 {
	var e uint64
	db.View(bg, func(tx *Tx) error { e = tx.r.State().SchemaEpoch; return nil })
	return e
}

// writers mutate P nodes until stop closes, and count what they did.
func writers(t *testing.T, db *DB, ids []NodeID, n int, stop chan struct{}, wg *sync.WaitGroup, ops *atomic.Int64) {
	for w := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 3))
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := ids[r.IntN(len(ids))]
				err := db.Update(bg, func(tx *Tx) error {
					switch r.IntN(6) {
					case 0:
						return tx.SetProperty(id, "p", value.Int(r.Int64N(1<<40)))
					case 1:
						return tx.RemoveProperty(id, "p")
					case 2:
						return tx.RemoveLabel(id, "P")
					case 3:
						return tx.AddLabel(id, "P")
					case 4:
						_, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"P"}, Properties: map[string]value.Value{"p": value.Float(float64(r.IntN(100)) + 0.5)}})
						return err
					}
					return tx.DeleteNode(id, true)
				})
				if err == nil {
					ops.Add(1)
				}
			}
		}()
	}
}

// IDX-03: a build with writers at work ends ONLINE and equal to a
// rebuild; while it runs the index answers no seek; the schema epoch rises
// in its first and last phases only.
func TestBuildIndexOnline(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	ids := seed(t, db, 400)
	before := epoch(t, db)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var ops atomic.Int64
	writers(t, db, ids, 3, stop, &wg, &ops)
	// Watch the build: BUILDING, invisible to seeks, the epoch still.
	var sawBuilding atomic.Bool
	watch := make(chan struct{})
	go func() {
		defer close(watch)
		for {
			st, err := db.IndexStatus("p")
			if err == nil && st.State != IndexBuilding {
				return
			}
			if err == nil {
				db.View(bg, func(tx *Tx) error {
					set, _ := db.g.Indexes.Get(bg, tx.r)
					if ix, ok := set.Named("p"); ok && ix.State == index.Building {
						if _, err := index.Seek(bg, tx.r, ix, index.Query{Op: index.All}); !errors.Is(err, ErrIndexBuilding) {
							t.Errorf("a seek on a BUILDING index: %v", err)
						}
						if e := tx.r.State().SchemaEpoch; e != before+1 {
							t.Errorf("epoch %d during the build, want %d", e, before+1)
						}
						sawBuilding.Store(true)
					}
					return nil
				})
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
	close(stop)
	wg.Wait()
	<-watch
	if err != nil || st.State != IndexOnline || st.Failure != nil || st.StartedAt == 0 || st.Estimated < 400 {
		t.Fatalf("BuildIndex: %+v, %v", st, err)
	}
	if !sawBuilding.Load() || ops.Load() == 0 {
		t.Fatalf("the build did not overlap the writers: building seen %v, %d writes", sawBuilding.Load(), ops.Load())
	}
	if e := epoch(t, db); e != before+2 {
		t.Fatalf("epoch %d after the build, want %d", e, before+2)
	}
	verifyDB(t, db)
	// Building again the same declaration returns at once.
	if st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"}); err != nil || st.State != IndexOnline {
		t.Fatalf("a second BuildIndex: %+v, %v", st, err)
	}
	if _, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "q"}); code(err) != CodeIndexExistsDifferent {
		t.Fatalf("another declaration: %v", err)
	}
}

// IDX-03: a power loss at any time during a build with writers resumes at
// the next open from the last batch, and ends equal to a rebuild.
func TestBuildIndexCrash(t *testing.T) {
	for i := range 6 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			fs := vfs.NewMemFS()
			db := openFS(t, fs, buildOptions())
			ids := seed(t, db, 300)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var ops atomic.Int64
			writers(t, db, ids, 2, stop, &wg, &ops)
			go db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
			// Crash after some batches: early, mid-way or near the end.
			target := uint64(1 + i*50)
			for {
				st, err := db.IndexStatus("p")
				if err == nil && (st.Scanned >= target || st.State != IndexBuilding) {
					break
				}
				time.Sleep(100 * time.Microsecond)
			}
			fs.Crash(vfs.CrashOptions{Seed: uint64(i), TornWrites: i%2 == 1, SectorSubset: i%2 == 1})
			close(stop)
			wg.Wait()
			db.work.stop()
			db.e.Close()
			fs.Restart()
			db = openFS(t, fs, buildOptions())
			defer db.Close()
			st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
			if err != nil || st.State != IndexOnline {
				t.Fatalf("after the crash: %+v, %v", st, err)
			}
			verifyDB(t, db)
		})
	}
}

// Close stops a build between batches; the next open resumes it.
func TestBuildIndexResumes(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openFS(t, fs, buildOptions())
	seed(t, db, 300)
	ctx, cancel := context.WithTimeout(bg, 5*time.Millisecond)
	st, err := db.BuildIndex(ctx, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
	cancel()
	if code(err) != CodeDeadline || st.State != IndexBuilding {
		t.Fatalf("a build past its context: %+v, %v", st, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openFS(t, fs, buildOptions())
	defer db.Close()
	st, err = db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
	if err != nil || st.State != IndexOnline || st.Scanned == 0 {
		t.Fatalf("after the reopen: %+v, %v", st, err)
	}
	verifyDB(t, db)
}

// IDX-04: a build over duplicates, or with a writer adding one, fails with
// the conflicting pairs; its entries are purged and the declaration stays
// FAILED until dropped.
func TestBuildIndexDuplicates(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	ids := seed(t, db, 50)
	db.Update(bg, func(tx *Tx) error { return tx.SetProperty(ids[3], "p", value.Float(1)) }) // equals Int 1 of ids[1]
	st, err := db.BuildIndex(bg, IndexDefinition{Name: "u", Scope: "P", Property: "p", Unique: true})
	var e *Error
	if err != nil || st.State != IndexFailed || !errors.As(st.Failure, &e) || e.Code != CodeUniqueViolation ||
		len(st.Conflicts) != 1 || st.Conflicts[0] != [2]uint64{uint64(ids[1]), uint64(ids[3])} {
		t.Fatalf("a unique build over duplicates: %+v, %v", st, err)
	}
	waitPurges(t, db)
	verifyDB(t, db)
	if st, _ := db.IndexStatus("u"); st.State != IndexFailed {
		t.Fatalf("the status after the purge: %+v", st)
	}
	if err := db.Update(bg, func(tx *Tx) error { return tx.DropIndex("u") }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.IndexStatus("u"); code(err) != CodeIndexNotFound {
		t.Fatalf("a dropped index: %v", err)
	}
	// A writer that duplicates a value during the build fails it too.
	db.Update(bg, func(tx *Tx) error { return tx.SetProperty(ids[3], "p", value.Int(3)) })
	stop := make(chan struct{})
	go func() {
		for {
			if st, err := db.IndexStatus("u2"); err == nil && st.State == IndexBuilding && st.Scanned > 2 {
				err := db.Update(bg, func(tx *Tx) error {
					_, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"P"}, Properties: map[string]value.Value{"p": value.Int(2)}})
					return err
				})
				if err != nil {
					t.Errorf("a writer duplicating during the build fails: %v", err)
				}
				close(stop)
				return
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()
	st, err = db.BuildIndex(bg, IndexDefinition{Name: "u2", Scope: "P", Property: "p", Unique: true})
	<-stop
	if err != nil || st.State != IndexFailed || st.Failure == nil || len(st.Conflicts) == 0 {
		t.Fatalf("a writer duplicating during the build: %+v, %v", st, err)
	}
	waitPurges(t, db)
	verifyDB(t, db)
}

// A value the index refuses fails the build with its error.
func TestBuildIndexRefusedValue(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	ids := seed(t, db, 20)
	db.Update(bg, func(tx *Tx) error { return tx.SetProperty(ids[5], "p", value.Float(math.NaN())) })
	st, err := db.BuildIndex(bg, IndexDefinition{Name: "n", Scope: "P", Property: "p"})
	var e *Error
	if err != nil || st.State != IndexFailed || !errors.As(st.Failure, &e) || e.Code != CodeInvalidArgument || strings.HasPrefix(e.Message, "tacdb") {
		t.Fatalf("a build over NaN: %+v, %v", st, err)
	}
	waitPurges(t, db)
	verifyDB(t, db)
}

// DropIndex cancels a build: its batches stop, its entries are purged, and
// the name can be built again with a new identifier whose entries never
// meet the old ones.
func TestBuildIndexDrop(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	seed(t, db, 300)
	done := make(chan error)
	go func() {
		_, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		done <- err
	}()
	var old uint64
	for {
		st, err := db.IndexStatus("p")
		if err == nil && st.Scanned > 20 {
			db.View(bg, func(tx *Tx) error {
				set, _ := db.g.Indexes.Get(bg, tx.r)
				ix, _ := set.Named("p")
				old = ix.ID
				return nil
			})
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	if err := db.Update(bg, func(tx *Tx) error { return tx.DropIndex("p") }); err != nil {
		t.Fatal(err)
	}
	if err := <-done; code(err) != CodeIndexNotFound {
		t.Fatalf("BuildIndex of a dropped index: %v", err)
	}
	st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
	if err != nil || st.State != IndexOnline {
		t.Fatalf("the build again: %+v, %v", st, err)
	}
	waitPurges(t, db)
	verifyDB(t, db)
	db.View(bg, func(tx *Tx) error {
		p := codec.AppendKeyUint64(nil, old)
		tx.r.Range(bg, catalog.SlotNodeProperty, p, codec.PrefixEnd(p), func(k, _ []byte) error {
			t.Fatalf("an entry %x of the dropped build", k)
			return nil
		})
		return nil
	})
}

// A handle opened read-only builds nothing; IndexBuildBatchBytes must be
// positive.
func TestBuildIndexOptions(t *testing.T) {
	o := buildOptions()
	o.IndexBuildBatchBytes = 0
	if _, err := OpenMemory(o); code(err) != CodeInvalidArgument {
		t.Fatalf("IndexBuildBatchBytes 0: %v", err)
	}
	db := memory(t)
	if _, err := db.BuildIndex(bg, IndexDefinition{Name: "x", Scope: "P", Property: "p", Kind: 2}); code(err) != CodeInvalidArgument {
		t.Fatalf("a kind: %v", err)
	}
	if _, err := db.IndexStatus("x"); code(err) != CodeIndexNotFound {
		t.Fatalf("the status of a missing index: %v", err)
	}
}

// Close with a transaction open fails with BUSY and the build goes on.
func TestBuildIndexCloseBusy(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	seed(t, db, 200)
	done := make(chan IndexBuildStatus)
	go func() {
		st, _ := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		done <- st
	}()
	for {
		if st, err := db.IndexStatus("p"); err == nil && st.Scanned > 5 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	r, _ := db.BeginRead(bg)
	if err := db.Close(); code(err) != CodeBusy {
		t.Fatalf("Close with a reader: %v", err)
	}
	r.Rollback()
	if st := <-done; st.State != IndexOnline {
		// The first BuildIndex may have seen its build stop; it resumes.
		st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		if err != nil || st.State != IndexOnline {
			t.Fatalf("after BUSY: %+v, %v", st, err)
		}
	}
	verifyDB(t, db)
}

// A purge scheduled while the purger ends its last pass is not lost.
func TestPurgeWake(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	seed(t, db, 30)
	for _, n := range []string{"a", "b"} {
		if err := db.Update(bg, func(tx *Tx) error { return tx.CreateIndex(IndexDefinition{Name: n, Scope: "P", Property: "p"}) }); err != nil {
			t.Fatal(err)
		}
	}
	db.Update(bg, func(tx *Tx) error { return tx.DropIndex("a") })
	waitPurges(t, db)
	// The purger of a may still end its last pass: the hook is for the next.
	for {
		db.work.mu.Lock()
		if !db.work.purging {
			break
		}
		db.work.mu.Unlock()
		time.Sleep(50 * time.Microsecond)
	}
	var once sync.Once
	var hooked atomic.Bool
	defer func() { db.work.mu.Lock(); db.work.emptyHook = nil; db.work.mu.Unlock() }()
	db.work.emptyHook = func() {
		once.Do(func() {
			// Drop b without waking the purger: only the pass in flight
			// can see it, through the request kept for it.
			tx, err := db.e.BeginWrite(bg)
			if err == nil {
				err = tx.Statement("t", func(s *engine.Stmt) error { return db.g.DropIndex(bg, s, "b") })
			}
			if err == nil {
				_, err = tx.Commit(bg)
			}
			if err != nil {
				t.Errorf("the drop inside the hook: %v", err)
			}
			hooked.Store(true)
			db.work.purge()
		})
	}
	db.work.mu.Unlock()
	db.work.purge() // a pass with nothing to purge, which meets the hook
	for !hooked.Load() {
		time.Sleep(50 * time.Microsecond)
	}
	waitPurges(t, db)
	verifyDB(t, db)
}

// A builder stopped by an error of the handle ends the wait of BuildIndex
// with that error, instead of starting again forever.
func TestBuildIndexHandleFails(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openFS(t, fs, buildOptions())
	defer db.Close()
	seed(t, db, 200)
	done := make(chan error)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		_, err := db.BuildIndex(ctx, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		done <- err
	}()
	for {
		if st, err := db.IndexStatus("p"); err == nil && st.Scanned > 3 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	fs.Inject(vfs.Fault{Op: vfs.OpWrite, Name: "b.tacdb-wal/*"}) // every log write fails from now on
	err := <-done
	if err == nil || code(err) == CodeDeadline || code(err) == CodeCanceled {
		t.Fatalf("BuildIndex on a failed handle: %v", err)
	}
}

// A Close that succeeds while BuildIndex waits ends the wait with CLOSED;
// the next open resumes the build.
func TestBuildIndexClosed(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openFS(t, fs, buildOptions())
	seed(t, db, 300)
	done := make(chan error)
	go func() {
		_, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		done <- err
	}()
	for {
		if st, err := db.IndexStatus("p"); err == nil && st.Scanned > 3 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	closeEventually(t, db)
	select {
	case err := <-done:
		if code(err) != CodeClosed {
			t.Fatalf("BuildIndex across Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("BuildIndex still waits after Close")
	}
	db = openFS(t, fs, buildOptions())
	defer db.Close()
	if st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"}); err != nil || st.State != IndexOnline {
		t.Fatalf("after the reopen: %+v, %v", st, err)
	}
}

// closeEventually closes db, trying again while it fails with BUSY: a
// BuildIndex waiting in the background reads the status now and then.
func closeEventually(t *testing.T, db *DB) {
	t.Helper()
	for err := db.Close(); err != nil; err = db.Close() {
		if code(err) != CodeBusy {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Microsecond)
	}
}
