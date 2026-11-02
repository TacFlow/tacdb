package graph

import (
	"context"
	"maps"
	"slices"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
)

// Verify recounts the graph that r reads and checks that the derived
// trees agree with the records (SPEC §12.1): every label entry, adjacency
// entry and degree counter is the one the records imply, every symbol has
// a name, every property belongs to a record, edges join live nodes and
// the counts of the state are the live entities. It holds the graph in
// memory and is meant for tests and the structural check.
func (g *Graph) Verify(ctx context.Context, r engine.Reader) error {
	const op = "graph.Verify"
	bad := func(format string, args ...any) error { return errs.Newf(errs.CodeCorrupt, op, format, args...) }
	named := func(k catalog.SymbolKind, id uint32) error {
		var ok bool
		var err error
		switch k {
		case catalog.KindLabel:
			_, ok, err = catalog.Name(ctx, r, catalog.LabelID(id))
		case catalog.KindProperty:
			_, ok, err = catalog.Name(ctx, r, catalog.PropertyID(id))
		default:
			_, ok, err = catalog.Name(ctx, r, catalog.EdgeTypeID(id))
		}
		if err == nil && !ok {
			err = bad("symbol %d of kind %d has no name", id, k)
		}
		return err
	}
	labels := map[string]bool{}
	nodes := map[uint64]bool{}
	adj := map[string]bool{}
	deg := map[string]uint64{}
	var edges uint64
	for _, e := range []Entity{NodeEntity, EdgeEntity} {
		var cur uint64
		err := r.Range(ctx, e.slot(), nil, nil, func(k, v []byte) error {
			kr := codec.NewKeyReader(k)
			id, err := kr.ReadUint64()
			if err != nil || id == 0 {
				return bad("%v key %x", e, k)
			}
			tag, err := kr.ReadUint8()
			if err != nil {
				return bad("%v key %x", e, k)
			}
			if tag == tagRecord && kr.Len() == 0 {
				cur = id
				if e == NodeEntity {
					rec, err := decodeNode(v)
					if err != nil {
						return err
					}
					nodes[id] = true
					for _, l := range rec.labels {
						if err := named(catalog.KindLabel, uint32(l)); err != nil {
							return err
						}
						labels[string(labelKey(l, id))] = true
					}
					return nil
				}
				rec, err := decodeEdge(v)
				if err != nil {
					return err
				}
				if !nodes[rec.from] || !nodes[rec.to] {
					return bad("edge %d joins %d and %d, not both live", id, rec.from, rec.to)
				}
				if err := named(catalog.KindEdgeType, uint32(rec.typ)); err != nil {
					return err
				}
				edges++
				adj[string(adjacencyKey(rec.from, dirOut, rec.typ, rec.to, id))] = true
				adj[string(adjacencyKey(rec.to, dirIn, rec.typ, rec.from, id))] = true
				for _, t := range []catalog.EdgeTypeID{0, rec.typ} {
					deg[string(degreeKey(rec.from, dirOut, t))]++
					deg[string(degreeKey(rec.to, dirIn, t))]++
					if rec.from == rec.to {
						deg[string(degreeKey(rec.from, dirSelf, t))]++
					}
				}
				return nil
			}
			pid, err := kr.ReadUint32()
			if tag != tagProperty || err != nil || kr.Len() != 0 || pid == 0 || id != cur {
				return bad("%v key %x without its record", e, k)
			}
			if err := named(catalog.KindProperty, pid); err != nil {
				return err
			}
			_, err = g.decode(v)
			return err
		})
		if err != nil {
			return err
		}
	}
	if st := r.State(); st.NodeCount != uint64(len(nodes)) || st.EdgeCount != edges {
		return bad("the state counts %d nodes and %d edges, the trees hold %d and %d",
			st.NodeCount, st.EdgeCount, len(nodes), edges)
	}
	same := func(slot int, lo []byte, want map[string]bool, what string) error {
		n := 0
		var hi []byte
		if lo != nil {
			hi = codec.PrefixEnd(lo)
		}
		err := r.Range(ctx, slot, lo, hi, func(k, v []byte) error {
			if !want[string(k)] || len(v) != 0 {
				return bad("%s entry %x without a record", what, k)
			}
			n++
			return nil
		})
		if err == nil && n != len(want) {
			err = bad("%d %s entries, the records imply %d", n, what, len(want))
		}
		return err
	}
	if err := same(catalog.SlotLabels, nil, labels, "label"); err != nil {
		return err
	}
	if err := same(catalog.SlotAdjacency, nil, adj, "adjacency"); err != nil {
		return err
	}
	got := map[string]uint64{}
	lo := codec.AppendKeyUint8(nil, prefixDegree)
	if err := r.Range(ctx, catalog.SlotStats, lo, codec.PrefixEnd(lo), func(k, v []byte) error {
		n, err := decodeCount(v)
		got[string(k)] = n
		return err
	}); err != nil {
		return err
	}
	if !maps.Equal(got, deg) {
		for _, k := range slices.Sorted(maps.Keys(deg)) {
			if got[k] != deg[k] {
				return bad("degree %x is %d, the edges imply %d", []byte(k), got[k], deg[k])
			}
		}
		for k := range got {
			if _, ok := deg[k]; !ok {
				return bad("degree %x is %d without edges", []byte(k), got[k])
			}
		}
	}
	return nil
}
