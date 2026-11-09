package tacdb

import (
	"context"
	"maps"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/budget"
	"github.com/TacFlow/tacdb/internal/query/exec"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/value"
)

// Result is the whole result of Exec (SPEC §31.1): the rows, materialized
// within ResultMemoryBytes, the counters, and, for DB.Exec of a query
// that writes, the receipt of its commit.
type Result struct {
	Columns []Column
	Rows    [][]QueryValue
	Stats   QueryStats
	// CommitID is the commit of DB.Exec: the new one of a query that
	// writes, the snapshot of a read. It is zero inside a write
	// transaction not committed yet.
	CommitID CommitID
	// Pending is set with COMMIT_PENDING: the commit is sealed, and its
	// outcome comes with Token from CommitStatus (SPEC §9).
	Pending bool
	Durable bool
	Token   CommitToken
}

// Exec runs a query to its end. A query that writes runs in a new write
// transaction, committed when the query succeeds and rolled back when it
// fails, so that it has all its effects or none; a read runs in a new
// read transaction. Errors of the text, the analysis and the parameters
// come before the transaction.
func (db *DB) Exec(ctx context.Context, text string, p Params) (Result, error) {
	pl, _, err := db.compile(text, p, true)
	if err != nil {
		return Result{}, err
	}
	if !pl.Write {
		tx, err := db.BeginRead(ctx)
		if err != nil {
			return Result{}, err
		}
		defer tx.r.Close()
		res, err := tx.run(ctx, pl, p, exec.Source{G: db.g, R: tx.r})
		if err != nil {
			return Result{}, err
		}
		res.CommitID, res.Durable = tx.Snapshot(), tx.r.Durable()
		return res, nil
	}
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		return Result{}, err
	}
	// Rolled back on any way out but a commit; after a sealed commit, a
	// no-op.
	defer tx.w.Rollback()
	res, err := tx.exec(ctx, pl, p)
	if err != nil {
		return Result{}, err
	}
	rc, err := tx.commit(ctx)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeCommitPending {
			res.Pending, res.Token = true, rc.Token
			return res, err
		}
		return Result{}, err
	}
	res.CommitID, res.Durable, res.Token = rc.ID, rc.Durable, rc.Token
	return res, nil
}

// Exec runs a query to its end in tx. A query that writes is one
// statement of the write transaction: when it fails, none of its effects
// remain and the transaction goes on. It fails with READ_ONLY in a read
// transaction and with BUSY while a Rows of tx is open.
func (tx *Tx) Exec(ctx context.Context, text string, p Params) (Result, error) {
	const op = "tacdb.Exec"
	if err := tx.enter(op); err != nil {
		return Result{}, err
	}
	defer tx.leave()
	if tx.closed {
		return Result{}, errs.New(errs.CodeTxClosed, op, "transaction closed")
	}
	pl, _, err := tx.db.compile(text, p, true)
	if err != nil {
		return Result{}, err
	}
	return tx.exec(ctx, pl, p)
}

// exec runs a planned query in tx, which the caller holds.
func (tx *Tx) exec(ctx context.Context, pl *plan.Plan, p Params) (Result, error) {
	const op = "tacdb.Exec"
	if !pl.Write {
		r := tx.reader()
		if err := r.Check(); err != nil {
			return Result{}, err
		}
		return tx.run(ctx, pl, p, exec.Source{G: tx.db.g, R: r})
	}
	if tx.w == nil {
		return Result{}, errs.New(errs.CodeReadOnly, op, "a read transaction cannot write")
	}
	if err := tx.openCursors(op); err != nil {
		return Result{}, err
	}
	var res Result
	err := tx.w.Statement(op, func(s *engine.Stmt) error {
		var err error
		res, err = tx.run(ctx, pl, p, exec.Source{G: tx.db.g, R: s, W: s})
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// run executes pl over src and materializes its rows. A query that writes
// without RETURN has no columns and no rows.
func (tx *Tx) run(ctx context.Context, pl *plan.Plan, p Params, src exec.Source) (Result, error) {
	lim := tx.db.q
	gov := tx.db.e.Governor()
	b, err := budget.New(ctx, gov, lim.budget)
	if err != nil {
		return Result{}, exec.Staged(err)
	}
	defer b.Close()
	// The rows go to the caller; the governor holds them until Exec
	// returns.
	rb, err := budget.New(ctx, gov, budget.Limits{MemoryBytes: lim.result})
	if err != nil {
		return Result{}, exec.Staged(err)
	}
	defer rb.Close()
	x := exec.Start(ctx, pl, src, maps.Clone(map[string]value.Value(p)), b, lim.exec)
	defer x.Close()
	res := Result{}
	for _, c := range pl.Columns {
		res.Columns = append(res.Columns, columnOf(c))
	}
	n := len(pl.Columns)
	for {
		row, ok := x.Next()
		if !ok {
			break
		}
		if n == 0 {
			continue
		}
		vals := make([]QueryValue, n)
		var bytes int64
		for i := range vals {
			v, size, err := tx.db.materialize(src.R, row[i])
			if err != nil {
				return Result{}, exec.Staged(err)
			}
			vals[i], bytes = v, bytes+size
		}
		if bytes > lim.result-rb.Used() {
			return Result{}, exec.Staged(errs.New(errs.CodeResourceLimit, "query", "result memory exhausted").
				With("requested", bytes).With("available", lim.result-rb.Used()).With("limit", lim.result))
		}
		if err := rb.Grow(bytes); err != nil {
			return Result{}, exec.Staged(err)
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := x.Err(); err != nil {
		return Result{}, err
	}
	st := x.Stats()
	res.Stats = QueryStats{Rows: uint64(len(res.Rows)), Examined: st.Examined, Snapshot: tx.Snapshot(),
		Complete: true, PlanMode: "syntactic", WorkUnits: b.WorkDone()}
	if tx.r != nil {
		res.Stats.Durable = tx.r.Durable()
	}
	res.Stats.mutations(st.Mutations)
	return res, nil
}
