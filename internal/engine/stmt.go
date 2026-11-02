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
	State() wal.State
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
