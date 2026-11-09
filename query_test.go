package tacdb

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/internal/query/sema"
	"github.com/TacFlow/tacdb/value"
)

// setup runs fn in a committed write transaction.
func setup(t *testing.T, db *DB, fn func(tx *Tx)) {
	t.Helper()
	tx, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	fn(tx)
	if _, err := tx.Commit(bg); err != nil {
		t.Fatal(err)
	}
}

func props(kv ...any) map[string]value.Value {
	m := map[string]value.Value{}
	for i := 0; i < len(kv); i += 2 {
		switch v := kv[i+1].(type) {
		case int:
			m[kv[i].(string)] = value.Int(int64(v))
		case float64:
			m[kv[i].(string)] = value.Float(v)
		case string:
			m[kv[i].(string)] = value.String(v)
		case value.Value:
			m[kv[i].(string)] = v
		}
	}
	return m
}

func mustNode(t *testing.T, tx *Tx, labels []string, p map[string]value.Value) NodeID {
	t.Helper()
	n, err := tx.CreateNode(CreateNodeOptions{Labels: labels, Properties: p})
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

func mustEdge(t *testing.T, tx *Tx, from, to NodeID, typ string) EdgeID {
	t.Helper()
	e, err := tx.CreateEdge(from, to, typ, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// collect reads every row as text, and the error that ended them.
func collect(rows *Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var r []qv.Value
		for _, v := range rows.Values() {
			r = append(r, fromQuery(v))
		}
		out = append(out, rowString(r))
	}
	return out, rows.Err()
}

// query runs src and returns its rows as text.
func query(t *testing.T, db *DB, src string, p Params) []string {
	t.Helper()
	rows, err := db.Query(bg, src, p)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	out, err := collect(rows)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return out
}

// queryErr runs src and returns the error of Query or of its rows.
func queryErr(db *DB, src string, p Params) error {
	rows, err := db.Query(bg, src, p)
	if err != nil {
		return err
	}
	_, err = collect(rows)
	return err
}

func expectRows(t *testing.T, db *DB, src string, p Params, want ...string) {
	t.Helper()
	got := query(t, db, src, p)
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got  %q\n want %q", src, got, want)
	}
}

// QRY-02: WITH, aggregation, DISTINCT and empty input keep scope and
// cardinality.
func TestQueryWithAggregation(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) {
		for i, g := range []int{1, 1, 2, 3, 3, 3} {
			mustNode(t, tx, []string{"P"}, props("g", g, "i", i))
		}
	})
	expectRows(t, db, "MATCH (n:P) WITH n.g AS g, count(*) AS c WHERE c > 1 RETURN g, c ORDER BY g", nil,
		"1 | 2", "3 | 3")
	expectRows(t, db, "MATCH (n:P) RETURN DISTINCT n.g ORDER BY n.g DESC", nil, "3", "2", "1")
	expectRows(t, db, "MATCH (n:P) RETURN DISTINCT n.g, n.i % 2 ORDER BY n.i % 2, n.g", nil,
		"1 | 0", "2 | 0", "3 | 0", "1 | 1", "3 | 1")
	expectRows(t, db, "MATCH (n:P) RETURN count(DISTINCT n.g) AS k, sum(n.g) AS s, avg(n.i) AS a", nil, "3 | 13 | 2.5")
	// No row: without keys one row, with keys none.
	expectRows(t, db, "MATCH (n:None) RETURN count(*), sum(n.x), avg(n.x), collect(n.x), min(n.x)", nil,
		"0 | 0 | null | [] | null")
	expectRows(t, db, "MATCH (n:None) RETURN n.g, count(*)", nil)
	// Groups by the codec of equality: Int 1 and Float 1.0 are one key.
	expectRows(t, db, "UNWIND [1, 1.0, 2, null, null] AS x RETURN x, count(*) AS c ORDER BY c DESC, x", nil,
		"1 | 2", "null | 2", "2 | 1")
	// WITH replaces the scope.
	if err := queryErr(db, "MATCH (n:P) WITH n.g AS g RETURN n", nil); code(err) != CodeQuerySemantic {
		t.Fatalf("a variable out of scope: %v", err)
	}
	// The WHERE of a WITH filters after its ORDER BY and LIMIT.
	expectRows(t, db, "MATCH (n:P) WITH n.i AS i ORDER BY i LIMIT 3 WHERE i > 0 RETURN i", nil, "1", "2")
	// An aggregate reads only the rows of its group, after a WITH too.
	expectRows(t, db, "MATCH (n:P) WITH n.g AS g, collect(n.i) AS xs RETURN g, size(xs) AS k ORDER BY k DESC, g LIMIT 2", nil,
		"3 | 3", "1 | 2")
	// ORDER BY sees the variables before a projection without
	// aggregation, and an alias hides a variable of the same name.
	expectRows(t, db, "MATCH (n:P) RETURN n.g AS g ORDER BY n.i DESC LIMIT 2", nil, "3", "3")
	expectRows(t, db, "MATCH (n:P) WHERE n.i < 2 RETURN n.i AS n ORDER BY n DESC", nil, "1", "0")
	// SKIP and LIMIT from parameters, checked before execution.
	expectRows(t, db, "MATCH (n:P) RETURN n.i ORDER BY n.i SKIP $s LIMIT $l", Params{"s": value.Int(4), "l": value.Int(5)}, "4", "5")
	if err := queryErr(db, "MATCH (n:P) RETURN n LIMIT $l", Params{"l": value.Int(-1)}); code(err) != CodeParamType {
		t.Fatalf("a negative LIMIT: %v", err)
	}
}

// QRY-06: OPTIONAL MATCH is a left outer apply; its WHERE stays inside,
// Nulls propagate, and no row of the left side is lost.
func TestOptionalMatch(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) {
		a := mustNode(t, tx, []string{"P"}, props("name", "a"))
		b := mustNode(t, tx, nil, props("name", "b"))
		mustNode(t, tx, []string{"P"}, props("name", "c"))
		d := mustNode(t, tx, []string{"P"}, props("name", "d"))
		mustEdge(t, tx, a, b, "R")
		mustEdge(t, tx, a, b, "R") // parallel
		mustEdge(t, tx, d, d, "R") // self-loop
		mustEdge(t, tx, b, d, "S")
	})
	q := func(src string, want ...string) {
		t.Helper()
		got := query(t, db, src, nil)
		sorted := slices.Clone(got)
		slices.Sort(sorted)
		w := slices.Clone(want)
		slices.Sort(w)
		if !slices.Equal(sorted, w) {
			t.Fatalf("%s:\n got  %q\n want %q", src, got, want)
		}
	}
	q("MATCH (x:P) OPTIONAL MATCH (x)-[:R]->(y) RETURN x.name, y.name",
		`"a" | "b"`, `"a" | "b"`, `"c" | null`, `"d" | "d"`)
	q("MATCH (x:P) OPTIONAL MATCH (x)-[:R]->(y) WHERE y.name = 'none' RETURN x.name, y.name",
		`"a" | null`, `"c" | null`, `"d" | null`)
	q("MATCH (x:P) OPTIONAL MATCH (x)-[:R]->(y:Unknown) RETURN x.name, y",
		`"a" | null`, `"c" | null`, `"d" | null`)
	q("MATCH (x:P) OPTIONAL MATCH (x)-[:R]->(y) OPTIONAL MATCH (y)-[:S]->(z) RETURN x.name, y.name, z.name",
		`"a" | "b" | "d"`, `"a" | "b" | "d"`, `"c" | null | null`, `"d" | "d" | null`)
	q("MATCH (x:P) OPTIONAL MATCH (x)-[r:R]->() RETURN x.name, count(r)",
		`"a" | 2`, `"c" | 0`, `"d" | 1`)
	q("MATCH (x {name: 'd'}) OPTIONAL MATCH (x)-[r]-(y) RETURN count(r)", "2") // the self-loop once, and b-[:S]->d
	q("OPTIONAL MATCH (n:Unknown) RETURN n", "null")
	q("MATCH (x:P) OPTIONAL MATCH (x)-[:R]->(y) WITH x, y WHERE y IS NULL RETURN x.name", `"c"`)
	// The edges of the optional pattern are distinct among themselves,
	// not from those of the clause before.
	q("MATCH (x {name: 'a'})-[r]->(y) OPTIONAL MATCH (x)-[s]->(y) RETURN count(*)", "4")
	q("MATCH (x {name: 'a'}) OPTIONAL MATCH (x)-[r]->(y), (x)-[s]->(y) RETURN count(*)", "2")
}

// QRY-08: the bindings of a scope up to the limit work; one more is
// LIMIT_EXCEEDED, and the handle keeps working.
func TestBindings(t *testing.T) {
	pattern := func(n int) string {
		var b strings.Builder
		b.WriteString("MATCH ")
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(a%d)", i)
		}
		b.WriteString(" RETURN count(*)")
		return b.String()
	}
	for _, c := range []struct {
		limit, n int
		ok       bool
	}{{0, 17, true}, {0, 256, true}, {0, 257, false}, {4096, 4096, true}, {20, 20, true}, {20, 21, false}} {
		o := options()
		if c.limit > 0 {
			o.MaxBindings = c.limit
		}
		db, err := OpenMemory(o)
		if err != nil {
			t.Fatal(err)
		}
		setup(t, db, func(tx *Tx) { mustNode(t, tx, nil, nil) })
		err = queryErr(db, pattern(c.n), nil)
		if c.ok && err != nil || !c.ok && code(err) != CodeLimitExceeded {
			t.Fatalf("limit %d, %d bindings: %v", c.limit, c.n, err)
		}
		if !c.ok {
			e := err.(*Error)
			if e.Details["limit"] != int64(o.MaxBindings) || e.Details["count"] != int64(c.n) {
				t.Fatalf("details %v", e.Details)
			}
		}
		expectRows(t, db, "MATCH (n) RETURN count(n)", nil, "1")
		db.Close()
	}
	o := options()
	o.MaxBindings = 4097
	if _, err := OpenMemory(o); code(err) != CodeInvalidArgument {
		t.Fatalf("MaxBindings above 4096: %v", err)
	}
}

// QRY-09: TopK and the ordered aggregate give what Sort with SKIP and
// LIMIT and the hash aggregate give, with Nulls, ties, duplicates and
// mixed types.
func TestTopKAndOrderedAggregate(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) {
		vals := []value.Value{value.Null(), value.Int(1), value.Float(1), value.Int(2), value.String("a"),
			value.String("B"), value.Float(-0.0), value.Int(0), value.Bool(true)}
		var prev NodeID
		for i := range 300 {
			p := map[string]value.Value{"v": vals[(i*7)%len(vals)], "w": value.Int(int64(i % 5))}
			if p["v"].IsNull() {
				delete(p, "v")
			}
			n := mustNode(t, tx, []string{"N"}, p)
			if prev != 0 && i%3 != 0 {
				mustEdge(t, tx, prev, n, "R")
				mustEdge(t, tx, n, prev, "R")
			}
			prev = n
		}
	})
	queries := []string{
		"MATCH (n:N) RETURN n.v, n.w ORDER BY n.v LIMIT 17",
		"MATCH (n:N) RETURN n.v, id(n) ORDER BY n.v DESC, n.w SKIP 5 LIMIT 40",
		"MATCH (n:N) RETURN n.w ORDER BY n.w SKIP 290 LIMIT 50",
		"MATCH (n:N) RETURN n.v ORDER BY n.v LIMIT 0",
		"MATCH (n:N) RETURN DISTINCT n.v ORDER BY n.v DESC LIMIT 4",
		"MATCH (n:N)-[:R]->(m) RETURN n.w, count(*) AS c ORDER BY c DESC, n.w LIMIT 3",
		"MATCH (n:N)-[:R]-(m) RETURN n, count(m), collect(m.w), sum(m.w) ORDER BY id(n) LIMIT 50",
		"MATCH (n:N) OPTIONAL MATCH (n)-[:R]->(m) RETURN n, n.v, count(m) ORDER BY id(n) LIMIT 60",
		"MATCH (n:N) WITH n, n.w AS w MATCH (n)-[:R]->(m) RETURN n, n.w, max(m.v) ORDER BY id(n)",
		"MATCH (n:N)-[:R]-(m) WITH n, m ORDER BY m.w RETURN n, count(*) ORDER BY id(n)",
		"UNWIND [1, 2] AS x MATCH (n:N) RETURN n, count(*) ORDER BY id(n) LIMIT 20",
		"MATCH (a:N {w: 1}), (n:N) RETURN n, count(*) ORDER BY id(n) LIMIT 20",
	}
	run := func(src string) []string {
		return query(t, db, src, nil)
	}
	for _, src := range queries {
		fast := run(src)
		db.q.plan.NoTopK, db.q.plan.NoOrderedAggregate = true, true
		plain := run(src)
		db.q.plan.NoTopK, db.q.plan.NoOrderedAggregate = false, false
		if !slices.Equal(fast, plain) {
			t.Fatalf("%s:\n fast  %q\n plain %q", src, fast, plain)
		}
	}
	for src, op := range map[string]string{
		queries[0]: `"TopK"`, queries[6]: `"OrderedAggregate"`, queries[7]: `"OrderedAggregate"`, queries[8]: `"OrderedAggregate"`,
	} {
		if p := planOf(t, db, src); !strings.Contains(p, op) {
			t.Fatalf("%s: no %s in %s", src, op, p)
		}
	}
	// Grouping by something besides the clustered node needs the hash
	// aggregate.
	if p := planOf(t, db, "MATCH (n:N)-[:R]->(m) RETURN n, m, count(*)"); strings.Contains(p, "OrderedAggregate") {
		t.Fatalf("ordered aggregate on two variables: %s", p)
	}
}

// QRY-04 in part: LIMIT after a selective late filter returns the rows
// the full execution would; TopK never runs before the filter.
func TestLimitAfterFilter(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) {
		for i := range 500 {
			mustNode(t, tx, []string{"N"}, props("i", i))
		}
	})
	expectRows(t, db, "MATCH (n:N) WHERE n.i % 100 = 99 RETURN n.i ORDER BY n.i DESC LIMIT 2", nil, "499", "399")
	expectRows(t, db, "MATCH (n:N) WITH n ORDER BY n.i LIMIT 10 WHERE n.i > 7 RETURN n.i", nil, "8", "9")
}

func TestQueryValues(t *testing.T) {
	db := memory(t)
	var a, b NodeID
	var r EdgeID
	setup(t, db, func(tx *Tx) {
		a = mustNode(t, tx, []string{"P", "Q"}, props("name", "a", "x", 1.5))
		b = mustNode(t, tx, nil, nil)
		e, err := tx.CreateEdge(a, b, "R", props("w", 2))
		if err != nil {
			t.Fatal(err)
		}
		r = e.ID
	})
	rows, err := db.Query(bg, "MATCH (a:P)-[r]->(b) RETURN a, r, b, collect(a) AS l, {n: a, k: 1} AS m, id(a) AS i, a.missing AS z", nil)
	if err != nil {
		t.Fatal(err)
	}
	cols := rows.Columns()
	types := []QueryType{TypeNode, TypeEdge, TypeNode, TypeList, TypeMap, TypeInt, TypeAny}
	for i, c := range cols {
		if c.Type != types[i] {
			t.Fatalf("column %s: %v, want %v", c.Name, c.Type, types[i])
		}
	}
	if cols[6].Nullable != true || cols[0].Nullable {
		t.Fatalf("nullability %+v", cols)
	}
	cols[0].Name = "changed"
	if rows.Columns()[0].Name != "a" {
		t.Fatal("Columns shares its slice")
	}
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	v := rows.Values()
	n, ok := v[0].Node()
	if !ok || n.ID != a || !slices.Equal(n.Labels, []string{"P", "Q"}) || !value.Identical(n.Properties["x"], value.Float(1.5)) {
		t.Fatalf("node %+v", n)
	}
	e, ok := v[1].Edge()
	if !ok || e.ID != r || e.From != a || e.To != b || e.Type != "R" || !value.Identical(e.Properties["w"], value.Int(2)) {
		t.Fatalf("edge %+v", e)
	}
	l, ok := v[3].List()
	if ln, _ := l[0].Node(); !ok || len(l) != 1 || ln.ID != a {
		t.Fatalf("collect of nodes %+v", l)
	}
	m, ok := v[4].Map()
	if mn, _ := m["n"].Node(); !ok || mn.ID != a {
		t.Fatalf("map with a node %+v", m)
	}
	if _, err := v[4].ToValue(); code(err) != CodeTypeMismatch {
		t.Fatalf("a map with a node as a property value: %v", err)
	}
	if x, ok := v[5].Value(); !ok || !value.Identical(x, value.Int(int64(a))) {
		t.Fatalf("id %v", x)
	}
	if !v[6].IsNull() {
		t.Fatalf("a missing property %v", v[6])
	}
	clone := rows.CloneValues()
	cn, _ := clone[0].Node()
	cn.Labels[0] = "changed"
	if n2, _ := rows.Values()[0].Node(); n2.Labels[0] != "P" {
		t.Fatal("CloneValues shares the labels")
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("one row only: %v", rows.Err())
	}
	st := rows.Stats()
	if !st.Complete || st.Rows != 1 || st.Examined == 0 || st.WorkUnits == 0 || st.PlanMode != "syntactic" {
		t.Fatalf("stats %+v", st)
	}
}

// The lifecycle of Rows (SPEC §31.1, design §3.3).
func TestRowsLifecycle(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) {
		for i := range 10 {
			mustNode(t, tx, []string{"N"}, props("i", i))
		}
	})
	const src = "MATCH (n:N) RETURN n.i"

	// A Rows of Tx.Query: Commit is BUSY and writes are BUSY while it is
	// open; Close leaves the transaction usable.
	tx, _ := db.BeginWrite(bg)
	rows, err := tx.Query(bg, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	if _, err := tx.Commit(bg); code(err) != CodeBusy {
		t.Fatalf("Commit with open rows: %v", err)
	}
	if _, err := tx.CreateNode(CreateNodeOptions{}); code(err) != CodeBusy {
		t.Fatalf("a write with open rows: %v", err)
	}
	if err := tx.CreateIndex(IndexDefinition{Name: "i", Kind: PropertyIndex, Scope: "N", Property: "i"}); code(err) != CodeBusy {
		t.Fatalf("DDL with open rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if rows.Next() {
		t.Fatal("Next after Close")
	}
	if _, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"N"}}); err != nil {
		t.Fatal(err)
	}
	// A query in a write transaction sees its writes.
	rows, _ = tx.Query(bg, "MATCH (n:N) RETURN count(*)", nil)
	if got, _ := collect(rows); !slices.Equal(got, []string{"11"}) {
		t.Fatalf("own writes: %v", got)
	}
	if _, err := tx.Commit(bg); err != nil {
		t.Fatalf("Commit after the rows ended: %v", err)
	}

	// A read transaction cannot commit with open rows either.
	tx, _ = db.BeginRead(bg)
	rows, _ = tx.Query(bg, src, nil)
	if _, err := tx.Commit(bg); code(err) != CodeBusy {
		t.Fatalf("Commit of a read transaction with open rows: %v", err)
	}
	rows.Close()
	if _, err := tx.Commit(bg); err != nil {
		t.Fatal(err)
	}

	// Rollback closes the rows, which then report TX_CLOSED.
	tx, _ = db.BeginRead(bg)
	rows, _ = tx.Query(bg, src, nil)
	rows.Next()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if rows.Next() || code(rows.Err()) != CodeTxClosed {
		t.Fatalf("after Rollback: %v", rows.Err())
	}
	if _, err := tx.Query(bg, src, nil); code(err) != CodeTxClosed {
		t.Fatalf("Query after Rollback: %v", err)
	}

	// A Rows of DB.Query owns its transaction: the handle cannot close
	// while it is open, and can after its end.
	rows, _ = db.Query(bg, src, nil)
	rows.Next()
	if err := db.Close(); code(err) != CodeBusy {
		t.Fatalf("Close with open rows: %v", err)
	}
	if n, _ := collect(rows); len(n) != 10 {
		t.Fatalf("the rest of the rows: %d", len(n))
	}

	// View closes the rows its function left open.
	var left *Rows
	db.View(bg, func(tx *Tx) error {
		left, _ = tx.Query(bg, src, nil)
		return nil
	})
	if left.Next() || code(left.Err()) != CodeTxClosed {
		t.Fatalf("rows left by View: %v", left.Err())
	}
	// Update fails with BUSY when its function leaves rows open.
	err = db.Update(bg, func(tx *Tx) error {
		_, err := tx.Query(bg, src, nil)
		return err
	})
	if code(err) != CodeBusy {
		t.Fatalf("Update with open rows: %v", err)
	}

	// One user at a time.
	tx, _ = db.BeginRead(bg)
	defer tx.Rollback()
	rows, _ = tx.Query(bg, src, nil)
	tx.busy.Store(true)
	if rows.Next() || code(rows.Err()) != CodeTxInUse {
		t.Fatalf("a collision: %v", rows.Err())
	}
	if err := rows.Close(); code(err) != CodeTxInUse {
		t.Fatalf("Close during a collision: %v", err)
	}
	tx.busy.Store(false)
	if got, err := collect(rows); len(got) != 11 || err != nil {
		t.Fatalf("after the collision: %d rows, %v", len(got), err)
	}
	if err := db.Close(); err == nil {
		t.Fatal("Close with a read transaction open")
	}
}

// A Rows whose snapshot expires reports SNAPSHOT_EXPIRED.
func TestRowsSnapshotExpired(t *testing.T) {
	o := options()
	o.MaxReadTxAge = 50 * time.Millisecond
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup(t, db, func(tx *Tx) {
		for range 300 {
			mustNode(t, tx, []string{"N"}, nil)
		}
	})
	rows, err := db.Query(bg, "MATCH (n:N) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	time.Sleep(150 * time.Millisecond)
	n := 0
	for rows.Next() {
		n++
	}
	if code(rows.Err()) != CodeSnapshotExpired || n >= 299 {
		t.Fatalf("%d rows, then %v; want SNAPSHOT_EXPIRED", n, rows.Err())
	}
	if err := db.Close(); err != nil {
		t.Fatalf("the expired rows kept their transaction: %v", err)
	}
}

func TestQueryErrors(t *testing.T) {
	o := options()
	o.MaxHops = 4
	o.MaxExpansions = 1000
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup(t, db, func(tx *Tx) {
		var prev NodeID
		for i := range 8 {
			n := mustNode(t, tx, []string{"C"}, props("i", i))
			if prev != 0 {
				mustEdge(t, tx, prev, n, "NEXT")
			}
			prev = n
		}
		// A cycle of MaxHops edges: the only edge past it is already in
		// the path.
		var ring []NodeID
		for i := range 4 {
			ring = append(ring, mustNode(t, tx, []string{"Y"}, props("i", i)))
		}
		for i := range ring {
			mustEdge(t, tx, ring[i], ring[(i+1)%4], "CYC")
		}
		hub := mustNode(t, tx, []string{"H"}, nil)
		for range 40 {
			x := mustNode(t, tx, nil, nil)
			mustEdge(t, tx, hub, x, "L")
			mustEdge(t, tx, x, hub, "L")
		}
	})
	for _, c := range []struct {
		src  string
		p    Params
		code Code
	}{
		{"CREATE (n)", nil, CodeQueryWriteRequiresExec},
		{"MATCH (n) SET n.x = 1", nil, CodeQueryWriteRequiresExec},
		{"MATCH (n) DETACH DELETE n RETURN count(*)", nil, CodeQueryWriteRequiresExec},
		{"EXPLAIN MATCH (n) RETURN n", nil, CodeQueryUnsupported},
		{"CALL tacdb.graph.shortest_path(1, 2, {}) YIELD path RETURN path", nil, CodeQuerySemantic},
		{"MATCH (n) USING VECTOR n.v RETURN n", nil, CodeQueryUnsupported},
		{"MATCH (n) WHERE n.v <=> [1.0] < 1 RETURN n", nil, CodeQueryUnsupported},
		{"MATCH (n) WHERE n.i = $x RETURN n", nil, CodeParamMissing},
		{"MATCH (a)-[:NEXT*3..1]->(b) RETURN b", nil, CodeQuerySemantic},
		{"MATCH (a)-[:NEXT*..5]->(b) RETURN b", nil, CodeResourceLimit},
		{"MATCH (a)-[:NEXT*7..]->(b) RETURN b", nil, CodeResourceLimit},
		{"MATCH (a:C {i: 0})-[:NEXT*]->(b) RETURN b", nil, CodeResourceLimit},
		{"MATCH (a:H)-[:L*..4]-(b) RETURN count(*)", nil, CodeResourceLimit},
		{"UNWIND range(1, 100000000) AS x RETURN x", nil, CodeResourceLimit},
		{"RETURN 1 / 0", nil, CodeInvalidArgument},
		{"MATCH (n RETURN n", nil, CodeQueryParse},
		{"MATCH (n:None) WHERE n.k = $x RETURN n", nil, CodeParamMissing},
		{"MATCH (n) RETURN n; MATCH (m) RETURN m", nil, CodeQueryParse},
	} {
		err := queryErr(db, c.src, c.p)
		if code(err) != c.code {
			t.Fatalf("%s: %v, want %s", c.src, err, c.code)
		}
		if c.code == CodeResourceLimit {
			e := err.(*Error)
			if _, ok := e.Details["limit"]; !ok {
				t.Fatalf("%s: no limit in %v", c.src, e.Details)
			}
		}
	}
	expectRows(t, db, "MATCH (a:Y {i: 0})-[:CYC*]->(b) RETURN count(*)", nil, "4")
	// A chain within MaxHops is complete.
	expectRows(t, db, "MATCH (a:C {i: 4})-[:NEXT*]->(b) RETURN count(b)", nil, "3")
	expectRows(t, db, "MATCH (a:C {i: 0})-[:NEXT*0]->(b) RETURN b.i", nil, "0")
	// Two variable-length relationships of a clause never share an edge.
	expectRows(t, db, "MATCH (a:C {i: 2})-[:NEXT*2]->(b)<-[:NEXT*0..2]-(c) RETURN c.i", nil, "4")
	expectRows(t, db, "MATCH (a:C {i: 2})-[:NEXT*2]->(b) MATCH (b)<-[:NEXT*0..2]-(c) RETURN c.i ORDER BY c.i", nil, "2", "3", "4")
}

func TestQueryBudget(t *testing.T) {
	o := options()
	o.QueryMemoryBytes = 64 << 10
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup(t, db, func(tx *Tx) {
		for i := range 3000 {
			mustNode(t, tx, []string{"N"}, props("s", strings.Repeat("x", 20)+fmt.Sprint(i)))
		}
	})
	for _, src := range []string{
		"MATCH (n:N) RETURN n.s ORDER BY n.s",
		"MATCH (n:N) RETURN DISTINCT n.s",
		"MATCH (n:N) RETURN n.s, count(*)",
		"MATCH (n:N) RETURN collect(n.s)",
		"MATCH (n:N) RETURN n.s ORDER BY n.s LIMIT 2900",
	} {
		err := queryErr(db, src, nil)
		e, ok := err.(*Error)
		if !ok || e.Code != CodeResourceLimit || e.Details["limit"] != o.QueryMemoryBytes {
			t.Fatalf("%s: %v", src, err)
		}
	}
	// Streaming and a small TopK fit.
	if n := len(query(t, db, "MATCH (n:N) RETURN n.s", nil)); n != 3000 {
		t.Fatalf("%d rows", n)
	}
	expectRows(t, db, "MATCH (n:N) RETURN n.s ORDER BY n.s DESC LIMIT 1", nil, `"xxxxxxxxxxxxxxxxxxxx999"`)

	o = options()
	o.QueryWorkUnits = 1000
	db2, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	setup(t, db2, func(tx *Tx) {
		for range 600 {
			mustNode(t, tx, nil, nil)
		}
	})
	if err := queryErr(db2, "MATCH (n) RETURN count(*)", nil); code(err) != CodeResourceLimit {
		t.Fatalf("work units: %v", err)
	}

	// A canceled context ends the query.
	c, cancel := context.WithCancel(bg)
	rows, err := db.Query(c, "MATCH (n:N) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	cancel()
	for rows.Next() {
	}
	if code(rows.Err()) != CodeCanceled {
		t.Fatalf("canceled: %v", rows.Err())
	}
}

// planOf renders the plan of src.
func planOf(t *testing.T, db *DB, src string) string {
	t.Helper()
	p, err := db.plan(src)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// plan renders the plan of src with the options of db.
func (db *DB) plan(src string) (string, error) {
	q, err := parser.Parse(src, db.q.parse)
	if err != nil {
		return "", err
	}
	info, err := sema.Analyze(q, src, db.q.sema)
	if err != nil {
		return "", err
	}
	p, err := plan.Build(q, info, src, db.q.plan)
	if err != nil {
		return "", err
	}
	return string(plan.Render(p)), nil
}

// Pattern maps may read elements bound later in the clause; a bound
// variable alone is a node or no row; a relationship bound by an earlier
// clause matches only itself; ExpandInto walks the side with fewer edges.
func TestPatternBindings(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) {
		mustNode(t, tx, nil, props("k", 1))
		mustNode(t, tx, nil, props("k", 2))
		mustNode(t, tx, []string{"B"}, props("k", 1))
		h := mustNode(t, tx, []string{"H"}, nil)
		for i := range 100 {
			l := mustNode(t, tx, []string{"L"}, props("i", i))
			mustEdge(t, tx, h, l, "R")
		}
	})
	expectRows(t, db, "MATCH (a {k: b.k}), (b:B) RETURN a.k, id(a) = id(b) ORDER BY id(a)", nil, "1 | false", "1 | true")
	expectRows(t, db, "OPTIONAL MATCH (n:Unknown) MATCH (n) RETURN count(*)", nil, "0")
	expectRows(t, db, "MATCH ()-[r:R]->() WITH r MATCH (x)-[r]->(y) RETURN count(*)", nil, "100")
	rows, err := db.Query(bg, "MATCH (h:H) MATCH (l:L {i: 5}) MATCH (h)-->(l) RETURN count(*)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := collect(rows); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("ExpandInto: %v", got)
	}
	if n := rows.Stats().Examined; n > 150 {
		t.Fatalf("ExpandInto walked the hub: %d entities examined", n)
	}
}

// The executor checks the context and the snapshot between rows, without
// I/O too; parameters are copied by Query.
func TestQueryChecksWithoutIO(t *testing.T) {
	db := memory(t)
	const src = "UNWIND range(1, 1000) AS a UNWIND range(1, 1000) AS b RETURN a"
	tx, _ := db.BeginRead(bg)
	defer tx.Rollback()
	c, cancel := context.WithCancel(bg)
	cancel()
	rows, err := tx.Query(c, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(rows); code(err) != CodeCanceled {
		t.Fatalf("canceled: %v", err)
	}
	c, cancel = context.WithTimeout(bg, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	rows, _ = tx.Query(c, src, nil)
	if _, err := collect(rows); code(err) != CodeDeadline {
		t.Fatalf("deadline: %v", err)
	}

	o := options()
	o.MaxReadTxAge = 50 * time.Millisecond
	db2, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	rows, _ = db2.Query(bg, src, nil)
	rows.Next()
	time.Sleep(150 * time.Millisecond)
	if _, err := collect(rows); code(err) != CodeSnapshotExpired {
		t.Fatalf("expired without I/O: %v", err)
	}

	p := Params{"x": value.Int(1)}
	rows, _ = db.Query(bg, "RETURN $x", p)
	p["x"] = value.Int(2)
	if got, _ := collect(rows); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("parameters shared with the caller: %v", got)
	}
}

// Rows closed by Rollback or by the end of View return their memory to
// the governor.
func TestRowsReleaseMemory(t *testing.T) {
	gov, err := govern.New(govern.Config{Limit: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	o := options()
	o.governor = gov
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const src = "UNWIND range(1, 20000) AS x RETURN x ORDER BY x DESC"
	execution := func() int64 { return gov.Stats().Used[govern.Execution] }
	tx, _ := db.BeginWrite(bg)
	rows, _ := tx.Query(bg, src, nil)
	rows.Next()
	if execution() == 0 {
		t.Fatal("the sort charged nothing")
	}
	tx.Rollback()
	if n := execution(); n != 0 {
		t.Fatalf("after Rollback the execution category holds %d bytes", n)
	}
	db.View(bg, func(tx *Tx) error {
		rows, _ := tx.Query(bg, src, nil)
		rows.Next()
		return nil
	})
	if n := execution(); n != 0 {
		t.Fatalf("after View the execution category holds %d bytes", n)
	}
}

// An ID seek finds a node by the = of expressions: a Float with an
// integer value finds it, another value finds nothing.
func TestIDSeekValues(t *testing.T) {
	db := memory(t)
	setup(t, db, func(tx *Tx) { mustNode(t, tx, nil, props("k", 1)) })
	for _, c := range []struct {
		src  string
		p    Params
		want []string
	}{
		{"MATCH (n) WHERE id(n) = 1.0 RETURN n.k", nil, []string{"1"}},
		{"MATCH (n) WHERE $x = id(n) RETURN n.k", Params{"x": value.Float(1)}, []string{"1"}},
		{"MATCH (n) WHERE id(n) = 1.5 RETURN n.k", nil, nil},
		{"MATCH (n) WHERE id(n) = $x RETURN n.k", Params{"x": value.Float(-1)}, nil},
		{"MATCH (n) WHERE id(n) = '1' RETURN n.k", nil, nil},
		{"MATCH (n) WHERE id(n) = null RETURN n.k", nil, nil},
		{"MATCH (n) WHERE id(n) = 0 RETURN n.k", nil, nil},
	} {
		if got := query(t, db, c.src, c.p); !slices.Equal(got, c.want) {
			t.Fatalf("%s: %q, want %q", c.src, got, c.want)
		}
	}
}

// Rows that operators produce without reading the graph check the
// context too, before an aggregate or a sort keeps them.
func TestCancelWithoutOutputRows(t *testing.T) {
	db := memory(t)
	for _, src := range []string{
		"UNWIND range(1, 1000) AS a UNWIND range(1, 1000) AS b RETURN count(*)",
		"UNWIND range(1, 1000) AS a UNWIND range(1, 1000) AS b WITH a WHERE a < 0 RETURN a",
	} {
		c, cancel := context.WithCancel(bg)
		rows, err := db.Query(c, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if out, err := collect(rows); code(err) != CodeCanceled {
			t.Fatalf("%s: %q, %v", src, out, err)
		}
	}
}

// Errors of the executor carry the execute stage; those of a
// variable-length relationship point at it.
func TestExecutionErrorStage(t *testing.T) {
	o := options()
	o.MaxHops, o.MaxExpansions, o.QueryWorkUnits = 3, 50, 2000
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup(t, db, func(tx *Tx) {
		var prev NodeID
		for i := range 10 {
			n := mustNode(t, tx, []string{"C"}, props("i", i))
			if prev != 0 {
				mustEdge(t, tx, prev, n, "NEXT")
				mustEdge(t, tx, prev, n, "NEXT")
			}
			prev = n
		}
	})
	for _, c := range []struct {
		src    string
		column int // of the location; 0 for none
	}{
		{"MATCH (a:C {i: 0})-[*]->(b) RETURN b", 19},
		{"MATCH (a:C)-[*..3]->(b) RETURN b", 12},
		{"UNWIND range(1, 1500) AS x RETURN x ORDER BY x", 0},
	} {
		e, ok := queryErr(db, c.src, nil).(*Error)
		if !ok || e.Code != CodeResourceLimit || e.Stage != StageExecute {
			t.Fatalf("%s: %v", c.src, e)
		}
		if c.column > 0 && (e.Location == nil || e.Location.Column != c.column) {
			t.Fatalf("%s: location %+v", c.src, e.Location)
		}
	}
}
