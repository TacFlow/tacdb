package plan_test

import (
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/sema"
)

func build(t *testing.T, src string, cfg plan.Config) (*plan.Plan, error) {
	t.Helper()
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	info, err := sema.Analyze(q, src, sema.Config{})
	if err != nil {
		return nil, err
	}
	return plan.Build(q, info, src, cfg)
}

func render(t *testing.T, src string) string {
	t.Helper()
	p, err := build(t, src, plan.Config{})
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return string(plan.Render(p))
}

// The syntactic plan (design §4.3) and its rendering: entry by a bound
// variable, then by an ID seek, then by the node with the most labels and
// properties, ties by position; expansions in the order of the text from
// the entry, ExpandInto when both ends are bound; OPTIONAL MATCH as an
// apply; an aggregate on a clustered scan is ordered; ORDER BY with LIMIT
// is a TopK.
func TestGolden(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{
			"MATCH (a)-[:R]->(b), (c:X)<-[r]-(d:X:Y) RETURN a",
			`{"version":1,"columns":["a"],"plan":{"op":"Project","details":{"items":"a AS a"},"inputs":[{"op":"ExpandAll","details":{"direction":"out","distinctFrom":"r","from":"a","relationship":"#1","to":"b","types":"R"},"inputs":[{"op":"NodeScan","details":{"variable":"a"},"inputs":[{"op":"ExpandAll","details":{"direction":"out","from":"d","relationship":"r","to":"c","toLabels":"X"},"inputs":[{"op":"LabelScan","details":{"label":"X","labels":"Y","variable":"d"},"inputs":[{"op":"SingleRow"}]}]}]}]}]}}`,
		},
		{
			"MATCH (a:A) MATCH (b)-[s]-(a)-[:T]->(b) RETURN s",
			`{"version":1,"columns":["s"],"plan":{"op":"Project","details":{"items":"s AS s"},"inputs":[{"op":"ExpandInto","details":{"direction":"both","distinctFrom":"#3","from":"a","relationship":"s","to":"b"},"inputs":[{"op":"ExpandAll","details":{"direction":"out","from":"a","relationship":"#3","to":"b","types":"T"},"inputs":[{"op":"LabelScan","details":{"label":"A","variable":"a"},"inputs":[{"op":"SingleRow"}]}]}]}]}}`,
		},
		{
			"MATCH (n) OPTIONAL MATCH (n)-[:R*1..3]->(m:M) WHERE m.k > 1 RETURN n, count(m) AS c ORDER BY c DESC LIMIT 5",
			`{"version":1,"columns":["n","c"],"plan":{"op":"TopK","details":{"key0":"c DESC","limit":"5"},"inputs":[{"op":"Project","details":{"items":"n AS n, count(m) AS c","sortKey0":"c"},"inputs":[{"op":"OrderedAggregate","details":{"aggregates":"count(m)","keys":"n"},"inputs":[{"op":"OptionalApply","details":{"nullable":"#1, m"},"inputs":[{"op":"NodeScan","details":{"variable":"n"},"inputs":[{"op":"SingleRow"}]},{"op":"Filter","details":{"predicate":"(m.k > 1)"},"inputs":[{"op":"VarExpand","details":{"direction":"out","edges":"#1","from":"n","max":"3","min":"1","to":"m","toLabels":"M","types":"R"},"inputs":[{"op":"Argument"}]}]}]}]}]}]}}`,
		},
		{
			"UNWIND $xs AS x WITH DISTINCT x WHERE x > 0 RETURN x ORDER BY x SKIP 1",
			`{"version":1,"columns":["x"],"plan":{"op":"Skip","details":{"count":"1"},"inputs":[{"op":"Sort","details":{"key0":"x"},"inputs":[{"op":"Project","details":{"items":"x AS x","sortKey0":"x"},"inputs":[{"op":"Filter","details":{"predicate":"(x > 0)"},"inputs":[{"op":"Distinct","details":{"columns":"1"},"inputs":[{"op":"Project","details":{"items":"x AS x"},"inputs":[{"op":"Unwind","details":{"expression":"$xs","variable":"x"},"inputs":[{"op":"SingleRow"}]}]}]}]}]}]}]}}`,
		},
		{
			"MATCH (n {k: 1})-[*]-(m) RETURN m.k AS k, collect(n) AS ns",
			`{"version":1,"columns":["k","ns"],"plan":{"op":"Project","details":{"items":"m.k AS k, collect(n) AS ns"},"inputs":[{"op":"Aggregate","details":{"aggregates":"collect(n)","keys":"m.k"},"inputs":[{"op":"VarExpand","details":{"direction":"both","edges":"#1","from":"n","max":"MaxHops","min":"1","to":"m"},"inputs":[{"op":"NodeScan","details":{"properties":"{k: 1}","variable":"n"},"inputs":[{"op":"SingleRow"}]}]}]}]}}`,
		},
	} {
		if got := render(t, c.src); got != c.want {
			t.Fatalf("%s:\n got  %s\n want %s", c.src, got, c.want)
		}
	}
}

// entry describes the operator that binds the first node of a plan.
func entry(t *testing.T, src string) string {
	t.Helper()
	p, err := build(t, src, plan.Config{})
	if err != nil {
		t.Fatal(err)
	}
	op := p.Root
	for len(op.Inputs()) > 0 && op.Inputs()[0].Name() != "SingleRow" {
		op = op.Inputs()[0]
	}
	switch o := op.(type) {
	case *plan.NodeScan:
		return "NodeScan"
	case *plan.LabelScan:
		return "LabelScan " + o.Label
	case *plan.IDSeek:
		return "IDSeek " + o.ID.Text
	}
	return op.Name()
}

func TestEntry(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"MATCH (a:X), (b:Y) RETURN a, b", "LabelScan X"},
		{"MATCH (a:X), (b:Y:Z) RETURN a, b", "LabelScan Y"},
		{"MATCH (a:X), (b {k: 1, j: 2}) RETURN a, b", "NodeScan"},
		{"MATCH (a:X:Y), (b) WHERE id(b) = 3 RETURN a, b", "IDSeek 3"},
		{"MATCH (a:X), (b) WHERE 3 = id(b) AND a.k = 1 RETURN a, b", "IDSeek 3"},
		{"MATCH (a:X), (b) WHERE id(b) = id(a) RETURN a, b", "LabelScan X"},
		{"MATCH (a:X), (b) WHERE id(b) = 3 OR true RETURN a, b", "LabelScan X"},
		{"MATCH (a), (b:X)-->(c:Y {k: 1}) RETURN a", "LabelScan Y"},
	} {
		if got := entry(t, c.src); got != c.want {
			t.Fatalf("%s: %s, want %s", c.src, got, c.want)
		}
	}
	// A pattern touching a bound variable starts there.
	if got := render(t, "MATCH (a:X) MATCH (b:Y:Z)-->(a) RETURN b"); !strings.Contains(got, `"op":"ExpandAll","details":{"direction":"in","from":"a"`) {
		t.Fatalf("bound entry: %s", got)
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		src   string
		code  errs.Code
		stage errs.Stage
	}{
		{"EXPLAIN MATCH (n) RETURN n", errs.CodeQueryUnsupported, errs.StagePlan},
		{"PROFILE MATCH (n) RETURN n", errs.CodeQueryUnsupported, errs.StagePlan},
		{"CREATE (n)", errs.CodeQueryWriteRequiresExec, errs.StageSemantic},
		{"MATCH (n) USING VECTOR n.v RETURN n", errs.CodeQueryUnsupported, errs.StagePlan},
		{"MATCH (n) RETURN n.t @@ 'x'", errs.CodeQueryUnsupported, errs.StagePlan},
		{"MATCH (n) WHERE [n.v <=> $q] = [] RETURN n", errs.CodeQueryUnsupported, errs.StagePlan},
		{"MATCH (n) RETURN {d: coalesce(-(n.v <=> $q))}", errs.CodeQueryUnsupported, errs.StagePlan},
		{"MATCH (n) WHERE n.v <=> $q IS NULL RETURN n", errs.CodeQueryUnsupported, errs.StagePlan},
		{"MATCH (a)-[*3..2]->(b) RETURN b", errs.CodeQuerySemantic, errs.StageSemantic},
		{"MATCH (a)-[*..65]->(b) RETURN b", errs.CodeResourceLimit, errs.StagePlan},
		{"MATCH (a)-[*65..]->(b) RETURN b", errs.CodeResourceLimit, errs.StagePlan},
	} {
		_, err := build(t, c.src, plan.Config{})
		e, ok := err.(*errs.Error)
		if !ok || e.Code != c.code || e.Stage != c.stage || e.Location == nil {
			t.Fatalf("%s: %v, want %s at %s", c.src, err, c.code, c.stage)
		}
	}
	for _, src := range []string{"MATCH (a)-[*..64]->(b) RETURN b", "MATCH (a)-[*64..]->(b) RETURN b", "MATCH (a)-[*..0]->(b) RETURN b"} {
		if _, err := build(t, src, plan.Config{}); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
	}
	if _, err := build(t, "MATCH (a)-[*..5]->(b) RETURN b", plan.Config{MaxHops: 4}); errs.CodeOf(err) != errs.CodeResourceLimit {
		t.Fatalf("MaxHops 4: %v", err)
	}
}

// QRY-09: the flags of the tests plan the plain operators.
func TestPlainOperators(t *testing.T) {
	src := "MATCH (n) RETURN n, count(*) AS c ORDER BY c LIMIT 2"
	p, err := build(t, src, plan.Config{NoTopK: true, NoOrderedAggregate: true})
	if err != nil {
		t.Fatal(err)
	}
	got := string(plan.Render(p))
	for _, op := range []string{`"Limit"`, `"Sort"`, `"Aggregate"`} {
		if !strings.Contains(got, op) {
			t.Fatalf("no %s in %s", op, got)
		}
	}
	if strings.Contains(got, "TopK") || strings.Contains(got, "OrderedAggregate") {
		t.Fatalf("optimized operators in %s", got)
	}
}
