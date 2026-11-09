package graph

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// ids walks s and returns the IDs it gives.
func ids(t *testing.T, s *Scan) []uint64 {
	t.Helper()
	var out []uint64
	for s.Next(ctx) {
		out = append(out, s.Item().ID)
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	return out
}

// Scans give every node, the nodes of a label and the edges of some types
// in ID order, across batches and past properties.
func TestScans(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	var all, as []uint64
	var rs, ss []uint64
	big := map[string]value.Value{}
	for i := range 40 {
		big[string(rune('a'+i%26))+string(rune('a'+i/26))] = value.Int(int64(i))
	}
	for i := range 3*cursorBatch + 7 {
		var labels []string
		if i%3 == 0 {
			labels = []string{"A"}
		}
		props := map[string]value.Value(nil)
		if i%5 == 0 {
			props = big // many property keys between two records
		}
		id := node(t, g, tx, labels, props)
		all = append(all, id)
		if i%3 == 0 {
			as = append(as, id)
		}
	}
	for i := 1; i < len(all); i++ {
		e := edge(t, g, tx, all[i-1], all[i], []string{"R", "S"}[i%2])
		if i%2 == 0 {
			rs = append(rs, e)
		} else {
			ss = append(ss, e)
		}
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	if got := ids(t, g.Nodes(ctx, r)); !slices.Equal(got, all) {
		t.Fatalf("nodes %v", got)
	}
	s, err := g.NodesByLabel(ctx, r, "A")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, s); !slices.Equal(got, as) {
		t.Fatalf("label A %v, want %v", got, as)
	}
	for _, unknown := range []string{"Z", ""} {
		s, err := g.NodesByLabel(ctx, r, unknown)
		if err != nil || len(ids(t, s)) != 0 {
			t.Fatalf("unknown label %q: %v", unknown, err)
		}
	}
	for _, c := range []struct {
		types []string
		want  []uint64
	}{{[]string{"R"}, rs}, {[]string{"S", "R", "S", "X"}, nil}, {nil, nil}, {[]string{"X"}, []uint64{}}} {
		s, err := g.EdgesByType(ctx, r, c.types)
		if err != nil {
			t.Fatal(err)
		}
		want := c.want
		if want == nil {
			want = slices.Sorted(slices.Values(append(slices.Clone(rs), ss...)))
		}
		var got []uint64
		for s.Next(ctx) {
			it := s.Item()
			got = append(got, it.ID)
			ref, err := g.EdgeEnds(ctx, r, it.ID)
			if err != nil || ref.From != it.From || ref.To != it.To {
				t.Fatalf("edge %d ends %v, scan %v", it.ID, ref, it)
			}
		}
		if s.Err() != nil || len(got) != len(want) || len(want) > 0 && !slices.Equal(got, want) {
			t.Fatalf("types %v: %v, want %v (%v)", c.types, got, want, s.Err())
		}
	}
	// Close stops a scan.
	s = g.Nodes(ctx, r)
	s.Next(ctx)
	s.Close()
	if s.Next(ctx) {
		t.Fatal("Next after Close")
	}
}

// Inside a write transaction a scan yields the entities that existed when
// it opened and still qualify when it reaches them (D-005).
func TestScanInWriteTx(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	var ns []uint64
	for range 2 * cursorBatch {
		ns = append(ns, node(t, g, tx, []string{"A"}, nil))
	}
	e1 := edge(t, g, tx, ns[0], ns[1], "R")
	e2 := edge(t, g, tx, ns[1], ns[2], "R")
	s, err := g.NodesByLabel(ctx, tx, "A")
	if err != nil {
		t.Fatal(err)
	}
	es, _ := g.EdgesByType(ctx, tx, []string{"R"})
	all := g.Nodes(ctx, tx)
	var seen []uint64
	n := 0
	for s.Next(ctx) {
		seen = append(seen, s.Item().ID)
		if n == 0 {
			// A new node with the label, a removed label further on, and a
			// deleted node further on.
			node(t, g, tx, []string{"A"}, nil)
			if err := tx.Statement("t", func(st *engine.Stmt) error { return g.RemoveLabel(ctx, st, ns[10], "A") }); err != nil {
				t.Fatal(err)
			}
			if err := tx.Statement("t", func(st *engine.Stmt) error { return g.DeleteNode(ctx, st, ns[100], false) }); err != nil {
				t.Fatal(err)
			}
			// A node of the batch already read, deleted before the scan
			// reaches it.
			if err := tx.Statement("t", func(st *engine.Stmt) error { return g.DeleteNode(ctx, st, ns[5], false) }); err != nil {
				t.Fatal(err)
			}
			if err := tx.Statement("t", func(st *engine.Stmt) error { return g.DeleteEdge(ctx, st, e2) }); err != nil {
				t.Fatal(err)
			}
			edge(t, g, tx, ns[3], ns[4], "R")
		}
		n++
	}
	if s.Err() != nil || len(seen) != len(ns)-3 || slices.Contains(seen, ns[10]) || slices.Contains(seen, ns[100]) || slices.Contains(seen, ns[5]) {
		t.Fatalf("label scan in a write transaction: %d nodes, %v", len(seen), s.Err())
	}
	if got := ids(t, all); len(got) != len(ns)-2 || slices.Contains(got, ns[100]) {
		t.Fatalf("node scan: %d nodes", len(got))
	}
	if got := ids(t, es); !slices.Equal(got, []uint64{e1}) {
		t.Fatalf("edge scan %v", got)
	}
}

// A scan whose snapshot expires stops with SNAPSHOT_EXPIRED; one whose
// transaction ended, with TX_CLOSED.
func TestScanExpires(t *testing.T) {
	o := options()
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	o.MaxReadTxAge = time.Minute
	db := openDB(t, vfs.NewMemFS(), o)
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	for range 2 * cursorBatch {
		node(t, g, tx, nil, nil)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	s := g.Nodes(ctx, r)
	s.Next(ctx)
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if s.Next(ctx) || errs.CodeOf(s.Err()) != errs.CodeSnapshotExpired {
		t.Fatalf("expired: %v", s.Err())
	}
	r2, _ := db.BeginRead(ctx)
	s = g.Nodes(ctx, r2)
	s.Next(ctx)
	r2.Close()
	if s.Next(ctx) || errs.CodeOf(s.Err()) != errs.CodeTxClosed {
		t.Fatalf("closed: %v", s.Err())
	}
}

// EdgesBetween reads the edges between two nodes, parallel ones and
// self-loops included, with or without types; DegreeOf counts edges of
// some types.
func TestEdgesBetween(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	a, b, c := node(t, g, tx, nil, nil), node(t, g, tx, nil, nil), node(t, g, tx, nil, nil)
	ab1, ab2 := edge(t, g, tx, a, b, "R"), edge(t, g, tx, a, b, "R")
	ab3 := edge(t, g, tx, a, b, "S")
	ba := edge(t, g, tx, b, a, "R")
	aa := edge(t, g, tx, a, a, "R")
	edge(t, g, tx, a, c, "R")
	between := func(from uint64, dir Direction, types []string, to uint64) []uint64 {
		cur, err := g.EdgesBetween(ctx, tx, from, dir, types, to)
		if err != nil {
			t.Fatal(err)
		}
		var out []uint64
		for cur.Next(ctx) {
			out = append(out, cur.Edge().ID)
		}
		if cur.Err() != nil {
			t.Fatal(cur.Err())
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		from  uint64
		dir   Direction
		types []string
		to    uint64
		want  []uint64
	}{
		{a, Out, []string{"R"}, b, []uint64{ab1, ab2}},
		{a, Out, nil, b, []uint64{ab1, ab2, ab3}},
		{a, In, nil, b, []uint64{ba}},
		{a, Both, []string{"R", "S"}, b, []uint64{ab1, ab2, ab3, ba}},
		{b, Both, nil, a, []uint64{ab1, ab2, ab3, ba}},
		{a, Both, nil, a, []uint64{aa}},
		{a, Out, []string{"R"}, a, []uint64{aa}},
		{a, Out, []string{"X"}, b, nil},
		{b, Out, nil, c, nil},
	} {
		if got := between(c.from, c.dir, c.types, c.to); !slices.Equal(got, c.want) {
			t.Fatalf("%d %d %v %d: %v, want %v", c.from, c.dir, c.types, c.to, got, c.want)
		}
	}
	if _, err := g.EdgesBetween(ctx, tx, a, Out, nil, 0); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("peer 0: %v", err)
	}
	for _, c := range []struct {
		dir   Direction
		types []string
		want  uint64
	}{{Out, nil, 5}, {Out, []string{"R"}, 4}, {Out, []string{"R", "S", "R", "X"}, 5}, {In, []string{"S"}, 0}, {Both, nil, 6}, {Out, []string{"X"}, 0}} {
		if d, err := g.DegreeOf(ctx, tx, a, c.dir, c.types); err != nil || d != c.want {
			t.Fatalf("degree %d %v: %d, want %d (%v)", c.dir, c.types, d, c.want, err)
		}
	}
	if l, err := g.Labels(ctx, tx, 999); errs.CodeOf(err) != errs.CodeNotFound || l != nil {
		t.Fatalf("labels of a missing node: %v", err)
	}
	if _, err := g.Properties(ctx, tx, EdgeEntity, 999); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("properties of a missing edge: %v", err)
	}
	if _, err := g.Properties(ctx, tx, NodeEntity, 999); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("properties of a missing node: %v", err)
	}
}
