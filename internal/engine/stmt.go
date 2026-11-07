package engine

import (
	"context"

	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Reader reads a snapshot: a ReadTx, or a WriteTx with its own writes.
type Reader interface {
	Get(ctx context.Context, slot int, key []byte) ([]byte, bool, error)
	Range(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error
	// RangeReverse is Range in descending key order.
	RangeReverse(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error
	State() wal.State
	// Check reports whether the reader can still be used: TX_CLOSED,
	// WRITE_TX_TIMEOUT or SNAPSHOT_EXPIRED otherwise.
	Check() error
}

var (
	_ Reader = (*ReadTx)(nil)
	_ Reader = (*WriteTx)(nil)
	_ Reader = (*Stmt)(nil)
)

// Stmt is a statement in progress: the writes of one operation over any
// trees, undone together if the operation fails (SPEC §10.4). It is valid
// only inside the function given to WriteTx.Statement.
type Stmt struct {
	tx   *WriteTx
	op   string
	done bool
}

// Statement runs fn as one statement: under a savepoint, with MaxTxBytes
// checked at the end. When fn or the check fails, or fn panics, every
// write of fn, over every tree and the counters of the state, is undone;
// a panic goes on after that. Statements nest: an inner failure undoes
// only the inner statement. While fn runs, the transaction cannot commit
// nor reach savepoints older than the statement.
func (tx *WriteTx) Statement(op string, fn func(s *Stmt) error) (err error) {
	if err := tx.check(op); err != nil {
		return err
	}
	sp, err := tx.Savepoint()
	if err != nil {
		return err
	}
	tx.stmts = append(tx.stmts, sp)
	s := &Stmt{tx: tx, op: op}
	ok := false
	defer func() {
		s.done = true
		tx.stmts = tx.stmts[:len(tx.stmts)-1]
		if !ok {
			if rerr := tx.rollbackTo(sp); rerr != nil && err != nil {
				err = rerr // the transaction ended: its writes go with it
			}
		}
		tx.release(sp)
	}()
	if err = fn(s); err == nil && tx.res.Bytes() > tx.db.opts.MaxTxBytes {
		err = errs.Newf(errs.CodeLimitExceeded, op, "the transaction holds more than MaxTxBytes").
			With("limit", tx.db.opts.MaxTxBytes)
	}
	if err == nil {
		err = tx.check(op) // expired while fn ran
	}
	ok = err == nil
	return err
}

// Base returns the state the transaction derives from.
func (s *Stmt) Base() wal.State { return s.tx.Base() }

// Check reports whether the statement can still be used.
func (s *Stmt) Check() error { return s.usable() }

// Check reports whether the transaction can still be used.
func (tx *WriteTx) Check() error { return tx.check("engine.Check") }

// IDBound returns an identifier of kind above every one handed out so
// far: those handed out later are at least it.
func (tx *WriteTx) IDBound(kind int) uint64 {
	a := &tx.db.ids
	a.mu.Lock()
	defer a.mu.Unlock()
	if kind != KindNode && kind != KindEdge {
		return 0
	}
	return a.kinds[kind].next
}

func (s *Stmt) usable() error {
	if s.done {
		return errs.New(errs.CodeTxClosed, s.op, "the statement has ended")
	}
	return s.tx.check(s.op)
}

func (s *Stmt) tree(slot int) (*btree.Tree, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.tx.Tree(slot)
}

// Get reads key, with the writes of the transaction.
func (s *Stmt) Get(ctx context.Context, slot int, key []byte) ([]byte, bool, error) {
	t, err := s.tree(slot)
	if err != nil {
		return nil, false, err
	}
	return t.Get(ctx, s.tx.pins, key)
}

// Range calls fn for the keys in [lo, hi), with the writes of the
// transaction; nil bounds are open. fn must not write.
func (s *Stmt) Range(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error {
	t, err := s.tree(slot)
	if err != nil {
		return err
	}
	return t.Range(ctx, s.tx.pins, lo, hi, fn)
}

// RangeReverse is Range in descending key order.
func (s *Stmt) RangeReverse(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error {
	t, err := s.tree(slot)
	if err != nil {
		return err
	}
	return t.RangeReverse(ctx, s.tx.pins, lo, hi, fn)
}

// Bytes returns the private pages the transaction holds, in bytes.
func (s *Stmt) Bytes() int64 { return s.tx.res.Bytes() }

// CheckLimit fails with LIMIT_EXCEEDED once the transaction holds more
// than MaxTxBytes, for long statements that check before their end.
func (s *Stmt) CheckLimit() error {
	if err := s.usable(); err != nil {
		return err
	}
	if s.tx.res.Bytes() > s.tx.db.opts.MaxTxBytes {
		return errs.Newf(errs.CodeLimitExceeded, s.op, "the transaction holds more than MaxTxBytes").
			With("limit", s.tx.db.opts.MaxTxBytes)
	}
	return nil
}

// Put writes key.
func (s *Stmt) Put(ctx context.Context, slot int, key, value []byte) error {
	t, err := s.tree(slot)
	if err != nil {
		return err
	}
	_, err = t.Put(ctx, s.tx.pins, key, value)
	return err
}

// Delete removes key and reports whether it existed.
func (s *Stmt) Delete(ctx context.Context, slot int, key []byte) (bool, error) {
	t, err := s.tree(slot)
	if err != nil {
		return false, err
	}
	found, _, err := t.Delete(ctx, s.tx.pins, key)
	return found, err
}

// State returns the working state of the transaction.
func (s *Stmt) State() wal.State {
	if s.usable() != nil {
		return wal.State{}
	}
	return s.tx.State()
}

// AddCounts adds to the live node and edge counts of the state. A count
// below zero means the bookkeeping is broken: CORRUPT.
func (s *Stmt) AddCounts(nodes, edges int64) error {
	if err := s.usable(); err != nil {
		return err
	}
	st := s.tx.view.State()
	if (nodes < 0 && uint64(-nodes) > st.NodeCount) || (edges < 0 && uint64(-edges) > st.EdgeCount) {
		return errs.Newf(errs.CodeCorrupt, s.op, "counts %d nodes and %d edges below a change of %d and %d",
			st.NodeCount, st.EdgeCount, nodes, edges)
	}
	st.NodeCount = uint64(int64(st.NodeCount) + nodes)
	st.EdgeCount = uint64(int64(st.EdgeCount) + edges)
	return nil
}

// BumpSchemaEpoch raises the schema epoch once per commit: later calls in
// the same transaction keep it (SPEC §7.4, §12.2).
func (s *Stmt) BumpSchemaEpoch() error {
	if err := s.usable(); err != nil {
		return err
	}
	if st := s.tx.view.State(); st.SchemaEpoch == s.tx.base.State.SchemaEpoch {
		st.SchemaEpoch++
	}
	return nil
}

// RaiseSymbolHWM keeps the aggregate symbol watermark at or above n.
func (s *Stmt) RaiseSymbolHWM(n uint64) error {
	if err := s.usable(); err != nil {
		return err
	}
	st := s.tx.view.State()
	st.SymbolHWM = max(st.SymbolHWM, n)
	return nil
}

// NextID returns a new node or edge identifier, as WriteTx.NextID: a
// failed statement does not give it back.
func (s *Stmt) NextID(ctx context.Context, kind int) (uint64, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	return s.tx.NextID(ctx, kind)
}

// State returns the working state of the transaction: its counts, epoch
// and watermarks so far.
func (tx *WriteTx) State() wal.State { return *tx.view.State() }

// Range calls fn for the keys in [lo, hi), with the writes of the
// transaction; nil bounds are open.
func (tx *WriteTx) Range(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error {
	t, err := tx.Tree(slot)
	if err != nil {
		return err
	}
	return t.Range(ctx, tx.pins, lo, hi, fn)
}

// RangeReverse is Range in descending key order.
func (tx *WriteTx) RangeReverse(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error {
	t, err := tx.Tree(slot)
	if err != nil {
		return err
	}
	return t.RangeReverse(ctx, tx.pins, lo, hi, fn)
}
