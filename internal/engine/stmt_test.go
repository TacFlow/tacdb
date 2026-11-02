package engine

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
)

var errStmt = errors.New("the operation failed")

// A failed statement undoes its writes over every tree and its changes to
// the counters; a successful one keeps them through the commit.
func TestStatementAcrossTrees(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write := func(s *Stmt, k string) error {
		for _, slot := range []int{0, 2, 9} {
			if err := s.Put(ctx, slot, []byte(k), []byte("v")); err != nil {
				return err
			}
		}
		if err := s.AddCounts(1, 1); err != nil {
			return err
		}
		s.BumpSchemaEpoch()
		s.RaiseSymbolHWM(7)
		return nil
	}
	err = tx.Statement("t", func(s *Stmt) error {
		if err := write(s, "a"); err != nil {
			return err
		}
		if _, ok, _ := s.Get(ctx, 9, []byte("a")); !ok {
			t.Fatal("a statement does not see its own write")
		}
		return errStmt
	})
	if err != errStmt {
		t.Fatalf("the error of fn is returned: %v", err)
	}
	for _, slot := range []int{0, 2, 9} {
		if _, ok, _ := tx.Get(ctx, slot, []byte("a")); ok {
			t.Fatalf("slot %d keeps a write of the failed statement", slot)
		}
	}
	if st := tx.State(); st.NodeCount != 0 || st.EdgeCount != 0 || st.SchemaEpoch != tx.Base().SchemaEpoch || st.SymbolHWM != 0 {
		t.Fatalf("the failed statement left counters %+v", st)
	}
	if err := tx.Statement("t", func(s *Stmt) error { return write(s, "b") }); err != nil {
		t.Fatal(err)
	}
	if err := tx.Statement("t", func(s *Stmt) error { return write(s, "c") }); err != nil {
		t.Fatal(err)
	}
	st := tx.State()
	if st.NodeCount != 2 || st.EdgeCount != 2 || st.SchemaEpoch != tx.Base().SchemaEpoch+1 || st.SymbolHWM != 7 {
		t.Fatalf("two statements: %+v (the epoch rises once per commit)", st)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	if s := r.State(); s.NodeCount != 2 || s.EdgeCount != 2 || s.SchemaEpoch != st.SchemaEpoch || s.SymbolHWM != 7 {
		t.Fatalf("published state %+v", s)
	}
	n := 0
	r.Range(ctx, 2, nil, nil, func(k, v []byte) error { n++; return nil })
	if n != 2 {
		t.Fatalf("%d keys in slot 2, want b and c", n)
	}
}

// An inner statement that fails undoes only itself; the outer one goes on.
func TestStatementNests(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	err := tx.Statement("outer", func(s *Stmt) error {
		if err := s.Put(ctx, 0, []byte("outer"), nil); err != nil {
			return err
		}
		inner := tx.Statement("inner", func(s *Stmt) error {
			s.Put(ctx, 0, []byte("inner"), nil)
			s.AddCounts(5, 0)
			return errStmt
		})
		if inner != errStmt {
			t.Fatalf("inner: %v", inner)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, outer, _ := tx.Get(ctx, 0, []byte("outer"))
	_, inner, _ := tx.Get(ctx, 0, []byte("inner"))
	if !outer || inner || tx.State().NodeCount != 0 {
		t.Fatalf("outer %v inner %v nodes %d", outer, inner, tx.State().NodeCount)
	}
}

// A statement past MaxTxBytes fails with LIMIT_EXCEEDED and leaves the
// transaction as it was; a Stmt used after its statement fails.
func TestStatementLimits(t *testing.T) {
	o := testOptions()
	o.MaxTxBytes = 64 << 10
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	var kept *Stmt
	err := tx.Statement("big", func(s *Stmt) error {
		kept = s
		for i := 0; i < 200; i++ {
			if err := s.Put(ctx, i%3, []byte{byte(i)}, bytes.Repeat([]byte{1}, 3000)); err != nil {
				return err
			}
		}
		return nil
	})
	var e *errs.Error
	if errs.CodeOf(err) != errs.CodeLimitExceeded || !errors.As(err, &e) || e.Details["limit"] != o.MaxTxBytes {
		t.Fatalf("over MaxTxBytes: %v", err)
	}
	for slot := range 3 {
		if _, ok, _ := tx.Get(ctx, slot, []byte{0}); ok {
			t.Fatal("the statement over the limit kept a write")
		}
	}
	if err := kept.Put(ctx, 0, []byte("late"), nil); errs.CodeOf(err) != errs.CodeTxClosed {
		t.Fatalf("a Stmt after its statement: %v", err)
	}
	if err := tx.Statement("t", func(s *Stmt) error { return s.AddCounts(-1, 0) }); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a count below zero: %v", err)
	}
}

// A panic inside fn undoes the statement, ends its Stmt and goes on; the
// transaction stays usable and commits nothing of it.
func TestStatementPanic(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	var kept *Stmt
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not go on")
			}
		}()
		tx.Statement("t", func(s *Stmt) error {
			kept = s
			s.Put(ctx, 0, []byte("half"), nil)
			s.AddCounts(1, 0)
			panic("boom")
		})
	}()
	if _, ok, _ := tx.Get(ctx, 0, []byte("half")); ok || tx.State().NodeCount != 0 || len(tx.saves) != 0 || len(tx.stmts) != 0 {
		t.Fatalf("after the panic: write %v, nodes %d, savepoints %d, statements %d", ok, tx.State().NodeCount, len(tx.saves), len(tx.stmts))
	}
	if err := kept.Put(ctx, 0, []byte("late"), nil); errs.CodeOf(err) != errs.CodeTxClosed {
		t.Fatalf("the Stmt of a panicked statement: %v", err)
	}
	if err := tx.Put(ctx, 0, []byte("ok"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Inside fn, the transaction cannot commit, roll back to or release a
// savepoint older than the statement; its own savepoints work.
func TestStatementGuardsItsSavepoint(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	outer, _ := tx.Savepoint()
	err := tx.Statement("t", func(s *Stmt) error {
		s.Put(ctx, 0, []byte("a"), nil)
		s.AddCounts(1, 0)
		if err := tx.RollbackTo(outer); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("RollbackTo an older savepoint: %v", err)
		}
		tx.Release(outer)
		if _, err := tx.Commit(ctx); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("Commit inside a statement: %v", err)
		}
		inner, _ := tx.Savepoint()
		s.Put(ctx, 0, []byte("b"), nil)
		if err := tx.RollbackTo(inner); err != nil {
			t.Fatal(err)
		}
		tx.Release(inner)
		return errStmt
	})
	if err != errStmt {
		t.Fatal(err)
	}
	if _, ok, _ := tx.Get(ctx, 0, []byte("a")); ok || tx.State().NodeCount != 0 {
		t.Fatal("the failed statement kept its write")
	}
	if len(tx.saves) != 1 {
		t.Fatalf("the savepoint taken before the statement is gone: %d savepoints", len(tx.saves))
	}
}

// A transaction that expires while fn runs fails the statement.
func TestStatementExpires(t *testing.T) {
	o := testOptions()
	o.MaxWriteTxDuration = 20 * time.Millisecond
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	err := tx.Statement("t", func(s *Stmt) error {
		s.Put(ctx, 0, []byte("a"), nil)
		time.Sleep(60 * time.Millisecond)
		return nil
	})
	if errs.CodeOf(err) != errs.CodeWriteTxTimeout {
		t.Fatalf("a statement over an expired transaction: %v", err)
	}
}

// Counters never change alone: a commit whose statements changed them
// without writing a page fails instead of dropping them. A Stmt after its
// statement refuses every change.
func TestCountersNeedPages(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	var kept *Stmt
	if err := tx.Statement("t", func(s *Stmt) error { kept = s; return s.BumpSchemaEpoch() }); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("a commit of counters alone: %v", err)
	}
	for _, err := range []error{kept.AddCounts(1, 0), kept.BumpSchemaEpoch(), kept.RaiseSymbolHWM(3)} {
		if errs.CodeOf(err) != errs.CodeTxClosed {
			t.Fatalf("a Stmt after its statement: %v", err)
		}
	}
	if kept.State().CommitID != 0 || kept.State().SchemaEpoch != 0 {
		t.Fatal("a Stmt after its statement shows the state")
	}
}

// CloseIdle fails with BUSY while a transaction is open and keeps the
// handle; a transaction that begins while it checks is refused, so none
// is left open by a Close that succeeds.
func TestCloseIdle(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	r, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var e *errs.Error
	if err := db.CloseIdle(); errs.CodeOf(err) != errs.CodeBusy || !errors.As(err, &e) || e.Details["active"] != 1 {
		t.Fatalf("CloseIdle with a reader: %v", err)
	}
	if err := r.Check(); err != nil {
		t.Fatalf("the reader after BUSY: %v", err)
	}
	r.Close()
	db.closing.Store(true) // as while CloseIdle checks
	if _, err := db.BeginRead(ctx); errs.CodeOf(err) != errs.CodeShuttingDown {
		t.Fatalf("a reader while CloseIdle checks: %v", err)
	}
	if _, err := db.BeginWrite(ctx); errs.CodeOf(err) != errs.CodeShuttingDown {
		t.Fatalf("a writer while CloseIdle checks: %v", err)
	}
	if db.Active() != 0 {
		t.Fatalf("refused transactions stay counted: %d", db.Active())
	}
	db.closing.Store(false)
	if err := db.CloseIdle(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginRead(ctx); errs.CodeOf(err) != errs.CodeClosed {
		t.Fatalf("a reader after CloseIdle: %v", err)
	}
}
