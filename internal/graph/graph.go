package graph

import (
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"slices"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/value"
)

// Defaults of the limits on entities (SPEC §24.1).
const (
	DefaultMaxLabels     = 64
	DefaultMaxProperties = 1024
)

// Graph holds the limits of a handle and its caches. Its methods write
// only inside an engine statement, keeping the indexes of every change in
// the same statement, and read any engine.Reader.
type Graph struct {
	MaxSymbols       uint32 // MaxSymbolsPerNamespace
	MaxLabels        int    // labels of a node
	MaxProperties    int    // properties of a node or an edge
	Limits           value.Limits
	IndexPrefixBytes int // IndexKeyPrefixBytes, for indexes created by this handle
	Symbols          *catalog.Cache
	Indexes          *index.Cache
}

// Entity says whether an ID names a node or an edge.
type Entity uint8

// Entities.
const (
	NodeEntity Entity = iota
	EdgeEntity
)

func (e Entity) slot() int {
	if e == EdgeEntity {
		return catalog.SlotEdges
	}
	return catalog.SlotNodes
}

func (e Entity) index() index.Entity {
	if e == EdgeEntity {
		return index.Edges
	}
	return index.Nodes
}

func (e Entity) String() string {
	if e == EdgeEntity {
		return "edge"
	}
	return "node"
}

// Node is a materialized node; it belongs to the caller.
type Node struct {
	ID         uint64
	Labels     []string // in insertion order
	Properties map[string]value.Value
}

// Edge is a materialized edge; it belongs to the caller.
type Edge struct {
	ID         uint64
	From, To   uint64
	Type       string
	Properties map[string]value.Value
}

var errStop = errors.New("stop")

func notFound(op string, e Entity, id uint64) error {
	return errs.Newf(errs.CodeNotFound, op, "%v %d does not exist", e, id)
}

func (g *Graph) node(ctx context.Context, r engine.Reader, op string, id uint64) (nodeRecord, error) {
	b, ok, err := r.Get(ctx, catalog.SlotNodes, recordKey(id))
	if err != nil {
		return nodeRecord{}, err
	}
	if !ok {
		return nodeRecord{}, notFound(op, NodeEntity, id)
	}
	return decodeNode(b)
}

func (g *Graph) edge(ctx context.Context, r engine.Reader, op string, id uint64) (edgeRecord, error) {
	b, ok, err := r.Get(ctx, catalog.SlotEdges, recordKey(id))
	if err != nil {
		return edgeRecord{}, err
	}
	if !ok {
		return edgeRecord{}, notFound(op, EdgeEntity, id)
	}
	return decodeEdge(b)
}

// scopes returns the index scopes of an entity, which must exist: the
// labels of a node, the type of an edge.
func (g *Graph) scopes(ctx context.Context, r engine.Reader, op string, e Entity, id uint64) ([]uint32, error) {
	if e == EdgeEntity {
		rec, err := g.edge(ctx, r, op, id)
		return []uint32{uint32(rec.typ)}, err
	}
	rec, err := g.node(ctx, r, op, id)
	return labelScopes(rec.labels), err
}

func labelScopes(ls []catalog.LabelID) []uint32 {
	out := make([]uint32, len(ls))
	for i, l := range ls {
		out[i] = uint32(l)
	}
	return out
}

// indexed returns the maintained indexes over property p of an entity of
// e in one of scopes.
func indexed(set *index.Set, e Entity, scopes []uint32, p catalog.PropertyID) []*index.Index {
	var out []*index.Index
	for _, sc := range scopes {
		for _, ix := range set.Scoped(e.index(), sc) {
			if ix.Property == p {
				out = append(out, ix)
			}
		}
	}
	return out
}

// name returns the name of a stored symbol; a missing one is corruption.
func name[T catalog.ID](ctx context.Context, g *Graph, r engine.Reader, id T) (string, error) {
	n, ok, err := catalog.CachedName(ctx, g.Symbols, r, id)
	if err == nil && !ok {
		err = errs.Newf(errs.CodeCorrupt, "graph", "symbol %d has no name", id)
	}
	return n, err
}

func (g *Graph) decode(b []byte) (value.Value, error) {
	return codec.DecodeValue(b, codec.DecodeOptions{Limits: g.Limits})
}

// stored decodes a stored value for maintenance, with the maxima of the
// format: a value written under larger limits stays removable.
func stored(b []byte) (value.Value, error) {
	return codec.DecodeValue(b, codec.DecodeOptions{Limits: index.StoredLimits})
}

// properties reads the properties of an entity, by name.
func (g *Graph) properties(ctx context.Context, r engine.Reader, e Entity, id uint64) (map[string]value.Value, error) {
	props := map[string]value.Value{}
	lo, hi := propertyRange(id)
	err := r.Range(ctx, e.slot(), lo, hi, func(k, v []byte) error {
		kr := codec.NewKeyReader(k[len(lo):])
		pid, err := kr.ReadUint32()
		if err != nil || kr.Len() != 0 || pid == 0 {
			return errs.Newf(errs.CodeCorrupt, "graph", "property key %x", k)
		}
		n, err := name(ctx, g, r, catalog.PropertyID(pid))
		if err != nil {
			return err
		}
		if props[n], err = g.decode(v); err != nil {
			return err
		}
		return nil
	})
	return props, err
}

// GetNode materializes node id.
func (g *Graph) GetNode(ctx context.Context, r engine.Reader, id uint64) (Node, error) {
	rec, err := g.node(ctx, r, "tacdb.GetNode", id)
	if err != nil {
		return Node{}, err
	}
	n := Node{ID: id, Labels: make([]string, len(rec.labels))}
	for i, l := range rec.labels {
		if n.Labels[i], err = name(ctx, g, r, l); err != nil {
			return Node{}, err
		}
	}
	n.Properties, err = g.properties(ctx, r, NodeEntity, id)
	return n, err
}

// GetEdge materializes edge id.
func (g *Graph) GetEdge(ctx context.Context, r engine.Reader, id uint64) (Edge, error) {
	rec, err := g.edge(ctx, r, "tacdb.GetEdge", id)
	if err != nil {
		return Edge{}, err
	}
	e := Edge{ID: id, From: rec.from, To: rec.to}
	if e.Type, err = name(ctx, g, r, rec.typ); err != nil {
		return Edge{}, err
	}
	e.Properties, err = g.properties(ctx, r, EdgeEntity, id)
	return e, err
}

// GetProperty reads one property of an entity, which must exist. An
// unknown name is absent and is not interned.
func (g *Graph) GetProperty(ctx context.Context, r engine.Reader, e Entity, id uint64, key string) (value.Value, bool, error) {
	if _, err := g.scopes(ctx, r, "tacdb.GetProperty", e, id); err != nil {
		return value.Value{}, false, err
	}
	pid, ok, err := catalog.CachedLookup[catalog.PropertyID](ctx, g.Symbols, r, key)
	if err != nil || !ok {
		return value.Value{}, false, err
	}
	b, ok, err := r.Get(ctx, e.slot(), propertyKey(id, pid))
	if err != nil || !ok {
		return value.Value{}, false, err
	}
	v, err := g.decode(b)
	return v, err == nil, err
}

// encoded is a property ready to be written.
type encoded struct {
	name string
	v    value.Value
	b    []byte
}

// encodeProps validates and copies properties, in name order.
func (g *Graph) encodeProps(op string, props map[string]value.Value) ([]encoded, error) {
	if len(props) > g.MaxProperties {
		return nil, errs.Newf(errs.CodeLimitExceeded, op, "%d properties", len(props)).
			With("limit", int64(g.MaxProperties))
	}
	out := make([]encoded, 0, len(props))
	for _, n := range slices.Sorted(maps.Keys(props)) {
		p, err := g.encodeProp(op, n, props[n])
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (g *Graph) encodeProp(op, n string, v value.Value) (encoded, error) {
	if err := catalog.ValidName(op, n); err != nil {
		return encoded{}, err
	}
	b, err := codec.AppendValue(nil, v, g.Limits)
	return encoded{n, v, b}, err
}

// putProps interns the names and writes the properties of a new entity,
// with their index entries.
func (g *Graph) putProps(ctx context.Context, s *engine.Stmt, e Entity, id uint64, scopes []uint32, props []encoded) error {
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return err
	}
	for _, p := range props {
		pid, err := catalog.Intern[catalog.PropertyID](ctx, s, p.name, g.MaxSymbols)
		if err != nil {
			return err
		}
		if err := s.Put(ctx, e.slot(), propertyKey(id, pid), p.b); err != nil {
			return err
		}
		for _, ix := range indexed(set, e, scopes, pid) {
			if err := index.Insert(ctx, s, ix, id, p.v); err != nil {
				return err
			}
		}
	}
	return nil
}

// labelIDs interns labels, without repeats, in the order given.
func (g *Graph) labelIDs(ctx context.Context, s *engine.Stmt, op string, labels []string) ([]catalog.LabelID, error) {
	var ids []catalog.LabelID
	for _, l := range labels {
		id, err := catalog.Intern[catalog.LabelID](ctx, s, l, g.MaxSymbols)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > g.MaxLabels {
		return nil, errs.Newf(errs.CodeLimitExceeded, op, "%d labels", len(ids)).With("limit", int64(g.MaxLabels))
	}
	return ids, nil
}

// CreateNode creates a node with labels, repeats ignored, and properties.
func (g *Graph) CreateNode(ctx context.Context, s *engine.Stmt, labels []string, props map[string]value.Value) (uint64, error) {
	const op = "tacdb.CreateNode"
	enc, err := g.encodeProps(op, props)
	if err != nil {
		return 0, err
	}
	ls, err := g.labelIDs(ctx, s, op, labels)
	if err != nil {
		return 0, err
	}
	id, err := s.NextID(ctx, engine.KindNode)
	if err != nil {
		return 0, err
	}
	if err := s.Put(ctx, catalog.SlotNodes, recordKey(id), nodeRecord{ls}.encode()); err != nil {
		return 0, err
	}
	for _, l := range ls {
		if err := s.Put(ctx, catalog.SlotLabels, labelKey(l, id), nil); err != nil {
			return 0, err
		}
	}
	if err := g.putProps(ctx, s, NodeEntity, id, labelScopes(ls), enc); err != nil {
		return 0, err
	}
	return id, s.AddCounts(1, 0)
}

// AddLabel adds label to node id; a label it has is a no-op.
func (g *Graph) AddLabel(ctx context.Context, s *engine.Stmt, id uint64, label string) error {
	const op = "tacdb.AddLabel"
	rec, err := g.node(ctx, s, op, id)
	if err != nil {
		return err
	}
	l, err := catalog.Intern[catalog.LabelID](ctx, s, label, g.MaxSymbols)
	if err != nil || rec.has(l) {
		return err
	}
	if len(rec.labels) >= g.MaxLabels {
		return errs.Newf(errs.CodeLimitExceeded, op, "%d labels", len(rec.labels)+1).With("limit", int64(g.MaxLabels))
	}
	rec.labels = append(rec.labels, l)
	if err := s.Put(ctx, catalog.SlotNodes, recordKey(id), rec.encode()); err != nil {
		return err
	}
	if err := s.Put(ctx, catalog.SlotLabels, labelKey(l, id), nil); err != nil {
		return err
	}
	return g.relabel(ctx, s, id, l, index.Insert)
}

// relabel inserts or removes the entries of node id in the indexes of
// label l, for the properties it has.
func (g *Graph) relabel(ctx context.Context, s *engine.Stmt, id uint64, l catalog.LabelID,
	change func(context.Context, *engine.Stmt, *index.Index, uint64, value.Value) error) error {
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return err
	}
	for _, ix := range set.Scoped(index.Nodes, uint32(l)) {
		b, ok, err := s.Get(ctx, catalog.SlotNodes, propertyKey(id, ix.Property))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		v, err := stored(b)
		if err != nil {
			return err
		}
		if err := change(ctx, s, ix, id, v); err != nil {
			return err
		}
	}
	return nil
}

// RemoveLabel removes label from node id; a label it lacks is a no-op.
func (g *Graph) RemoveLabel(ctx context.Context, s *engine.Stmt, id uint64, label string) error {
	rec, err := g.node(ctx, s, "tacdb.RemoveLabel", id)
	if err != nil {
		return err
	}
	l, ok, err := catalog.Lookup[catalog.LabelID](ctx, s, label)
	if err != nil || !ok || !rec.has(l) {
		return err
	}
	if err := g.relabel(ctx, s, id, l, index.Remove); err != nil {
		return err
	}
	rec.labels = slices.DeleteFunc(rec.labels, func(x catalog.LabelID) bool { return x == l })
	if err := s.Put(ctx, catalog.SlotNodes, recordKey(id), rec.encode()); err != nil {
		return err
	}
	found, err := s.Delete(ctx, catalog.SlotLabels, labelKey(l, id))
	if err == nil && !found {
		err = errs.Newf(errs.CodeCorrupt, "tacdb.RemoveLabel", "node %d has label %d without its entry", id, l)
	}
	return err
}

// SetProperty writes one property of an entity; Null is a value.
func (g *Graph) SetProperty(ctx context.Context, s *engine.Stmt, e Entity, id uint64, key string, v value.Value) error {
	const op = "tacdb.SetProperty"
	scopes, err := g.scopes(ctx, s, op, e, id)
	if err != nil {
		return err
	}
	p, err := g.encodeProp(op, key, v)
	if err != nil {
		return err
	}
	pid, err := catalog.Intern[catalog.PropertyID](ctx, s, key, g.MaxSymbols)
	if err != nil {
		return err
	}
	k := propertyKey(id, pid)
	old, had, err := s.Get(ctx, e.slot(), k)
	if err != nil {
		return err
	}
	if !had {
		n := 0
		lo, hi := propertyRange(id)
		err := s.Range(ctx, e.slot(), lo, hi, func(_, _ []byte) error {
			if n++; n >= g.MaxProperties {
				return errStop
			}
			return nil
		})
		if err != nil && err != errStop {
			return err
		}
		if n >= g.MaxProperties {
			return errs.Newf(errs.CodeLimitExceeded, op, "%d properties", n+1).With("limit", int64(g.MaxProperties))
		}
	}
	if err := s.Put(ctx, e.slot(), k, p.b); err != nil {
		return err
	}
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return err
	}
	for _, ix := range indexed(set, e, scopes, pid) {
		if had {
			w, err := stored(old)
			if err != nil {
				return err
			}
			if err := index.Remove(ctx, s, ix, id, w); err != nil {
				return err
			}
		}
		if err := index.Insert(ctx, s, ix, id, v); err != nil {
			return err
		}
	}
	return nil
}

// RemoveProperty removes one property of an entity; an absent one is a
// no-op.
func (g *Graph) RemoveProperty(ctx context.Context, s *engine.Stmt, e Entity, id uint64, key string) error {
	scopes, err := g.scopes(ctx, s, "tacdb.RemoveProperty", e, id)
	if err != nil {
		return err
	}
	pid, ok, err := catalog.Lookup[catalog.PropertyID](ctx, s, key)
	if err != nil || !ok {
		return err
	}
	return g.deleteProp(ctx, s, e, id, scopes, propertyKey(id, pid), pid)
}

// deleteProp removes one property, if present, with its index entries.
func (g *Graph) deleteProp(ctx context.Context, s *engine.Stmt, e Entity, id uint64, scopes []uint32, k []byte, pid catalog.PropertyID) error {
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return err
	}
	if ixs := indexed(set, e, scopes, pid); len(ixs) > 0 {
		b, ok, err := s.Get(ctx, e.slot(), k)
		if err != nil || !ok {
			return err
		}
		v, err := stored(b)
		if err != nil {
			return err
		}
		for _, ix := range ixs {
			if err := index.Remove(ctx, s, ix, id, v); err != nil {
				return err
			}
		}
	}
	_, err = s.Delete(ctx, e.slot(), k)
	return err
}

// deleteProps removes every property of an entity, with their index
// entries.
func (g *Graph) deleteProps(ctx context.Context, s *engine.Stmt, e Entity, id uint64, scopes []uint32) error {
	var keys [][]byte
	lo, hi := propertyRange(id)
	if err := s.Range(ctx, e.slot(), lo, hi, func(k, _ []byte) error {
		keys = append(keys, slices.Clone(k))
		return nil
	}); err != nil {
		return err
	}
	for _, k := range keys {
		pid := catalog.PropertyID(binary.BigEndian.Uint32(k[len(k)-4:]))
		if err := g.deleteProp(ctx, s, e, id, scopes, k, pid); err != nil {
			return err
		}
	}
	return nil
}

// CreateEdge creates an edge of type typ from one existing node to
// another, which may be the same.
func (g *Graph) CreateEdge(ctx context.Context, s *engine.Stmt, from, to uint64, typ string, props map[string]value.Value) (uint64, error) {
	const op = "tacdb.CreateEdge"
	for _, n := range []uint64{from, to} {
		if _, err := g.node(ctx, s, op, n); err != nil {
			return 0, err
		}
	}
	enc, err := g.encodeProps(op, props)
	if err != nil {
		return 0, err
	}
	t, err := catalog.Intern[catalog.EdgeTypeID](ctx, s, typ, g.MaxSymbols)
	if err != nil {
		return 0, err
	}
	id, err := s.NextID(ctx, engine.KindEdge)
	if err != nil {
		return 0, err
	}
	if err := s.Put(ctx, catalog.SlotEdges, recordKey(id), edgeRecord{from, to, t}.encode()); err != nil {
		return 0, err
	}
	if err := link(ctx, s, edgeRecord{from, to, t}, id, 1); err != nil {
		return 0, err
	}
	if err := g.putProps(ctx, s, EdgeEntity, id, []uint32{uint32(t)}, enc); err != nil {
		return 0, err
	}
	return id, s.AddCounts(0, 1)
}

// link writes (by +1) or removes (by -1) the two adjacency entries of an
// edge and adjusts the degrees of its endpoints.
func link(ctx context.Context, s *engine.Stmt, e edgeRecord, id uint64, by int64) error {
	entries := []adjacency{{e.from, dirOut, e.typ, e.to, id}, {e.to, dirIn, e.typ, e.from, id}}
	for _, a := range entries {
		k := adjacencyKey(a.node, a.dir, a.typ, a.peer, a.edge)
		if by > 0 {
			if err := s.Put(ctx, catalog.SlotAdjacency, k, nil); err != nil {
				return err
			}
		} else if found, err := s.Delete(ctx, catalog.SlotAdjacency, k); err != nil {
			return err
		} else if !found {
			return errs.Newf(errs.CodeCorrupt, "graph", "edge %d without its adjacency entry", id)
		}
		for _, t := range []catalog.EdgeTypeID{0, e.typ} {
			if err := addDegree(ctx, s, degreeKey(a.node, a.dir, t), by); err != nil {
				return err
			}
		}
	}
	if e.from == e.to {
		for _, t := range []catalog.EdgeTypeID{0, e.typ} {
			if err := addDegree(ctx, s, degreeKey(e.from, dirSelf, t), by); err != nil {
				return err
			}
		}
	}
	return nil
}

// addDegree adds by to a degree counter, removing it when it reaches zero.
func addDegree(ctx context.Context, s *engine.Stmt, k []byte, by int64) error {
	b, ok, err := s.Get(ctx, catalog.SlotStats, k)
	if err != nil {
		return err
	}
	var n uint64
	if ok {
		if n, err = decodeCount(b); err != nil {
			return err
		}
	}
	if by < 0 && n < uint64(-by) {
		return errs.Newf(errs.CodeCorrupt, "graph", "degree %x is %d, below a change of %d", k, n, by)
	}
	if n = uint64(int64(n) + by); n == 0 {
		_, err = s.Delete(ctx, catalog.SlotStats, k)
		return err
	}
	return s.Put(ctx, catalog.SlotStats, k, encodeCount(n))
}

// DeleteEdge removes edge id with its properties and adjacency.
func (g *Graph) DeleteEdge(ctx context.Context, s *engine.Stmt, id uint64) error {
	rec, err := g.edge(ctx, s, "tacdb.DeleteEdge", id)
	if err != nil {
		return err
	}
	return g.deleteEdge(ctx, s, rec, id)
}

func (g *Graph) deleteEdge(ctx context.Context, s *engine.Stmt, rec edgeRecord, id uint64) error {
	if err := g.deleteProps(ctx, s, EdgeEntity, id, []uint32{uint32(rec.typ)}); err != nil {
		return err
	}
	if err := link(ctx, s, rec, id, -1); err != nil {
		return err
	}
	if _, err := s.Delete(ctx, catalog.SlotEdges, recordKey(id)); err != nil {
		return err
	}
	return s.AddCounts(0, -1)
}

// incident returns the edges with an endpoint at node, each once.
func incident(ctx context.Context, r engine.Reader, node uint64) ([]uint64, error) {
	var ids []uint64
	lo := codec.AppendKeyUint64(nil, node)
	err := r.Range(ctx, catalog.SlotAdjacency, lo, codec.PrefixEnd(lo), func(k, _ []byte) error {
		a, err := decodeAdjacency(k)
		if err != nil {
			return err
		}
		if a.dir == dirIn && a.peer == node {
			return nil // a self-loop, already seen from its outgoing entry
		}
		ids = append(ids, a.edge)
		return nil
	})
	return ids, err
}

// DeleteNode removes node id. With incident edges it fails, unless detach
// removes them first.
func (g *Graph) DeleteNode(ctx context.Context, s *engine.Stmt, id uint64, detach bool) error {
	const op = "tacdb.DeleteNode"
	rec, err := g.node(ctx, s, op, id)
	if err != nil {
		return err
	}
	edges, err := incident(ctx, s, id)
	if err != nil {
		return err
	}
	if len(edges) > 0 && !detach {
		return errs.Newf(errs.CodeInvalidArgument, op, "node %d has %d edges", id, len(edges)).
			With("edges", int64(len(edges)))
	}
	for _, e := range edges {
		er, err := g.edge(ctx, s, op, e)
		if errs.CodeOf(err) == errs.CodeNotFound {
			err = errs.Newf(errs.CodeCorrupt, op, "adjacency of node %d names a missing edge %d", id, e)
		}
		if err != nil {
			return err
		}
		if err := g.deleteEdge(ctx, s, er, e); err != nil {
			return err
		}
	}
	lo := degreeNodePrefix(id)
	left := false
	if err := s.Range(ctx, catalog.SlotStats, lo, codec.PrefixEnd(lo), func(_, _ []byte) error {
		left = true
		return errStop
	}); err != nil && err != errStop {
		return err
	}
	if left {
		return errs.Newf(errs.CodeCorrupt, op, "node %d keeps degrees without edges", id)
	}
	for _, l := range rec.labels {
		if found, err := s.Delete(ctx, catalog.SlotLabels, labelKey(l, id)); err != nil {
			return err
		} else if !found {
			return errs.Newf(errs.CodeCorrupt, op, "node %d has label %d without its entry", id, l)
		}
	}
	if err := g.deleteProps(ctx, s, NodeEntity, id, labelScopes(rec.labels)); err != nil {
		return err
	}
	if _, err := s.Delete(ctx, catalog.SlotNodes, recordKey(id)); err != nil {
		return err
	}
	return s.AddCounts(-1, 0)
}
