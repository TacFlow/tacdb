package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// commitAsync starts a commit of kv in its own goroutine; the slot is
// free again once it is sealed.
func commitAsync(t *testing.T, db *DB, kv ...string) <-chan result {
	t.Helper()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(kv); i += 2 {
		if err := tx.Put(ctx, 0, []byte(kv[i]), []byte(kv[i+1])); err != nil {
			t.Fatal(err)
		}
	}
	ch := make(chan result, 1)
	go func() {
		r, err := tx.Commit(ctx)
		ch <- result{r, err}
	}()
	waitFor(t, "the seal", func() bool { return tx.state.Load() == txSealed })
	return ch
}

type result struct {
	r   Receipt
	err error
}

func queued(db *DB) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.queue)
}

// TX-11, TX-12 and TX-16: eight intents sealed while a Sync is held form
// one group with one Sync, in the order of their parents and with one
// image per page; a reader admitted meanwhile reads the old snapshot
// without waiting, and the group is published at once after the Sync.
func TestGroupCommit(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	db := open(t, g, testOptions())
	defer db.Close()
	put(t, db, "hot", "0")
	base := db.Stats()
	g.hold()
	first := commitAsync(t, db, "hot", "first")
	<-g.entered // the first group is syncing
	var rest []<-chan result
	for i := 1; i <= 8; i++ {
		rest = append(rest, commitAsync(t, db, "hot", fmt.Sprint(i), fmt.Sprintf("k%d", i), "v"))
	}
	if queued(db) != 8 {
		t.Fatalf("%d intents queued, want 8", queued(db))
	}
	r, err := db.BeginRead(ctx) // TX-12: admitted and read during the Sync
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(t, r); got["hot"] != "0" || len(got) != 1 {
		t.Fatalf("a reader during the Sync: %v", got)
	}
	r.Close()
	g.release()
	f := <-first
	if f.err != nil || !f.r.Durable {
		t.Fatalf("first: %+v", f)
	}
	for i, ch := range rest {
		res := <-ch
		if res.err != nil || !res.r.Durable || res.r.CommitID != f.r.CommitID+uint64(i)+1 {
			t.Fatalf("intent %d: %+v", i+1, res)
		}
		if db.CommitStatus(res.r.Token) != Durable {
			t.Fatalf("intent %d is not durable by token", i+1)
		}
	}
	s := db.Stats()
	if s.Groups-base.Groups != 2 || s.Syncs-base.Syncs != 2 || s.Intents-base.Intents != 9 {
		t.Fatalf("groups %d, syncs %d, intents %d; want 2, 2, 9", s.Groups-base.Groups, s.Syncs-base.Syncs, s.Intents-base.Intents)
	}
	if s.CoalescedPages == 0 {
		t.Fatal("the hot page was appended more than once in the group")
	}
	if got := read(t, db); got["hot"] != "8" || got["k8"] != "v" || len(got) != 9 {
		t.Fatalf("after the group: %v", got)
	}
	structural(t, db)
	// One PAGE per PageID in every group of the log.
	res, err := wal.Scan(ctx, wal.ScanInput{FS: g, Dir: name + "-wal", UUID: db.uuid, FloorSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, sg := range res.Groups {
		seen := map[page.ID]bool{}
		for _, pr := range sg.Pages {
			if seen[pr.ID] {
				t.Fatalf("group %d appends page %d twice", sg.ID, pr.ID)
			}
			seen[pr.ID] = true
		}
	}
	if n := len(res.Groups[len(res.Groups)-1].Begins); n != 8 {
		t.Fatalf("the last group has %d intents, want 8", n)
	}
}

// TX-16: three intents that change the same page form a coalesced group;
// a crash before its Sync recovers the state before it, and after, the
// whole group.
func TestGroupCrash(t *testing.T) {
	for _, synced := range []bool{false, true} {
		mem := vfs.NewMemFS()
		g := &gateFS{FS: mem}
		db := open(t, g, testOptions())
		put(t, db, "hot", "0")
		g.hold()
		first := commitAsync(t, db, "hot", "1") // group 1, alone
		<-g.entered
		var rest []<-chan result
		for i := 2; i <= 4; i++ {
			rest = append(rest, commitAsync(t, db, "hot", fmt.Sprint(i), fmt.Sprintf("k%d", i), "v"))
		}
		g.next() // group 1 is synced; the Sync of group 2 is held
		if res := <-first; res.err != nil {
			t.Fatal(res.err)
		}
		<-g.entered
		if s := db.Stats(); s.CoalescedPages == 0 {
			t.Fatal("group 2 did not coalesce the hot page")
		}
		var tokens [][16]byte
		if synced {
			g.release()
			for _, ch := range rest {
				res := <-ch
				if res.err != nil {
					t.Fatal(res.err)
				}
				tokens = append(tokens, res.r.Token)
			}
			mem.Crash(vfs.CrashOptions{Seed: 1, TornWrites: true, Reorder: true})
		} else {
			mem.Crash(vfs.CrashOptions{Seed: 1, TornWrites: true, Reorder: true})
			g.release()
			for _, ch := range rest {
				res := <-ch
				if !isCode(res.err, errs.CodeCommitUnknown) {
					t.Fatalf("a commit lost in the crash: %v", res.err)
				}
				tokens = append(tokens, res.r.Token)
			}
		}
		db.Close()
		db = open(t, g, testOptions())
		got := read(t, db)
		switch {
		case synced && (got["hot"] != "4" || len(got) != 4):
			t.Fatalf("after a synced group: %v", got)
		case !synced && (got["hot"] != "1" || len(got) != 1):
			t.Fatalf("after a group lost before its Sync: %v", got)
		}
		for _, tok := range tokens {
			if want := map[bool]Status{true: Durable, false: Unknown}[synced]; db.CommitStatus(tok) != want {
				t.Fatalf("synced=%v: status %v, want %v", synced, db.CommitStatus(tok), want)
			}
		}
		structural(t, db)
		db.Close()
	}
}

// TX-13: writer B derives from A, sealed; the Sync of A fails. Nothing of
// B succeeds, the handle needs recovery, and reopening recovers a prefix.
func TestSyncFailure(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	db := open(t, g, testOptions())
	put(t, db, "base", "1")
	g.failNext(errs.New(errs.CodeIO, "test", "fsync failed"))
	g.hold()
	a := commitAsync(t, db, "a", "1")
	<-g.entered
	b, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := b.Get(ctx, 0, []byte("a")); string(v) != "1" {
		t.Fatal("B must derive from the sealed A")
	}
	b.Put(ctx, 0, []byte("b"), []byte("1"))
	g.release()
	if res := <-a; !isCode(res.err, errs.CodeCommitUnknown) {
		t.Fatalf("A after a failed Sync: %v", res.err)
	} else if db.CommitStatus(res.r.Token) != Unknown {
		t.Fatal("the token of A must be unknown")
	}
	if _, err := b.Commit(ctx); !isCode(err, errs.CodeRecoveryRequired) {
		t.Fatalf("B after the failure: %v", err)
	}
	if _, err := db.BeginRead(ctx); !isCode(err, errs.CodeRecoveryRequired) {
		t.Fatalf("a Begin after the failure: %v", err)
	}
	if _, err := db.BeginWrite(ctx); !isCode(err, errs.CodeRecoveryRequired) {
		t.Fatalf("a writer after the failure: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open(t, g, testOptions())
	defer db.Close()
	got := read(t, db)
	if got["base"] != "1" || got["b"] != "" || (got["a"] != "" && got["a"] != "1") {
		t.Fatalf("not a prefix: %v", got)
	}
}

// Async: commits are published once appended with Durable=false; Flush
// makes them durable; a crash loses at most a suffix.
func TestAsync(t *testing.T) {
	mem := vfs.NewMemFS()
	o := testOptions()
	o.Durability = Async
	o.AsyncSyncInterval = time.Hour // only Flush syncs
	db := open(t, mem, o)
	r := put(t, db, "a", "1")
	if r.Durable || db.CommitStatus(r.Token) != Volatile || read(t, db)["a"] != "1" {
		t.Fatalf("an async commit: %+v, %v", r, db.CommitStatus(r.Token))
	}
	if rd, _ := db.BeginReadDurable(ctx); contents(t, rd)["a"] != "" {
		t.Fatal("BeginReadDurable sees a volatile commit")
	} else {
		rd.Close()
	}
	if err := db.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if db.CommitStatus(r.Token) != Durable {
		t.Fatal("Flush did not make the commit durable")
	}
	for i := 0; i < 20; i++ {
		put(t, db, fmt.Sprintf("k%02d", i), "v")
	}
	mem.Crash(vfs.CrashOptions{Seed: 7, TornWrites: true, Reorder: true})
	db.Close()
	db = open(t, mem, o)
	defer db.Close()
	got := read(t, db)
	if got["a"] != "1" {
		t.Fatal("a flushed commit was lost")
	}
	for i := 1; i < 20; i++ { // a prefix: a later commit never without the earlier
		if got[fmt.Sprintf("k%02d", i)] != "" && got[fmt.Sprintf("k%02d", i-1)] == "" {
			t.Fatalf("commit %d recovered without %d", i, i-1)
		}
	}
	structural(t, db)
}

// TX-05 and TX-18: identifiers are never handed out twice, across
// rollbacks and crashes; blocks grow under pressure and are reserved ahead,
// so dedicated Syncs stay rare.
func TestIdentifiers(t *testing.T) {
	mem := vfs.NewMemFS()
	db := open(t, mem, testOptions())
	seen := map[uint64]bool{}
	var last uint64
	take := func(tx *WriteTx, n int) {
		for i := 0; i < n; i++ {
			id, err := tx.NextID(ctx, KindNode)
			if err != nil {
				t.Fatal(err)
			}
			if seen[id] || id <= last {
				t.Fatalf("identifier %d after %d, seen %v", id, last, seen[id])
			}
			seen[id], last = true, id
		}
	}
	tx, _ := db.BeginWrite(ctx)
	take(tx, 10)
	tx.Rollback() // TX-05: a rollback returns nothing
	for c := 0; c < 200; c++ {
		tx, _ := db.BeginWrite(ctx)
		take(tx, 100)
		tx.Put(ctx, 0, []byte("n"), []byte(fmt.Sprint(c)))
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s := db.Stats()
	if s.IDReservationSyncs > 2 {
		t.Fatalf("%d dedicated Syncs for identifiers", s.IDReservationSyncs)
	}
	if size := db.ids.kinds[KindNode].size; size <= db.opts.IDReservationInitial {
		t.Fatalf("blocks did not grow under pressure: %d", size)
	}
	tx, _ = db.BeginWrite(ctx)
	take(tx, 20000) // past the watermark of the last commit: blocks reserved after it
	mem.Crash(vfs.CrashOptions{Seed: 3, TornWrites: true})
	tx.Rollback()
	db.Close()
	db = open(t, mem, testOptions())
	defer db.Close()
	tx, _ = db.BeginWrite(ctx)
	take(tx, 10)
	tx.Rollback()
	if _, err := tx.NextID(ctx, 3); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("NextID after Rollback: %v", err)
	}
}

// TX-17: an isolated writer in GroupSync pays exactly one Sync per commit
// and never waits for a window.
func TestIsolatedWriter(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	base := db.Stats()
	for i := 0; i < 50; i++ {
		put(t, db, fmt.Sprintf("k%d", i), "v")
	}
	s := db.Stats()
	if s.Syncs-base.Syncs != 50 || s.Groups-base.Groups != 50 {
		t.Fatalf("%d Syncs and %d groups for 50 isolated commits", s.Syncs-base.Syncs, s.Groups-base.Groups)
	}
}

// In Sync mode each intent is its own group, even while a Sync is held.
func TestSyncModeGroups(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	o := testOptions()
	o.Durability = Sync
	db := open(t, g, o)
	defer db.Close()
	base := db.Stats()
	g.hold()
	first := commitAsync(t, db, "hot", "0")
	<-g.entered
	var rest []<-chan result
	for i := 1; i <= 3; i++ {
		rest = append(rest, commitAsync(t, db, "hot", fmt.Sprint(i)))
	}
	g.release()
	for _, ch := range append([]<-chan result{first}, rest...) {
		if res := <-ch; res.err != nil || !res.r.Durable {
			t.Fatalf("%+v", res)
		}
	}
	if s := db.Stats(); s.Groups-base.Groups != 4 || s.Syncs-base.Syncs != 4 || s.CoalescedPages != 0 {
		t.Fatalf("Sync mode: %d groups, %d Syncs, %d coalesced", s.Groups-base.Groups, s.Syncs-base.Syncs, s.CoalescedPages)
	}
}

// The queue of commits not yet durable is bounded (SPEC §9.2): TryCommit
// gets COMMIT_QUEUE_FULL, Commit waits under the caller's deadline, and in
// both cases the transaction is not sealed. A commit past MaxWALBytes gets
// WAL_LIMIT.
func TestCommitQueueAndWALLimit(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	o := testOptions()
	o.MaxPendingCommits = 2
	db := open(t, g, o)
	defer db.Close()
	g.hold()
	a := commitAsync(t, db, "a", "1")
	<-g.entered
	b := commitAsync(t, db, "b", "1")
	tx, _ := db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("c"), []byte("1"))
	if _, err := tx.TryCommit(ctx); !isCode(err, errs.CodeCommitQueueFull) {
		t.Fatalf("TryCommit with a full queue: %v", err)
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := tx.Commit(dctx); !isCode(err, errs.CodeDeadline) {
		t.Fatalf("Commit waiting for room: %v", err)
	}
	if tx.state.Load() != txActive {
		t.Fatal("a refused commit sealed the transaction")
	}
	done := make(chan result, 1)
	go func() { r, err := tx.Commit(ctx); done <- result{r, err} }()
	g.release()
	for _, ch := range []<-chan result{a, b, done} {
		if res := <-ch; res.err != nil {
			t.Fatal(res.err)
		}
	}
	if got := read(t, db); len(got) != 3 {
		t.Fatalf("after the queue drained: %v", got)
	}

	db.opts.MaxWALBytes = db.co.walBytes(db) + 1000
	tx, _ = db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("d"), []byte("1"))
	if _, err := tx.Commit(ctx); !isCode(err, errs.CodeWALLimit) {
		t.Fatalf("past MaxWALBytes: %v", err)
	}
	tx.Rollback()
}

// Async: a Flush made while a Sync runs is not lost when that Sync ends
// (the commit sealed meanwhile becomes durable without a tick).
func TestAsyncFlushDuringSync(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	o := testOptions()
	o.Durability = Async
	o.AsyncSyncInterval = time.Hour
	db := open(t, g, o)
	defer db.Close()
	put(t, db, "a", "1")
	g.hold()
	f1 := make(chan error, 1)
	go func() { f1 <- db.Flush(ctx) }()
	<-g.entered
	c2 := commitAsync(t, db, "b", "1")
	f2 := make(chan error, 1)
	go func() { f2 <- db.Flush(ctx) }()
	waitFor(t, "the second Flush", func() bool { return db.co.flush.Load() })
	g.release()
	if err := <-f1; err != nil {
		t.Fatal(err)
	}
	if r := <-c2; r.err != nil {
		t.Fatal(r.err)
	}
	select {
	case err := <-f2:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Flush made during a Sync was lost")
	}
}

// Async: a commit waiting for room in a full queue asks for the Sync that
// frees it, by count and by bytes.
func TestAsyncFullQueueSyncs(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		o := testOptions()
		o.Durability = Async
		o.AsyncSyncInterval = time.Hour
		if byBytes {
			o.MaxPendingCommitBytes = 3 * (page.Size + wal.RecordHeaderSize + intentOverhead)
			o.AsyncSyncBytes = o.MaxPendingCommitBytes
		} else {
			o.MaxPendingCommits = 4
		}
		db := open(t, vfs.NewMemFS(), o)
		for i := 0; i < 10; i++ {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			tx, _ := db.BeginWrite(ctx)
			tx.Put(ctx, 0, []byte(fmt.Sprint(i)), []byte("v"))
			if _, err := tx.Commit(c); err != nil {
				t.Fatalf("byBytes=%v: commit %d: %v", byBytes, i, err)
			}
			cancel()
		}
		db.Close()
	}
	o := testOptions()
	o.AsyncSyncBytes = o.MaxPendingCommitBytes + 1
	if _, err := Open(ctx, vfs.NewMemFS(), name, o); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("AsyncSyncBytes above MaxPendingCommitBytes: %v", err)
	}
}

// MaxWALBytes counts a commit once: a group appended and waiting for its
// Sync is in the log already, not also in the queue.
func TestWALLimitCountsOnce(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	db := open(t, g, testOptions())
	defer db.Close()
	g.hold()
	first := commitAsync(t, db, "big", string(make([]byte, 40<<10)))
	<-g.entered // appended: in walBytes, still pending
	db.mu.Lock()
	db.opts.MaxWALBytes = db.co.walBytes(db) + db.pendBytes/2
	db.mu.Unlock()
	tx, _ := db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("small"), []byte("v"))
	done := make(chan result, 1)
	go func() { r, err := tx.Commit(ctx); done <- result{r, err} }()
	waitFor(t, "the seal", func() bool { return tx.state.Load() == txSealed || queued(db) > 0 || len(done) > 0 })
	g.release()
	if res := <-first; res.err != nil {
		t.Fatal(res.err)
	}
	if res := <-done; res.err != nil {
		t.Fatalf("a commit that fits was refused: %v", res.err)
	}
}
