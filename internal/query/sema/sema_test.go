package sema

import (
	"errors"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// procedures is a registry for the tests, shaped like the one of SPEC
// §15.5.
func procedures(name []string) (*Procedure, bool) {
	switch strings.Join(name, ".") {
	case "tacdb.graph.shortest_path":
		return &Procedure{Name: "tacdb.graph.shortest_path", Args: []qv.Type{qv.TInt, qv.TInt, qv.TMap},
			Yields: []Column{{"path", qv.TPath}, {"hops", qv.TInt}}}, true
	case "test.write":
		return &Procedure{Name: "test.write", Yields: []Column{{"n", qv.TInt}}, Write: true}, true
	}
	return nil, false
}

func analyze(t *testing.T, src string, cfg Config) (*Info, error) {
	t.Helper()
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	if cfg.Procedure == nil {
		cfg.Procedure = procedures
	}
	return Analyze(q, src, cfg)
}

// Every QUERY_SEMANTIC rule of SPEC §15.2, §31.3 and the design, and
// queries that must pass.
func TestRules(t *testing.T) {
	for _, c := range []struct {
		src  string
		want string // "" passes; otherwise a part of the message
	}{
		// variables
		{"MATCH (n) RETURN n", ""},
		{"MATCH (n) RETURN m", "variable m is not defined"},
		{"MATCH (n)-[r]->(m), (m)-[r]->(x) RETURN n", "used twice in one MATCH"},
		{"MATCH (n)-[r]->(m)-[r]->(x) RETURN n", "used twice in one MATCH"},
		{"MATCH (n)-[r]->(m) MATCH (m)-[r]->(x) RETURN n", ""},
		{"MATCH (n)-->(n) RETURN n", ""},
		{"MATCH (n)-[r]->() MATCH (r) RETURN n", "is bound to Edge, not Node"},
		{"MATCH (n) MATCH ()-[n]->() RETURN n", "is bound to Node, not Edge"},
		{"UNWIND [1] AS x MATCH (x) RETURN x", ""},
		{"UNWIND [1, 2] AS x UNWIND [3] AS x RETURN x", "already bound"},
		{"MATCH (n) WITH n AS m RETURN n", "variable n is not defined"},
		{"OPTIONAL MATCH (n) RETURN n", ""},
		// projections
		{"MATCH (n) RETURN n.name, count(*)", ""},
		{"MATCH (n) RETURN n.name, count(*) + n.age", "must be a grouping key"},
		{"MATCH (n) RETURN n, count(*) + size(n.name)", ""},
		{"MATCH (n) RETURN n.name, count(*) + size(n.name)", ""},
		{"MATCH (n) RETURN toString(count(n)), [count(*)], {c: count(*)}", ""},
		{"MATCH (n) WITH n.name RETURN 1", "needs an alias"},
		{"MATCH (n) WITH n RETURN n", ""},
		{"MATCH (n) WITH n.name AS x RETURN x", ""},
		{"MATCH (n) RETURN n.x AS a, n.y AS a", "projected twice"},
		{"MATCH (n) RETURN n.x, n.x", "projected twice"},
		{"MATCH (n) RETURN n.name ORDER BY n.age", ""},
		{"MATCH (n) RETURN DISTINCT n.name ORDER BY n.age", "variable n is not defined"},
		{"MATCH (n) RETURN DISTINCT n.name ORDER BY n.name", ""},
		{"MATCH (n) RETURN DISTINCT n.name AS k ORDER BY k", ""},
		{"MATCH (n) RETURN n.k AS k, count(*) AS c ORDER BY c DESC, k", ""},
		{"MATCH (n) RETURN n.k AS k, count(*) ORDER BY count(*)", ""},
		{"MATCH (n) RETURN n.k AS k, count(*) ORDER BY sum(n.v)", ""},
		{"MATCH (n) RETURN n.k AS k, count(*) ORDER BY n.v", "variable n is not defined"},
		{"MATCH (n) RETURN n.k ORDER BY count(*)", "needs a projection that aggregates"},
		{"MATCH (n) RETURN count(*) ORDER BY {a: 1, a: 2}", "duplicate key"},
		{"MATCH (n) WITH n.k AS k, count(*) AS c WHERE c > 1 RETURN k", ""},
		{"MATCH (n) WITH n.k AS k WHERE n.x > 1 RETURN k", "variable n is not defined"},
		// aggregates
		{"MATCH (n) WHERE count(n) > 1 RETURN n", "allowed only in WITH, RETURN"},
		{"MATCH (n) RETURN count(count(n))", "cannot be nested"},
		{"MATCH (n) RETURN count(*), count(), count(DISTINCT n), COUNT(n)", ""},
		{"MATCH (n {k: count(n)}) RETURN n", "allowed only in WITH, RETURN"},
		{"UNWIND [count(1)] AS x RETURN x", "allowed only in WITH, RETURN"},
		{"MATCH (n) SET n.k = count(n)", "allowed only in WITH, RETURN"},
		// functions
		{"RETURN foo(1)", "unknown function foo"},
		{"RETURN size(1, 2)", "size takes 1 argument, got 2"},
		{"RETURN substring('a')", "substring takes 2 to 3 arguments"},
		{"RETURN TOUPPER('a'), Coalesce()", ""},
		{"RETURN toUpper(DISTINCT 'a')", "DISTINCT applies only to aggregates"},
		// static types (SPEC §31.3)
		{"RETURN 'a' - 1", "String - Int cannot succeed"},
		{"MATCH (n) RETURN n < 1", "Node < Int cannot succeed"},
		{"MATCH (n)-[r]->() RETURN n + r", "Node + Edge cannot succeed"},
		{"RETURN true + 1", "Bool + Int cannot succeed"},
		{"RETURN 1 IN 2", "cannot succeed"},
		{"UNWIND [1] AS x RETURN x IN 1, $p IN 1, $p @@ 1, $p <=> 1", ""},
		{"RETURN 'a' CONTAINS 1", "cannot succeed"},
		{"RETURN -'a'", "cannot negate String"},
		{"RETURN 1.x", "cannot read a property of Int"},
		{"WITH 1 AS x RETURN x < 'a'", "Int < String cannot succeed"},
		{"RETURN (1 < 2) + 1", "cannot succeed"},
		{"RETURN 'a' + 'b', 1 + 2.5, [1] + 'x', 'a' < 'b', 1 < 2.5, null + 1, null < 'a'", ""},
		{"MATCH (n) WHERE 1 RETURN n", ""},
		{"MATCH (n) WHERE n.k + 1 > $p AND NOT n.x RETURN n", ""},
		{"RETURN {a: 1}.a, $m.k", ""},
		{"RETURN 1 <=> 2", "cannot succeed"},
		{"RETURN 'a' @@ 1", "cannot succeed"},
		// SKIP and LIMIT
		{"MATCH (n) RETURN n SKIP -1", "SKIP and LIMIT need"},
		{"MATCH (n) RETURN n LIMIT 1.5", "SKIP and LIMIT need"},
		{"MATCH (n) RETURN n LIMIT 1 + 1", "SKIP and LIMIT need"},
		{"MATCH (n) RETURN n SKIP 0 LIMIT $l", ""},
		// writes
		{"MATCH (a) CREATE (a:L)", "cannot add labels or properties"},
		{"MATCH (a) CREATE (a {k: 1})-[:R]->(b)", "cannot add labels or properties"},
		{"MATCH (a) CREATE (a)", "creates nothing"},
		{"MATCH (a) CREATE (a)-[:R]->(b:L {x: a.y})", ""},
		{"CREATE (a {x: 1}), (b {y: a.x})", ""},
		{"CREATE (a {x: a.y})", "variable a is not defined"},
		{"CREATE (a)-[:R {w: b.x}]->(b)", "variable b is not defined"},
		{"CREATE (a)-[:R]->(a)", ""},
		{"CREATE (a)-[:R]->(a:L)", "cannot add labels or properties"},
		{"CREATE (a)-[:R|S]->(b)", "exactly one relationship type"},
		{"CREATE (a)-[]->(b)", "exactly one relationship type"},
		{"CREATE (a)-[:R]-(b)", "needs a relationship direction"},
		{"CREATE (a)-[r:R]->(b)-[r:R]->(c)", "already bound"},
		{"MATCH ()-[r]->() CREATE (a)-[r:R]->(b)", "already bound"},
		{"MATCH ()-[r]->() CREATE (r)-[:R]->(b)", "not a node"},
		{"MERGE (a {k: null})", "cannot match a null property"},
		{"MERGE (a {k: $p})", ""},
		{"MERGE (a)-[:R]-(b)", ""},
		{"MERGE (a)-[:R*]->(b)", "variable-length"},
		{"MERGE (a {k: 1})-[:R]->(b {k: a.k})", "variable a is not defined"},
		{"MATCH (a) MERGE (a)-[:R]-(b) ON CREATE SET b.x = 1 ON MATCH SET a:Seen, b += {y: 2}", ""},
		{"MATCH (a) MERGE (a:L)-[:R]->(b)", "cannot add labels or properties"},
		{"MATCH (a) MERGE (a)", "creates nothing"},
		{"MATCH (a)-[r]->() SET r:L", "only nodes have labels"},
		{"MATCH (a) SET a = 1", "needs a map of properties"},
		{"MATCH (a) SET a = {k: 1}, a += $m, a.k = null, a:L:M", ""},
		{"MATCH (a) WITH a.x AS x SET x.k = 1", ""},
		{"WITH 1 AS x SET x.k = 1", "only nodes and relationships have properties"},
		{"MATCH (a) SET b.k = 1", "variable b is not defined"},
		{"MATCH (a) REMOVE a.k, a:L", ""},
		{"MATCH ()-[r]->() REMOVE r:L", "only nodes have labels"},
		{"MATCH (a) DELETE a", ""},
		{"MATCH (a)-[r]->() DETACH DELETE a, r", ""},
		{"DELETE 1", "DELETE needs a node"},
		{"MATCH (a) DELETE a.k", ""},
		// maps
		{"RETURN {a: 1, a: 2}", "duplicate key a"},
		{"MATCH (n {a: 1, a: 2}) RETURN n", "duplicate key a"},
		// CALL
		{"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path, hops RETURN path", ""},
		{"CALL tacdb.graph.shortest_path(1, 2, {})", ""},
		{"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path AS p WHERE p IS NOT NULL", ""},
		{"CALL x.y()", "unknown procedure x.y"},
		{"CALL tacdb.graph.shortest_path(1, 2)", "takes 3 arguments, got 2"},
		{"CALL tacdb.graph.shortest_path('a', 2, {})", "argument 1 of tacdb.graph.shortest_path cannot be String"},
		{"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD nope RETURN 1", "does not yield nope"},
		{"MATCH (path) CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path RETURN 1", "already bound"},
		{"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path, path RETURN 1", "already bound"},
		// USING VECTOR
		{"MATCH (n:L) USING VECTOR n.v OPTIONS {k: 3} RETURN n", ""},
		{"MATCH (n:L)-[r]->() USING VECTOR r.v RETURN n", "needs a node variable"},
		{"MATCH (n:L) USING VECTOR m.v RETURN n", "variable m is not defined"},
	} {
		_, err := analyze(t, c.src, Config{})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%q: %v", c.src, err)
		case c.want != "":
			var e *errs.Error
			if !errors.As(err, &e) || e.Code != errs.CodeQuerySemantic || !strings.Contains(e.Message, c.want) ||
				e.Stage != errs.StageSemantic || e.Location == nil {
				t.Errorf("%q: want QUERY_SEMANTIC %q, got %v", c.src, c.want, err)
			}
		}
	}
}

// Columns, classification and parameters.
func TestInfo(t *testing.T) {
	for _, c := range []struct {
		src    string
		write  bool
		cols   []Column
		params []string
	}{
		{"MATCH (n) RETURN n, n.name, count(*) AS c", false, []Column{{"n", qv.TNode}, {"n.name", qv.TAny}, {"c", qv.TInt}}, nil},
		{"OPTIONAL MATCH (n) WITH n AS m RETURN m", false, []Column{{"m", qv.TNode | qv.TNull}}, nil},
		{"RETURN 1 + 2, 'a' + 'b', $x", false, []Column{{"1 + 2", qv.TInt | qv.TNull}, {"'a' + 'b'", qv.TString | qv.TNull}, {"$x", qv.TAny}}, []string{"x"}},
		{"CREATE (n {k: $b}) SET n.j = $a", true, nil, []string{"a", "b"}},
		{"MATCH (n) DELETE n RETURN count(*)", true, []Column{{"count(*)", qv.TInt}}, nil},
		{"CALL tacdb.graph.shortest_path($s, $t, {})", false, []Column{{"path", qv.TPath}, {"hops", qv.TInt}}, []string{"s", "t"}},
		{"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD hops AS h", false, []Column{{"h", qv.TInt}}, nil},
		{"CALL test.write() YIELD n RETURN n", true, []Column{{"n", qv.TInt}}, nil},
		{"MATCH (n) RETURN n LIMIT $l", false, []Column{{"n", qv.TNode}}, []string{"l"}},
	} {
		in, err := analyze(t, c.src, Config{})
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if in.Write != c.write || !sameColumns(in.Columns, c.cols) || strings.Join(in.Params, ",") != strings.Join(c.params, ",") {
			t.Errorf("%q: write %v cols %v params %v", c.src, in.Write, in.Columns, in.Params)
		}
	}
}

func sameColumns(a, b []Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The name of a column without alias is the expression as written, with
// spaces and comments made one space outside of quotes.
func TestColumnNames(t *testing.T) {
	for src, want := range map[string]string{
		"MATCH (n) RETURN n.name":            "n.name",
		"RETURN  1  +\n\t2 ":                 "1 + 2",
		"RETURN 1 // one\n + 2":              "1 + 2",
		"RETURN 'a  b' + \"c // d\"":         "'a  b' + \"c // d\"",
		"RETURN 'it\\'s  x'":                 "'it\\'s  x'",
		"MATCH (`a  b`) RETURN `a  b`.k":     "`a  b`.k",
		"RETURN count( * )":                  "count( * )",
		"RETURN toUpper('x')":                "toUpper('x')",
		"RETURN [1,2]":                       "[1,2]",
		"RETURN (1 + 2) * 3":                 "(1 + 2) * 3",
		"RETURN (1) + 2":                     "(1) + 2",
		"RETURN -(1)":                        "-(1)",
		"RETURN ( 1 + 2 ) ":                  "( 1 + 2 )",
		"UNWIND [1] AS x RETURN x, x + 1":    "x",
		"UNWIND [1] AS x RETURN x + 1 AS y ": "y",
	} {
		q, err := parser.Parse(src, parser.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		in, err := Analyze(q, src, Config{})
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if in.Columns[0].Name != want {
			t.Errorf("%q: column %q, want %q", src, in.Columns[0].Name, want)
		}
	}
}

// Variables resolve to symbols: the same symbol through a clause, a new
// one after WITH; CREATE reuses a bound node.
func TestSymbols(t *testing.T) {
	src := "MATCH (n)-[r]->(m) WITH n, r AS e MATCH (n)-->(x) CREATE (n)-[:T]->(y) RETURN n, e, y"
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	in, err := Analyze(q, src, Config{})
	if err != nil {
		t.Fatal(err)
	}
	m1 := q.Clauses[0].(*ast.Match)
	m2 := q.Clauses[2].(*ast.Match)
	cr := q.Clauses[3].(*ast.Create)
	ret := q.Clauses[4].(*ast.Return)
	with := q.Clauses[1].(*ast.With)
	n1, n2 := in.Decls[m1.Patterns[0].Nodes[0]], in.Decls[m2.Patterns[0].Nodes[0]]
	if n1 == nil || n2 == nil || n1 == n2 || n2 != in.Decls[with.Projection.Items[0]] {
		t.Fatalf("n before WITH %v, after %v", n1, n2)
	}
	if n2.Type != qv.TNode || in.Decls[with.Projection.Items[1]].Type != qv.TEdge {
		t.Fatalf("types through WITH: %v %v", n2.Type, in.Decls[with.Projection.Items[1]].Type)
	}
	if in.Decls[cr.Patterns[0].Nodes[0]] != n2 || !in.Bound[cr.Patterns[0].Nodes[0]] || in.Bound[cr.Patterns[0].Nodes[1]] {
		t.Fatal("CREATE reuses the bound n and creates y")
	}
	for _, it := range ret.Projection.Items {
		if in.Refs[it.X.(*ast.Variable)] == nil {
			t.Fatalf("%s is not resolved", it.X.(*ast.Variable).Name)
		}
	}
	if in.Refs[ret.Projection.Items[0].X.(*ast.Variable)] != n2 {
		t.Fatal("RETURN n is the n of the WITH")
	}
	if len(in.Symbols) != 10 { // n r m | n e | x | y | n e y of RETURN
		t.Fatalf("%d symbols", len(in.Symbols))
	}
}

// The analysis of a projection: grouping keys, aggregates of the items and
// of ORDER BY, and ORDER BY expressions that are items.
func TestProjection(t *testing.T) {
	src := "MATCH (n) RETURN n.k AS k, count(*) AS c, sum(n.v) + 1 AS s ORDER BY n.k, count(*), max(n.v), c"
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	in, err := Analyze(q, src, Config{})
	if err != nil {
		t.Fatal(err)
	}
	p := q.Clauses[1].(*ast.Return).Projection
	pr := in.Projections[p]
	if !pr.Aggregating || len(pr.Keys) != 1 || pr.Keys[0] != 0 {
		t.Fatalf("keys %v", pr.Keys)
	}
	var names []string
	for _, a := range pr.Aggregates {
		names = append(names, ast.FormatExpr(a))
	}
	if got := strings.Join(names, " "); got != "count(*) sum(n.v) max(n.v)" {
		t.Fatalf("aggregates %s", got)
	}
	if pr.OrderItems[p.Order[0].X] != 0 || pr.OrderItems[p.Order[1].X] != 1 {
		t.Fatalf("order items %v", pr.OrderItems)
	}
	if _, ok := pr.OrderItems[p.Order[2].X]; ok {
		t.Fatal("max(n.v) is not an item")
	}
	if !pr.Items[2].Aggregate || pr.Items[0].Aggregate {
		t.Fatal("aggregate items")
	}
}

// Parameters are checked before execution (SPEC §15.2).
func TestCheckParams(t *testing.T) {
	in, err := analyze(t, "MATCH (n) MERGE (m {k: $k}) RETURN n SKIP $s LIMIT $l", Config{})
	if err != nil {
		t.Fatal(err)
	}
	ok := map[string]value.Value{"k": value.Int(1), "s": value.Int(0), "l": value.Int(10)}
	if err := in.CheckParams(ok, 0); err != nil {
		t.Fatal(err)
	}
	with := func(k string, v value.Value) map[string]value.Value {
		p := map[string]value.Value{}
		for a, b := range ok {
			p[a] = b
		}
		if v.Kind() == value.KindVector { // a marker for absent
			delete(p, k)
		} else {
			p[k] = v
		}
		return p
	}
	absent := value.Vector([]float32{1})
	for _, c := range []struct {
		p    map[string]value.Value
		code errs.Code
	}{
		{with("l", absent), errs.CodeParamMissing},
		{with("l", value.Int(-1)), errs.CodeParamType},
		{with("s", value.Float(1)), errs.CodeParamType},
		{with("s", value.Null()), errs.CodeParamType},
		{with("k", value.Null()), errs.CodeParamType},
		{with("extra", value.Int(1)), ""},
	} {
		err := in.CheckParams(c.p, 0)
		if errs.CodeOf(err) != c.code {
			t.Errorf("%v: %v", c.p, err)
		}
		var e *errs.Error
		if c.code != "" && (!errors.As(err, &e) || e.Location == nil || e.Location.Line != 1) {
			t.Errorf("%v: no location: %v", c.p, err)
		}
	}
	if err := in.CheckParams(ok, 2); errs.CodeOf(err) != errs.CodeLimitExceeded {
		t.Fatalf("3 parameters over a limit of 2: %v", err)
	}
}

// Bindings and parameters beyond their limits are LIMIT_EXCEEDED (SPEC
// §15.4).
func TestLimits(t *testing.T) {
	_, err := analyze(t, "MATCH (a)-->(b)-->(c) RETURN a", Config{MaxBindings: 2})
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded || e.Details["limit"] != 2 || e.Details["count"] != 3 {
		t.Fatalf("bindings: %v", err)
	}
	if _, err := analyze(t, "MATCH (a)-->(b) WITH a MATCH (a)-->(c) RETURN a", Config{MaxBindings: 2}); err != nil {
		t.Fatalf("WITH drops b: %v", err)
	}
	_, err = analyze(t, "RETURN [$a, $b, $c, $a]", Config{MaxParams: 2})
	if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded || e.Details["count"] != 3 || e.Location == nil || e.Location.Column != 17 {
		t.Fatalf("parameters: %v", err)
	}
	if _, err := analyze(t, "RETURN [$a, $b, $a]", Config{MaxParams: 2}); err != nil {
		t.Fatal(err)
	}
}

// An error points at the offending text, in lines and code points.
func TestErrorLocation(t *testing.T) {
	_, err := analyze(t, "MATCH (n)\nRETURN 'é', m", Config{})
	var e *errs.Error
	if !errors.As(err, &e) || e.Location.Line != 2 || e.Location.Column != 13 {
		t.Fatalf("%v at %+v", err, e.Location)
	}
}

// An alias hides the variable of its name in ORDER BY: text that reads it
// is resolved by name, not matched to an item written the same way.
func TestOrderByShadowing(t *testing.T) {
	for _, c := range []struct {
		src  string
		item int // the item ORDER BY must read
	}{
		{"MATCH (a), (b) RETURN a AS b, b AS a ORDER BY a", 1},
		{"MATCH (n) RETURN n.x AS n, n AS m ORDER BY n", 0},
		{"MATCH (n) RETURN n, n.x AS x ORDER BY n", 0},
	} {
		q, err := parser.Parse(c.src, parser.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		in, err := Analyze(q, c.src, Config{})
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		p := q.Clauses[1].(*ast.Return).Projection
		x := p.Order[0].X
		got := -1
		if i, ok := in.Projections[p].OrderItems[x]; ok {
			got = i
		} else if sym := in.Refs[x.(*ast.Variable)]; sym != nil {
			for i, it := range in.Projections[p].Items {
				if it.Symbol == sym {
					got = i
				}
			}
		}
		if got != c.item {
			t.Errorf("%q: ORDER BY reads item %d, want %d", c.src, got, c.item)
		}
	}
}
