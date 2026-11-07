package query_test

import (
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/query"
)

// Hostile names in every position an identifier can take.
func TestQuoteIdentifierPositions(t *testing.T) {
	names := []string{"`", "``", "```", "a`", "`a", " ", "\t\r\n", "\ufeff", "\u2028", "//", "/*", "'", "\"", "\\",
		"$p", "CASE", "all", "count", "*", "x`)--(y", strings.Repeat("`", 255), strings.Repeat("é", 127) + "`"}
	for b := 1; b < 0x80; b++ {
		names = append(names, string(rune(b)), "a"+string(rune(b))+"`")
	}
	for _, name := range names {
		q, err := query.QuoteIdentifier(query.Variable, name)
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		src := "MATCH (" + q + ":" + q + " {" + q + ": 1})-[" + q + ":" + q + "|" + q + "]->() " +
			"SET " + q + "." + q + " = 1, " + q + ":" + q + " WITH " + q + " AS " + q + " " +
			"UNWIND [1] AS " + q + " RETURN " + q + "." + q + " AS " + q
		tree, err := parser.Parse(src, parser.DefaultLimits())
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		m := tree.Clauses[0].(*ast.Match)
		n := m.Patterns[0].Nodes[0]
		r := m.Patterns[0].Rels[0]
		ret := tree.Clauses[len(tree.Clauses)-1].(*ast.Return).Projection.Items[0]
		if n.Var != name || n.Labels[0] != name || n.Properties.Entries[0].Key != name || r.Var != name ||
			len(r.Types) != 2 || r.Types[1] != name || ret.Alias != name || ret.X.(*ast.Property).Key != name || len(tree.Clauses) != 5 {
			t.Errorf("%q: read back differently: %s", name, ast.Format(tree))
		}
	}
}
