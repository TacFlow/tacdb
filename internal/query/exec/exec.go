// Package exec runs the plans of tacdb-cypher/1 read queries (SPEC
// §15.3, §33): each operator of the plan becomes an iterator with Next
// and Close, pulled one row at a time from the root. Close runs on
// success, error, cancellation and an early end by LIMIT. Rows are
// slices of slots; operators that keep rows (Sort, TopK, Aggregate,
// Distinct) charge them to the budget of the execution before they keep
// them.
package exec

import (
	"context"
	"errors"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/budget"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// Defaults of SPEC §15.4.
const (
	DefaultMaxExpansions = 1_000_000
	// checkEvery is how many rows or expansions pass between two checks of
	// the context.
	checkEvery = 256
)

// Row is a row of slots.
type Row []qv.Value

// Slot returns slot i.
func (r Row) Slot(i int) qv.Value { return r[i] }

// Source reads the graph of one transaction for the executor and for the
// expressions it evaluates.
type Source struct {
	G *graph.Graph
	R engine.Reader
}

var _ eval.Graph = Source{}

func entity(k qv.Kind) graph.Entity {
	if k == qv.KindEdge {
		return graph.EdgeEntity
	}
	return graph.NodeEntity
}

// Property returns a property of a node or an edge.
func (s Source) Property(ctx context.Context, k qv.Kind, id uint64, key string) (value.Value, bool, error) {
	return s.G.GetProperty(ctx, s.R, entity(k), id, key)
}

// Properties returns every property of a node or an edge.
func (s Source) Properties(ctx context.Context, k qv.Kind, id uint64) (map[string]value.Value, error) {
	return s.G.Properties(ctx, s.R, entity(k), id)
}

// Labels returns the labels of a node.
func (s Source) Labels(ctx context.Context, id uint64) ([]string, error) {
	return s.G.Labels(ctx, s.R, id)
}

// EdgeType returns the type of an edge.
func (s Source) EdgeType(ctx context.Context, id uint64) (string, error) {
	e, err := s.G.EdgeEnds(ctx, s.R, id)
	return e.Type, err
}

// Config holds the limits of an execution besides its budget.
type Config struct {
	MaxExpansions int64 // 0 is the default
}

// Stats counts the work of an execution.
type Stats struct {
	Rows     uint64 // rows of the result
	Examined uint64 // entities scanned and edges read
}

// Exec is one execution of a plan. It is not safe for concurrent use.
type Exec struct {
	env      *eval.Env
	src      Source
	b        *budget.Budget
	cfg      Config
	plan     *plan.Plan
	root     iter
	stats    Stats
	expanded int64
	ticks    int
	err      error
	done     bool
}

// Start begins an execution of p over src with params, charged to b.
func Start(ctx context.Context, p *plan.Plan, src Source, params map[string]value.Value, b *budget.Budget, cfg Config) *Exec {
	if cfg.MaxExpansions <= 0 {
		cfg.MaxExpansions = DefaultMaxExpansions
	}
	x := &Exec{env: &eval.Env{Ctx: ctx, Params: params, Graph: src, Budget: b}, src: src, b: b, cfg: cfg, plan: p}
	x.root = x.build(p.Root, nil)
	return x
}

// Next returns the next row of the result: its first len(Columns) slots
// are the columns. The row is valid until the next call. At the end, or
// on error, it returns false, and Err tells which; the iterators are
// closed then.
func (x *Exec) Next() (Row, bool) {
	if x.done {
		return nil, false
	}
	r, ok, err := x.root.next()
	if err != nil || !ok {
		x.err = staged(err)
		x.Close()
		return nil, false
	}
	x.stats.Rows++
	return r, true
}

// staged marks an error of the execution that has no stage yet, such as
// a limit of the budget, as an execution error.
func staged(err error) error {
	if e, ok := err.(*errs.Error); ok && e.Stage == "" {
		c := *e
		c.Stage = errs.StageExecute
		return &c
	}
	return err
}

// Err returns the error that ended the execution, if any.
func (x *Exec) Err() error { return x.err }

// Stats returns the counters of the execution.
func (x *Exec) Stats() Stats { return x.stats }

// Close ends the execution and its iterators; it is idempotent.
func (x *Exec) Close() {
	if !x.done {
		x.done = true
		x.root.close()
	}
}

// work charges n work units: rows produced by an operator, entities
// scanned, edges read (SPEC §15.4). Each charge is a tick: every row,
// entity or edge counts toward the next check of the context.
func (x *Exec) work(n uint64) error {
	if x.b != nil {
		if err := x.b.Work(n); err != nil {
			return err
		}
	}
	return x.tick()
}

// tick counts one row or expansion and checks the context every
// checkEvery of them.
func (x *Exec) tick() error {
	x.ticks++
	if x.ticks%checkEvery != 0 {
		return nil
	}
	return x.checkContext()
}

func (x *Exec) checkContext() error {
	err := x.env.Ctx.Err()
	switch {
	case err == nil:
		return x.src.R.Check()
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeDeadline, "query", err, "deadline of the query")
	default:
		return errs.Wrap(errs.CodeCanceled, "query", err, "query canceled")
	}
}

// grow charges bytes a kept row or value takes.
func (x *Exec) grow(n int64) error {
	if x.b == nil {
		return nil
	}
	return x.b.Grow(n)
}

func (x *Exec) shrink(n int64) {
	if x.b != nil {
		x.b.Shrink(n)
	}
}

// rowBytes is what a kept row costs.
func rowBytes(r Row) int64 {
	n := int64(24) // the slice header
	for _, v := range r {
		n += v.Size()
	}
	return n
}

// eval evaluates e on row.
func (x *Exec) eval(e plan.Expr, row eval.Row) (qv.Value, error) {
	return e.E.Eval(x.env, row)
}

// iter is an operator of an execution.
type iter interface {
	// next returns the next row; the row belongs to the caller.
	next() (Row, bool, error)
	close()
}
