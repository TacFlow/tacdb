package eval_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
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

// compile parses RETURN expr and compiles its expression against names.
func compile(t *testing.T, expr string, names vars) *eval.Expr {
	t.Helper()
	src := "RETURN " + expr
	q, err := parser.Parse(src, parser.DefaultLimits())
	must(t, err)
	e, err := eval.Compile(q.Clauses[0].(*ast.Return).Projection.Items[0].X, names, src)
	must(t, err)
	return e
}

// An error of the graph reaches the caller with its code, located at the
// expression that read it.
func TestGraphErrors(t *testing.T) {
	g := newGraph(t, fixture{})
	for _, expr := range []string{"n.k", "labels(n)", "properties(n)", "type(r)", "properties(r)"} {
		e := compile(t, expr, vars{"n", "r"})
		_, err := e.Eval(&eval.Env{Graph: g}, eval.Slots{qv.Node(99), qv.Edge(99)})
		var ee *errs.Error
		if !errors.As(err, &ee) || ee.Code != errs.CodeNotFound || ee.Location == nil || ee.Stage != errs.StageExecute {
			t.Fatalf("%s: %v", expr, err)
		}
	}
}

// What an evaluation builds is charged while it runs and returned when it
// ends; over the memory of the query it is RESOURCE_LIMIT.
func TestEvalBudget(t *testing.T) {
	long := "'" + strings.Repeat("a", 40) + "'"
	for _, expr := range []string{"range(1, 100)", "[range(1, 10), {a: range(1, 10)}]", "split('a,b,c', ',')",
		"replace(" + long + ", 'a', 'bb')", "toUpper(" + long + ") + " + long, "tail(range(1, 10))", "[1] + [2]", "toString(1e300)"} {
		e := compile(t, expr, nil)
		b, _ := budget.New(context.Background(), nil, budget.DefaultLimits())
		if _, err := e.Eval(&eval.Env{Budget: b}, nil); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if b.Used() != 0 || b.Peak() == 0 {
			t.Fatalf("%s: used %d peak %d", expr, b.Used(), b.Peak())
		}
		small, _ := budget.New(context.Background(), nil, budget.Limits{MemoryBytes: 64, WorkUnits: 1, Values: value.DefaultLimits()})
		_, err := e.Eval(&eval.Env{Budget: small}, nil)
		var ee *errs.Error
		if !errors.As(err, &ee) || ee.Code != errs.CodeResourceLimit || ee.Location == nil {
			t.Fatalf("%s under 64 bytes: %v", expr, err)
		}
		if small.Used() != 0 {
			t.Fatalf("%s: %d bytes left charged after an error", expr, small.Used())
		}
	}
	// Nesting deeper than MaxDepth is refused while the value is built.
	lim := value.DefaultLimits()
	lim.MaxDepth = 3
	b, _ := budget.New(context.Background(), nil, budget.Limits{MemoryBytes: 1 << 20, WorkUnits: 1, Values: lim})
	if _, err := compile(t, "[[[1]]]", nil).Eval(&eval.Env{Budget: b}, nil); errs.CodeOf(err) != errs.CodeResourceLimit {
		t.Fatalf("depth 4 under a limit of 3: %v", err)
	}
	if _, err := compile(t, "[[1]]", nil).Eval(&eval.Env{Budget: b}, nil); err != nil {
		t.Fatal(err)
	}
}

// Errors carry the stage and the location of the operator that failed.
func TestErrorLocation(t *testing.T) {
	e := compile(t, "1 +\n  (2 / 0)", nil)
	_, err := e.Eval(&eval.Env{}, nil)
	var ee *errs.Error
	if !errors.As(err, &ee) || ee.Stage != errs.StageExecute || ee.Location == nil || ee.Location.Line != 2 || ee.Location.Column != 4 {
		t.Fatalf("%v at %+v", err, ee.Location)
	}
	if _, err := eval.Compile(&ast.MapLit{Entries: []*ast.MapEntry{{Key: "a", Value: &ast.Literal{}}, {Key: "a", Value: &ast.Literal{}}}}, vars{}, ""); errs.CodeOf(err) != errs.CodeQuerySemantic {
		t.Fatalf("duplicate key compiles: %v", err)
	}
	_, err = eval.Compile(&ast.Variable{Name: "x", Span: ast.Span{Start: 7, End: 8}}, vars{}, "RETURN x")
	if !errors.As(err, &ee) || ee.Code != errs.CodeQuerySemantic || ee.Location.Column != 8 {
		t.Fatalf("unbound variable: %v", err)
	}
}

// A parameter is read once per evaluation environment and may be any
// property value.
func TestParameters(t *testing.T) {
	e := compile(t, "$p + 1", nil)
	env := &eval.Env{Params: map[string]value.Value{"p": value.Int(41)}}
	for i := 0; i < 2; i++ {
		v, err := e.Eval(env, nil)
		if err != nil || !qv.Identical(v, qv.Int(42)) {
			t.Fatalf("%v %v", v, err)
		}
	}
	_, err := e.Eval(&eval.Env{}, nil)
	if errs.CodeOf(err) != errs.CodeParamMissing {
		t.Fatalf("missing parameter: %v", err)
	}
}

// Aggregates are read from the slots the executor fills.
type aggScope struct{ slot int }

func (aggScope) Variable(string) (int, bool)           { return 0, false }
func (s aggScope) Aggregate(*ast.FuncCall) (int, bool) { return s.slot, true }
func (noAgg) Variable(string) (int, bool)              { return 0, false }
func (noAgg) Aggregate(*ast.FuncCall) (int, bool)      { return 0, false }

type noAgg struct{}

func TestAggregateSlots(t *testing.T) {
	src := "RETURN count(*) + 1"
	q, err := parser.Parse(src, parser.DefaultLimits())
	must(t, err)
	x := q.Clauses[0].(*ast.Return).Projection.Items[0].X
	e, err := eval.Compile(x, aggScope{slot: 0}, src)
	must(t, err)
	v, err := e.Eval(&eval.Env{}, eval.Slots{qv.Int(4)})
	if err != nil || !qv.Identical(v, qv.Int(5)) {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := eval.Compile(x, noAgg{}, src); errs.CodeOf(err) != errs.CodeQuerySemantic {
		t.Fatalf("aggregate without a slot: %v", err)
	}
}

// FuzzEval evaluates single expressions: no panic, only the errors of
// execution, the budget given back, the same result twice and after the
// canonical text is parsed again.
func FuzzEval(f *testing.F) {
	for _, s := range []string{"1 + 2", "[1, 'a'] + null", "range(1, 3)", "toString(1e300)", "-9223372036854775808 % -1",
		"substring('héllo', 1.5, 2)", "split('a,b', '')", "{a: [1, {b: 2.5}]}.a", "2 ^ 0.5", "1 IN [1.0, null]",
		"toInteger('1_0')", "replace('aa', 'a', 'bb')", "$p * 3", "coalesce(null, $p)", "NOT (1 < 'a') XOR true"} {
		f.Add(s)
	}
	allowed := map[errs.Code]bool{errs.CodeInvalidArgument: true, errs.CodeTypeMismatch: true,
		errs.CodeResourceLimit: true, errs.CodeQueryUnsupported: true}
	f.Fuzz(func(t *testing.T, s string) {
		// The input nests at most 32 deep, so that its canonical text,
		// which adds parentheses, fits the default limit (as FuzzParser).
		small := parser.Limits{MaxQueryBytes: 1 << 16, MaxDepth: 32}
		run := func(src string, lim parser.Limits) (qv.Value, error, bool) {
			q, err := parser.Parse(src, lim)
			if err != nil || len(q.Clauses) != 1 {
				return qv.Null(), nil, false
			}
			ret, ok := q.Clauses[0].(*ast.Return)
			if !ok || len(ret.Projection.Items) != 1 {
				return qv.Null(), nil, false
			}
			info, err := sema.Analyze(q, src, sema.Config{})
			if err != nil || len(info.Projections[ret.Projection].Aggregates) > 0 {
				return qv.Null(), nil, false
			}
			params := map[string]value.Value{}
			for _, p := range info.Params {
				params[p] = value.Int(3)
			}
			e, err := eval.Compile(ret.Projection.Items[0].X, noAgg{}, src)
			if err != nil {
				t.Fatalf("%q: analyzed but does not compile: %v", src, err)
			}
			b, _ := budget.New(context.Background(), nil, budget.Limits{MemoryBytes: 1 << 20, WorkUnits: 1, Values: value.DefaultLimits()})
			v, err := e.Eval(&eval.Env{Params: params, Budget: b}, nil)
			if b.Used() != 0 {
				t.Fatalf("%q: %d bytes left charged", src, b.Used())
			}
			if err != nil {
				var ee *errs.Error
				if !errors.As(err, &ee) || !allowed[ee.Code] || ee.Stage != errs.StageExecute || ee.Location == nil {
					t.Fatalf("%q: unexpected error %v", src, err)
				}
			}
			again, err2 := e.Eval(&eval.Env{Params: params, Budget: b}, nil)
			if errs.CodeOf(err) != errs.CodeOf(err2) || !same(v, again) {
				t.Fatalf("%q: %v, %v then %v, %v", src, v, err, again, err2)
			}
			return v, err, true
		}
		src := "RETURN " + s
		v, err, ok := run(src, small)
		if !ok {
			return
		}
		q, _ := parser.Parse(src, small)
		canon := ast.Format(q)
		w, err2, ok := run(canon, parser.DefaultLimits())
		if !ok {
			t.Fatalf("%q: the canonical text %q does not evaluate", src, canon)
		}
		if errs.CodeOf(err) != errs.CodeOf(err2) || !same(v, w) {
			t.Fatalf("%q: %v, %v; canonical %q: %v, %v", src, v, err, canon, w, err2)
		}
	})
}

// Parameters, properties and concatenations are charged before they are
// copied, so a large value is refused without being allocated.
func TestChargedBeforeCopy(t *testing.T) {
	big := make([]value.Value, 1_000_000)
	for i := range big {
		big[i] = value.Int(int64(i))
	}
	p := value.List(big...)
	g := newGraph(t, fixture{})
	g.props[qv.KindNode][1] = map[string]value.Value{"k": p}
	for _, expr := range []string{"size($p)", "size($p + $p)", "size(n.k)", "size(n.k + 1)", "size(tail($p))"} {
		e := compile(t, expr, vars{"n"})
		b, _ := budget.New(context.Background(), nil, budget.Limits{MemoryBytes: 1 << 20, WorkUnits: 1, Values: value.DefaultLimits()})
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := e.Eval(&eval.Env{Params: map[string]value.Value{"p": p}, Graph: g, Budget: b}, eval.Slots{qv.Node(1)})
		runtime.ReadMemStats(&after)
		if errs.CodeOf(err) != errs.CodeResourceLimit {
			t.Fatalf("%s under 1 MiB: %v", expr, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
			t.Fatalf("%s allocated %d bytes before it was refused", expr, grew)
		}
	}
	// Each operand fits MaxValueElements, the concatenation does not.
	lim := value.DefaultLimits()
	lim.MaxValueElements = 1_000_001
	for _, expr := range []string{"size($p + $p)", "size($p + [1, 2])", "size([$p] + $p)"} {
		b, _ := budget.New(context.Background(), nil, budget.Limits{MemoryBytes: 1 << 40, WorkUnits: 1, Values: lim})
		_, err := compile(t, expr, nil).Eval(&eval.Env{Params: map[string]value.Value{"p": p}, Budget: b}, nil)
		if errs.CodeOf(err) != errs.CodeResourceLimit {
			t.Fatalf("%s over the elements limit: %v", expr, err)
		}
	}
}
