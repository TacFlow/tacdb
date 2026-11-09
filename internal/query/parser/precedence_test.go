package parser

import (
	"fmt"
	"testing"

	"github.com/TacFlow/tacdb/internal/query/ast"
)

// The reference's precedence table (SPEC §31.3), lowest first. Each binary
// operator is listed with its level; ^ is the only right-associative one.
var levels = []struct {
	op    string
	level int
}{
	{"OR", 1}, {"XOR", 2}, {"AND", 3},
	{"=", 5}, {"<>", 5}, {"IN", 5}, {"CONTAINS", 5}, {"STARTS WITH", 5}, {"ENDS WITH", 5},
	{"<", 6}, {"<=", 6}, {">", 6}, {">=", 6}, {"<=>", 6}, {"@@", 6},
	{"+", 7}, {"-", 7},
	{"*", 8}, {"/", 8}, {"%", 8},
	{"^", 9},
}

func expr(t *testing.T, src string) (string, error) {
	t.Helper()
	q, err := Parse("RETURN "+src, DefaultLimits())
	if err != nil {
		return "", err
	}
	return ast.FormatExpr(q.Clauses[0].(*ast.Return).Projection.Items[0].X), nil
}

// Every ordered pair of binary operators, as `a op1 b op2 c`.
func TestPrecedenceBinaryPairs(t *testing.T) {
	for _, x := range levels {
		for _, y := range levels {
			src := fmt.Sprintf("a %s b %s c", x.op, y.op)
			want := fmt.Sprintf("((a %s b) %s c)", x.op, y.op)
			if x.level < y.level || x.level == y.level && x.op == "^" {
				want = fmt.Sprintf("(a %s (b %s c))", x.op, y.op)
			}
			got, err := expr(t, src)
			if err != nil || got != want {
				t.Errorf("%s: got %s %v, want %s", src, got, err, want)
			}
		}
	}
}

// Prefix NOT (level 4) and prefix minus (level 10) against every binary
// level, on both sides, and postfix IS NULL (level 5) on both sides.
func TestPrecedencePrefixPostfix(t *testing.T) {
	for _, y := range levels {
		cases := map[string]string{}
		// NOT x op y
		if y.level >= 5 {
			cases["NOT a "+y.op+" b"] = fmt.Sprintf("(NOT (a %s b))", y.op)
		} else {
			cases["NOT a "+y.op+" b"] = fmt.Sprintf("((NOT a) %s b)", y.op)
		}
		// -x op y and x op -y (a variable, so no literal folding)
		cases["-a "+y.op+" b"] = fmt.Sprintf("((- a) %s b)", y.op)
		cases["a "+y.op+" -b"] = fmt.Sprintf("(a %s (- b))", y.op)
		// x op y IS NULL
		if y.level >= 5 {
			cases["a "+y.op+" b IS NULL"] = fmt.Sprintf("((a %s b) IS NULL)", y.op)
		} else {
			cases["a "+y.op+" b IS NULL"] = fmt.Sprintf("(a %s (b IS NULL))", y.op)
		}
		// x IS NULL op y: in the reference's Pratt loop, IS NULL is an
		// infix step at the equality level; whatever operator follows it is
		// applied to the IS NULL result.
		cases["a IS NULL "+y.op+" b"] = fmt.Sprintf("((a IS NULL) %s b)", y.op)
		// x op NOT y: NOT is a prefix operator, accepted wherever an operand
		// starts; its operand is read at level 4.
		cases["a "+y.op+" NOT b"] = fmt.Sprintf("(a %s (NOT b))", y.op)
		for src, want := range cases {
			got, err := expr(t, src)
			if err != nil || got != want {
				t.Errorf("%s: got %s %v, want %s", src, got, err, want)
			}
		}
	}
}

// Property access (level 12) binds tighter than unary minus (level 10), on
// a literal as on anything else: -1.x is -(1.x), as -a.x is -(a.x).
func TestPrecedenceMinusLiteralProperty(t *testing.T) {
	for src, want := range map[string]string{
		"-a.x":   "(- a.x)",
		"-1.x":   "(- (1).x)",
		"-1.5.x": "(- (1.5).x)",
	} {
		got, err := expr(t, src)
		if err != nil || got != want {
			t.Errorf("%s: got %s %v, want %s", src, got, err, want)
		}
	}
}

// The documented quirks.
func TestPrecedenceQuirks(t *testing.T) {
	for src, want := range map[string]string{
		"-2^2":                  "(-2 ^ 2)",
		"2^-2":                  "(2 ^ -2)",
		"2^3^2":                 "(2 ^ (3 ^ 2))",
		"-a^b^c":                "((- a) ^ (b ^ c))",
		"a = b IS NULL":         "((a = b) IS NULL)",
		"a < b IS NULL":         "((a < b) IS NULL)",
		"x.e <=> $q < 0.5":      "((x.e <=> $q) < 0.5)",
		"a @@ b = c":            "((a @@ b) = c)",
		"a = b @@ c":            "(a = (b @@ c))",
		"a STARTS WITH b < c":   "(a STARTS WITH (b < c))",
		"a IN b + c":            "(a IN (b + c))",
		"NOT NOT a = b":         "(NOT (NOT (a = b)))",
		"1 < 2 < 3":             "((1 < 2) < 3)",
		"a IS NOT NULL IS NULL": "((a IS NOT NULL) IS NULL)",
		"(n).name":              "n.name",
		"f(x).p":                "(f(x)).p",
		"[1].p":                 "([1]).p",
	} {
		got, err := expr(t, src)
		if err != nil || got != want {
			t.Errorf("%s: got %s %v, want %s", src, got, err, want)
		}
	}
}
