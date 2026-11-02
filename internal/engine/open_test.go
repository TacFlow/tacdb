package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/check"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

var ctx = context.Background()

const name = "db.tacdb"

// testOptions are small enough for many segments in a test.
func testOptions() Options {
	o := DefaultOptions()
	o.Create = true
	o.WALSegmentBytes = 256 << 10
	o.CacheBytes = 1 << 20
	gov, err := govern.New(govern.Config{Limit: 1 << 30})
	if err != nil {
		panic(err)
	}
	o.Governor = gov
	return o
}

func open(t *testing.T, fs vfs.FS, o Options) *DB {
	t.Helper()
	db, err := Open(ctx, fs, name, o)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// put commits key=value in root slot 0 and returns the receipt.
func put(t *testing.T, db *DB, kv ...string) Receipt {
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
	r, err := tx.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// contents reads root slot 0 of a snapshot into a map.
func contents(t *testing.T, rt *ReadTx) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := rt.Range(ctx, 0, nil, nil, func(k, v []byte) error {
		m[string(k)] = string(v)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func read(t *testing.T, db *DB) map[string]string {
	t.Helper()
	rt, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	return contents(t, rt)
}

func isCodeT(t *testing.T, err error, code errs.Code) {
	t.Helper()
	if !isCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// structural runs the checker of M1 over the durable state of a closed
// database, as the main file and the log say after recovery.
func structural(t *testing.T, db *DB) {
	t.Helper()
	rt, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	st := rt.State()
	if _, err := check.Structural(ctx, rt.view, st.Roots, st.PageCount, st.FreePageCount); err != nil {
		t.Fatal(err)
	}
}

func TestCreateReopenAndReadOnly(t *testing.T) {
	fs := vfs.NewMemFS()
	o := testOptions()
	o.Create = false
	if _, err := Open(ctx, fs, name, o); !isCode(err, errs.CodeNotFound) {
		t.Fatalf("opening a missing database: %v", err)
	}
	db := open(t, fs, testOptions())
	if s := db.durable.Load().State; s.CommitID != 0 || s.PageCount != 2 {
		t.Fatalf("a new database: %+v", s)
	}
	r := put(t, db, "a", "1", "b", "2")
	if r.CommitID != 1 || !r.Durable || db.CommitStatus(r.Token) != Durable {
		t.Fatalf("receipt %+v, status %v", r, db.CommitStatus(r.Token))
	}
	if _, err := Open(ctx, fs, name, testOptions()); !isCode(err, errs.CodeLocked) {
		t.Fatalf("a second owner: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal("Close is idempotent")
	}
	db = open(t, fs, testOptions())
	if got := read(t, db); got["a"] != "1" || got["b"] != "2" || len(got) != 2 {
		t.Fatalf("after reopen: %v", got)
	}
	if db.CommitStatus(r.Token) != Unknown {
		t.Fatal("the last checkpoint of Close forgets the tokens it covers")
	}
	structural(t, db)
	db.Close()

	o = testOptions()
	o.Create, o.ReadOnly = false, true
	before, _ := fs.List(name + "-wal")
	ro := open(t, fs, o)
	defer ro.Close()
	if got := read(t, ro); got["a"] != "1" {
		t.Fatalf("read-only: %v", got)
	}
	if _, err := ro.BeginWrite(ctx); !isCode(err, errs.CodeReadOnly) {
		t.Fatalf("BeginWrite read-only: %v", err)
	}
	if after, _ := fs.List(name + "-wal"); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a read-only open changed the log: %v, then %v", before, after)
	}
}

// A durable commit survives a power loss; an unsynced tail does not
// come back as a partial group.
func TestReplayAfterCrash(t *testing.T) {
	for seed := uint64(0); seed < 20; seed++ {
		fs := vfs.NewMemFS()
		db := open(t, fs, testOptions())
		model := map[string]string{}
		var tokens [][16]byte
		for i := 0; i < 30; i++ {
			k, v := fmt.Sprintf("k%02d", i%17), strings.Repeat(string(rune('a'+i%26)), 100+i*37)
			r := put(t, db, k, v)
			model[k] = v
			tokens = append(tokens, r.Token)
		}
		fs.Crash(vfs.CrashOptions{Seed: seed, TornWrites: true, SectorSubset: true, Reorder: true})
		db.Close() // the handles are dead; this releases the lock
		db = open(t, fs, testOptions())
		got := read(t, db)
		if len(got) != len(model) {
			t.Fatalf("seed %d: %d keys, want %d", seed, len(got), len(model))
		}
		for k, v := range model {
			if got[k] != v {
				t.Fatalf("seed %d: key %s lost", seed, k)
			}
		}
		for _, tok := range tokens {
			if db.CommitStatus(tok) != Durable {
				t.Fatalf("seed %d: a durable token is unknown after recovery", seed)
			}
		}
		structural(t, db)
		put(t, db, "after", "crash") // the log continues
		db.Close()
		db = open(t, fs, testOptions())
		if read(t, db)["after"] != "crash" {
			t.Fatal("a commit after recovery was lost")
		}
		db.Close()
	}
}

// REC-12: a log left by a creation that crashed before the main file was
// published is replaced; any other log is not.
func TestOrphanLog(t *testing.T) {
	for _, p := range []vfs.CrashPoint{
		{Op: vfs.OpCreate, Name: name + ".*.tmp", Nth: 1},
		{Op: vfs.OpWrite, Name: name + ".*.tmp", Nth: 1, After: true},
		{Op: vfs.OpRename, Nth: 1},
		{Op: vfs.OpSyncDir, Name: ".", Nth: 2},
	} {
		fs := vfs.NewMemFS()
		fs.InjectCrash(p)
		if _, err := Open(ctx, fs, name, testOptions()); err == nil {
			t.Fatalf("%+v: the crash point did not fire", p)
		}
		fs.Restart()
		db, err := Open(ctx, fs, name, testOptions())
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		put(t, db, "k", "v")
		db.Close()
	}

	// A log with a record and no main file is never replaced.
	fs := vfs.NewMemFS()
	w, err := wal.Create(fs, name+"-wal", [16]byte{1}, wal.Options{SegmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendReserve(wal.Reserve{Kind: KindNode, HighWatermark: 10}); err != nil {
		t.Fatal(err)
	}
	w.Sync()
	w.Close()
	if _, err := Open(ctx, fs, name, testOptions()); !isCode(err, errs.CodeDestinationExists) {
		t.Fatalf("a log with records: %v", err)
	}

	// An existing database without its log needs recovery.
	fs = vfs.NewMemFS()
	db := open(t, fs, testOptions())
	db.Close()
	names, _ := fs.List(name + "-wal")
	for _, n := range names {
		fs.Remove(name + "-wal/" + n)
	}
	fs.Remove(name + "-wal")
	if _, err := Open(ctx, fs, name, testOptions()); !isCode(err, errs.CodeRecoveryRequired) {
		t.Fatalf("a missing log: %v", err)
	}
}

// gateFS blocks the Syncs of log segments while a gate is set, so tests
// can hold a Sync in progress (TX-11, TX-12).
type gateFS struct {
	vfs.FS
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
	fail    error        // the next segment Sync fails with it
	ops     atomic.Int64 // calls that change state, on any file
}

func (g *gateFS) wrap(f vfs.File, name string, err error) (vfs.File, error) {
	if err != nil {
		return nil, err
	}
	return &gateFile{File: f, g: g, name: name}, nil
}

func (g *gateFS) Create(name string) (vfs.File, error) {
	g.ops.Add(1)
	f, err := g.FS.Create(name)
	return g.wrap(f, name, err)
}

func (g *gateFS) Open(name string) (vfs.File, error) {
	f, err := g.FS.Open(name)
	return g.wrap(f, name, err)
}

// hold makes the next segment Syncs wait until release.
func (g *gateFS) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate = make(chan struct{})
	g.entered = make(chan struct{}, 100)
}

func (g *gateFS) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.gate)
	g.gate = nil
}

// next lets the held Syncs go and holds the following ones.
func (g *gateFS) next() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.gate)
	g.gate = make(chan struct{})
}

func (g *gateFS) Rename(oldName, newName string) error {
	g.ops.Add(1)
	return g.FS.Rename(oldName, newName)
}

func (g *gateFS) Remove(name string) error {
	g.ops.Add(1)
	return g.FS.Remove(name)
}

func (g *gateFS) failNext(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = err
}

type gateFile struct {
	vfs.File
	g    *gateFS
	name string
}

func (f *gateFile) wait() error {
	if !strings.HasSuffix(f.name, ".wal") {
		return nil
	}
	f.g.mu.Lock()
	gate, entered, fail := f.g.gate, f.g.entered, f.g.fail
	f.g.fail = nil
	f.g.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
	return fail
}

func (f *gateFile) WriteAt(p []byte, off int64) error {
	f.g.ops.Add(1)
	return f.File.WriteAt(p, off)
}

func (f *gateFile) Truncate(size int64) error {
	f.g.ops.Add(1)
	return f.File.Truncate(size)
}

func (f *gateFile) Sync() error {
	f.g.ops.Add(1)
	if err := f.wait(); err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *gateFile) SyncData() error {
	f.g.ops.Add(1)
	if err := f.wait(); err != nil {
		return err
	}
	return f.File.SyncData()
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func equal(a, b []byte) bool { return bytes.Equal(a, b) }
