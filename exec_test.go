package tacdb

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// execOK runs src with DB.Exec and fails the test on an error.
func execOK(t *testing.T, db *DB, src string, p Params) Result {
	t.Helper()
	res, err := db.Exec(bg, src, p)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return res
}

// texts renders the rows of a result as collect does.
func texts(res Result) []string {
	var out []string
	for _, r := range res.Rows {
		var row []qv.Value
		for _, v := range r {
			row = append(row, fromQuery(v))
		}
		out = append(out, rowString(row))
	}
	return out
}

func expectExec(t *testing.T, db *DB, src string, p Params, want ...string) Result {
	t.Helper()
	res := execOK(t, db, src, p)
	if got := texts(res); !slices.Equal(got, want) {
		t.Fatalf("%s:\n got  %q\n want %q", src, got, want)
	}
	return res
}

// counters lists the effects of a result: nodes created and deleted,
// edges created and deleted, properties set, labels added and removed.
func counters(s QueryStats) [7]uint64 {
	return [7]uint64{s.NodesCreated, s.NodesDeleted, s.EdgesCreated, s.EdgesDeleted, s.PropertiesSet, s.LabelsAdded, s.LabelsRemoved}
}

func expectCounters(t *testing.T, res Result, want [7]uint64) {
	t.Helper()
	if got := counters(res.Stats); got != want {
		t.Fatalf("counters %v, want %v", got, want)
	}
}

func execCode(t *testing.T, db *DB, src string, p Params, want Code) *Error {
	t.Helper()
	_, err := db.Exec(bg, src, p)
	if code(err) != want {
		t.Fatalf("%s: %v, want %s", src, err, want)
	}
	e, _ := err.(*Error)
	return e
}

// The API of Exec: reads and writes, transactions, cursors and receipts.
func TestExecAPI(t *testing.T) {
	db := memory(t)
	res := expectExec(t, db, "CREATE (n:N {k: 1}) RETURN n.k AS k", nil, "1")
	if len(res.Columns) != 1 || res.Columns[0].Name != "k" || res.CommitID == 0 || !res.Durable || res.Token == (CommitToken{}) || res.Pending {
		t.Fatalf("result of a write: %+v", res)
	}
	expectCounters(t, res, [7]uint64{1, 0, 0, 0, 1, 1, 0})
	// A write without RETURN has no columns nor rows.
	res = execOK(t, db, "CREATE (:N {k: 2})", nil)
	if res.Columns != nil || res.Rows != nil || res.Stats.Rows != 0 || !res.Stats.Complete {
		t.Fatalf("a write without RETURN: %+v", res)
	}
	// A read through Exec returns its rows and its snapshot.
	res = expectExec(t, db, "MATCH (n:N) RETURN n.k ORDER BY n.k", nil, "1", "2")
	if res.CommitID == 0 || res.Token != (CommitToken{}) {
		t.Fatalf("result of a read: %+v", res)
	}
	if err := queryErr(db, "CREATE (n)", nil); code(err) != CodeQueryWriteRequiresExec {
		t.Fatalf("Query of a write: %v", err)
	}

	// Tx.Exec: a statement of the transaction, visible to it, committed
	// with it.
	tx, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	res, err = tx.Exec(bg, "CREATE (n:N {k: 3}) RETURN n.k", nil)
	if err != nil || res.CommitID != 0 || res.Token != (CommitToken{}) {
		t.Fatalf("Tx.Exec: %v %+v", err, res)
	}
	rows, err := tx.Query(bg, "MATCH (n:N) RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(bg, "CREATE (:N)", nil); code(err) != CodeBusy {
		t.Fatalf("Exec with open rows: %v", err)
	}
	// A read through Exec does not change the transaction: allowed.
	if res, err := tx.Exec(bg, "MATCH (n:N) RETURN count(n)", nil); err != nil || texts(res)[0] != "3" {
		t.Fatalf("a read with open rows: %v %v", err, texts(res))
	}
	if got, err := collect(rows); err != nil || got[0] != "3" {
		t.Fatalf("the transaction reads its statement: %q %v", got, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	expectRows(t, db, "MATCH (n:N) RETURN count(n)", nil, "2")
	if _, err := tx.Exec(bg, "CREATE (:N)", nil); code(err) != CodeTxClosed {
		t.Fatalf("Exec after Rollback: %v", err)
	}

	// A read transaction runs reads and refuses writes.
	rtx, err := db.BeginRead(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rtx.Exec(bg, "CREATE (:N)", nil); code(err) != CodeReadOnly {
		t.Fatalf("a write in a read transaction: %v", err)
	}
	if res, err := rtx.Exec(bg, "MATCH (n:N) RETURN count(n)", nil); err != nil || texts(res)[0] != "2" {
		t.Fatalf("a read in a read transaction: %v", err)
	}
	if _, err := rtx.Commit(bg); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"MATCH (n) RETURN n", "CREATE (n)"} {
		if _, err := rtx.Exec(bg, src, nil); code(err) != CodeTxClosed {
			t.Fatalf("%s after Commit of a read: %v", src, err)
		}
	}
	// A read through DB.Exec does not wait for the writer.
	wtx, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(bg, 2*time.Second)
	if _, err := db.Exec(tctx, "MATCH (n:N) RETURN count(n)", nil); err != nil {
		t.Fatalf("a read while the writer is taken: %v", err)
	}
	cancel()
	wtx.Rollback()

	// Inside Update, Exec takes part in the transaction.
	err = db.Update(bg, func(tx *Tx) error {
		if _, err := tx.Exec(bg, "CREATE (:N {k: 4})", nil); err != nil {
			return err
		}
		_, err := tx.Exec(bg, "MATCH (n:N {k: 4}) SET n.k = 5", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	expectRows(t, db, "MATCH (n:N) RETURN n.k ORDER BY n.k", nil, "1", "2", "5")

	// Parameters are copied: the caller may change its map afterwards.
	p := Params{"k": value.Int(9)}
	expectExec(t, db, "CREATE (n:P {k: $k}) RETURN n.k", p, "9")
	if _, err := db.Exec(bg, "CREATE (n:P {k: $missing})", nil); code(err) != CodeParamMissing {
		t.Fatalf("a missing parameter: %v", err)
	}
	if _, err := db.Exec(bg, "MERGE (n:P {k: $k})", Params{"k": value.Null()}); code(err) != CodeParamType {
		t.Fatalf("a null parameter in MERGE: %v", err)
	}
}

// QRY-05: an error in the text, in the analysis or in the middle of the
// execution leaves no effect; in a transaction, the failed statement goes
// and the earlier ones stay.
func TestExecNoPartialEffects(t *testing.T) {
	db := memory(t)
	execOK(t, db, "UNWIND range(1, 3) AS i CREATE (:N {i: i})", nil)
	before := dumpDB(t, db)
	for _, c := range []struct {
		src  string
		code Code
	}{
		{"CREATE (n:X) RETURN n trailing", CodeQueryParse},
		{"CREATE (n:X); CREATE (m:X)", CodeQueryParse},
		{"CREATE (n:X) UNION CREATE (m:X)", CodeQueryUnsupported},
		{"CREATE (n:X) RETURN m", CodeQuerySemantic},
		{"MATCH (n:N) CREATE (n:X)", CodeQuerySemantic},
		{"MERGE (n:X {k: null})", CodeQuerySemantic},
		{"CREATE (n:X) SET n = 1", CodeQuerySemantic},
		{"MATCH (n:N) CREATE (:X {i: n.i}) WITH n CREATE (:Y {v: 10 / (n.i - 2)})", CodeInvalidArgument},
		{"MATCH (n:N) SET n.i = n.i + 100 WITH n MATCH (m:N) SET m.bad = m", CodeTypeMismatch},
		{"MATCH (n:N) DELETE n", CodeQueryWriteRequiresExec}, // checked below with edges
		{"UNWIND [1, 2, 3] AS i CREATE (:X) WITH i UNWIND range(1, 100000000) AS j RETURN j", CodeResourceLimit},
	} {
		if c.code == CodeQueryWriteRequiresExec {
			continue
		}
		execCode(t, db, c.src, nil, c.code)
		if got := dumpDB(t, db); !slices.Equal(got, before) {
			t.Fatalf("%s changed the graph:\n got  %q\n want %q", c.src, got, before)
		}
	}
	// A node with edges cannot go without DETACH, and nothing goes.
	execOK(t, db, "MATCH (a:N {i: 1}), (b:N {i: 2}) CREATE (a)-[:R]->(b)", nil)
	before = dumpDB(t, db)
	e := execCode(t, db, "MATCH (n:N) DELETE n", nil, CodeInvalidArgument)
	if e.Details["edges"] != int64(1) || e.Location == nil || e.Stage != StageExecute {
		t.Fatalf("DELETE of a node with edges: %+v", e)
	}
	if got := dumpDB(t, db); !slices.Equal(got, before) {
		t.Fatalf("a failed DELETE changed the graph: %q", got)
	}
	// A canceled context: CANCELED, no effect.
	cctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := db.Exec(cctx, "CREATE (:X)", nil); code(err) != CodeCanceled {
		t.Fatalf("a canceled Exec: %v", err)
	}

	tx, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(bg, "CREATE (:T {k: 1})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(bg, "UNWIND [1, 0] AS d CREATE (:T {k: 1 / d})", nil); code(err) != CodeInvalidArgument {
		t.Fatalf("a failing statement: %v", err)
	}
	if _, err := tx.Exec(bg, "CREATE (:T {k: 2})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(bg); err != nil {
		t.Fatal(err)
	}
	expectRows(t, db, "MATCH (n:T) RETURN n.k ORDER BY n.k", nil, "1", "2")
	expectRows(t, db, "MATCH (n:X) RETURN count(n)", nil, "0")
}

// Every clause that writes reads all its rows first, and the clauses
// after it read all its effects (design §4.4), past the batches of a
// scan.
func TestExecBarriers(t *testing.T) {
	db := memory(t)
	execOK(t, db, "UNWIND range(1, 300) AS i CREATE (:N {i: i})", nil)
	res := expectExec(t, db, "MATCH (n:N) CREATE (m:N {i: n.i + 1000}) WITH count(*) AS k MATCH (x:N) RETURN k, count(x)", nil, "300 | 600")
	expectCounters(t, res, [7]uint64{300, 0, 0, 0, 300, 300, 0})
	expectExec(t, db, "MATCH (n:N) WHERE n.i > 1000 DETACH DELETE n WITH count(*) AS k MATCH (x:N) RETURN k, count(x)", nil, "300 | 300")
	// A SET does not see its own changes as new rows, and a later MATCH
	// sees them all.
	expectExec(t, db, "MATCH (n:N) SET n:M WITH count(*) AS k MATCH (x:M) RETURN k, count(x)", nil, "300 | 300")
	// Effects of the rows before reach the expressions of the rows after.
	execOK(t, db, "CREATE (:C {v: 0})", nil)
	expectExec(t, db, "UNWIND range(1, 5) AS i MATCH (c:C) SET c.v = c.v + i RETURN c.v", nil, "15", "15", "15", "15", "15")
}

func TestExecCreate(t *testing.T) {
	db := memory(t)
	res := expectExec(t, db, "CREATE (a:A:A:B {x: 1, n: null})-[r:R {w: 2}]->(b {y: a.x + 1}), (b)-[:S]->(a) RETURN labels(a), properties(a), b.y, r.w", nil,
		"[\"A\", \"B\"] | {\"x\": 1} | 2 | 2")
	expectCounters(t, res, [7]uint64{2, 0, 2, 0, 3, 2, 0})
	expectExec(t, db, "MATCH (a:A)<-[:S]-(b) RETURN b.y", nil, "2")
	// <- writes the relationship from right to left.
	expectExec(t, db, "CREATE (a:L {k: 1})<-[:T]-(b:L {k: 2}) WITH a MATCH (x)-[:T]->(y) RETURN x.k, y.k", nil, "2 | 1")
	// A self-loop through a variable repeated in the pattern.
	expectExec(t, db, "CREATE (a:Loop)-[:T]->(a) WITH a MATCH (a)-[r:T]->(a) RETURN count(r)", nil, "1")
	for _, c := range []struct {
		src  string
		code Code
	}{
		{"MATCH (n:A) CREATE (:X {owner: n})", CodeTypeMismatch},
		{"MATCH (n:A) CREATE (:X {owners: [1, n]})", CodeTypeMismatch},
		{"MATCH (n:A) CREATE (:X {m: {k: n}})", CodeTypeMismatch},
		{"MATCH (n:A) CREATE (n)-[:R {owner: n}]->(:X)", CodeTypeMismatch},
		{"UNWIND [1] AS x MATCH (n:A) CREATE (n)-[:R]->(x)", CodeTypeMismatch},
		{"WITH 1 AS x MATCH (n:A) CREATE (n)-[:R]->(x)", CodeQuerySemantic},
		{"MATCH (n:A) WITH n, 1 AS x CREATE (n)-[:R]->(m) WITH x MATCH (y) CREATE (y)-[:R]->(:Q)", ""},
		{"OPTIONAL MATCH (n:Nothing) CREATE (n)-[:R]->(:X)", CodeTypeMismatch},
		{"UNWIND [1] AS x WITH collect(x)[0] AS y RETURN y", CodeQueryUnsupported},
		{"MATCH (n:A) WITH [n] AS l UNWIND l AS x CREATE (x)-[:R]->(:X) RETURN count(*)", ""},
		{"UNWIND [1] AS x WITH x CREATE (a)-[:R]->(b) WITH a, x UNWIND [x] AS y CREATE (y)-[:R]->(a)", CodeTypeMismatch},
	} {
		before := dumpDB(t, db)
		_, err := db.Exec(bg, c.src, nil)
		if code(err) != c.code {
			t.Fatalf("%s: %v, want %q", c.src, err, c.code)
		}
		if after := dumpDB(t, db); err != nil && !slices.Equal(after, before) {
			t.Fatalf("%s failed and changed the graph", c.src)
		}
	}
}

func TestExecMerge(t *testing.T) {
	db := memory(t)
	// Each row sees what the rows before created.
	res := expectExec(t, db, "UNWIND [1, 1, 2, 1.0] AS k MERGE (n:K {k: k}) RETURN k = n.k, n.k", nil, "true | 1", "true | 1", "true | 2", "true | 1")
	expectCounters(t, res, [7]uint64{2, 0, 0, 0, 2, 2, 0})
	// Actions run in order: ON CREATE on a new node, ON MATCH on each
	// match.
	res = expectExec(t, db, "MERGE (n:M {k: 1}) ON CREATE SET n.a = 1 ON MATCH SET n.b = 1 ON CREATE SET n.a = n.a + 1, n:New RETURN n.a, n.b, labels(n)", nil, "2 | null | [\"M\", \"New\"]")
	expectCounters(t, res, [7]uint64{1, 0, 0, 0, 3, 2, 0})
	execOK(t, db, "CREATE (:M {k: 1})", nil)
	res = expectExec(t, db, "MERGE (n:M {k: 1}) ON CREATE SET n.a = 0 ON MATCH SET n.hit = coalesce(n.hit, 0) + 1 RETURN n.hit", nil, "1", "1")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 2, 0, 0})
	// MERGE of a relationship between bound nodes is idempotent.
	execOK(t, db, "CREATE (:P {name: 'a'}), (:P {name: 'b'})", nil)
	q := "MATCH (a:P {name: 'a'}), (b:P {name: 'b'}) MERGE (a)-[r:KNOWS]->(b) ON CREATE SET r.n = 1 ON MATCH SET r.n = r.n + 1 RETURN r.n"
	expectExec(t, db, q, nil, "1")
	expectExec(t, db, q, nil, "2")
	expectRows(t, db, "MATCH (:P)-[r:KNOWS]->(:P) RETURN count(r)", nil, "1")
	// Undirected: matches either direction, creates from left to right.
	expectExec(t, db, "MATCH (a:P {name: 'b'}), (b:P {name: 'a'}) MERGE (a)-[r:KNOWS]-(b) RETURN r.n", nil, "2")
	expectExec(t, db, "MATCH (a:P {name: 'b'}), (b:P {name: 'a'}) MERGE (a)-[r:LIKES]-(b) RETURN r.n", nil, "null")
	expectRows(t, db, "MATCH (a)-[:LIKES]->(b) RETURN a.name, b.name", nil, "\"b\" | \"a\"")
	// The whole pattern: when it does not match, every unbound element is
	// created, even nodes equal to existing ones.
	res = execOK(t, db, "MERGE (a:P {name: 'a'})-[r:HATES]->(b:P {name: 'b'})", nil)
	expectCounters(t, res, [7]uint64{2, 0, 1, 0, 2, 2, 0})
	expectRows(t, db, "MATCH (n:P) RETURN count(n)", nil, "4")
	res = execOK(t, db, "MERGE (a:P {name: 'a'})-[r:HATES]->(b:P {name: 'b'})", nil)
	expectCounters(t, res, [7]uint64{})
	// <- creates from right to left.
	execOK(t, db, "MERGE (a:In {k: 1})<-[:T]-(b:In {k: 2})", nil)
	expectRows(t, db, "MATCH (x:In)-[:T]->(y:In) RETURN x.k, y.k", nil, "2 | 1")
	expectExec(t, db, "MERGE (a:In {k: 1})<-[:T]-(b:In {k: 2}) RETURN count(*)", nil, "1")
	expectRows(t, db, "MATCH (x:In) RETURN count(x)", nil, "2")
	// A self-loop through a repeated variable.
	expectExec(t, db, "MERGE (a:Loop {k: 1})-[:T]->(a) RETURN count(*)", nil, "1")
	expectExec(t, db, "MERGE (a:Loop {k: 1})-[:T]->(a) RETURN count(*)", nil, "1")
	expectRows(t, db, "MATCH (a:Loop)-[r:T]->(a) RETURN count(r)", nil, "1")
	// Null cannot be matched: a literal is refused before execution, a
	// parameter before execution too, a computed value when it comes.
	execCode(t, db, "MERGE (n:P {name: null})", nil, CodeQuerySemantic)
	execCode(t, db, "MERGE (n:P {name: $x})", Params{"x": value.Null()}, CodeParamType)
	e := execCode(t, db, "MATCH (n:K) MERGE (m:P {name: n.missing})", nil, CodeInvalidArgument)
	if e.Location == nil || e.Stage != StageExecute {
		t.Fatalf("MERGE of a null value: %+v", e)
	}
	// A bound endpoint that is Null cannot get a relationship.
	execCode(t, db, "OPTIONAL MATCH (n:Nothing) MATCH (m:K {k: 2}) MERGE (n)-[:R]->(m)", nil, CodeTypeMismatch)
	execCode(t, db, "MATCH (n:K) MERGE (m:P {owner: n})", nil, CodeTypeMismatch)
	execCode(t, db, "MATCH (n:K) MERGE (n:P)", nil, CodeQuerySemantic)
	execCode(t, db, "MERGE (a)-[:R*2]->(b)", nil, CodeQuerySemantic)
}

func TestExecSetRemove(t *testing.T) {
	db := memory(t)
	execOK(t, db, "CREATE (:N {a: 1, b: 'x'})-[:R {w: 1}]->(:M {c: [1, 2]})", nil)
	res := expectExec(t, db, "MATCH (n:N) SET n.a = null, n.z = 3, n.b = 'y' RETURN properties(n)", nil, "{\"b\": \"y\", \"z\": 3}")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 3, 0, 0})
	res = expectExec(t, db, "MATCH (n:N) SET n += {b: null, q: 1} RETURN properties(n)", nil, "{\"q\": 1, \"z\": 3}")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 2, 0, 0})
	res = expectExec(t, db, "MATCH (n:N) SET n = {k: 'v', drop: null} RETURN properties(n)", nil, "{\"k\": \"v\"}")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 3, 0, 0})
	res = expectExec(t, db, "MATCH ()-[r:R]->() SET r = {} RETURN properties(r)", nil, "{}")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 1, 0, 0})
	res = expectExec(t, db, "MATCH (n:N) SET n:A:N:B, n:A RETURN labels(n)", nil, "[\"N\", \"A\", \"B\"]")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 0, 2, 0})
	res = expectExec(t, db, "MATCH (n:N) REMOVE n:A:Z, n.k, n.none RETURN labels(n), properties(n)", nil, "[\"N\", \"B\"] | {}")
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 1, 0, 1})
	// Null targets and Null maps change nothing.
	res = execOK(t, db, "OPTIONAL MATCH (n:Nothing) SET n.a = 1, n:X, n = {a: 1} REMOVE n.a, n:Y", nil)
	expectCounters(t, res, [7]uint64{})
	res = execOK(t, db, "MATCH (n:M) SET n = null, n += null", nil)
	expectCounters(t, res, [7]uint64{})
	// Values are property values: references are refused at any depth.
	before := dumpDB(t, db)
	for _, c := range []struct {
		src  string
		code Code
	}{
		{"MATCH (n:M) SET n.self = n", CodeTypeMismatch},
		{"MATCH (n:M) SET n.l = [1, n]", CodeTypeMismatch},
		{"MATCH (n:M) SET n += {m: {k: n}}", CodeTypeMismatch},
		{"MATCH (n:M) SET n = {self: n}", CodeTypeMismatch},
		{"MATCH (n:M) WITH n, n.c AS c SET n = c", CodeTypeMismatch},
		{"MATCH ()-[r]->() WITH collect(r) AS rs UNWIND rs AS x SET x:L", CodeTypeMismatch},
		{"UNWIND [1] AS x SET x.p = 1", CodeTypeMismatch},
		{"UNWIND [1] AS x REMOVE x.p", CodeTypeMismatch},
		{"MATCH (n:M) SET n = 5", CodeQuerySemantic},
		{"MATCH ()-[r]->() SET r:L", CodeQuerySemantic},
		{"MATCH (n:M) SET m.p = 1", CodeQuerySemantic},
		// An entity deleted earlier in the query.
		{"MATCH (n:M) DETACH DELETE n SET n.p = 1", CodeNotFound},
		{"MATCH (n:M) DETACH DELETE n SET n += {}", CodeNotFound},
		{"MATCH (n:M) DETACH DELETE n SET n = null", CodeNotFound},
		{"MATCH (n:M) DETACH DELETE n SET n:L", CodeNotFound},
		{"MATCH (n:M) DETACH DELETE n REMOVE n.c", CodeNotFound},
		{"MATCH ()-[r]->() DELETE r SET r = {}", CodeNotFound},
		{"MATCH (n:M) DETACH DELETE n RETURN n", CodeNotFound},
		{"MATCH (n:M) DETACH DELETE n RETURN n.c", CodeNotFound},
	} {
		e := execCode(t, db, c.src, nil, c.code)
		if c.code != CodeQuerySemantic && e.Stage != StageExecute {
			t.Fatalf("%s: stage %q", c.src, e.Stage)
		}
		if got := dumpDB(t, db); !slices.Equal(got, before) {
			t.Fatalf("%s changed the graph", c.src)
		}
	}
	expectExec(t, db, "MATCH (n:M) DETACH DELETE n RETURN id(n) > 0, count(*)", nil, "true | 1")
	if e := execCode(t, db, "UNWIND [1] AS x SET x:L", nil, CodeTypeMismatch); !strings.Contains(e.Message, "labels") {
		t.Fatalf("labels on an Int: %v", e)
	}
	// = removes what the map drops: two created, two removed.
	res = execOK(t, db, "CREATE (a:Z {x: 1, y: 2}) SET a = {x: null}", nil)
	expectCounters(t, res, [7]uint64{1, 0, 0, 0, 4, 1, 0})
	// An error of a read before a write keeps its own place.
	e := execCode(t, db, "CREATE (n:Q) DELETE n WITH n MATCH (n)-[]->(m) SET m.x = 1", nil, CodeNotFound)
	if e.Location != nil && e.Location.Column > 40 {
		t.Fatalf("the error of the MATCH placed at the SET: %+v", e.Location)
	}
}

// The rows a clause that writes holds are charged to QueryMemoryBytes
// while it holds them, and released as they go on: a chain of clauses
// holds each row once.
func TestExecMemory(t *testing.T) {
	o := options()
	o.QueryMemoryBytes = 64 << 10
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := Params{"s": value.String(strings.Repeat("x", 1000))}
	execOK(t, db, "UNWIND range(1, 40) AS i WITH i, $s AS s CREATE (a:A) SET a.x = 1 WITH a, s SET a.y = 2 MERGE (b:B) RETURN count(*)", p)
	e := execCode(t, db, "UNWIND range(1, 100) AS i WITH i, $s AS s CREATE (a:C)", p, CodeResourceLimit)
	if e.Stage != StageExecute || e.Location == nil || e.Details["limit"] != int64(64<<10) {
		t.Fatalf("the rows of a CREATE over the limit: %+v", e)
	}
	expectRows(t, db, "MATCH (n:C) RETURN count(n)", nil, "0")
}

func TestExecDelete(t *testing.T) {
	db := memory(t)
	execOK(t, db, "CREATE (a:A)-[:R]->(b:B), (a)-[:R]->(b), (b)-[:S]->(b), (c:C)", nil)
	// The relationships of the clause go first, so a node whose
	// relationships all go with it can go too.
	res := execOK(t, db, "MATCH (a:A)-[r]-() DELETE r, a", nil)
	expectCounters(t, res, [7]uint64{0, 1, 0, 2, 0, 0, 0})
	// DETACH counts a self-loop once; deleting twice counts once; Null is
	// skipped.
	res = execOK(t, db, "MATCH (b:B) OPTIONAL MATCH (x:Nothing) DETACH DELETE b, b, x", nil)
	expectCounters(t, res, [7]uint64{0, 1, 0, 1, 0, 0, 0})
	res = execOK(t, db, "MATCH (c:C), (d:C) DELETE c, d", nil)
	expectCounters(t, res, [7]uint64{0, 1, 0, 0, 0, 0, 0})
	// A relationship deleted by an earlier clause is skipped.
	execOK(t, db, "CREATE (:E)-[:R]->(:E)", nil)
	res = execOK(t, db, "MATCH ()-[r:R]->() DELETE r WITH r DELETE r", nil)
	expectCounters(t, res, [7]uint64{0, 0, 0, 1, 0, 0, 0})
	execOK(t, db, "MATCH (n:E) DELETE n", nil)
	execCode(t, db, "UNWIND [1] AS x DELETE x", nil, CodeTypeMismatch)
	execCode(t, db, "CREATE (n) DELETE 1", nil, CodeQuerySemantic)
	expectRows(t, db, "MATCH (n) RETURN count(n)", nil, "0")
	// A later clause of the query reads without the deleted entities.
	execOK(t, db, "UNWIND range(1, 3) AS i CREATE (:D {i: i})", nil)
	expectExec(t, db, "MATCH (n:D {i: 2}) DELETE n WITH count(*) AS k MATCH (m:D) RETURN k, collect(m.i)", nil, "1 | [1, 3]")
}

// A LIMIT after a clause that writes never cuts its effects; one before
// it limits the rows it changes.
func TestExecLimitEffects(t *testing.T) {
	db := memory(t)
	for _, c := range []struct {
		src  string
		p    Params
		rows int
	}{
		{"UNWIND range(1, 5) AS i CREATE (n:L) RETURN n LIMIT 0", nil, 0},
		{"UNWIND range(1, 5) AS i CREATE (n:L) RETURN i ORDER BY i LIMIT 0", nil, 0},
		{"UNWIND range(1, 5) AS i CREATE (n:L) RETURN i ORDER BY i DESC LIMIT 1", nil, 1},
		{"UNWIND range(1, 5) AS i CREATE (n:L) WITH n LIMIT $k RETURN count(*)", Params{"k": value.Int(0)}, 1},
		{"UNWIND range(1, 5) AS i CREATE (n:L) RETURN i SKIP 4 LIMIT 3", nil, 1},
	} {
		res, err := db.Exec(bg, c.src, c.p)
		if err != nil || len(res.Rows) != c.rows || res.Stats.NodesCreated != 5 {
			t.Fatalf("%s: %v, %d rows, %d created", c.src, err, len(res.Rows), res.Stats.NodesCreated)
		}
	}
	res := execOK(t, db, "MATCH (n:L) WITH n LIMIT 2 SET n.x = 1", nil)
	expectCounters(t, res, [7]uint64{0, 0, 0, 0, 2, 0, 0})
	expectRows(t, db, "MATCH (n:L) RETURN count(n), count(n.x)", nil, "25 | 2")
}

func TestExecResultMemory(t *testing.T) {
	o := options()
	o.ResultMemoryBytes = 4 << 10
	db, err := OpenMemory(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	big := strings.Repeat("x", 1000)
	e := execCode(t, db, "UNWIND range(1, 10) AS i CREATE (n:N {s: $s}) RETURN n", Params{"s": value.String(big)}, CodeResourceLimit)
	if e.Details["limit"] != int64(4<<10) || !strings.Contains(e.Message, "result memory") || e.Stage != StageExecute {
		t.Fatalf("details %v", e.Details)
	}
	expectRows(t, db, "MATCH (n) RETURN count(n)", nil, "0")
	// The same rows within the limit, and a write without rows past it.
	execOK(t, db, "UNWIND range(1, 2) AS i CREATE (n:N {s: $s}) RETURN n", Params{"s": value.String(big)})
	execOK(t, db, "UNWIND range(1, 10) AS i CREATE (n:N {s: $s})", Params{"s": value.String(big)})
	o.ResultMemoryBytes = 0
	if _, err := OpenMemory(o); code(err) != CodeInvalidArgument {
		t.Fatalf("ResultMemoryBytes 0: %v", err)
	}
}

// holdFS holds the Syncs of the log while held, so that a commit stays
// sealed and not yet durable.
type holdFS struct {
	vfs.FS
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

type holdFile struct {
	vfs.File
	fs   *holdFS
	name string
}

func (h *holdFS) Create(name string) (vfs.File, error) {
	f, err := h.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &holdFile{File: f, fs: h, name: name}, nil
}

func (h *holdFS) Open(name string) (vfs.File, error) {
	f, err := h.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &holdFile{File: f, fs: h, name: name}, nil
}

func (f *holdFile) wait() {
	if !strings.HasSuffix(f.name, ".wal") {
		return
	}
	f.fs.mu.Lock()
	gate, entered := f.fs.gate, f.fs.entered
	f.fs.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
}

func (f *holdFile) Sync() error     { f.wait(); return f.File.Sync() }
func (f *holdFile) SyncData() error { f.wait(); return f.File.SyncData() }

// A commit whose wait is canceled after the seal: COMMIT_PENDING, with
// the rows, the counters and the token in the result.
func TestExecPending(t *testing.T) {
	h := &holdFS{FS: vfs.NewMemFS()}
	db := openFS(t, h, options())
	defer db.Close()
	// The first write reserves IDs, which syncs the log too.
	execOK(t, db, "CREATE (:Warm)", nil)
	h.mu.Lock()
	h.gate, h.entered = make(chan struct{}), make(chan struct{}, 100)
	h.mu.Unlock()
	release := sync.OnceFunc(func() {
		h.mu.Lock()
		close(h.gate)
		h.gate = nil
		h.mu.Unlock()
	})
	defer release()
	cctx, cancel := context.WithCancel(bg)
	go func() { <-h.entered; cancel() }()
	res, err := db.Exec(cctx, "CREATE (n:N {k: 1}) RETURN n.k", nil)
	if code(err) != CodeCommitPending || !res.Pending || res.Token == (CommitToken{}) || texts(res)[0] != "1" || res.Stats.NodesCreated != 1 {
		t.Fatalf("a pending Exec: %v %+v", err, res)
	}
	release()
	if err := db.Flush(bg); err != nil {
		t.Fatal(err)
	}
	if st, err := db.CommitStatus(res.Token); err != nil || st != CommitDurable {
		t.Fatalf("status %v %v", st, err)
	}
	expectRows(t, db, "MATCH (n:N) RETURN n.k", nil, "1")
}

// The graph without IDs, sorted: each node by its labels and properties,
// each edge by its endpoints, type and properties.

func nodeText(labels []string, props map[string]value.Value) string {
	return "(" + strings.Join(labels, ":") + " " + propsText(props) + ")"
}

func edgeText(from, typ string, props map[string]value.Value, to string) string {
	return from + "-[" + typ + " " + propsText(props) + "]->" + to
}

func propsText(props map[string]value.Value) string {
	var s []string
	for _, k := range slices.Sorted(maps.Keys(props)) {
		s = append(s, k+": "+qv.FromValue(props[k]).String())
	}
	return "{" + strings.Join(s, ", ") + "}"
}

// dumpDB renders the graph of db as oGraph.dump does.
func dumpDB(t *testing.T, db *DB) []string {
	t.Helper()
	nodes, err := db.Exec(ctx, "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := db.Exec(ctx, "MATCH ()-[r]->() RETURN r", nil)
	if err != nil {
		t.Fatal(err)
	}
	text := map[NodeID]string{}
	var out []string
	for _, r := range nodes.Rows {
		n, _ := r[0].Node()
		text[n.ID] = nodeText(n.Labels, n.Properties)
		out = append(out, text[n.ID])
	}
	for _, r := range edges.Rows {
		e, _ := r[0].Edge()
		out = append(out, edgeText(text[e.From], e.Type, e.Properties, text[e.To]))
	}
	sort.Strings(out)
	return out
}
