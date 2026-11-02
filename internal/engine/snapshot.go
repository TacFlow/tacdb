package engine

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/versions"
	"github.com/TacFlow/tacdb/internal/wal"
)

// snapshot is a registered reader or writer (design §4.3). It registers
// first, with a provisional floor, the base T in force; then it loads its
// head. A checkpointer loads
// durableHead first and then scans the registry: a registration the scan
// missed happened after it, and the head that reader loads is at or
// beyond the durable head the checkpointer read.
//
// A version resolved to the log before a checkpoint publishes a new base
// may be read after it, so a snapshot pins the log floor in force while
// it can resolve: a reader during each operation, a writer for its life.
// An idle reader pins nothing beyond what its floor commit keeps.
type snapshot struct {
	base   uint64                             // the base T when it registered: the provisional floor
	seg    atomic.Uint64                      // the log floor segment it pins; noPin when idle
	dir    atomic.Pointer[versions.Directory] // its floor once loaded; nil while provisional
	writer bool                               // a writer is never revoked: MaxWriteTxDuration bounds it
	began  time.Time
	shard  int

	// Revocation is cooperative (SPEC §10.1): a revoked snapshot fails its
	// next operation, and leaves the registry once the operation in
	// progress ends.
	mu      sync.Mutex
	ops     int
	revoked bool
	gone    bool // removed from the registry
}

// floor is the oldest commit the snapshot may read through.
func (s *snapshot) floor() uint64 {
	if d := s.dir.Load(); d != nil {
		return d.State.CommitID
	}
	return s.base
}

// noPin is the seg of a snapshot that pins no segment.
const noPin = math.MaxUint64

const regShards = 16

// registry holds the snapshots in shards, so that Begin and the end of a
// read take no global lock.
type registry struct {
	next   atomic.Uint32
	shards [regShards]struct {
		mu sync.Mutex
		m  map[*snapshot]struct{}
		_  [40]byte // keep shards on separate cache lines
	}
}

func (r *registry) register(s *snapshot) {
	s.shard = int(r.next.Add(1) % regShards)
	sh := &r.shards[s.shard]
	sh.mu.Lock()
	if sh.m == nil {
		sh.m = map[*snapshot]struct{}{}
	}
	sh.m[s] = struct{}{}
	sh.mu.Unlock()
}

func (r *registry) remove(s *snapshot) {
	sh := &r.shards[s.shard]
	sh.mu.Lock()
	delete(sh.m, s)
	sh.mu.Unlock()
}

// all returns the registered snapshots.
func (r *registry) all() []*snapshot {
	var out []*snapshot
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.Lock()
		for s := range sh.m {
			out = append(out, s)
		}
		sh.mu.Unlock()
	}
	return out
}

// leave removes s from the registry once. s.mu is held.
func (s *snapshot) leaveLocked(r *registry) {
	if !s.gone {
		s.gone = true
		r.remove(s)
	}
}

// enterOp starts an operation of the snapshot, unless it was revoked. The
// outermost operation (a callback may nest another) pins floor before any
// version is resolved.
func (s *snapshot) enterOp(op string, floor func() uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		return errs.New(errs.CodeSnapshotExpired, op, "the snapshot was revoked")
	}
	if s.ops == 0 {
		s.seg.Store(floor())
	}
	s.ops++
	return nil
}

// exitOp ends an operation. The outermost one unpins, and a snapshot
// revoked meanwhile leaves now.
func (s *snapshot) exitOp(r *registry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ops--; s.ops == 0 {
		s.seg.Store(noPin)
		if s.revoked {
			s.leaveLocked(r)
		}
	}
}

// revoke marks s revoked and reports whether it was live. It leaves the
// registry at once if no operation is in progress.
func (s *snapshot) revoke(r *registry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked || s.gone {
		return false
	}
	s.revoked = true
	if s.ops == 0 {
		s.leaveLocked(r)
	}
	return true
}

// target is the commit a checkpoint may write (SPEC §11.1): the durable
// head, or the oldest snapshot if older. The durable head is loaded
// before the registry is scanned. dir is nil when the target is a
// provisional floor, the base itself: there is nothing to write then.
func (db *DB) target() (commit uint64, dir *versions.Directory) {
	dir = db.durable.Load()
	commit = dir.State.CommitID
	for _, s := range db.reg.all() {
		if f := s.floor(); f < commit {
			commit, dir = f, s.dir.Load()
			if dir == nil || dir.State.CommitID != f { // provisional, or loaded meanwhile
				dir = nil
			}
		}
	}
	return commit, dir
}

// checkpointTarget returns the commit of target.
func (db *DB) checkpointTarget() uint64 {
	c, _ := db.target()
	return c
}

// enter counts a new transaction against MaxActiveTransactions.
func (db *DB) enter(op string) error {
	for {
		n := db.active.Load()
		if n >= int64(db.opts.MaxActiveTransactions) {
			return errs.Newf(errs.CodeTooManyTransactions, op, "%d transactions open", n).
				With("limit", int64(db.opts.MaxActiveTransactions))
		}
		if db.active.CompareAndSwap(n, n+1) {
			if db.closing.Load() { // CloseIdle may have counted before this
				db.active.Add(-1)
				return errs.New(errs.CodeShuttingDown, op, "database closing")
			}
			return nil
		}
	}
}

// ReadTx is a read-only transaction over one snapshot. It never appends
// to the log, reserves IDs or syncs (SPEC §10.1). A ReadTx is used by one
// goroutine at a time.
type ReadTx struct {
	db      *DB
	snap    *snapshot
	dir     *versions.Directory
	view    *versions.View
	pins    *cache.Pins
	durable bool
	closed  bool
}

// BeginRead opens a snapshot of the visible head.
func (db *DB) BeginRead(ctx context.Context) (*ReadTx, error) {
	return db.beginRead(ctx, &db.visible, "engine.BeginRead")
}

// BeginReadDurable opens a snapshot of the durable head.
func (db *DB) BeginReadDurable(ctx context.Context) (*ReadTx, error) {
	return db.beginRead(ctx, &db.durable, "engine.BeginReadDurable")
}

func (db *DB) beginRead(ctx context.Context, head *atomic.Pointer[versions.Directory], op string) (*ReadTx, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctxErr(err, op)
	}
	if err := db.usable(op); err != nil {
		return nil, err
	}
	if err := db.enter(op); err != nil {
		return nil, err
	}
	s := &snapshot{base: db.b.Base(), began: db.opts.Now()}
	s.seg.Store(noPin)
	db.reg.register(s)
	if h := db.hooks.afterRegister; h != nil {
		h()
	}
	d := head.Load()
	s.dir.Store(d)
	return &ReadTx{db: db, snap: s, dir: d, view: db.b.Snapshot(d), pins: cache.NewPins(0),
		durable: d.State.CommitID <= db.durable.Load().State.CommitID}, nil
}

// begin starts an operation: the transaction is open, the snapshot not
// revoked and not older than MaxReadTxAge. The caller ends it with
// t.snap.exitOp.
func (t *ReadTx) begin(op string) error {
	if t.closed {
		return errs.New(errs.CodeTxClosed, op, "transaction closed")
	}
	if age := t.db.opts.Now().Sub(t.snap.began); age > t.db.opts.MaxReadTxAge {
		for _, ev := range t.db.revoke([]*snapshot{t.snap}, EventSnapshotExpired, nil) {
			t.db.emit(ev)
		}
	}
	return t.snap.enterOp(op, t.db.walFloor.Load)
}

// State returns the state of the snapshot.
func (t *ReadTx) State() wal.State { return t.dir.State }

// Durable reports whether the snapshot was durable when it began.
func (t *ReadTx) Durable() bool { return t.durable }

// Get reads key in the tree of root slot.
func (t *ReadTx) Get(ctx context.Context, slot int, key []byte) ([]byte, bool, error) {
	if err := checkSlot(slot); err != nil {
		return nil, false, err
	}
	if err := t.begin("engine.Get"); err != nil {
		return nil, false, err
	}
	defer t.snap.exitOp(&t.db.reg)
	return btree.Get(ctx, t.view, t.pins, t.dir.State.Roots[slot], key)
}

// Range calls fn for the keys in [lo, hi) of the tree of root slot; nil
// bounds are open.
func (t *ReadTx) Range(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error {
	if err := checkSlot(slot); err != nil {
		return err
	}
	if err := t.begin("engine.Range"); err != nil {
		return err
	}
	defer t.snap.exitOp(&t.db.reg)
	return btree.Range(ctx, t.view, t.pins, t.dir.State.Roots[slot], lo, hi, fn)
}

// Check reports whether the transaction can still be used: open, its
// snapshot neither revoked nor older than MaxReadTxAge.
func (t *ReadTx) Check() error {
	if err := t.begin("engine.Check"); err != nil {
		return err
	}
	t.snap.exitOp(&t.db.reg)
	return nil
}

// Close ends the transaction; it is idempotent and does no I/O.
func (t *ReadTx) Close() error {
	if !t.closed {
		t.closed = true
		t.snap.mu.Lock()
		t.snap.leaveLocked(&t.db.reg)
		t.snap.mu.Unlock()
		t.db.active.Add(-1)
	}
	return nil
}

func checkSlot(slot int) error {
	if slot < 0 || slot >= page.UsedRoots {
		return errs.Newf(errs.CodeInvalidArgument, "engine", "root slot %d outside [0, %d)", slot, page.UsedRoots)
	}
	return nil
}
