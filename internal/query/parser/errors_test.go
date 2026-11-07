package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
)

func err(t *testing.T, src string, lim Limits) *errs.Error {
	t.Helper()
	_, err := Parse(src, lim)
	var e *errs.Error
	if !errors.As(err, &e) {
		return nil
	}
	return e
}

// Out-of-range number literals are QUERY_LEX with the lex stage (design
// §3.4, §8.1).
func TestLiteralRangeStage(t *testing.T) {
	for _, src := range []string{"RETURN 9223372036854775808", "RETURN -9223372036854775809", "RETURN 1e999", "RETURN -1e999"} {
		e := err(t, src, DefaultLimits())
		if e == nil || e.Code != errs.CodeQueryLex || e.Stage != errs.StageLex {
			t.Errorf("%s: %v (stage %q), want QUERY_LEX in stage %q", src, e, stageOf(e), errs.StageLex)
		}
	}
}

func stageOf(e *errs.Error) errs.Stage {
	if e == nil {
		return ""
	}
	return e.Stage
}

// The LIMIT_EXCEEDED of an oversized source carries its stage, as every
// query error does (design §3.4).
func TestSourceLimitStage(t *testing.T) {
	e := err(t, "RETURN 12345", Limits{MaxQueryBytes: 8, MaxDepth: 128})
	if e == nil || e.Code != errs.CodeLimitExceeded || e.Stage == "" {
		t.Errorf("%v: stage %q, want a stage", e, stageOf(e))
	}
}

// A backslash before a raw NUL byte escapes it, as \c stands for c.
func TestBackslashNul(t *testing.T) {
	src := "RETURN '\\\x00' AS x"
	q, err := Parse(src, DefaultLimits())
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	if got := ast.FormatExpr(q.Clauses[0].(*ast.Return).Projection.Items[0].X); got != `'\0'` {
		t.Errorf("%q: %s", src, got)
	}
}

// A non-zero float literal that rounds to zero is out of range: QUERY_LEX
// (design §8.1).
func TestFloatUnderflow(t *testing.T) {
	for _, src := range []string{"RETURN 1e-400", "RETURN 2e-324", "RETURN -1e-999"} {
		e := err(t, src, DefaultLimits())
		if e == nil || e.Code != errs.CodeQueryLex {
			t.Errorf("%s: %v, want QUERY_LEX out of range", src, e)
		}
	}
}

// (x) - -1 is arithmetic, not the start of a relationship.
func TestParenVariableMinusNegative(t *testing.T) {
	for _, src := range []string{"RETURN (x) - -1", "RETURN (x) - - 1", "UNWIND [1] AS x RETURN (x) - -x"} {
		if _, err := Parse(src, DefaultLimits()); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
}

// More constructions outside the dialect are QUERY_UNSUPPORTED, naming
// the construction.
func TestUnsupportedVariants(t *testing.T) {
	for src, what := range map[string]string{
		"CREATE VECTOR INDEX v FOR (n:D) ON n.e":          "schema commands",
		"CREATE FULLTEXT INDEX f FOR (n:D) ON EACH [n.t]": "schema commands",
		"CREATE TEXT INDEX t FOR (n:D) ON (n.t)":          "schema commands",
		"RETURN `tacdb`.f(1)":                             "namespaced functions",
		"RETURN tacdb.`f`(1)":                             "namespaced functions",
		"MATCH (n) REMOVE n.a.b RETURN n":                 "nested property",
		"CALL (x) { RETURN 1 } RETURN 1":                  "CALL subqueries",
		"MATCH (n) WHERE ()-->(n) RETURN n":               "pattern predicates",
		"MATCH (a)-[r*1..2]->(b) RETURN a":                "variable",
		"MATCH (a)-[*1..2 {w: 1}]->(b) RETURN a":          "propert",
	} {
		e := err(t, src, DefaultLimits())
		if e == nil || e.Code != errs.CodeQueryUnsupported || !strings.Contains(e.Message, what) {
			t.Errorf("%s: %v, want QUERY_UNSUPPORTED about %q", src, e, what)
		}
	}
}
