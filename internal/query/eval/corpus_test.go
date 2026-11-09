package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/budget"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/internal/query/sema"
	"github.com/TacFlow/tacdb/value"
)

// typed is a value of the corpus: a type and its value; floats are
// decimal text that reads back exactly, or NaN, +Inf, -Inf.
type typed struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	ID    uint64          `json:"id"`
	Nodes []uint64        `json:"nodes"`
	Edges []uint64        `json:"edges"`
}

func (v typed) decode(t *testing.T) qv.Value {
	t.Helper()
	switch v.Type {
	case "Null":
		return qv.Null()
	case "Node":
		return qv.Node(v.ID)
	case "Edge":
		return qv.Edge(v.ID)
	case "Path":
		return qv.PathOf(&qv.Path{Nodes: v.Nodes, Edges: v.Edges})
	case "Bool":
		var b bool
		must(t, json.Unmarshal(v.Value, &b))
		return qv.Bool(b)
	case "Int":
		var i int64
		must(t, json.Unmarshal(v.Value, &i))
		return qv.Int(i)
	case "Float":
		var s string
		must(t, json.Unmarshal(v.Value, &s))
		f, err := strconv.ParseFloat(s, 64)
		must(t, err)
		return qv.Float(f)
	case "String":
		var s string
		must(t, json.Unmarshal(v.Value, &s))
		return qv.String(s)
	case "List":
		var items []typed
		must(t, json.Unmarshal(v.Value, &items))
		out := make([]qv.Value, len(items))
		for i, item := range items {
			out[i] = item.decode(t)
		}
		return qv.List(out)
	case "Map":
		var m map[string]typed
		must(t, json.Unmarshal(v.Value, &m))
		var entries []qv.Entry
		for k, item := range m {
			entries = append(entries, qv.Entry{Key: k, Value: item.decode(t)})
		}
		out, err := qv.MapOf(entries)
		must(t, err)
		return out
	}
	t.Fatalf("unknown type %q", v.Type)
	return qv.Null()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// same is Identical, with every NaN the same.
func same(a, b qv.Value) bool {
	if qv.IsNaN(a) && qv.IsNaN(b) {
		return true
	}
	if a.Kind() == qv.KindList && b.Kind() == qv.KindList && a.Len() == b.Len() {
		for i := 0; i < a.Len(); i++ {
			if !same(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	}
	return qv.Identical(a, b)
}

type fixture struct {
	Nodes []struct {
		ID         uint64           `json:"id"`
		Labels     []string         `json:"labels"`
		Properties map[string]typed `json:"properties"`
	} `json:"nodes"`
	Edges []struct {
		ID         uint64           `json:"id"`
		Type       string           `json:"type"`
		Properties map[string]typed `json:"properties"`
	} `json:"edges"`
}

// graph is an eval.Graph over a fixture.
type graph struct {
	labels map[uint64][]string
	types  map[uint64]string
	props  map[qv.Kind]map[uint64]map[string]value.Value
}

var errNoEntity = errs.New(errs.CodeNotFound, "test", "no such entity")

func newGraph(t *testing.T, f fixture) *graph {
	g := &graph{labels: map[uint64][]string{}, types: map[uint64]string{},
		props: map[qv.Kind]map[uint64]map[string]value.Value{qv.KindNode: {}, qv.KindEdge: {}}}
	conv := func(m map[string]typed) map[string]value.Value {
		out := map[string]value.Value{}
		for k, v := range m {
			pv, err := qv.ToValue(v.decode(t))
			must(t, err)
			out[k] = pv
		}
		return out
	}
	for _, n := range f.Nodes {
		g.labels[n.ID] = n.Labels
		g.props[qv.KindNode][n.ID] = conv(n.Properties)
	}
	for _, e := range f.Edges {
		g.types[e.ID] = e.Type
		g.props[qv.KindEdge][e.ID] = conv(e.Properties)
	}
	return g
}

func (g *graph) Property(_ context.Context, k qv.Kind, id uint64, key string) (value.Value, bool, error) {
	props, ok := g.props[k][id]
	if !ok {
		return value.Value{}, false, errNoEntity
	}
	v, ok := props[key]
	return v, ok, nil
}

func (g *graph) Properties(_ context.Context, k qv.Kind, id uint64) (map[string]value.Value, error) {
	props, ok := g.props[k][id]
	if !ok {
		return nil, errNoEntity
	}
	return props, nil
}

func (g *graph) Labels(_ context.Context, id uint64) ([]string, error) {
	l, ok := g.labels[id]
	if !ok {
		return nil, errNoEntity
	}
	return l, nil
}

func (g *graph) EdgeType(_ context.Context, id uint64) (string, error) {
	t, ok := g.types[id]
	if !ok {
		return "", errNoEntity
	}
	return t, nil
}

// vars is a scope of named slots, without aggregates.
type vars []string

func (v vars) Variable(name string) (int, bool) {
	i := slices.Index(v, name)
	return i, i >= 0
}

func (vars) Aggregate(*ast.FuncCall) (int, bool) { return 0, false }

type exprCase struct {
	Expr     string           `json:"expr"`
	Result   *typed           `json:"result"`
	Error    string           `json:"error"`
	Kind     string           `json:"kind"`
	Source   string           `json:"source"`
	Decision string           `json:"decision"`
	Note     string           `json:"note"`
	Params   map[string]typed `json:"params"`
	Vars     map[string]typed `json:"vars"`
}

// evaluate runs RETURN expr through the parser, the semantic analysis
// when the case binds no variables, and the evaluator.
func evaluate(t *testing.T, c exprCase, g *graph) (qv.Value, error) {
	src := "RETURN " + c.Expr
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		return qv.Null(), err
	}
	params := map[string]value.Value{}
	for k, v := range c.Params {
		pv, err := qv.ToValue(v.decode(t))
		must(t, err)
		params[k] = pv
	}
	var names vars
	var row eval.Slots
	for k, v := range c.Vars {
		names = append(names, k)
		row = append(row, v.decode(t))
	}
	if len(c.Vars) == 0 {
		info, err := sema.Analyze(q, src, sema.Config{})
		if err != nil {
			return qv.Null(), err
		}
		if err := info.CheckParams(params, 0); err != nil {
			return qv.Null(), err
		}
	}
	x := q.Clauses[0].(*ast.Return).Projection.Items[0].X
	e, err := eval.Compile(x, names, src)
	if err != nil {
		return qv.Null(), err
	}
	b, err := budget.New(context.Background(), nil, budget.DefaultLimits())
	must(t, err)
	v, err := e.Eval(&eval.Env{Ctx: context.Background(), Params: params, Graph: g, Budget: b}, row)
	if b.Used() != 0 {
		t.Errorf("%s: %d bytes still charged after the evaluation", c.Expr, b.Used())
	}
	return v, err
}

func checkCase(t *testing.T, label string, kind, decision string, want *typed, wantErr string, got qv.Value, err error) {
	t.Helper()
	switch kind {
	case "parity", "tacdb":
	case "divergence":
		if decision == "" {
			t.Errorf("%s: a divergence names its decision", label)
		}
	default:
		t.Errorf("%s: unknown kind %q", label, kind)
	}
	if wantErr != "" {
		var e *errs.Error
		if !errors.As(err, &e) || string(e.Code) != wantErr {
			t.Errorf("%s: want %s, got %v (%v)", label, wantErr, got, err)
			return
		}
		if e.Stage == "" {
			t.Errorf("%s: %v has no stage", label, err)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: %v", label, err)
		return
	}
	if w := want.decode(t); !same(got, w) {
		t.Errorf("%s = %v (%v), want %v (%v)", label, got, got.Kind(), w, w.Kind())
	}
}

type corpus struct {
	Functions []struct {
		Name      string `json:"name"`
		Min       int    `json:"min"`
		Max       int    `json:"max"`
		Aggregate bool   `json:"aggregate"`
	} `json:"functions"`
	Graph      fixture    `json:"graph"`
	Cases      []exprCase `json:"cases"`
	Aggregates []struct {
		Aggregate string  `json:"aggregate"`
		Over      []typed `json:"over"`
		Distinct  bool    `json:"distinct"`
		Star      bool    `json:"star"`
		Ordered   bool    `json:"ordered"`
		Result    *typed  `json:"result"`
		Error     string  `json:"error"`
		Kind      string  `json:"kind"`
		Decision  string  `json:"decision"`
	} `json:"aggregates"`
}

func load(t *testing.T, name string) corpus {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	must(t, err)
	var c corpus
	must(t, json.Unmarshal(b, &c))
	return c
}

// The expression corpus (SPEC §15.2, §31.3).
func TestExpressions(t *testing.T) {
	c := load(t, "expressions.json")
	g := newGraph(t, c.Graph)
	for _, ec := range c.Cases {
		v, err := evaluate(t, ec, g)
		checkCase(t, ec.Expr, ec.Kind, ec.Decision, ec.Result, ec.Error, v, err)
	}
}

// The function matrix: the table, every scalar case and every aggregate
// case (SPEC §15.1, §15.2).
func TestFunctions(t *testing.T) {
	c := load(t, "functions.json")
	table := eval.Funcs()
	if len(table) != len(c.Functions) {
		t.Fatalf("%d functions in the table, %d in functions.json", len(table), len(c.Functions))
	}
	covered := map[string]bool{}
	for _, f := range c.Functions {
		fn, ok := eval.Lookup(f.Name)
		if !ok || fn.Name != f.Name || fn.Min != f.Min || fn.Max != f.Max || fn.Aggregate != f.Aggregate {
			t.Fatalf("%s: table %+v differs from functions.json %+v", f.Name, fn, f)
		}
	}
	g := newGraph(t, c.Graph)
	for _, ec := range c.Cases {
		v, err := evaluate(t, ec, g)
		checkCase(t, ec.Expr, ec.Kind, ec.Decision, ec.Result, ec.Error, v, err)
		for _, f := range c.Functions {
			if len(ec.Expr) > len(f.Name) && eqFold(ec.Expr[:len(f.Name)+1], f.Name+"(") {
				covered[f.Name] = true
			}
		}
	}
	r := rand.New(rand.NewSource(1))
	for _, ac := range c.Aggregates {
		fn, ok := eval.Lookup(ac.Aggregate)
		if !ok || !fn.Aggregate {
			t.Fatalf("%s is not an aggregate", ac.Aggregate)
		}
		covered[fn.Name] = true
		over := make([]qv.Value, len(ac.Over))
		for i, v := range ac.Over {
			over[i] = v.decode(t)
		}
		label := ac.Aggregate + " over " + qv.List(over).String()
		got, err := aggregate(t, fn, ac.Distinct, ac.Star, over)
		checkCase(t, label, ac.Kind, ac.Decision, ac.Result, ac.Error, got, err)
		if ac.Ordered || err != nil {
			continue
		}
		// DET-01: the result does not depend on the order of the rows.
		for i := 0; i < 20; i++ {
			shuffled := slices.Clone(over)
			r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			again, err := aggregate(t, fn, ac.Distinct, ac.Star, shuffled)
			if err != nil || !same(again, got) {
				t.Fatalf("%s: %v in another order gives %v, %v", label, got, again, err)
			}
		}
	}
	for _, f := range c.Functions {
		if !covered[f.Name] {
			t.Errorf("%s has no case", f.Name)
		}
	}
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i]|0x20, b[i]|0x20
		if x != y {
			return false
		}
	}
	return true
}

func aggregate(t *testing.T, fn *eval.Func, distinct, star bool, over []qv.Value) (qv.Value, error) {
	b, err := budget.New(context.Background(), nil, budget.DefaultLimits())
	must(t, err)
	acc := eval.NewAccumulator(fn, distinct, star, b)
	defer func() {
		acc.Close()
		if b.Used() != 0 {
			t.Errorf("%s: %d bytes charged after Close", fn.Name, b.Used())
		}
	}()
	for _, v := range over {
		if err := acc.Add(v); err != nil {
			return qv.Null(), err
		}
	}
	return acc.Result()
}

// The sum of Ints refuses overflow only on its final value, so the order
// of the rows never matters (DET-01).
func TestSumOverflowIsOrderFree(t *testing.T) {
	fn, _ := eval.Lookup("sum")
	vals := []qv.Value{qv.Int(math.MaxInt64), qv.Int(math.MaxInt64), qv.Int(math.MinInt64), qv.Int(math.MinInt64), qv.Int(5)}
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 50; i++ {
		r.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
		got, err := aggregate(t, fn, false, false, vals)
		if err != nil || !qv.Identical(got, qv.Int(3)) {
			t.Fatalf("sum %v = %v, %v", vals, got, err)
		}
	}
}
