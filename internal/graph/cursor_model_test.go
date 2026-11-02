package graph

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// The cursor against a brute-force expectation, over many batches, mixed
// types, self-loops and parallel edges, in every direction and filter.
func TestCursorAgainstModel(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	hub := node(t, g, tx, nil, nil)
	peers := []uint64{node(t, g, tx, nil, nil), node(t, g, tx, nil, nil), node(t, g, tx, nil, nil)}
	types := []string{"A", "B", "C"}
	type e struct {
		id, from, to uint64
		typ          string
	}
	var all []e
	rng := rand.New(rand.NewPCG(1, 2))
	for range 3*cursorBatch + 7 {
		typ := types[rng.IntN(3)]
		var from, to uint64
		switch rng.IntN(3) {
		case 0:
			from, to = hub, hub
		case 1:
			from, to = hub, peers[rng.IntN(3)]
		default:
			from, to = peers[rng.IntN(3)], hub
		}
		all = append(all, e{edge(t, g, tx, from, to, typ), from, to, typ})
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	verify(t, g, r)
	for _, dir := range []Direction{Out, In, Both} {
		for _, filter := range [][]string{nil, {"A"}, {"C", "A", "Z", "A"}, {"Z"}} {
			want := map[uint64]int{}
			for _, x := range all {
				if len(filter) > 0 && !slices.Contains(filter, x.typ) {
					continue
				}
				if (dir == Out || dir == Both) && x.from == hub {
					want[x.id]++
				}
				if (dir == In || dir == Both) && x.to == hub && !(dir == Both && x.from == hub) {
					want[x.id]++
				}
			}
			c, err := g.Edges(ctx, r, hub, dir, filter)
			if err != nil {
				t.Fatal(err)
			}
			got := map[uint64]int{}
			for c.Next(ctx) {
				ref := c.Edge()
				got[ref.ID]++
				x := all[slices.IndexFunc(all, func(x e) bool { return x.id == ref.ID })]
				if ref.From != x.from || ref.To != x.to || ref.Type != x.typ {
					t.Fatalf("dir %d %v: ref %+v, edge %+v", dir, filter, ref, x)
				}
			}
			if c.Err() != nil {
				t.Fatal(c.Err())
			}
			if len(got) != len(want) {
				t.Fatalf("dir %d %v: %d edges, want %d", dir, filter, len(got), len(want))
			}
			for id, n := range want {
				if got[id] != n {
					t.Fatalf("dir %d %v: edge %d seen %d times, want %d", dir, filter, id, got[id], n)
				}
			}
			for _, ty := range []catalog.EdgeTypeID{0, 1, 2, 3} {
				_ = ty
			}
		}
		d, err := Degree(ctx, r, hub, dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := g.Edges(ctx, r, hub, dir, nil)
		n := uint64(0)
		for c.Next(ctx) {
			n++
		}
		if d != n {
			t.Fatalf("dir %d: degree %d, cursor %d", dir, d, n)
		}
	}
	// Detach removes every edge once and leaves the trees consistent.
	w := begin(t, db)
	defer w.Rollback()
	do(t, w, func(s *engine.Stmt) error { return g.DeleteNode(ctx, s, hub, true) })
	verify(t, g, w)
	if st := w.State(); st.EdgeCount != 0 || st.NodeCount != 3 {
		t.Fatalf("after detach: %+v", st)
	}
}

// A record under ID 0 is corruption for Dump as for Verify.
func TestDumpRejectsZero(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	node(t, g, tx, nil, nil)
	do(t, tx, func(s *engine.Stmt) error {
		return s.Put(ctx, catalog.SlotNodes, recordKey(0), nodeRecord{}.encode())
	})
	if err := g.Verify(ctx, tx); err == nil {
		t.Fatal("Verify accepted node 0")
	}
	var b bytesBuffer
	err := g.Dump(ctx, tx, &b)
	if err == nil {
		t.Fatalf("Dump accepted node 0 and wrote %q", string(b))
	}
}

type bytesBuffer []byte

func (b *bytesBuffer) Write(p []byte) (int, error) { *b = append(*b, p...); return len(p), nil }
