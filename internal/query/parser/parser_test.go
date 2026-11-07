package parser

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/value"
)

func parse(t *testing.T, src string) *ast.Query {
	t.Helper()
	q, err := Parse(src, DefaultLimits())
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	roundTrip(t, q)
	return q
}

// roundTrip checks that the canonical text parses back to the same tree.
func roundTrip(t *testing.T, q *ast.Query) {
	t.Helper()
	text := ast.Format(q)
	back, err := Parse(text, DefaultLimits())
	if err != nil {
		t.Fatalf("the canonical text %q does not parse: %v", text, err)
	}
	if !sameTree(reflect.ValueOf(q), reflect.ValueOf(back)) {
		t.Fatalf("round trip changed the tree:\n%s\n%s", text, ast.Format(back))
	}
}

var spanType = reflect.TypeOf(ast.Span{})

// sameTree compares two trees, spans aside; values by their bits.
func sameTree(a, b reflect.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		if a.Kind() == reflect.Interface && a.Elem().Type() != b.Elem().Type() {
			return false
		}
		return sameTree(a.Elem(), b.Elem())
	case reflect.Struct:
		switch a.Type() {
		case spanType:
			return true
		case reflect.TypeOf(value.Value{}):
			return value.Identical(a.Interface().(value.Value), b.Interface().(value.Value))
		}
		for i := 0; i < a.NumField(); i++ {
			if !sameTree(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !sameTree(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// Every clause of §15.1 parses into the canonical form given.
func TestClauses(t *testing.T) {
	for src, want := range map[string]string{
		"match (n) return n": "MATCH (n) RETURN n",
		"MATCH (a:Person:Admin {name: 'x', age: 3})-[r:KNOWS]->(b) RETURN a, r, b;":                                    "MATCH (a:Person:Admin {name: 'x', age: 3})-[r:KNOWS]->(b) RETURN a, r, b",
		"MATCH (a)<-[:T|U]-(b)--(c)-->(d)<--(e) RETURN 1":                                                              "MATCH (a)<-[:T|U]-(b)-[]-(c)-[]->(d)<-[]-(e) RETURN 1",
		"MATCH (a)<-[]->(b) RETURN a":                                                                                  "MATCH (a)-[]-(b) RETURN a",
		"MATCH (a)-[:T|:U*2..5]->(b) RETURN b":                                                                         "MATCH (a)-[:T|U*2..5]->(b) RETURN b",
		"MATCH (a)-[*]-(b), (c)-[*3]->(d), (e)-[*..4]->(f), (g)-[*0..]->(h) RETURN 1":                                  "MATCH (a)-[*..]-(b), (c)-[*3]->(d), (e)-[*..4]->(f), (g)-[*0..]->(h) RETURN 1",
		"OPTIONAL MATCH (n) WHERE n.x > 1 RETURN n":                                                                    "OPTIONAL MATCH (n) WHERE (n.x > 1) RETURN n",
		"MATCH (d:Doc) USING VECTOR d.e OPTIONS {mode: 'approximate', ef_search: $ef} WHERE d.e <=> $v < 0.8 RETURN d": "MATCH (d:Doc) USING VECTOR d.e OPTIONS {mode: 'approximate', ef_search: $ef} WHERE ((d.e <=> $v) < 0.8) RETURN d",
		"UNWIND [1, 2] AS x RETURN x":                                                                                  "UNWIND [1, 2] AS x RETURN x",
		"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path, hops AS h WHERE h > 1 RETURN path":                       "CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path, hops AS h WHERE (h > 1) RETURN path",
		"CALL tacdb.graph.shortest_path(1, 2)":                                                                         "CALL tacdb.graph.shortest_path(1, 2)",
		"MATCH (n) WITH DISTINCT n.x AS x ORDER BY x DESC, n.y SKIP 1 LIMIT 2 WHERE x > 0 RETURN x":                    "MATCH (n) WITH DISTINCT n.x AS x ORDER BY x DESC, n.y SKIP 1 LIMIT 2 WHERE (x > 0) RETURN x",
		"RETURN count(*), count(DISTINCT n), coalesce(a, 1), f()":                                                      "RETURN count(*), count(DISTINCT n), coalesce(a, 1), f()",
		"CREATE (a:P {x: 1})-[:R {w: 2.5}]->(b), (c)":                                                                  "CREATE (a:P {x: 1})-[:R {w: 2.5}]->(b), (c)",
		"MERGE (a:P {k: 1}) ON CREATE SET a.c = 1 ON MATCH SET a.m = 2, a += {z: 3} ON CREATE SET a:New":               "MERGE (a:P {k: 1}) ON CREATE SET a.c = 1 ON MATCH SET a.m = 2, a += {z: 3} ON CREATE SET a:New",
		"MATCH (n) SET n.x = 1, n = {a: 1}, n += $m, n:A:B":                                                            "MATCH (n) SET n.x = 1, n = {a: 1}, n += $m, n:A:B",
		"MATCH (n) REMOVE n.x, n:A:B":                                                                                  "MATCH (n) REMOVE n.x, n:A:B",
		"MATCH (n) DETACH DELETE n":                                                                                    "MATCH (n) DETACH DELETE n",
		"MATCH (n)-[r]->() DELETE r, n":                                                                                "MATCH (n)-[r]->() DELETE r, n",
		"EXPLAIN MATCH (n) RETURN n":                                                                                   "EXPLAIN MATCH (n) RETURN n",
		"PROFILE MATCH (n) RETURN n":                                                                                   "PROFILE MATCH (n) RETURN n",
		"RETURN 1":                                                                                                     "RETURN 1",
		"MATCH (n) WHERE n.t @@ $q RETURN n":                                                                           "MATCH (n) WHERE (n.t @@ $q) RETURN n",
		"MATCH (`my var`:`a label`) RETURN `my var`.`the key`":                                                         "MATCH (`my var`:`a label`) RETURN `my var`.`the key`",
		"MATCH (n:Match) RETURN n.limit, {order: 1}":                                                                   "MATCH (n:`Match`) RETURN n.`limit`, {`order`: 1}",
		"CREATE (a) WITH a MATCH (b) RETURN b":                                                                         "CREATE (a) WITH a MATCH (b) RETURN b",
		"MATCH (n) CREATE (m) SET m.x = 1":                                                                             "MATCH (n) CREATE (m) SET m.x = 1",
		"// a comment\nRETURN 1 // another":                                                                            "RETURN 1",
		"MATCH (n)\r\n\tRETURN n":                                                                                      "MATCH (n) RETURN n",
		"MATCH (`a``b`:`1a`) RETURN `a``b`":                                                                            "MATCH (`a``b`:`1a`) RETURN `a``b`",
	} {
		q := parse(t, src)
		if got := ast.Format(q); got != want {
			t.Errorf("%s\n got %s\nwant %s", src, got, want)
		}
	}
}

// Precedence follows the reference (SPEC §31.3): one case per pair of
// adjacent levels, and the documented quirks.
func TestPrecedence(t *testing.T) {
	for src, want := range map[string]string{
		"a OR b XOR c":      "(a OR (b XOR c))",
		"a XOR b AND c":     "(a XOR (b AND c))",
		"NOT a AND b":       "((NOT a) AND b)",
		"NOT a = b":         "(NOT (a = b))",
		"a = b < c":         "(a = (b < c))",
		"a < b + c":         "(a < (b + c))",
		"a + b * c":         "(a + (b * c))",
		"a * b ^ c":         "(a * (b ^ c))",
		"a ^ b ^ c":         "(a ^ (b ^ c))",
		"-a ^ b":            "((- a) ^ b)",
		"-2 ^ 2":            "(-2 ^ 2)",
		"-a.b":              "(- a.b)",
		"a - b - c":         "((a - b) - c)",
		"a = b = c":         "((a = b) = c)",
		"a + b IS NULL":     "((a + b) IS NULL)",
		"a = b IS NOT NULL": "((a = b) IS NOT NULL)",
		"x IN l = true":     "((x IN l) = TRUE)",
		"s STARTS WITH 'a' AND s ENDS WITH 'b' OR s CONTAINS 'c'": "(((s STARTS WITH 'a') AND (s ENDS WITH 'b')) OR (s CONTAINS 'c'))",
		"n.embedding <=> $v < 0.3":                                "((n.embedding <=> $v) < 0.3)",
		"a @@ b AND c":                                            "((a @@ b) AND c)",
		"a % b / c":                                               "((a % b) / c)",
		"+a":                                                      "a",
		"a - -1":                                                  "(a - -1)",
		"(a OR b) AND c":                                          "((a OR b) AND c)",
		"[1, 2.5, 'x', TRUE, NULL, -0.0]":                         "[1, 2.5, 'x', TRUE, NULL, -0.0]",
	} {
		q := parse(t, "RETURN "+src)
		got := ast.FormatExpr(q.Clauses[0].(*ast.Return).Projection.Items[0].X)
		if got != want {
			t.Errorf("%s\n got %s\nwant %s", src, got, want)
		}
	}
}

func TestLiterals(t *testing.T) {
	lit := func(src string) value.Value {
		q := parse(t, "RETURN "+src)
		return q.Clauses[0].(*ast.Return).Projection.Items[0].X.(*ast.Literal).Value
	}
	for src, want := range map[string]value.Value{
		"007":                         value.Int(7),
		"-9223372036854775808":        value.Int(math.MinInt64),
		"9223372036854775807":         value.Int(math.MaxInt64),
		"1e10":                        value.Float(1e10),
		"1.5E-3":                      value.Float(1.5e-3),
		"-0.0":                        value.Float(math.Copysign(0, -1)),
		`'a\'b\"c\\d\n\t\r\b\f\0é\x'`: value.String("a'b\"c\\d\n\t\r\b\f\x00éx"),
		`"it's"`:                      value.String("it's"),
		"'é'":                         value.String("é"),
		"null":                        value.Null(),
		"True":                        value.Bool(true),
	} {
		if got := lit(src); !value.Identical(got, want) {
			t.Errorf("%s: %v, want %v", src, got, want)
		}
	}
	q := parse(t, "RETURN $p, $0, $_x")
	for i, n := range []string{"p", "0", "_x"} {
		if got := q.Clauses[0].(*ast.Return).Projection.Items[i].X.(*ast.Parameter).Name; got != n {
			t.Errorf("parameter %d: %s", i, got)
		}
	}
}

type errCase struct {
	src          string
	code         errs.Code
	line, column int
	message      string
}

func TestErrors(t *testing.T) {
	for _, c := range []errCase{
		{"", errs.CodeQueryParse, 1, 1, "empty"},
		{"   // nothing", errs.CodeQueryParse, 1, 1, "empty"},
		{"MATCH (n) RETURN n; MATCH (m) RETURN m", errs.CodeQueryParse, 1, 21, "one statement"},
		{"MATCH (n RETURN n", errs.CodeQueryParse, 1, 10, "')'"},
		{"MATCH (n)\nRETURN n n", errs.CodeQueryParse, 2, 10, "expected a clause"},
		{"MATCH (n) RETURN n WHERE n.x = 1", errs.CodeQueryParse, 1, 20, "WHERE must follow"},
		{"MATCH (n) LIMIT 1 RETURN n", errs.CodeQueryParse, 1, 11, "LIMIT must follow"},
		{"RETURN 1 MATCH (n) RETURN n", errs.CodeQueryParse, 1, 10, "RETURN must be the last"},
		{"MATCH (n)", errs.CodeQueryParse, 1, 1, "must end with RETURN"},
		{"CREATE (n) MATCH (m) RETURN m", errs.CodeQueryParse, 1, 12, "needs WITH"},
		{"MATCH (n) CALL p() RETURN n", errs.CodeQueryParse, 1, 11, "needs YIELD"},
		{"RETURN 'abc", errs.CodeQueryLex, 1, 8, "unterminated string"},
		{"RETURN 'a\nb'", errs.CodeQueryLex, 1, 8, "line break"},
		{"RETURN '\\u12G4'", errs.CodeQueryLex, 1, 9, "four hexadecimal"},
		{"RETURN '\\uD800'", errs.CodeQueryLex, 1, 9, "not a valid character"},
		{"RETURN 9223372036854775808", errs.CodeQueryLex, 1, 8, "out of range"},
		{"RETURN 1e999", errs.CodeQueryLex, 1, 8, "out of range"},
		{"RETURN 1e", errs.CodeQueryLex, 1, 8, "exponent"},
		{"RETURN 12abc", errs.CodeQueryLex, 1, 8, "invalid number"},
		{"RETURN $", errs.CodeQueryLex, 1, 8, "parameter needs a name"},
		{"RETURN `a", errs.CodeQueryLex, 1, 8, "unterminated quoted"},
		{"RETURN ``", errs.CodeQueryLex, 1, 8, "empty quoted"},
		{"RETURN `a\x00b`", errs.CodeQueryLex, 1, 10, "NUL"},
		{"RETURN é", errs.CodeQueryLex, 1, 8, "unexpected character"},
		{"RETURN a ~ b", errs.CodeQueryLex, 1, 10, "unexpected character"},
		{"RETURN \xff", errs.CodeQueryLex, 1, 8, "UTF-8"},
		{"MATCH (n)-[:A:B]->(m) RETURN n", errs.CodeQueryParse, 1, 14, "one type"},
		{"MATCH (n)-[*99999999999]->(m) RETURN n", errs.CodeQueryParse, 1, 13, "hop count"},
		{"RETURN [1,]", errs.CodeQueryParse, 1, 11, "expected an expression"},
		{"RETURN match", errs.CodeQueryParse, 1, 8, "expected an expression"},
		{"RETURN f(*)", errs.CodeQueryParse, 1, 10, "only count"},
		{"RETURN a STARTS b", errs.CodeQueryParse, 1, 17, "WITH after STARTS"},
		{"RETURN a IS 1", errs.CodeQueryParse, 1, 13, "NULL after IS"},
		{"MATCH (n) USING VECTOR n OPTIONS {} RETURN n", errs.CodeQueryParse, 1, 26, "needs a property"},
		{"MATCH (n) SET n.a.b = 1 RETURN n", errs.CodeQueryUnsupported, 1, 18, "nested property"},
		{"MATCH (n) SET n RETURN n", errs.CodeQueryParse, 1, 17, "after the variable in SET"},
	} {
		_, err := Parse(c.src, DefaultLimits())
		var e *errs.Error
		if !errors.As(err, &e) || e.Code != c.code || e.Location == nil || e.Location.Line != c.line ||
			e.Location.Column != c.column || !strings.Contains(e.Message, c.message) {
			t.Errorf("%q: %v (location %+v); want %s at %d:%d with %q", c.src, err, e.Location, c.code, c.line, c.column, c.message)
			continue
		}
		if e.Stage == "" {
			t.Errorf("%q: no stage", c.src)
		}
	}
}

// Constructions outside the dialect are refused as such, never accepted
// in part (SPEC §15.1).
func TestUnsupported(t *testing.T) {
	for src, what := range map[string]string{
		"MATCH (n) RETURN n UNION MATCH (m) RETURN m":  "UNION",
		"LOAD CSV FROM 'x' AS l RETURN l":              "LOAD CSV",
		"FOREACH (x IN [1] | CREATE (n))":              "FOREACH",
		"CREATE INDEX FOR (n:P) ON (n.x)":              "schema commands",
		"RETURN CASE WHEN 1 THEN 2 END":                "CASE",
		"MATCH (n) RETURN *":                           "RETURN *",
		"MATCH (n) WITH * RETURN n":                    "WITH *",
		"RETURN [x IN [1] | x]":                        "list comprehensions",
		"RETURN [x IN [1] WHERE x > 0]":                "list comprehensions",
		"WITH [1] AS l RETURN l[0]":                    "list indexing",
		"MATCH (n) WHERE n:P RETURN n":                 "label predicates",
		"MATCH (n) WHERE (n)-->() RETURN n":            "pattern predicates",
		"MATCH (n) WHERE EXISTS { (n)-->() } RETURN n": "EXISTS subqueries",
		"MATCH (n) RETURN n {.x}":                      "map projection",
		"MATCH p = (n)-->(m) RETURN p":                 "path variables",
		"MATCH shortestPath((a)-[*]-(b)) RETURN a":     "shortestPath",
		"RETURN 'a' =~ 'b'":                            "=~",
		"MATCH (n $m) RETURN n":                        "parameter as the properties",
		"CALL { MATCH (n) RETURN n } RETURN 1":         "CALL subqueries",
		"CALL p() YIELD * RETURN 1":                    "YIELD *",
		"RETURN reduce(s = 0, x IN [1] | s + x)":       "REDUCE",
		"RETURN all(x IN [1] WHERE x > 0)":             "ALL",
		"MATCH (n) USING INDEX n:P(x) RETURN n":        "USING",
		"RETURN tacdb.f(1)":                            "namespaced functions",
		"DROP INDEX x":                                 "DROP commands",
	} {
		_, err := Parse(src, DefaultLimits())
		var e *errs.Error
		if !errors.As(err, &e) || e.Code != errs.CodeQueryUnsupported || !strings.Contains(e.Message, what) || e.Location == nil {
			t.Errorf("%q: %v; want QUERY_UNSUPPORTED about %q", src, err, what)
		}
	}
}

func TestLimits(t *testing.T) {
	lim := Limits{MaxQueryBytes: 32, MaxDepth: 10}
	exact := "RETURN '" + strings.Repeat("x", 23) + "'" // 32 bytes
	if _, err := Parse(exact, lim); err != nil {
		t.Fatalf("a query of exactly MaxQueryBytes: %v", err)
	}
	_, err := Parse(exact+" ", lim)
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded || e.Details["limit"] != 32 || e.Details["bytes"] != 33 {
		t.Fatalf("a query one byte above MaxQueryBytes: %v", err)
	}
	for _, src := range []string{
		"RETURN " + strings.Repeat("(", 10) + "1" + strings.Repeat(")", 10), // depth 11
		"RETURN " + strings.Repeat("(", 11) + "1" + strings.Repeat(")", 11),
		"RETURN " + strings.Repeat("[", 11) + strings.Repeat("]", 11),
		"RETURN " + strings.Repeat("NOT ", 11) + "1",
		"RETURN " + strings.Repeat("- ", 11) + "a",
		"RETURN 1" + strings.Repeat("^1", 11),
	} {
		_, err := Parse(src, Limits{MaxQueryBytes: 1 << 20, MaxDepth: 10})
		if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded || e.Details["limit"] != 10 {
			t.Fatalf("%s: %v", src, err)
		}
	}
	if _, err := Parse("RETURN "+strings.Repeat("(", 9)+"1"+strings.Repeat(")", 9), Limits{MaxQueryBytes: 1 << 20, MaxDepth: 10}); err != nil {
		t.Fatalf("within the depth: %v", err)
	}
}

// Spans point into the source.
func TestSpans(t *testing.T) {
	src := "MATCH (a)-[r:T]->(b) WHERE a.x = 1 RETURN a.x + 1 AS y"
	q := parse(t, src)
	m := q.Clauses[0].(*ast.Match)
	r := q.Clauses[1].(*ast.Return)
	for got, want := range map[ast.Span]string{
		m.Span:                        "MATCH (a)-[r:T]->(b) WHERE a.x = 1",
		m.Patterns[0].Rels[0].Span:    "-[r:T]->",
		m.Patterns[0].Nodes[1].Span:   "(b)",
		m.Where.Pos():                 "a.x = 1",
		r.Projection.Items[0].Span:    "a.x + 1 AS y",
		r.Projection.Items[0].X.Pos(): "a.x + 1",
		q.Span:                        src,
	} {
		if src[got.Start:got.End] != want {
			t.Errorf("span %v is %q, want %q", got, src[got.Start:got.End], want)
		}
	}
}

func FuzzParser(f *testing.F) {
	for _, s := range []string{"MATCH (a:P {x: 1})-[r:T*1..3]->(b) WHERE a.x > 1 AND b.y IS NULL RETURN a, count(*) ORDER BY a.x DESC LIMIT 3",
		"UNWIND [1, 2.5, 'x'] AS x WITH x WHERE x <> 1 RETURN x", "MERGE (a {k: -1}) ON CREATE SET a += {x: $p}",
		"CALL tacdb.graph.shortest_path(1, 2) YIELD path RETURN path", "RETURN `a``b`, '\\u00e9', -2^2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		q, err := Parse(src, Limits{MaxQueryBytes: 1 << 16, MaxDepth: 32})
		if err != nil {
			var e *errs.Error
			if !errors.As(err, &e) || e.Code == "" {
				t.Fatalf("%q: an error without a code: %v", src, err)
			}
			if e.Code != errs.CodeLimitExceeded && (e.Location == nil || e.Location.Start > len(src)) {
				t.Fatalf("%q: a bad location %+v", src, e.Location)
			}
			return
		}
		roundTrip(t, q)
	})
}
