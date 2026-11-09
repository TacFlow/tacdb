package tacdb

import (
	"math"
	"slices"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

// The convenience calls of SPEC §31.1 answer what the equivalent queries
// answer, in their documented order and limit.
func TestConvenience(t *testing.T) {
	db := memory(t)
	var p []NodeID
	var hub NodeID
	var r1, r2 EdgeID
	setup(t, db, func(tx *Tx) {
		for i, v := range []value.Value{value.Int(1), value.Float(1), value.Int(2), value.Null(), value.Float(math.NaN())} {
			pr := map[string]value.Value{"v": v}
			if i == 3 {
				pr = nil
			}
			p = append(p, mustNode(t, tx, []string{"P"}, pr))
		}
		mustNode(t, tx, []string{"Q"}, props("v", 1))
		hub = mustNode(t, tx, nil, nil)
		e, _ := tx.CreateEdge(hub, p[0], "R", props("w", 1))
		r1 = e.ID
		e, _ = tx.CreateEdge(hub, p[1], "R", props("w", 1.0))
		r2 = e.ID
		tx.CreateEdge(p[2], hub, "S", props("w", 2))
	})
	tx, _ := db.BeginRead(bg)
	defer tx.Rollback()
	for id, want := range map[NodeID]bool{p[0]: true, hub: true, 999: false} {
		if ok, err := tx.NodeExists(id); ok != want || err != nil {
			t.Fatalf("NodeExists(%d): %v %v", id, ok, err)
		}
	}
	if ids, err := tx.NodesByLabel("P", 0); err != nil || !slices.Equal(ids, p) {
		t.Fatalf("NodesByLabel: %v %v", ids, err)
	}
	if ids, _ := tx.NodesByLabel("P", 2); !slices.Equal(ids, p[:2]) {
		t.Fatalf("NodesByLabel with a limit: %v", ids)
	}
	if ids, _ := tx.NodesByLabel("Unknown", 0); len(ids) != 0 {
		t.Fatalf("an unknown label: %v", ids)
	}
	for _, c := range []struct {
		v     value.Value
		limit int
		want  []NodeID
	}{
		{value.Int(1), 0, p[:2]}, {value.Float(1), 1, p[:1]}, {value.Int(2), 0, p[2:3]},
		{value.Null(), 0, nil}, {value.Float(math.NaN()), 0, nil}, {value.String("1"), 0, nil},
	} {
		if ids, err := tx.FindNodesByLabelProperty("P", "v", c.v, c.limit); err != nil || !slices.Equal(ids, c.want) {
			t.Fatalf("FindNodesByLabelProperty(%v): %v %v", c.v, ids, err)
		}
	}
	if ids, _ := tx.FindEdgesByTypeProperty("R", "w", value.Int(1), 0); !slices.Equal(ids, []EdgeID{r1, r2}) {
		t.Fatalf("FindEdgesByTypeProperty: %v", ids)
	}
	if ids, _ := tx.FindEdgesByTypeProperty("R", "w", value.Null(), 0); len(ids) != 0 {
		t.Fatalf("FindEdgesByTypeProperty of Null: %v", ids)
	}
	out, err := tx.OutgoingEdges(hub)
	if err != nil || len(out) != 2 || out[0].ID != r1 || out[1].To != p[1] {
		t.Fatalf("OutgoingEdges: %v %v", out, err)
	}
	if in, _ := tx.IncomingEdges(hub, "S"); len(in) != 1 || in[0].From != p[2] {
		t.Fatalf("IncomingEdges: %v", in)
	}
	if in, _ := tx.IncomingEdges(hub, "R"); len(in) != 0 {
		t.Fatalf("IncomingEdges of R: %v", in)
	}
	if _, err := tx.OutgoingEdges(999); code(err) != CodeNotFound {
		t.Fatalf("edges of a missing node: %v", err)
	}
	if _, err := tx.CreateNodes([]CreateNodeOptions{{}}); code(err) != CodeReadOnly {
		t.Fatalf("CreateNodes in a read transaction: %v", err)
	}

	// CreateNodes creates all or none.
	w, _ := db.BeginWrite(bg)
	defer w.Rollback()
	ns, err := w.CreateNodes([]CreateNodeOptions{{Labels: []string{"B", "B"}}, {Labels: []string{"B"}, Properties: props("k", 1)}})
	if err != nil || len(ns) != 2 || !slices.Equal(ns[0].Labels, []string{"B"}) {
		t.Fatalf("CreateNodes: %v %v", ns, err)
	}
	bad := []CreateNodeOptions{{Labels: []string{"B"}}, {Labels: []string{""}}}
	if _, err := w.CreateNodes(bad); err == nil {
		t.Fatal("CreateNodes with an empty label")
	}
	if ids, _ := w.NodesByLabel("B", 0); len(ids) != 2 {
		t.Fatalf("a failed CreateNodes left %d nodes", len(ids)-2)
	}
}
