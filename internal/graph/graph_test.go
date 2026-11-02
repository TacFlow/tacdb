package graph

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

var ctx = context.Background()

var errFail = errors.New("the operation failed")

func options() engine.Options {
	o := engine.DefaultOptions()
	o.Create = true
	o.WALSegmentBytes = 256 << 10
	o.CacheBytes = 1 << 20
	gov, err := govern.New(govern.Config{Limit: 1 << 30})
	if err != nil {
		panic(err)
	}
	o.Governor = gov
	return o
}

func newGraph() *Graph {
	return &Graph{MaxSymbols: 1000, MaxLabels: DefaultMaxLabels, MaxProperties: DefaultMaxProperties,
		Limits: value.DefaultLimits(), Symbols: catalog.NewCache(1024)}
}

func openDB(t *testing.T, fs vfs.FS, o engine.Options) *engine.DB {
	t.Helper()
	db, err := engine.Open(ctx, fs, "g.tacdb", o)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func begin(t *testing.T, db *engine.DB) *engine.WriteTx {
	t.Helper()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// do runs fn as a statement and fails the test on error.
func do(t *testing.T, tx *engine.WriteTx, fn func(s *engine.Stmt) error) {
	t.Helper()
	if err := tx.Statement("test", fn); err != nil {
		t.Fatal(err)
	}
}

func node(t *testing.T, g *Graph, tx *engine.WriteTx, labels []string, props map[string]value.Value) uint64 {
	t.Helper()
	var id uint64
	do(t, tx, func(s *engine.Stmt) (err error) {
		id, err = g.CreateNode(ctx, s, labels, props)
		return err
	})
	return id
}

func edge(t *testing.T, g *Graph, tx *engine.WriteTx, from, to uint64, typ string) uint64 {
	t.Helper()
	var id uint64
	do(t, tx, func(s *engine.Stmt) (err error) {
		id, err = g.CreateEdge(ctx, s, from, to, typ, nil)
		return err
	})
	return id
}

func dump(t *testing.T, g *Graph, r engine.Reader) string {
	t.Helper()
	var b bytes.Buffer
	if err := g.Dump(ctx, r, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func verify(t *testing.T, g *Graph, r engine.Reader) {
	t.Helper()
	if err := g.Verify(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func TestRecords(t *testing.T) {
	n := nodeRecord{labels: []catalog.LabelID{3, 1, 2}}
	b := n.encode()
	if len(b) != 8+12 || binary.LittleEndian.Uint16(b) != 2 || binary.LittleEndian.Uint32(b[4:]) != 3 {
		t.Fatalf("node record %x", b)
	}
	if got, err := decodeNode(b); err != nil || !slices.Equal(got.labels, n.labels) {
		t.Fatalf("%v, %v", got, err)
	}
	e := edgeRecord{from: 7, to: 9, typ: 4}
	if got, err := decodeEdge(e.encode()); err != nil || got != e {
		t.Fatalf("%v, %v", got, err)
	}
	set := func(b []byte, off int, v uint32) []byte {
		b = slices.Clone(b)
		binary.LittleEndian.PutUint32(b[off:], v)
		return b
	}
	ver := func(b []byte, v uint16) []byte {
		b = slices.Clone(b)
		binary.LittleEndian.PutUint16(b, v)
		return b
	}
	if _, err := decodeNode(ver(b, 3)); errs.CodeOf(err) != errs.CodeUnsupportedFormat {
		t.Fatalf("a newer node record: %v", err)
	}
	if _, err := decodeEdge(ver(e.encode(), 3)); errs.CodeOf(err) != errs.CodeUnsupportedFormat {
		t.Fatalf("a newer edge record: %v", err)
	}
	for name, bad := range map[string][]byte{
		"version 1":        ver(b, 1),
		"reserved bits":    set(b, 0, 2|1<<16),
		"short":            b[:3],
		"count too large":  set(b, 4, 4),
		"count too small":  set(b, 4, 2),
		"zero label":       set(b, 12, 0),
		"repeated label":   set(b, 12, 3),
		"no count":         b[:6],
		"trailing partial": append(slices.Clone(b), 1),
	} {
		if _, err := decodeNode(bad); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("node record %s: %v", name, err)
		}
	}
	eb := e.encode()
	for name, bad := range map[string][]byte{
		"version 1":     ver(eb, 1),
		"reserved bits": set(eb, 0, 2|1<<16),
		"reserved tail": set(eb, 24, 1),
		"zero source":   set(set(eb, 4, 0), 8, 0),
		"zero type":     set(eb, 20, 0),
		"short":         eb[:27],
		"long":          append(slices.Clone(eb), 0),
	} {
		if _, err := decodeEdge(bad); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("edge record %s: %v", name, err)
		}
	}
	if _, err := decodeAdjacency(adjacencyKey(1, 2, 1, 1, 1)); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("adjacency in direction 2: %v", err)
	}
	if _, err := decodeCount(encodeCount(0)); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a zero counter: %v", err)
	}
}

// Labels keep insertion order without repeats; properties keep Null apart
// from absence; removing what is not there is a no-op that interns
// nothing.
func TestNodeLifecycle(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	id := node(t, g, tx, []string{"A", "B", "A"}, map[string]value.Value{"x": value.Int(1), "n": value.Null()})
	n, err := g.GetNode(ctx, tx, id)
	if err != nil || !slices.Equal(n.Labels, []string{"A", "B"}) || len(n.Properties) != 2 ||
		!value.Identical(n.Properties["n"], value.Null()) {
		t.Fatalf("%+v, %v", n, err)
	}
	do(t, tx, func(s *engine.Stmt) error {
		for _, step := range []error{
			g.AddLabel(ctx, s, id, "C"),
			g.AddLabel(ctx, s, id, "A"),
			g.RemoveLabel(ctx, s, id, "B"),
			g.RemoveLabel(ctx, s, id, "Unknown"),
			g.SetProperty(ctx, s, NodeEntity, id, "x", value.Float(2)),
			g.RemoveProperty(ctx, s, NodeEntity, id, "n"),
			g.RemoveProperty(ctx, s, NodeEntity, id, "Unknown"),
		} {
			if step != nil {
				return step
			}
		}
		return nil
	})
	n, _ = g.GetNode(ctx, tx, id)
	if !slices.Equal(n.Labels, []string{"A", "C"}) || len(n.Properties) != 1 || !value.Identical(n.Properties["x"], value.Float(2)) {
		t.Fatalf("%+v", n)
	}
	if _, ok, _ := g.GetProperty(ctx, tx, NodeEntity, id, "n"); ok {
		t.Fatal("a removed property is still there")
	}
	if v, ok, err := g.GetProperty(ctx, tx, NodeEntity, id, "x"); !ok || err != nil || !value.Identical(v, value.Float(2)) {
		t.Fatalf("GetProperty: %v %v %v", v, ok, err)
	}
	if _, ok, _ := catalog.Lookup[catalog.LabelID](ctx, tx, "Unknown"); ok {
		t.Fatal("RemoveLabel interned a name")
	}
	if _, ok, _ := catalog.Lookup[catalog.PropertyID](ctx, tx, "Unknown"); ok {
		t.Fatal("RemoveProperty interned a name")
	}
	if _, ok, err := g.GetProperty(ctx, tx, NodeEntity, id, "Nope"); ok || err != nil {
		t.Fatalf("an unknown property: %v %v", ok, err)
	}
	verify(t, g, tx)
	do(t, tx, func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, id, false) })
	if _, err := g.GetNode(ctx, tx, id); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("a deleted node: %v", err)
	}
	if st := tx.State(); st.NodeCount != 0 {
		t.Fatalf("%d nodes after the delete", st.NodeCount)
	}
	verify(t, g, tx)
	if d := dump(t, g, tx); d != "" {
		t.Fatalf("an empty graph dumps %q", d)
	}
	for _, err := range []error{
		tx.Statement("t", func(s *engine.Stmt) error { return g.AddLabel(ctx, s, id, "A") }),
		tx.Statement("t", func(s *engine.Stmt) error { return g.RemoveLabel(ctx, s, id, "A") }),
		tx.Statement("t", func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, id, "x", value.Int(1)) }),
		tx.Statement("t", func(s *engine.Stmt) error { return g.RemoveProperty(ctx, s, NodeEntity, id, "x") }),
		tx.Statement("t", func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, id, true) }),
		tx.Statement("t", func(s *engine.Stmt) error { return g.SetProperty(ctx, s, EdgeEntity, 99, "x", value.Int(1)) }),
	} {
		if errs.CodeOf(err) != errs.CodeNotFound {
			t.Fatalf("an operation on a missing entity: %v", err)
		}
	}
	if _, _, err := g.GetProperty(ctx, tx, NodeEntity, id, "x"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("GetProperty of a missing node: %v", err)
	}
}

// Parallel edges are distinct; a self-loop has two adjacency entries and
// counts once in Both; DeleteNode refuses incident edges unless detach,
// which removes each edge once.
func TestEdgesAndDegrees(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	a, b := node(t, g, tx, nil, nil), node(t, g, tx, nil, nil)
	e1, e2 := edge(t, g, tx, a, b, "R"), edge(t, g, tx, a, b, "R")
	e3 := edge(t, g, tx, a, a, "S")
	e4 := edge(t, g, tx, b, a, "S")
	if e1 == e2 {
		t.Fatal("parallel edges share an ID")
	}
	r, _, _ := catalog.Lookup[catalog.EdgeTypeID](ctx, tx, "R")
	s, _, _ := catalog.Lookup[catalog.EdgeTypeID](ctx, tx, "S")
	for _, c := range []struct {
		node uint64
		dir  Direction
		typ  catalog.EdgeTypeID
		want uint64
	}{
		{a, Out, 0, 3}, {a, In, 0, 2}, {a, Both, 0, 4}, {a, Out, r, 2}, {a, Both, s, 2}, {a, In, s, 2},
		{b, Out, 0, 1}, {b, In, 0, 2}, {b, Both, 0, 3}, {b, Both, r, 2},
	} {
		if got, err := Degree(ctx, tx, c.node, c.dir, c.typ); err != nil || got != c.want {
			t.Fatalf("degree of %d dir %d type %d: %d, %v; want %d", c.node, c.dir, c.typ, got, err, c.want)
		}
	}
	scan := func(n uint64, dir Direction, types ...string) []EdgeRef {
		c, err := g.Edges(ctx, tx, n, dir, types)
		if err != nil {
			t.Fatal(err)
		}
		var out []EdgeRef
		for c.Next(ctx) {
			out = append(out, c.Edge())
		}
		if c.Err() != nil {
			t.Fatal(c.Err())
		}
		return out
	}
	both := scan(a, Both)
	if len(both) != 4 {
		t.Fatalf("Both on a: %v", both)
	}
	want := []EdgeRef{{e1, a, b, "R"}, {e2, a, b, "R"}, {e3, a, a, "S"}, {e4, b, a, "S"}}
	if !slices.Equal(both, want) {
		t.Fatalf("Both on a:\n%v\nwant\n%v", both, want)
	}
	if got := scan(a, In); !slices.Equal(got, []EdgeRef{{e3, a, a, "S"}, {e4, b, a, "S"}}) {
		t.Fatalf("In on a: %v", got)
	}
	if got := scan(a, Out, "S", "Nope", "S"); !slices.Equal(got, []EdgeRef{{e3, a, a, "S"}}) {
		t.Fatalf("Out S on a: %v", got)
	}
	if got := scan(a, Both, "Nope"); len(got) != 0 {
		t.Fatalf("an unknown type: %v", got)
	}
	if _, ok, _ := catalog.Lookup[catalog.EdgeTypeID](ctx, tx, "Nope"); ok {
		t.Fatal("Edges interned a type")
	}
	if _, err := g.Edges(ctx, tx, 99, Both, nil); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("Edges of a missing node: %v", err)
	}
	err := tx.Statement("t", func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, a, false) })
	var e *errs.Error
	if errs.CodeOf(err) != errs.CodeInvalidArgument || !errors.As(err, &e) || e.Details["edges"] != 4 {
		t.Fatalf("DeleteNode with edges: %v", err)
	}
	verify(t, g, tx)
	do(t, tx, func(s *engine.Stmt) error { return g.DeleteEdge(ctx, s, e2) })
	if got, _ := Degree(ctx, tx, b, In, r); got != 1 {
		t.Fatalf("degree after DeleteEdge: %d", got)
	}
	verify(t, g, tx)
	do(t, tx, func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, a, true) })
	verify(t, g, tx)
	if st := tx.State(); st.NodeCount != 1 || st.EdgeCount != 0 {
		t.Fatalf("after detach: %+v", st)
	}
	if got, _ := Degree(ctx, tx, b, Both, 0); got != 0 {
		t.Fatalf("b keeps degree %d", got)
	}
	if err := tx.Statement("t", func(s *engine.Stmt) error { return g.DeleteEdge(ctx, s, e1) }); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("a detached edge: %v", err)
	}
}

// A failed operation leaves nothing: no record, no symbol, no counter.
func TestFailuresLeaveNothing(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	g.MaxLabels, g.MaxProperties = 2, 2
	tx := begin(t, db)
	defer tx.Rollback()
	a := node(t, g, tx, []string{"A"}, map[string]value.Value{"p": value.Int(1)})
	before, state := dump(t, g, tx), tx.State()
	for name, c := range map[string]struct {
		fn   func(s *engine.Stmt) error
		code errs.Code
	}{
		"edge to a missing node": {func(s *engine.Stmt) error {
			_, err := g.CreateEdge(ctx, s, a, 99, "T", nil)
			return err
		}, errs.CodeNotFound},
		"invalid property value": {func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, []string{"New"}, map[string]value.Value{"q": value.String("\xff")})
			return err
		}, errs.CodeInvalidArgument},
		"too many labels": {func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, []string{"X", "Y", "Z", "X"}, nil)
			return err
		}, errs.CodeLimitExceeded},
		"third label": {func(s *engine.Stmt) error {
			if err := g.AddLabel(ctx, s, a, "B"); err != nil {
				return err
			}
			return g.AddLabel(ctx, s, a, "C")
		}, errs.CodeLimitExceeded},
		"third property": {func(s *engine.Stmt) error {
			if err := g.SetProperty(ctx, s, NodeEntity, a, "q", value.Int(2)); err != nil {
				return err
			}
			return g.SetProperty(ctx, s, NodeEntity, a, "r", value.Int(3))
		}, errs.CodeLimitExceeded},
		"too many properties": {func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, nil, map[string]value.Value{"a": {}, "b": {}, "c": {}})
			return err
		}, errs.CodeLimitExceeded},
		"bad label name": {func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, []string{"ok", ""}, nil)
			return err
		}, errs.CodeInvalidArgument},
		"bad type name": {func(s *engine.Stmt) error {
			_, err := g.CreateEdge(ctx, s, a, a, "a\x00b", nil)
			return err
		}, errs.CodeInvalidArgument},
		"late failure": {func(s *engine.Stmt) error {
			b, err := g.CreateNode(ctx, s, []string{"Late"}, nil)
			if err != nil {
				return err
			}
			if _, err := g.CreateEdge(ctx, s, a, b, "Late", nil); err != nil {
				return err
			}
			return errFail
		}, ""},
	} {
		err := tx.Statement("t", c.fn)
		if errs.CodeOf(err) != c.code || err == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after := dump(t, g, tx); after != before || tx.State() != state {
			t.Fatalf("%s left\n%s\n%+v", name, after, tx.State())
		}
		for _, n := range []string{"New", "X", "B", "Late"} {
			if _, ok, _ := catalog.Lookup[catalog.LabelID](ctx, tx, n); ok {
				t.Fatalf("%s left the label %s", name, n)
			}
		}
	}
	// At the limit, updating a property it has still works.
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, a, "q", value.Int(2)) })
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, a, "q", value.Int(3)) })
	verify(t, g, tx)
}

// A cursor reads in batches and resumes after the last key it gave: it
// yields every edge once, in key order, across many batches.
func TestCursorBatches(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	hub := node(t, g, tx, nil, nil)
	var out []uint64
	for i := range 3*cursorBatch + 5 {
		peer := node(t, g, tx, nil, nil)
		typ := []string{"A", "B", "C"}[i%3]
		out = append(out, edge(t, g, tx, hub, peer, typ))
		edge(t, g, tx, peer, hub, typ)
	}
	edge(t, g, tx, hub, hub, "B")
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	count := func(dir Direction, types ...string) (n int, ids []uint64) {
		c, err := g.Edges(ctx, r, hub, dir, types)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint64]bool{}
		for c.Next(ctx) {
			id := c.Edge().ID
			if seen[id] {
				t.Fatalf("edge %d twice", id)
			}
			seen[id] = true
			ids = append(ids, id)
			n++
		}
		if c.Err() != nil {
			t.Fatal(c.Err())
		}
		return n, ids
	}
	total := 3*cursorBatch + 5
	if n, _ := count(Both); n != 2*total+1 {
		t.Fatalf("Both: %d edges, want %d", n, 2*total+1)
	}
	// Types in the order they were interned, each group in peer order.
	nA, nC := (total+2)/3, (total+0)/3
	if n, ids := count(Out, "C", "A"); n != nA+nC || !slices.IsSorted(ids[:nA]) || !slices.IsSorted(ids[nA:]) ||
		ids[0] != out[0] || ids[nA] != out[2] {
		t.Fatalf("Out A and C: %d edges", n)
	}
	for _, dir := range []Direction{Out, In, Both} {
		n, _ := count(dir)
		d, _ := Degree(ctx, r, hub, dir, 0)
		if uint64(n) != d {
			t.Fatalf("direction %d: the cursor gives %d edges, the degree says %d", dir, n, d)
		}
	}
	// A cursor holds at most one batch, whatever the degree.
	c, _ := g.Edges(ctx, r, hub, Both, nil)
	for range cursorBatch + 2 {
		c.Next(ctx)
		if len(c.buf) >= cursorBatch {
			t.Fatalf("the cursor holds %d edges", len(c.buf))
		}
	}
	c.Close()
	if c.Next(ctx) || c.Err() != nil {
		t.Fatal("a closed cursor goes on")
	}
	c, _ = g.Edges(ctx, r, hub, Both, nil)
	r.Close()
	if c.Next(ctx) || errs.CodeOf(c.Err()) != errs.CodeTxClosed {
		t.Fatalf("a cursor of a closed transaction: %v", c.Err())
	}
}

func TestDumpValues(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	nan := math.Float64frombits(0x7ff8000000000001)
	nan32 := math.Float32frombits(0x7fc00001)
	a := node(t, g, tx, []string{"Z", "A b"}, map[string]value.Value{
		"i": value.Int(1), "f": value.Float(1), "z": value.Float(math.Copysign(0, -1)), "nan": value.Float(nan),
		"inf": value.Float(math.Inf(-1)), "s": value.String("x\"y\n"), "by": value.Bytes([]byte{0, 0xff}),
		"v": value.Vector([]float32{1.5, nan32}),
		"l": value.List(value.Null(), value.Bool(true), value.Int(-2)),
		"m": value.MapOf(map[string]value.Value{"b": value.Int(2), "a": value.Float(0.1)}),
	})
	edge(t, g, tx, a, a, "LOOP")
	got := dump(t, g, tx)
	want := `node 1 ["Z" "A b"] {"by": 0x00ff, "f": float(1), "i": 1, "inf": float(-Inf), "l": [null, true, -2], "m": {"a": float(0.1), "b": 2}, "nan": float(NaN:7ff8000000000001), "s": "x\"y\n", "v": vector[1.5 NaN:7fc00001], "z": float(-0)}
edge 1 1 -> 1 "LOOP" {}
`
	if got != want {
		t.Fatalf("dump:\n%s\nwant\n%s", got, want)
	}
}

// The same operations dump the same bytes whether or not failed and
// rolled back statements came between them.
func TestDumpIgnoresUndoneWork(t *testing.T) {
	build := func(noise bool) string {
		db := openDB(t, vfs.NewMemFS(), options())
		defer db.Close()
		g := newGraph()
		tx := begin(t, db)
		if noise {
			tx.Statement("t", func(s *engine.Stmt) error {
				id, _ := g.CreateNode(ctx, s, []string{"Ghost"}, map[string]value.Value{"g": value.Int(1)})
				g.CreateEdge(ctx, s, id, id, "GHOST", nil)
				return errFail
			})
		}
		a := node(t, g, tx, []string{"P"}, map[string]value.Value{"name": value.String("a")})
		sp, _ := tx.Savepoint()
		if noise {
			b := node(t, g, tx, []string{"Q"}, nil)
			edge(t, g, tx, a, b, "T")
			if err := tx.RollbackTo(sp); err != nil {
				t.Fatal(err)
			}
		}
		b := node(t, g, tx, []string{"Q"}, nil)
		edge(t, g, tx, a, b, "T")
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		r, _ := db.BeginRead(ctx)
		defer r.Close()
		verify(t, g, r)
		// IDs burned by undone work differ; the dump names entities by ID,
		// so compare it with the IDs renumbered in order.
		return renumber(dump(t, g, r))
	}
	if a, b := build(false), build(true); a != b {
		t.Fatalf("clean:\n%s\nwith undone work:\n%s", a, b)
	}
}

// renumber replaces node and edge IDs by their rank, for dumps of
// histories that burned different IDs.
func renumber(d string) string {
	nodes, edges := map[string]string{}, map[string]string{}
	rank := func(m map[string]string, id string) string {
		if r, ok := m[id]; ok {
			return r
		}
		m[id] = "#" + string(rune('a'+len(m)))
		return m[id]
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		f := strings.SplitN(line, " ", 3)
		if f[0] == "node" {
			f[1] = rank(nodes, f[1])
		} else {
			f[1] = rank(edges, f[1])
			g := strings.SplitN(f[2], " ", 4) // from -> to rest
			g[0], g[2] = rank(nodes, g[0]), rank(nodes, g[2])
			f[2] = strings.Join(g, " ")
		}
		out = append(out, strings.Join(f, " "))
	}
	return strings.Join(out, "\n")
}

// The graph survives a commit, a close and recovery.
func TestRecover(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openDB(t, fs, options())
	g := newGraph()
	tx := begin(t, db)
	a := node(t, g, tx, []string{"A"}, map[string]value.Value{"k": value.String("v")})
	edge(t, g, tx, a, node(t, g, tx, []string{"B"}, nil), "T")
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	before := dump(t, g, r)
	r.Close()
	db.Close()
	db = openDB(t, fs, options())
	defer db.Close()
	r, _ = db.BeginRead(ctx)
	defer r.Close()
	g = newGraph()
	if after := dump(t, g, r); after != before {
		t.Fatalf("before:\n%s\nafter:\n%s", before, after)
	}
	verify(t, g, r)
}

// Verify finds each kind of damage to the derived trees, and operations
// that meet broken bookkeeping fail with CORRUPT instead of going on.
func TestVerifyFindsDamage(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	a := node(t, g, tx, []string{"A"}, map[string]value.Value{"p": value.Int(1)})
	b := node(t, g, tx, nil, nil)
	e := edge(t, g, tx, a, b, "T")
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	l, _, _ := catalog.Lookup[catalog.LabelID](ctx, db0(t, db), "A")
	typ := catalog.EdgeTypeID(1)
	for name, damage := range map[string]func(s *engine.Stmt) error{
		"extra label entry":   func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotLabels, labelKey(l, b), nil) },
		"missing label entry": func(s *engine.Stmt) error { _, err := s.Delete(ctx, catalog.SlotLabels, labelKey(l, a)); return err },
		"missing adjacency": func(s *engine.Stmt) error {
			_, err := s.Delete(ctx, catalog.SlotAdjacency, adjacencyKey(b, dirIn, typ, a, e))
			return err
		},
		"extra adjacency": func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotAdjacency, adjacencyKey(b, dirOut, typ, a, e), nil)
		},
		"degree off": func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotStats, degreeKey(a, dirOut, 0), encodeCount(2))
		},
		"stray degree": func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotStats, degreeKey(b, dirSelf, 0), encodeCount(1))
		},
		"count off":       func(s *engine.Stmt) error { return s.AddCounts(1, 0) },
		"edge count off":  func(s *engine.Stmt) error { return s.AddCounts(0, -1) },
		"orphan property": func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotNodes, propertyKey(77, 1), []byte{0}) },
		"edge to a dead node": func(s *engine.Stmt) error {
			if _, err := s.Delete(ctx, catalog.SlotNodes, recordKey(b)); err != nil {
				return err
			}
			return s.AddCounts(-1, 0)
		},
		"nameless symbol": func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotNodes, propertyKey(a, 9), []byte{0})
		},
		"bad value": func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotNodes, propertyKey(a, 1), []byte{0x09}) },
	} {
		tx := begin(t, db)
		do(t, tx, damage)
		if err := g.Verify(ctx, tx); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("%s: Verify says %v", name, err)
		}
		tx.Rollback()
	}
	broken := func(name string, damage, op func(s *engine.Stmt) error) {
		tx := begin(t, db)
		defer tx.Rollback()
		do(t, tx, damage)
		if err := tx.Statement("t", op); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("%s: %v", name, err)
		}
	}
	broken("delete with a missing adjacency entry",
		func(s *engine.Stmt) error {
			_, err := s.Delete(ctx, catalog.SlotAdjacency, adjacencyKey(b, dirIn, typ, a, e))
			return err
		},
		func(s *engine.Stmt) error { return g.DeleteEdge(ctx, s, e) })
	broken("delete with a missing degree",
		func(s *engine.Stmt) error {
			_, err := s.Delete(ctx, catalog.SlotStats, degreeKey(b, dirIn, typ))
			return err
		},
		func(s *engine.Stmt) error { return g.DeleteEdge(ctx, s, e) })
	broken("detach of an edge without a record",
		func(s *engine.Stmt) error { _, err := s.Delete(ctx, catalog.SlotEdges, recordKey(e)); return err },
		func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, a, true) })
	broken("delete of a node with stray degrees",
		func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotStats, degreeKey(b, dirSelf, 0), encodeCount(1))
		},
		func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, b, true) })
	broken("remove a label without its entry",
		func(s *engine.Stmt) error { _, err := s.Delete(ctx, catalog.SlotLabels, labelKey(l, a)); return err },
		func(s *engine.Stmt) error { return g.RemoveLabel(ctx, s, a, "A") })
	broken("delete a node with a label without its entry",
		func(s *engine.Stmt) error { _, err := s.Delete(ctx, catalog.SlotLabels, labelKey(l, a)); return err },
		func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, a, true) })
	broken("more self-loops than edges",
		func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotStats, degreeKey(a, dirSelf, 0), encodeCount(1))
		},
		func(s *engine.Stmt) error { _, err := Degree(ctx, s, a, Both, 0); return err })
	broken("read a nameless property",
		func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotNodes, propertyKey(a, 9), []byte{0}) },
		func(s *engine.Stmt) error { _, err := g.GetNode(ctx, s, a); return err })
}

// db0 opens a read transaction for a lookup; it is closed with the test.
func db0(t *testing.T, db *engine.DB) engine.Reader {
	r, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// A cursor holds no snapshot operation between batches, and a snapshot
// that expires while it walks stops it at the next edge, even inside a
// batch already read.
func TestCursorExpires(t *testing.T) {
	o := options()
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	o.MaxReadTxAge = time.Minute
	db := openDB(t, vfs.NewMemFS(), o)
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	hub := node(t, g, tx, nil, nil)
	for range 2 * cursorBatch {
		edge(t, g, tx, hub, hub, "L")
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	c, err := g.Edges(ctx, r, hub, Out, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Next(ctx)
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	n := 1
	for c.Next(ctx) {
		n++
	}
	if n != 1 || errs.CodeOf(c.Err()) != errs.CodeSnapshotExpired {
		t.Fatalf("%d edges, then %v; want 1, then SNAPSHOT_EXPIRED", n, c.Err())
	}
}

// REV-18: the record and the properties of an entity are one contiguous
// run of keys, whatever the order of the writes.
func TestColocated(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	var ids []uint64
	for range 50 {
		ids = append(ids, node(t, g, tx, []string{"N"}, map[string]value.Value{"a": value.Int(1)}))
	}
	for i, id := range slices.Backward(ids) {
		do(t, tx, func(s *engine.Stmt) error {
			return g.SetProperty(ctx, s, NodeEntity, id, "p"+strconv.Itoa(i%7), value.String(strings.Repeat("v", 200)))
		})
	}
	var run []uint64
	err := tx.Range(ctx, catalog.SlotNodes, nil, nil, func(k, _ []byte) error {
		id := binary.BigEndian.Uint64(k)
		if len(run) == 0 || run[len(run)-1] != id {
			run = append(run, id)
		}
		return nil
	})
	if err != nil || !slices.Equal(run, ids) {
		t.Fatalf("entities in key order %v, want each once in ID order %v: %v", run, ids, err)
	}
}
