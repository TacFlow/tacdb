package query_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/query"
)

// Hostile names quote into exactly one identifier: the parser reads the
// name back unchanged, and nothing of it leaks into the syntax (SEC-06).
func TestQuoteIdentifier(t *testing.T) {
	for _, name := range []string{"plain", "with space", "a`b", "``", "x`) DETACH DELETE (n", "MATCH", "é ü", "`) RETURN 1 //", "a\nb", "'quote'", strings.Repeat("x", 255)} {
		q, err := query.QuoteIdentifier(query.Label, name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		tree, err := parser.Parse("MATCH (n:"+q+") RETURN n", parser.DefaultLimits())
		if err != nil {
			t.Fatalf("%q quoted as %s does not parse: %v", name, q, err)
		}
		m := tree.Clauses[0].(*ast.Match)
		if got := m.Patterns[0].Nodes[0].Labels; len(got) != 1 || got[0] != name || len(tree.Clauses) != 2 {
			t.Fatalf("%q: labels %q, %d clauses", name, got, len(tree.Clauses))
		}
		for _, kind := range []query.IdentifierKind{query.RelationshipType, query.PropertyKey, query.Variable} {
			if q2, err := query.QuoteIdentifier(kind, name); err != nil || q2 != q {
				t.Fatalf("%q as kind %d: %s, %v", name, kind, q2, err)
			}
		}
	}
	for name, code := range map[string]errs.Code{"": errs.CodeInvalidArgument, "a\x00b": errs.CodeInvalidArgument,
		"\xff": errs.CodeInvalidArgument, strings.Repeat("x", 256): errs.CodeLimitExceeded} {
		_, err := query.QuoteIdentifier(query.PropertyKey, name)
		var e *errs.Error
		if !errors.As(err, &e) || e.Code != code {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if _, err := query.QuoteIdentifier(9, "x"); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("kind 9: %v", err)
	}
}
