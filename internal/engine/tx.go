package engine

import (
	"context"
	"crypto/rand"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/alloc"
	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/versions"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Transaction states (SPEC §10.3). A sealed transaction belongs to the
// coordinator; an aborted or expired one only accepts Rollback.
const (
	txActive int32 = iota
	txSealed
	txAborted
	txExpired // MaxWriteTxDuration passed
)

// Receipt is the answer of a commit (SPEC §9.2).
type Receipt struct {
	CommitID uint64
	Token    [16]byte
	Durable  bool
}

// WriteTx is the transaction that holds the writer slot. It works on a
// private overlay over stagedHead; trees opened with Tree share it. A
// WriteTx is used by one goroutine at a time.
type WriteTx struct {
	db    *DB
	snap  *snapshot
	base  *versions.Directory
	view  *versions.View
	alloc *alloc.Allocator
	res   *govern.Reservation // the overlay of the transaction
	pins  *cache.Pins
	trees map[int]*btree.Tree
	saves []map[int]page.ID // roots of the open trees at each savepoint
	stmts []int             // savepoints of the statements in progress, innermost last
	state atomic.Int32
	timer *time.Timer
	in    *intent // once sealed
}

// newWriteTx registers the writer like a snapshot, then derives from
// stagedHead. Its floor is the durable head, at or below its base: the
// checkpoint may only write commits that are durable. db.mu is held, so
// stagedHead does not move.
func (db *DB) newWriteTx() *WriteTx {
	s := &snapshot{base: db.b.Base(), writer: true, began: db.opts.Now()}
	s.seg.Store(db.walFloor.Load())
	db.reg.register(s)
	base := db.staged.Load()
	s.dir.Store(db.durable.Load())
	gov := db.opts.Governor
	if gov == nil {
		gov = govern.Default()
	}
	res, _ := gov.Reserve(context.Background(), govern.Overlay, 0) // zero bytes never waits
	tx := &WriteTx{db: db, snap: s, base: base, res: res, pins: cache.NewPins(0), trees: map[int]*btree.Tree{}}
	tx.view = db.b.Writer(base, res)
	tx.alloc = alloc.New(tx.view)
	tx.timer = time.AfterFunc(db.opts.MaxWriteTxDuration, tx.expire)
	return tx
}

// expire undoes a transaction that held the slot too long: the queue
// moves on, and its next operation gets WRITE_TX_TIMEOUT. Intents already
// sealed are not affected.
func (tx *WriteTx) expire() {
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	if tx.state.Load() == txActive {
		tx.endLocked(txExpired)
	}
}

// endLocked ends an active transaction without sealing. db.mu is held.
func (tx *WriteTx) endLocked(state int32) {
	tx.state.Store(state)
	tx.timer.Stop()
	tx.res.Close()
	tx.db.releaseWriterLocked(tx)
}

func (tx *WriteTx) check(op string) error {
	switch tx.state.Load() {
	case txActive:
		return nil
	case txExpired:
		return errs.New(errs.CodeWriteTxTimeout, op, "the transaction passed MaxWriteTxDuration and was rolled back")
	}
	return errs.New(errs.CodeTxClosed, op, "transaction closed")
}

// Base returns the state the transaction derives from.
func (tx *WriteTx) Base() wal.State { return tx.base.State }

// Tree returns the tree of root slot, over the overlay of the transaction.
func (tx *WriteTx) Tree(slot int) (*btree.Tree, error) {
	if err := tx.check("engine.Tree"); err != nil {
		return nil, err
	}
	if err := checkSlot(slot); err != nil {
		return nil, err
	}
	t, ok := tx.trees[slot]
	if !ok {
		t = btree.New(tx.view, tx.alloc, tx.view.Root(slot))
		tx.trees[slot] = t
	}
	return t, nil
}

// Pins returns the pin budget of the transaction's reads.
func (tx *WriteTx) Pins() *cache.Pins { return tx.pins }

// Get reads key, with the writes of the transaction.
func (tx *WriteTx) Get(ctx context.Context, slot int, key []byte) ([]byte, bool, error) {
	t, err := tx.Tree(slot)
	if err != nil {
		return nil, false, err
	}
	return t.Get(ctx, tx.pins, key)
}

// Put writes key as one statement: on error, the transaction is back to
// where it was before the call (SPEC §10.4).
func (tx *WriteTx) Put(ctx context.Context, slot int, key, value []byte) error {
	return tx.Statement("engine.Put", func(s *Stmt) error { return s.Put(ctx, slot, key, value) })
}

// Delete removes key as one statement and reports whether it existed.
func (tx *WriteTx) Delete(ctx context.Context, slot int, key []byte) (bool, error) {
	var found bool
	err := tx.Statement("engine.Delete", func(s *Stmt) error {
		var err error
		found, err = s.Delete(ctx, slot, key)
		return err
	})
	return found, err
}

// Savepoint marks the current state in O(1) (SPEC §10.4, TX-20).
func (tx *WriteTx) Savepoint() (int, error) {
	if err := tx.check("engine.Savepoint"); err != nil {
		return 0, err
	}
	roots := make(map[int]page.ID, len(tx.trees))
	for slot, t := range tx.trees {
		roots[slot] = t.Root()
	}
	tx.saves = append(tx.saves, roots)
	return tx.view.Savepoint(), nil
}

// RollbackTo restores savepoint sp, which stays active. Inside a
// statement, a savepoint older than the statement's own is out of reach.
func (tx *WriteTx) RollbackTo(sp int) error {
	if n := len(tx.stmts); n > 0 && sp < tx.stmts[n-1] {
		return errs.Newf(errs.CodeInvalidArgument, "engine.RollbackTo", "savepoint %d is older than the statement in progress", sp)
	}
	return tx.rollbackTo(sp)
}

func (tx *WriteTx) rollbackTo(sp int) error {
	if err := tx.check("engine.RollbackTo"); err != nil {
		return err
	}
	if sp < 0 || sp >= len(tx.saves) {
		return errs.Newf(errs.CodeInvalidArgument, "engine.RollbackTo", "no savepoint %d", sp)
	}
	if err := tx.view.RollbackTo(sp); err != nil {
		return err
	}
	tx.saves = tx.saves[:sp+1]
	for slot, t := range tx.trees {
		if r, ok := tx.saves[sp][slot]; ok {
			t.Reset(r)
		} else {
			t.Reset(tx.view.Root(slot)) // opened after the savepoint
		}
	}
	return nil
}

// Release drops savepoint sp and the later ones. Inside a statement, the
// statement's own savepoint and older ones are out of reach.
func (tx *WriteTx) Release(sp int) {
	if n := len(tx.stmts); n > 0 && sp <= tx.stmts[n-1] {
		return
	}
	tx.release(sp)
}

func (tx *WriteTx) release(sp int) {
	if tx.check("engine.Release") != nil || sp < 0 || sp >= len(tx.saves) {
		return
	}
	tx.view.Release(sp)
	tx.saves = tx.saves[:sp]
}

// NextID returns a new node (kind 1) or edge (kind 2) identifier. It is
// never handed out again, not even after a rollback (SPEC §9.3).
func (tx *WriteTx) NextID(ctx context.Context, kind int) (uint64, error) {
	if err := tx.check("engine.NextID"); err != nil {
		return 0, err
	}
	return tx.db.ids.next(ctx, kind)
}

// Rollback ends the transaction without effect. After a commit sealed it,
// it returns COMMIT_PENDING while the commit is pending and TX_CLOSED
// after; it is idempotent after an abort.
func (tx *WriteTx) Rollback() error {
	db := tx.db
	db.mu.Lock()
	defer db.mu.Unlock()
	switch tx.state.Load() {
	case txActive:
		tx.endLocked(txAborted)
	case txSealed:
		select {
		case <-tx.in.done:
			return errs.New(errs.CodeTxClosed, "engine.Rollback", "the transaction was sealed and its commit has ended")
		default:
			return errs.New(errs.CodeCommitPending, "engine.Rollback", "the commit is sealed and pending")
		}
	}
	return nil
}

// Commit seals the transaction and waits as the durability mode says
// (SPEC §9.2). Before it seals, a failure leaves the transaction active;
// after, canceling the wait returns COMMIT_PENDING with the token.
func (tx *WriteTx) Commit(ctx context.Context) (Receipt, error) { return tx.commit(ctx, true) }

// TryCommit is Commit without waiting for room in the commit queue: a full
// queue gives COMMIT_QUEUE_FULL.
func (tx *WriteTx) TryCommit(ctx context.Context) (Receipt, error) { return tx.commit(ctx, false) }

// counted reports whether the statements changed the counters of the
// state: counts, schema epoch or symbol watermark.
func (tx *WriteTx) counted() bool {
	st, b := tx.view.State(), &tx.base.State
	return st.NodeCount != b.NodeCount || st.EdgeCount != b.EdgeCount ||
		st.SchemaEpoch != b.SchemaEpoch || st.SymbolHWM != b.SymbolHWM
}

// intentOverhead is what an intent costs beyond its pages: BEGIN, its
// share of COMMIT, receipt and bookkeeping.
const intentOverhead = 1024

func (tx *WriteTx) commit(ctx context.Context, wait bool) (Receipt, error) {
	const op = "engine.Commit"
	if err := tx.check(op); err != nil {
		return Receipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, ctxErr(err, op)
	}
	if len(tx.stmts) > 0 {
		return Receipt{}, errs.New(errs.CodeInvalidArgument, op, "commit inside a statement")
	}
	db := tx.db
	parent := tx.base.State.CommitID
	if tx.view.Dirty() == 0 && tx.counted() {
		// Counts, epoch and watermarks only change with the records they
		// describe: alone, they would be dropped by a commit without pages.
		return Receipt{}, errs.New(errs.CodeInvalidArgument, op, "the state changed without any page written")
	}
	if tx.view.Dirty() == 0 {
		// Nothing to commit: no COMMIT record, but the end respects the
		// durability barrier of the base (SPEC §9.2 step 1).
		db.mu.Lock()
		if tx.state.Load() == txActive {
			tx.endLocked(txAborted)
		}
		db.mu.Unlock()
		if db.opts.Durability != Async {
			if err := db.WaitDurable(ctx, parent); err != nil && !isCode(err, errs.CodeInvalidArgument) {
				return Receipt{CommitID: parent}, err
			}
		}
		return Receipt{CommitID: parent, Durable: db.durable.Load().State.CommitID >= parent}, nil
	}
	for slot, t := range tx.trees {
		tx.view.SetRoot(slot, t.Root())
	}
	st := tx.view.State()
	st.NodeHWM, st.EdgeHWM = max(st.NodeHWM, db.ids.limit(KindNode)), max(st.EdgeHWM, db.ids.limit(KindEdge))
	bytes := int64(tx.view.Dirty())*(page.Size+wal.RecordHeaderSize) + intentOverhead
	if bytes > MaxGroupBytes { // trees used directly bypass the MaxTxBytes of statements
		return Receipt{}, errs.Newf(errs.CodeLimitExceeded, op, "the transaction changes %d pages, more than a group holds", tx.view.Dirty())
	}
	// While the commit waits for room, the writer resolves no version: its
	// pin is dropped, so that the pass that relieves MaxWALBytes may remove
	// the segments it held, and taken again at the floor of now.
	tx.snap.seg.Store(noPin)
	err := db.reserveQueue(ctx, bytes, wait)
	tx.snap.seg.Store(db.walFloor.Load())
	if err != nil {
		return Receipt{}, err // not sealed: the transaction stays active
	}
	in := &intent{commit: parent + 1, parent: parent, bytes: bytes, done: make(chan struct{})}
	rand.Read(in.token[:])
	final, err := tx.view.Finalize(in.commit, parent, db.opts.PruneBudget)
	db.mu.Lock()
	if err == nil {
		err = db.usable(op)
	}
	if err == nil {
		err = tx.check(op) // expired while finalizing
	}
	if err != nil {
		db.unreserveLocked(bytes)
		if tx.state.Load() == txActive {
			tx.endLocked(txAborted) // the view is closed after Finalize
		}
		db.mu.Unlock()
		return Receipt{}, err
	}
	in.final = final
	in.res = tx.res.Transfer(govern.Sealed)
	tx.in = in
	tx.state.Store(txSealed)
	tx.timer.Stop()
	db.staged.Store(final.Dir)
	db.queue = append(db.queue, in)
	db.tokens[in.token] = token{Pending, in.commit}
	db.releaseWriterLocked(tx)
	db.mu.Unlock()
	db.co.signal()

	select {
	case <-in.done:
		return in.receipt, in.err
	case <-ctx.Done():
		return Receipt{CommitID: in.commit, Token: in.token},
			errs.Wrap(errs.CodeCommitPending, op, ctx.Err(), "the commit is sealed; ask CommitStatus with its token")
	}
}
