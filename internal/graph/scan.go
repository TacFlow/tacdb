package graph

import (
	"context"
	"slices"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// scanKeys bounds the keys one batch of a scan visits, records and
// properties together, so that a batch stays one short operation of its
// transaction.
const scanKeys = 1024

// Scan walks nodes or edges in ID order: every node, the nodes of a
// label, or the edges of some types. Like EdgeCursor (D-005) it reads in
// batches, each one operation of its transaction that resumes after the
// last key it saw, checks the transaction before each entity, and inside
// a write transaction yields the entities that existed when it was
// opened and still qualify when it reaches them. A scan has one user at a
// time.
type Scan struct {
	r      engine.Reader
	slot   int
	label  catalog.LabelID      // a label scan; 0 otherwise
	types  []catalog.EdgeTypeID // an edge scan of these types; nil for every type
	edges  bool
	next   []byte // where the next batch starts
	hi     []byte
	buf    []ScanItem
	bound  uint64 // in a write transaction, the first ID handed out after the open
	cur    ScanItem
	err    error
	done   bool
	end    bool // the last batch was read
	writer bool
}

// ScanItem is an entity found by a scan: a node, or an edge with its
// endpoints and type.
type ScanItem struct {
	ID       uint64
	From, To uint64
	Type     catalog.EdgeTypeID
}

func newScan(r engine.Reader, slot int, kind int) *Scan {
	s := &Scan{r: r, slot: slot}
	if w, ok := r.(interface{ IDBound(kind int) uint64 }); ok {
		s.bound, s.writer = w.IDBound(kind), true
	}
	return s
}

// Nodes returns a scan of every node.
func (g *Graph) Nodes(ctx context.Context, r engine.Reader) *Scan {
	return newScan(r, catalog.SlotNodes, engine.KindNode)
}

// NodesByLabel returns a scan of the nodes of label. An unknown label
// matches nothing and is not interned.
func (g *Graph) NodesByLabel(ctx context.Context, r engine.Reader, label string) (*Scan, error) {
	s := newScan(r, catalog.SlotLabels, engine.KindNode)
	id, ok, err := catalog.CachedLookup[catalog.LabelID](ctx, g.Symbols, r, label)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.done = true
		return s, nil
	}
	s.label = id
	s.next = codec.AppendKeyUint32(nil, uint32(id))
	s.hi = codec.PrefixEnd(s.next)
	return s, nil
}

// EdgesByType returns a scan of the edges of one of types, or of every
// edge when types is empty. Unknown types match nothing.
func (g *Graph) EdgesByType(ctx context.Context, r engine.Reader, types []string) (*Scan, error) {
	s := newScan(r, catalog.SlotEdges, engine.KindEdge)
	s.edges = true
	if len(types) > 0 {
		s.types = []catalog.EdgeTypeID{}
		for _, t := range types {
			id, ok, err := catalog.CachedLookup[catalog.EdgeTypeID](ctx, g.Symbols, r, t)
			if err != nil {
				return nil, err
			}
			if ok && !slices.Contains(s.types, id) {
				s.types = append(s.types, id)
			}
		}
		if len(s.types) == 0 {
			s.done = true
		}
	}
	return s, nil
}

// Next moves to the next entity and reports whether there is one; at the
// end or on error it returns false, and Err tells which.
func (s *Scan) Next(ctx context.Context) bool {
	for {
		if s.done || s.err != nil {
			return false
		}
		if s.err = s.r.Check(); s.err != nil {
			return false
		}
		if len(s.buf) == 0 {
			if s.end {
				s.done = true
				return false
			}
			if s.err = s.fill(ctx); s.err != nil {
				return false
			}
			continue
		}
		s.cur, s.buf = s.buf[0], s.buf[1:]
		if !s.writer {
			return true
		}
		// A write transaction may have changed it since its batch.
		ok, err := s.still(ctx, s.cur)
		if s.err = err; err != nil {
			return false
		}
		if ok {
			return true
		}
	}
}

// still reports whether an entity of a batch still qualifies.
func (s *Scan) still(ctx context.Context, it ScanItem) (bool, error) {
	slot := catalog.SlotNodes
	if s.edges {
		slot = catalog.SlotEdges
	}
	b, ok, err := s.r.Get(ctx, slot, recordKey(it.ID))
	if err != nil || !ok || s.label == 0 {
		return ok, err
	}
	rec, err := decodeNode(b)
	return err == nil && rec.has(s.label), err
}

// Item returns the current entity.
func (s *Scan) Item() ScanItem { return s.cur }

// Err returns the error that ended the walk, if any.
func (s *Scan) Err() error { return s.err }

// Close ends the scan.
func (s *Scan) Close() { s.done, s.buf = true, nil }

// fill reads the next batch.
func (s *Scan) fill(ctx context.Context) error {
	found, keys := 0, 0
	var last []byte
	err := s.r.Range(ctx, s.slot, s.next, s.hi, func(k, v []byte) error {
		if found == cursorBatch || keys == scanKeys {
			return errStop
		}
		keys++
		last = append(last[:0], k...)
		it, ok, err := s.decode(k, v)
		if err != nil || !ok {
			return err
		}
		if s.bound != 0 && it.ID >= s.bound {
			return nil // created after the scan opened
		}
		found++
		s.buf = append(s.buf, it)
		return nil
	})
	switch {
	case err == errStop:
		s.next = append(last, 0) // the least key after last
	case err != nil:
		return err
	default:
		s.end = true
	}
	return nil
}

// decode reads one key of the scanned tree; ok is false for a key that is
// not an entity of the scan.
func (s *Scan) decode(k, v []byte) (ScanItem, bool, error) {
	kr := codec.NewKeyReader(k)
	if s.label != 0 {
		l, err := kr.ReadUint32()
		id, err2 := kr.ReadUint64()
		if err != nil || err2 != nil || kr.Len() != 0 || id == 0 || catalog.LabelID(l) != s.label {
			return ScanItem{}, false, errs.Newf(errs.CodeCorrupt, "graph", "label key %x", k)
		}
		return ScanItem{ID: id}, true, nil
	}
	id, err := kr.ReadUint64()
	if err != nil || id == 0 {
		return ScanItem{}, false, errs.Newf(errs.CodeCorrupt, "graph", "entity key %x", k)
	}
	tag, err := kr.ReadUint8()
	if err != nil {
		return ScanItem{}, false, errs.Newf(errs.CodeCorrupt, "graph", "entity key %x", k)
	}
	if tag != tagRecord || kr.Len() != 0 {
		return ScanItem{}, false, nil // a property
	}
	if !s.edges {
		return ScanItem{ID: id}, true, nil
	}
	rec, err := decodeEdge(v)
	if err != nil {
		return ScanItem{}, false, err
	}
	if s.types != nil && !slices.Contains(s.types, rec.typ) {
		return ScanItem{}, false, nil
	}
	return ScanItem{ID: id, From: rec.from, To: rec.to, Type: rec.typ}, true, nil
}

// LabelID returns the ID of a label, and false when no node ever had it.
func (g *Graph) LabelID(ctx context.Context, r engine.Reader, label string) (catalog.LabelID, bool, error) {
	return catalog.CachedLookup[catalog.LabelID](ctx, g.Symbols, r, label)
}

// HasLabels reports whether node id has every label of ls.
func (g *Graph) HasLabels(ctx context.Context, r engine.Reader, id uint64, ls []catalog.LabelID) (bool, error) {
	rec, err := g.node(ctx, r, "tacdb.Query", id)
	if err != nil {
		return false, err
	}
	for _, l := range ls {
		if !rec.has(l) {
			return false, nil
		}
	}
	return true, nil
}

// Labels returns the labels of node id in insertion order.
func (g *Graph) Labels(ctx context.Context, r engine.Reader, id uint64) ([]string, error) {
	rec, err := g.node(ctx, r, "tacdb.Query", id)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rec.labels))
	for i, l := range rec.labels {
		if out[i], err = name(ctx, g, r, l); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EdgeEnds returns the endpoints and the type of edge id.
func (g *Graph) EdgeEnds(ctx context.Context, r engine.Reader, id uint64) (EdgeRef, error) {
	rec, err := g.edge(ctx, r, "tacdb.Query", id)
	if err != nil {
		return EdgeRef{}, err
	}
	t, err := name(ctx, g, r, rec.typ)
	return EdgeRef{ID: id, From: rec.from, To: rec.to, Type: t}, err
}

// TypeName returns the name of an edge type.
func (g *Graph) TypeName(ctx context.Context, r engine.Reader, id catalog.EdgeTypeID) (string, error) {
	return name(ctx, g, r, id)
}

// Properties returns every property of an entity, by name.
func (g *Graph) Properties(ctx context.Context, r engine.Reader, e Entity, id uint64) (map[string]value.Value, error) {
	if e == EdgeEntity {
		if _, err := g.edge(ctx, r, "tacdb.Query", id); err != nil {
			return nil, err
		}
	} else if _, err := g.node(ctx, r, "tacdb.Query", id); err != nil {
		return nil, err
	}
	return g.properties(ctx, r, e, id)
}

// DegreeOf returns the number of edges of node in dir with one of types,
// or of any type when types is empty, from the degree counters. Unknown
// types count nothing.
func (g *Graph) DegreeOf(ctx context.Context, r engine.Reader, node uint64, dir Direction, types []string) (uint64, error) {
	if len(types) == 0 {
		return Degree(ctx, r, node, dir, 0)
	}
	var seen []catalog.EdgeTypeID
	var n uint64
	for _, t := range types {
		id, ok, err := catalog.CachedLookup[catalog.EdgeTypeID](ctx, g.Symbols, r, t)
		if err != nil {
			return 0, err
		}
		if !ok || slices.Contains(seen, id) {
			continue
		}
		seen = append(seen, id)
		d, err := Degree(ctx, r, node, dir, id)
		if err != nil {
			return 0, err
		}
		n += d
	}
	return n, nil
}
