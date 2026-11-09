package parser

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/value"
)

func roundTripText(t *testing.T, src string) {
	t.Helper()
	q, err := Parse(src, DefaultLimits())
	if err != nil {
		t.Errorf("%q: %v", src, err)
		return
	}
	text := ast.Format(q)
	back, err := Parse(text, DefaultLimits())
	if err != nil {
		t.Errorf("%q formats as %q, which does not parse: %v", src, text, err)
		return
	}
	if !sameTree(reflect.ValueOf(q), reflect.ValueOf(back)) {
		t.Errorf("%q: round trip changed the tree: %q -> %q", src, text, ast.Format(back))
	}
}

// Names that start a construction outside the dialect in some positions
// (CASE, all(...), any(...), ...) are written with backticks, so that the
// canonical text reads back as names.
func TestRoundTripContextualWords(t *testing.T) {
	for _, src := range []string{
		"RETURN `CASE`",
		"WITH 1 AS `case` RETURN `case`",
		"RETURN `all`(1)",
		"RETURN `any`(1)",
		"RETURN `none`(1)",
		"RETURN `single`(1)",
		"RETURN `reduce`(1)",
	} {
		roundTripText(t, src)
	}
}

// Edge cases of the canonical text: floats, strings with every escape,
// names that need quotes, unary minus, properties of literals, empty
// lists and maps, and length bounds.
func TestRoundTripEdges(t *testing.T) {
	var all strings.Builder
	for r := rune(1); r < 0x800; r++ {
		all.WriteRune(r)
	}
	all.WriteRune(0xFFFD)
	all.WriteRune(0x10FFFF)
	for _, f := range []float64{math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 2.2250738585072014e-308,
		1e21, 1e-7, 123456789012345678901234.0, math.MaxFloat64, -math.MaxFloat64, math.Copysign(0, -1), 0.1, 1.0 / 3} {
		q := &ast.Query{Clauses: []ast.Clause{&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{X: &ast.Literal{Value: value.Float(f)}},
			{X: &ast.Property{X: &ast.Literal{Value: value.Float(f)}, Key: "k"}},
			{X: &ast.Unary{Op: ast.OpNeg, X: &ast.Literal{Value: value.Float(f)}}},
			{X: &ast.Binary{Op: ast.OpSub, L: &ast.Literal{Value: value.Float(f)}, R: &ast.Literal{Value: value.Float(f)}}},
		}}}}}
		roundTripText(t, ast.Format(q))
	}
	for _, s := range []string{all.String(), "\x00\\'\"\n\r\t\b\f`", "\u2028\u0085"} {
		name := strings.ReplaceAll(s, "\x00", "") // names cannot hold NUL; strings can
		q := &ast.Query{Clauses: []ast.Clause{&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{X: &ast.Literal{Value: value.String(s)}, Alias: name},
			{X: &ast.MapLit{Entries: []*ast.MapEntry{{Key: name, Value: &ast.Variable{Name: name}}}}},
		}}}}}
		roundTripText(t, ast.Format(q))
	}
	for _, src := range []string{
		"RETURN -9223372036854775808, - -9223372036854775808, -(1), - - 1, -+1, +-1, 1 - -1, 1--1, -0, -0.0",
		"RETURN {}.a, [].a, ({a: {}}).a.b, [[], {}, [{}]]",
		"MATCH (a)-[*0]->(b), (c)-[*..0]->(d), (e)-[*1..1]->(f), (g)-[*0..0]->(h), (i)-[ * 4294967295 .. ]-(j) RETURN 1",
		"MATCH (`a b`:`MATCH`:`1`)-[`r`:`x|y`|ORDER]->(:```` {`limit`: 1, `x y`: 2}) RETURN `a b`.`ORDER` AS `AS`",
		"RETURN 1.e5",
		"CALL `a b`.`MATCH`.c() YIELD `x y` AS `CASE` RETURN 1",
		"MATCH (n) USING VECTOR (n).e RETURN n",
	} {
		roundTripText(t, src)
	}
}
