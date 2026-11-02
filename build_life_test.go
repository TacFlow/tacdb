package tacdb

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// pendingPurges counts the purges not yet done.
func pendingPurges(db *DB) int {
	var n int
	db.View(bg, func(tx *Tx) error {
		ps, err := index.Purges(bg, tx.r)
		n = len(ps)
		return err
	})
	return n
}

// Readers that fill MaxActiveTransactions while a DropIndex commits make
// the workers' reads fail with TOO_MANY_TRANSACTIONS for a while; the
// workers try again, and the purge ends while the handle stays open.
func TestPurgeUnderReaderPressure(t *testing.T) {
	o := buildOptions()
	o.MaxActiveTransactions = 3
	db := openFS(t, vfs.NewMemFS(), o)
	defer db.Close()
	seed(t, db, 50)
	lost := 0
	for round := range 40 {
		name := "x"
		for {
			err := db.Update(bg, func(tx *Tx) error {
				return tx.CreateIndex(IndexDefinition{Name: name, Scope: "P", Property: "p"})
			})
			if err == nil {
				break
			}
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var spins atomic.Int64
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					db.View(bg, func(tx *Tx) error { spins.Add(1); time.Sleep(20 * time.Microsecond); return nil })
				}
			}()
		}
		for {
			err := db.Update(bg, func(tx *Tx) error { return tx.DropIndex(name) })
			if err == nil {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		close(stop)
		wg.Wait()
		// No reader is left: a live purger finishes quickly.
		deadline := time.Now().Add(2 * time.Second)
		for pendingPurges(db) > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if pendingPurges(db) > 0 {
			lost++
			db.work.mu.Lock()
			running := db.work.purging
			db.work.mu.Unlock()
			t.Logf("round %d: the purge stays pending with the handle idle; purger running: %v", round, running)
			db.work.purge() // a wake drains it: the purge was only abandoned
			waitPurges(t, db)
			break
		}
	}
	if lost > 0 {
		t.Fatalf("a purge was lost while the handle stayed open")
	}
}

// Close failing with BUSY stops the builder and starts it again; a
// BuildIndex waiting for it keeps waiting for the end of the build.
func TestBuildIndexWaitsThroughCloseBusy(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	seed(t, db, 300)
	type res struct {
		st  IndexBuildStatus
		err error
	}
	done := make(chan res, 1)
	go func() {
		st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		done <- res{st, err}
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
	got := <-done
	if got.err == nil && got.st.State == IndexBuilding {
		t.Fatalf("BuildIndex returned before the end of the build: state BUILDING, scanned %d, err nil", got.st.Scanned)
	}
}

// A BuildIndex waiting for a build that DropIndex cancels returns
// INDEX_NOT_FOUND, even when the name was declared again meanwhile.
func TestBuildIndexOfReplacedIndex(t *testing.T) {
	db := openFS(t, vfs.NewMemFS(), buildOptions())
	defer db.Close()
	seed(t, db, 300)
	type res struct {
		st  IndexBuildStatus
		err error
	}
	done := make(chan res, 1)
	go func() {
		st, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
		done <- res{st, err}
	}()
	for {
		if st, err := db.IndexStatus("p"); err == nil && st.Scanned > 5 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	// Drop and declare again in one transaction, with another definition.
	err := db.Update(bg, func(tx *Tx) error {
		if err := tx.DropIndex("p"); err != nil {
			return err
		}
		return tx.CreateIndex(IndexDefinition{Name: "p", Scope: "P", Property: "q"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := <-done
	if code(got.err) != CodeIndexNotFound {
		t.Fatalf("the BuildIndex of the dropped build returned %+v, %v", got.st, got.err)
	}
}

// Close on a read-only handle that fails with BUSY starts no builder
// nor purger.
func TestReadOnlyCloseBusyStartsNoWorkers(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openFS(t, fs, buildOptions())
	seed(t, db, 300)
	go db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
	for {
		if st, err := db.IndexStatus("p"); err == nil && st.Scanned > 5 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	closeEventually(t, db)
	o := buildOptions()
	o.ReadOnly, o.Create = true, false
	ro := openFS(t, fs, o)
	defer ro.Close()
	r, err := ro.BeginRead(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); code(err) != CodeBusy {
		t.Fatalf("Close with a reader: %v", err)
	}
	r.Rollback()
	ro.work.mu.Lock()
	started := ro.work.cancel != nil
	ro.work.mu.Unlock()
	if started {
		t.Fatalf("a read-only handle started its workers after Close failed with BUSY")
	}
}

// Close fails with BUSY when transactions are open, maybe at
// MaxActiveTransactions; the workers start again once they can read, and
// the build ends while the handle stays open.
func TestCloseBusyAtTxLimitResumesBuild(t *testing.T) {
	o := buildOptions()
	o.MaxActiveTransactions = 1
	db := openFS(t, vfs.NewMemFS(), o)
	defer db.Close()
	seed(t, db, 300)
	go func() {
		// With one transaction allowed, the first phase meets the reads
		// of this test and may fail with TOO_MANY_TRANSACTIONS.
		for {
			_, err := db.BuildIndex(bg, IndexDefinition{Name: "p", Scope: "P", Property: "p"})
			if code(err) != CodeTooManyTransactions {
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if st, err := db.IndexStatus("p"); err == nil && st.Scanned > 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the build never started")
		}
		time.Sleep(50 * time.Microsecond)
	}
	for attempt := range 10 {
		var r *Tx
		for {
			var err error
			if r, err = db.BeginRead(bg); err == nil {
				break
			}
		}
		if err := db.Close(); code(err) != CodeBusy {
			t.Fatalf("Close with a reader: %v", err)
		}
		r.Rollback()
		first, _ := db.IndexStatus("p")
		time.Sleep(300 * time.Millisecond)
		st, err := db.IndexStatus("p")
		if err != nil || st.State != IndexBuilding {
			t.Logf("attempt %d: %+v, %v", attempt, st, err)
			return
		}
		if st.Scanned == first.Scanned {
			t.Fatalf("attempt %d: the build is stranded: BUILDING, scanned %d for 300ms with the handle idle", attempt, st.Scanned)
		}
	}
}
