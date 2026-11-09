// Package plan turns an analyzed query of tacdb-cypher/1 into a tree of
// physical operators (SPEC §15.3, §33): the syntactic plan, without
// statistics. A plan is immutable once built and safe for
// concurrent use; the executor instantiates its operators for each
// execution.
//
// Rows are slots resolved here: each part of the query, between two
// projections, has its own layout, with a slot per variable in scope, per
// anonymous pattern element the plan keeps, and per sort key. No row holds
// a map of names.
package plan

import (
	"fmt"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/sema"
)

// DefaultMaxHops bounds a variable-length relationship (SPEC §15.4).
const DefaultMaxHops = 64

// Config holds the options of planning.
type Config struct {
	MaxHops int // 0 is the default
	// NoTopK and NoOrderedAggregate plan the plain operators, so that
	// tests compare the optimized ones with them (QRY-09).
	NoTopK             bool
	NoOrderedAggregate bool
	// Write plans the clauses that write, for Exec; without it a query
	// that writes is QUERY_WRITE_REQUIRES_EXEC.
	Write bool
}

// Part is the layout of the rows of one part of a query.
type Part struct {
	Width int
	Names []string // the variable of each slot; "" for a hidden slot
}

func (p *Part) slot(name string) int {
	p.Names = append(p.Names, name)
	p.Width++
	return p.Width - 1
}

// Expr is a compiled expression with its text, for the rendered plan.
type Expr struct {
	E    *eval.Expr
	Text string
}

// PropTest is an entry of a property map in a pattern: the property key
// of the element equals the value by the = of expressions.
type PropTest struct {
	Key   string
	Value Expr
}

// Op is a physical operator.
type Op interface {
	Name() string
	Inputs() []Op
}

// Plan is a planned read query.
type Plan struct {
	Root    Op
	Out     *Part
	Columns []sema.Column // the first len(Columns) slots of Out
	MaxHops int
	Src     string // the source, for the locations of execution errors
	Write   bool   // the query writes
}

// Operators. Each one reads the rows of its input, if any; a scan or a
// seek runs once per input row, so that a disconnected pattern is the
// cross product of its parts.
type (
	// SingleRow yields one row with every slot Null.
	SingleRow struct{ Part *Part }
	// Argument yields the row OptionalApply gives its inner plan.
	Argument struct{ Part *Part }
	// NodeScan binds Slot to every node with Labels and Props.
	NodeScan struct {
		In     Op
		Slot   int
		Labels []string
		Props  []PropTest
	}
	// LabelScan binds Slot to every node of Label that also has Labels
	// and Props.
	LabelScan struct {
		In     Op
		Slot   int
		Label  string
		Labels []string
		Props  []PropTest
	}
	// IDSeek binds Slot to the node whose ID is the value of ID, if it
	// exists and has Labels and Props.
	IDSeek struct {
		In     Op
		Slot   int
		ID     Expr
		Labels []string
		Props  []PropTest
	}
	// NodeFilter keeps the rows where Slot, bound before, is a node with
	// Labels and Props.
	NodeFilter struct {
		In     Op
		Slot   int
		Labels []string
		Props  []PropTest
	}
	// Expand follows one relationship from the node in From. Into
	// matches the node bound in To; otherwise the far node is bound to To
	// and must have ToLabels and ToProps. RelBound requires the edge bound
	// in Rel by an earlier clause. The edge differs from the edges in
	// Unique and in the edge lists in Lists, the other relationships of
	// the same clause (SPEC §33.3).
	Expand struct {
		In       Op
		From, To int
		Rel      int
		Dir      graph.Direction
		Types    []string
		Into     bool
		RelBound bool
		RelProps []PropTest
		ToLabels []string
		ToProps  []PropTest
		Unique   []int
		Lists    []int
	}
	// VarExpand follows Min to Max relationships from From with distinct
	// edges, depth first; List holds the edges of each path. Unbounded
	// stops at MaxHops, and a path that could go on past it is
	// RESOURCE_LIMIT.
	VarExpand struct {
		In        Op
		From, To  int
		List      int
		Dir       graph.Direction
		Types     []string
		Min, Max  int
		Unbounded bool
		Into      bool
		ToLabels  []string
		ToProps   []PropTest
		Unique    []int
		Lists     []int
		Span      ast.Span // of the relationship, for the location of its errors
	}
	// Filter keeps the rows where Pred is true.
	Filter struct {
		In   Op
		Pred Expr
	}
	// Unwind binds Slot to each element of X: a List gives its elements,
	// Null no row, any other value one row.
	Unwind struct {
		In   Op
		X    Expr
		Slot int
	}
	// OptionalApply runs Inner for each row of In, which Inner's Argument
	// yields; without a result it yields the row of In, whose Slots, those
	// of the optional pattern, are still Null (SPEC §15.5).
	OptionalApply struct {
		In    Op
		Inner Op
		Slots []int
	}
	// Project computes Items into the first slots of rows of Out, then
	// Keys, the sort keys, into the next ones. Keys read a row made of the
	// input row followed by the items.
	Project struct {
		In    Op
		Out   *Part
		Items []Expr
		Keys  []Expr
	}
	// Aggregate groups the rows of In by Keys, with the codec of
	// equality, and yields one row per group: the keys, then the result
	// of each aggregate. Without keys, an empty input still gives one row.
	// Ordered keeps only the current group: its input comes clustered by
	// the keys.
	Aggregate struct {
		In      Op
		Part    *Part
		Keys    []Expr
		Aggs    []Agg
		Ordered bool
	}
	// Distinct keeps the first row of each class of the first N slots.
	Distinct struct {
		In Op
		N  int
	}
	// Sort orders the rows by Keys, stably.
	Sort struct {
		In   Op
		Keys []SortKey
	}
	// TopK yields what Sort, Skip and Limit would, keeping at most
	// Skip+Limit rows (SPEC §33.4). Exhaustive reads its input even for
	// a LIMIT of 0.
	TopK struct {
		In          Op
		Keys        []SortKey
		Skip, Limit *Expr // Skip may be nil
		Exhaustive  bool
	}
	// Skip drops the first X rows.
	Skip struct {
		In Op
		X  Expr
	}
	// Limit yields at most X rows. Exhaustive, after a clause that
	// writes, reads the rest of its input too (design §4.4).
	Limit struct {
		In         Op
		X          Expr
		Exhaustive bool
	}
)

// Agg is an aggregate call.
type Agg struct {
	Func     *eval.Func
	Distinct bool
	Star     bool
	Arg      *Expr // nil for count(*)
	Text     string
}

// SortKey is a slot to order by.
type SortKey struct {
	Slot int
	Desc bool
	Text string
}

func (*SingleRow) Name() string  { return "SingleRow" }
func (*Argument) Name() string   { return "Argument" }
func (*NodeScan) Name() string   { return "NodeScan" }
func (*LabelScan) Name() string  { return "LabelScan" }
func (*IDSeek) Name() string     { return "IDSeek" }
func (*NodeFilter) Name() string { return "NodeFilter" }
func (o *Expand) Name() string {
	return map[bool]string{false: "ExpandAll", true: "ExpandInto"}[o.Into]
}
func (*VarExpand) Name() string     { return "VarExpand" }
func (*Filter) Name() string        { return "Filter" }
func (*Unwind) Name() string        { return "Unwind" }
func (*OptionalApply) Name() string { return "OptionalApply" }
func (*Project) Name() string       { return "Project" }
func (o *Aggregate) Name() string {
	return map[bool]string{false: "Aggregate", true: "OrderedAggregate"}[o.Ordered]
}
func (*Distinct) Name() string { return "Distinct" }
func (*Sort) Name() string     { return "Sort" }
func (*TopK) Name() string     { return "TopK" }
func (*Skip) Name() string     { return "Skip" }
func (o *Limit) Name() string {
	return map[bool]string{false: "Limit", true: "ExhaustiveLimit"}[o.Exhaustive]
}

func in(o Op) []Op { return []Op{o} }

func (*SingleRow) Inputs() []Op       { return nil }
func (*Argument) Inputs() []Op        { return nil }
func (o *NodeScan) Inputs() []Op      { return in(o.In) }
func (o *LabelScan) Inputs() []Op     { return in(o.In) }
func (o *IDSeek) Inputs() []Op        { return in(o.In) }
func (o *NodeFilter) Inputs() []Op    { return in(o.In) }
func (o *Expand) Inputs() []Op        { return in(o.In) }
func (o *VarExpand) Inputs() []Op     { return in(o.In) }
func (o *Filter) Inputs() []Op        { return in(o.In) }
func (o *Unwind) Inputs() []Op        { return in(o.In) }
func (o *OptionalApply) Inputs() []Op { return []Op{o.In, o.Inner} }
func (o *Project) Inputs() []Op       { return in(o.In) }
func (o *Aggregate) Inputs() []Op     { return in(o.In) }
func (o *Distinct) Inputs() []Op      { return in(o.In) }
func (o *Sort) Inputs() []Op          { return in(o.In) }
func (o *TopK) Inputs() []Op          { return in(o.In) }
func (o *Skip) Inputs() []Op          { return in(o.In) }
func (o *Limit) Inputs() []Op         { return in(o.In) }

// failf returns a planning error at span.
func failf(src string, span ast.Span, code errs.Code, stage errs.Stage, format string, args ...any) error {
	e := errs.Newf(code, "query", format, args...)
	e.Stage = stage
	e.Location = ast.Locate(src, span)
	return e
}

// Build plans the query q, analyzed into info. A query that writes is
// planned only with cfg.Write; a procedure call and the search hint are
// not planned here.
func Build(q *ast.Query, info *sema.Info, src string, cfg Config) (*Plan, error) {
	if cfg.MaxHops <= 0 {
		cfg.MaxHops = DefaultMaxHops
	}
	if q.Explain || q.Profile {
		return nil, failf(src, q.Span, errs.CodeQueryUnsupported, errs.StagePlan, "EXPLAIN and PROFILE are not available in this version")
	}
	if info.Write && !cfg.Write {
		return nil, failf(src, q.Span, errs.CodeQueryWriteRequiresExec, errs.StageSemantic, "a query that writes needs Exec")
	}
	part := &Part{}
	p := &planner{info: info, src: src, cfg: cfg, part: part, names: map[string]int{}, cluster: -1}
	p.op = &SingleRow{Part: part}
	p.single = true
	for _, c := range q.Clauses {
		if err := p.clause(c); err != nil {
			return nil, err
		}
	}
	return &Plan{Root: p.op, Out: p.part, Columns: info.Columns, MaxHops: cfg.MaxHops, Src: src, Write: info.Write}, nil
}

// planner builds the plan clause by clause.
type planner struct {
	info  *sema.Info
	src   string
	cfg   Config
	part  *Part
	names map[string]int // variables in scope and their slots
	op    Op
	// single is set while the plan yields at most the one row of
	// SingleRow, so that a scan on it yields its nodes in ID order.
	single bool
	// cluster is the slot whose equal values are contiguous in the rows
	// of op, and never come back once they end; -1 when unknown.
	cluster int
	// write is set after a clause that writes: a LIMIT then reads all its
	// input, so that it never cuts effects.
	write bool
}

func (p *planner) clause(c ast.Clause) error {
	switch c := c.(type) {
	case *ast.Match:
		return p.match(c)
	case *ast.Unwind:
		x, err := p.compile(c.X, p.scope())
		if err != nil {
			return err
		}
		slot := p.declare(c.Alias)
		p.op, p.single = &Unwind{In: p.op, X: x, Slot: slot}, false
	case *ast.With:
		if err := p.projection(c.Projection); err != nil {
			return err
		}
		if c.Where != nil {
			pred, err := p.compile(c.Where, p.scope())
			if err != nil {
				return err
			}
			p.op = &Filter{In: p.op, Pred: pred}
		}
	case *ast.Return:
		return p.projection(c.Projection)
	case *ast.Call:
		return failf(p.src, c.Span, errs.CodeQueryUnsupported, errs.StagePlan, "CALL is not available in this version")
	case *ast.Create:
		return p.create(c)
	case *ast.Merge:
		return p.merge(c)
	case *ast.Set:
		items, err := p.setItems(c.Items)
		if err != nil {
			return err
		}
		p.written(&Set{In: p.op, Items: items, Span: c.Span})
	case *ast.Remove:
		p.written(&Remove{In: p.op, Items: p.removeItems(c.Items), Span: c.Span})
	case *ast.Delete:
		return p.delete(c)
	}
	return nil
}

// declare gives a variable a new slot of the current part.
func (p *planner) declare(name string) int {
	s := p.part.slot(name)
	p.names[name] = s
	return s
}

func (p *planner) compile(e ast.Expr, sc eval.Scope) (Expr, error) {
	if b := search(e); b != nil {
		return Expr{}, failf(p.src, b.Span, errs.CodeQueryUnsupported, errs.StagePlan, "%s is not available in this version", map[ast.BinaryOp]string{ast.OpVecDist: "<=>", ast.OpFTSMatch: "@@"}[b.Op])
	}
	x, err := eval.Compile(e, sc, p.src)
	if err != nil {
		return Expr{}, err
	}
	return Expr{E: x, Text: ast.FormatExpr(e)}, nil
}

// search returns an operator of vector or full-text search in e: they
// are analyzed now and run with their indexes in later versions.
func search(e ast.Expr) *ast.Binary {
	switch e := e.(type) {
	case *ast.Binary:
		if e.Op == ast.OpVecDist || e.Op == ast.OpFTSMatch {
			return e
		}
		if b := search(e.L); b != nil {
			return b
		}
		return search(e.R)
	case *ast.Property:
		return search(e.X)
	case *ast.Unary:
		return search(e.X)
	case *ast.IsNull:
		return search(e.X)
	case *ast.ListLit:
		for _, x := range e.Elems {
			if b := search(x); b != nil {
				return b
			}
		}
	case *ast.MapLit:
		for _, en := range e.Entries {
			if b := search(en.Value); b != nil {
				return b
			}
		}
	case *ast.FuncCall:
		for _, x := range e.Args {
			if b := search(x); b != nil {
				return b
			}
		}
	}
	return nil
}

// scope resolves the variables in scope.
func (p *planner) scope() eval.Scope { return names(p.names) }

type names map[string]int

func (n names) Variable(name string) (int, bool) {
	s, ok := n[name]
	return s, ok
}

func (names) Aggregate(*ast.FuncCall) (int, bool) { return 0, false }

// hiddenPrefix starts the names of hidden slots.
const hiddenPrefix = "\x00"

// String renders a slot as its variable, or #n when hidden.
func (p *Part) String(slot int) string {
	if slot < len(p.Names) && p.Names[slot] != "" && !strings.HasPrefix(p.Names[slot], hiddenPrefix) {
		return p.Names[slot]
	}
	return fmt.Sprintf("#%d", slot)
}
