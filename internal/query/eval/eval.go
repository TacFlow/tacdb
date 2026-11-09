// Package eval compiles and evaluates the expressions of tacdb-cypher/1
// (SPEC §15.2, §31.3) and holds the table of functions and the
// accumulators of aggregations. An expression is compiled once against the
// slots of its scope and evaluated on each row; values it builds are
// charged to the budget of the execution while it runs.
package eval

import (
	"context"
	"math"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/budget"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// Graph reads the entities that expressions and functions look into.
type Graph interface {
	// Property returns a property of a node (kind qv.KindNode) or an edge
	// (qv.KindEdge), and false when it is absent.
	Property(ctx context.Context, kind qv.Kind, id uint64, key string) (value.Value, bool, error)
	// Properties returns every property of a node or an edge.
	Properties(ctx context.Context, kind qv.Kind, id uint64) (map[string]value.Value, error)
	// Labels returns the labels of a node in insertion order.
	Labels(ctx context.Context, id uint64) ([]string, error)
	// EdgeType returns the type of an edge.
	EdgeType(ctx context.Context, id uint64) (string, error)
}

// Env is what an execution gives to every expression besides the row. It
// belongs to one execution and is not safe for concurrent use.
type Env struct {
	Ctx    context.Context
	Params map[string]value.Value
	Graph  Graph
	Budget *budget.Budget // nil: no limits

	params map[string]qv.Value // runtime forms of Params, built on first use
}

// Row gives the value of each slot of a row.
type Row interface {
	Slot(i int) qv.Value
}

// Slots is a Row backed by a slice.
type Slots []qv.Value

// Slot returns slot i.
func (s Slots) Slot(i int) qv.Value { return s[i] }

// Scope resolves names when an expression is compiled.
type Scope interface {
	// Variable returns the slot of a variable.
	Variable(name string) (slot int, ok bool)
	// Aggregate returns the slot where the executor puts the result of an
	// aggregate call; ok is false where an aggregate is not allowed.
	Aggregate(call *ast.FuncCall) (slot int, ok bool)
}

// ExprScope is a Scope that also resolves whole subexpressions: an
// expression it maps to a slot is read from that slot instead of being
// evaluated, as an ORDER BY reads an item it repeats, or an aggregating
// projection a grouping key.
type ExprScope interface {
	Scope
	Expr(e ast.Expr) (slot int, ok bool)
}

// Expr is a compiled expression. It is immutable and safe for concurrent
// use.
type Expr struct {
	root node
	src  string
}

type node func(f *frame) (qv.Value, error)

// frame is one evaluation: it charges what the expression builds and
// returns it when the evaluation ends, because the caller charges what it
// keeps.
type frame struct {
	env  *Env
	row  Row
	src  string
	used int64
}

// Compile compiles e against scope. src is the source of the query, for
// the locations of errors. A name the scope does not resolve, an unknown
// function, a wrong number of arguments, DISTINCT or * where they do not
// apply, and an aggregate where it is not allowed are QUERY_SEMANTIC.
func Compile(e ast.Expr, scope Scope, src string) (*Expr, error) {
	c := compiler{scope: scope, src: src}
	root, err := c.expr(e)
	if err != nil {
		return nil, err
	}
	return &Expr{root: root, src: src}, nil
}

// Eval evaluates the expression on row.
func (e *Expr) Eval(env *Env, row Row) (qv.Value, error) {
	f := frame{env: env, row: row, src: e.src}
	v, err := e.root(&f)
	if f.used > 0 {
		env.Budget.Shrink(f.used)
	}
	return v, err
}

// grow charges bytes for a value being built.
func (f *frame) grow(bytes int64) error {
	if f.env.Budget == nil {
		return nil
	}
	if err := f.env.Budget.Grow(bytes); err != nil {
		return err
	}
	f.used += bytes
	return nil
}

// unlimited checks shapes when an evaluation has no budget, so that no
// intermediate value escapes the default limits of values.
var unlimited, _ = budget.New(context.Background(), nil, budget.Limits{
	MemoryBytes: math.MaxInt64, WorkUnits: math.MaxUint64, Values: value.DefaultLimits(),
})

// build charges a List or a Map of the given shape before it is built.
func (f *frame) build(elems int64, depth int, bytes int64) error {
	b := f.env.Budget
	if b == nil {
		b = unlimited
	}
	if err := b.CheckShape(elems, depth); err != nil {
		return err
	}
	return f.grow(bytes)
}

func (f *frame) ctx() context.Context {
	if f.env.Ctx == nil {
		return context.Background()
	}
	return f.env.Ctx
}

// fail returns an execution error at span.
func (f *frame) fail(span ast.Span, code errs.Code, format string, args ...any) error {
	e := errs.Newf(code, "query", format, args...)
	e.Stage = errs.StageExecute
	e.Location = ast.Locate(f.src, span)
	return e
}

// at gives an execution error without a location the location of span.
func (f *frame) at(span ast.Span, err error) error {
	if e, ok := err.(*errs.Error); ok && e.Location == nil && e.Stage == "" {
		c := *e
		c.Stage = errs.StageExecute
		c.Location = ast.Locate(f.src, span)
		return &c
	}
	return err
}

type compiler struct {
	scope Scope
	src   string
}

func (c *compiler) fail(span ast.Span, format string, args ...any) error {
	e := errs.Newf(errs.CodeQuerySemantic, "query", format, args...)
	e.Stage = errs.StageSemantic
	e.Location = ast.Locate(c.src, span)
	return e
}

func (c *compiler) exprs(list []ast.Expr) ([]node, error) {
	out := make([]node, len(list))
	for i, e := range list {
		n, err := c.expr(e)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

func (c *compiler) expr(e ast.Expr) (node, error) {
	if es, ok := c.scope.(ExprScope); ok {
		if slot, ok := es.Expr(e); ok {
			return func(f *frame) (qv.Value, error) { return f.row.Slot(slot), nil }, nil
		}
	}
	switch e := e.(type) {
	case *ast.Literal:
		v := qv.FromValue(e.Value)
		return func(*frame) (qv.Value, error) { return v, nil }, nil
	case *ast.Parameter:
		return parameter(e), nil
	case *ast.Variable:
		slot, ok := c.scope.Variable(e.Name)
		if !ok {
			return nil, c.fail(e.Span, "variable %s is not defined", ast.Name(e.Name))
		}
		return func(f *frame) (qv.Value, error) { return f.row.Slot(slot), nil }, nil
	case *ast.Property:
		x, err := c.expr(e.X)
		if err != nil {
			return nil, err
		}
		return property(e, x), nil
	case *ast.ListLit:
		elems, err := c.exprs(e.Elems)
		if err != nil {
			return nil, err
		}
		return listLit(e, elems), nil
	case *ast.MapLit:
		return c.mapLit(e)
	case *ast.Unary:
		x, err := c.expr(e.X)
		if err != nil {
			return nil, err
		}
		if e.Op == ast.OpNot {
			return func(f *frame) (qv.Value, error) {
				v, err := x(f)
				if err != nil {
					return qv.Value{}, err
				}
				return qv.Bool(!qv.Truthy(v)), nil
			}, nil
		}
		return negate(e, x), nil
	case *ast.Binary:
		l, err := c.expr(e.L)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(e.R)
		if err != nil {
			return nil, err
		}
		return infix(e, l, r), nil
	case *ast.IsNull:
		x, err := c.expr(e.X)
		if err != nil {
			return nil, err
		}
		return func(f *frame) (qv.Value, error) {
			v, err := x(f)
			if err != nil {
				return qv.Value{}, err
			}
			return qv.Bool(v.IsNull() != e.Not), nil
		}, nil
	case *ast.FuncCall:
		return c.call(e)
	}
	panic("eval: unknown expression node")
}

// parameter reads a parameter. Its runtime form is built once per
// environment and charged on every read, like a value the expression
// builds.
func parameter(e *ast.Parameter) node {
	return func(f *frame) (qv.Value, error) {
		if v, ok := f.env.params[e.Name]; ok {
			if err := f.build(v.Elements(), v.Depth(), v.Size()); err != nil {
				return qv.Value{}, f.at(e.Span, err)
			}
			return v, nil
		}
		p, ok := f.env.Params[e.Name]
		if !ok {
			return qv.Value{}, f.fail(e.Span, errs.CodeParamMissing, "parameter $%s is missing", e.Name)
		}
		if err := f.build(qv.ShapeOf(p)); err != nil {
			return qv.Value{}, f.at(e.Span, err)
		}
		v := qv.FromValue(p)
		if f.env.params == nil {
			f.env.params = make(map[string]qv.Value)
		}
		f.env.params[e.Name] = v
		return v, nil
	}
}

func property(e *ast.Property, x node) node {
	return func(f *frame) (qv.Value, error) {
		v, err := x(f)
		if err != nil {
			return qv.Value{}, err
		}
		switch v.Kind() {
		case qv.KindNull:
			return qv.Value{}, nil
		case qv.KindNode, qv.KindEdge:
			p, _, err := f.env.Graph.Property(f.ctx(), v.Kind(), v.ID(), e.Key)
			if err != nil {
				return qv.Value{}, f.at(e.Span, err)
			}
			if err := f.build(qv.ShapeOf(p)); err != nil {
				return qv.Value{}, f.at(e.Span, err)
			}
			return qv.FromValue(p), nil // absent is Null (SPEC §5.2)
		case qv.KindMap:
			item, _ := v.Lookup(e.Key)
			return item, nil
		}
		return qv.Value{}, f.fail(e.Span, errs.CodeTypeMismatch, "cannot read a property of %s", v.Kind())
	}
}

func listLit(e *ast.ListLit, elems []node) node {
	return func(f *frame) (qv.Value, error) {
		items := make([]qv.Value, len(elems))
		for i, el := range elems {
			v, err := el(f)
			if err != nil {
				return qv.Value{}, err
			}
			items[i] = v
		}
		v, err := f.list(items)
		if err != nil {
			return qv.Value{}, f.at(e.Span, err)
		}
		return v, nil
	}
}

// list charges and builds a List.
func (f *frame) list(items []qv.Value) (qv.Value, error) {
	elems, depth, bytes := int64(len(items)), 1, int64(qv.ElementBytes)
	for i := range items {
		elems += items[i].Elements()
		depth = max(depth, items[i].Depth()+1)
		bytes += items[i].Size()
	}
	if err := f.build(elems, depth, bytes); err != nil {
		return qv.Value{}, err
	}
	return qv.List(items), nil
}

func (c *compiler) mapLit(e *ast.MapLit) (node, error) {
	keys := make([]string, len(e.Entries))
	vals := make([]node, len(e.Entries))
	seen := make(map[string]bool, len(e.Entries))
	for i, en := range e.Entries {
		if seen[en.Key] {
			return nil, c.fail(en.Span, "duplicate key %s in a map", ast.Name(en.Key))
		}
		seen[en.Key] = true
		v, err := c.expr(en.Value)
		if err != nil {
			return nil, err
		}
		keys[i], vals[i] = en.Key, v
	}
	return func(f *frame) (qv.Value, error) {
		entries := make([]qv.Entry, len(vals))
		elems, depth, bytes := int64(len(vals)), 1, int64(qv.ElementBytes)
		for i, el := range vals {
			v, err := el(f)
			if err != nil {
				return qv.Value{}, err
			}
			entries[i] = qv.Entry{Key: keys[i], Value: v}
			elems += v.Elements()
			depth = max(depth, v.Depth()+1)
			bytes += v.Size() + int64(len(keys[i]))
		}
		if err := f.build(elems, depth, bytes); err != nil {
			return qv.Value{}, f.at(e.Span, err)
		}
		return qv.MapOf(entries)
	}, nil
}

func negate(e *ast.Unary, x node) node {
	return func(f *frame) (qv.Value, error) {
		v, err := x(f)
		if err != nil {
			return qv.Value{}, err
		}
		switch v.Kind() {
		case qv.KindNull:
			return qv.Value{}, nil
		case qv.KindInt:
			i, _ := v.AsInt()
			if i == math.MinInt64 {
				return qv.Value{}, f.fail(e.Span, errs.CodeInvalidArgument, "integer overflow")
			}
			return qv.Int(-i), nil
		case qv.KindFloat:
			x, _ := v.AsFloat()
			return float(-x), nil
		}
		return qv.Value{}, f.fail(e.Span, errs.CodeTypeMismatch, "cannot negate %s", v.Kind())
	}
}
