package tacdb

import (
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// The convenience calls of SPEC §31.1 wrap the same transactional
// primitives as queries: scans in ID order, adjacency in the order of
// EdgeCursor, and writes in one statement of the transaction. A limit of
// 0 or less returns every match.

// NodeExists reports whether node id exists in tx.
func (tx *Tx) NodeExists(id NodeID) (bool, error) {
	var ok bool
	err := tx.read("tacdb.NodeExists", func(r engine.Reader) error {
		_, err := tx.db.g.HasLabels(ctx, r, uint64(id), nil)
		if errs.CodeOf(err) == errs.CodeNotFound {
			return nil
		}
		ok = err == nil
		return err
	})
	return ok, err
}

// NodesByLabel returns the nodes of label in ID order, at most limit.
func (tx *Tx) NodesByLabel(label string, limit int) ([]NodeID, error) {
	return tx.findNodes("tacdb.NodesByLabel", label, "", value.Value{}, false, limit)
}

// FindNodesByLabelProperty returns the nodes of label whose property key
// equals v, in ID order, at most limit. Equality is the one of an index
// seek (D-007): Int 1 finds Float 1.0, and Null and NaN find nothing.
func (tx *Tx) FindNodesByLabelProperty(label, key string, v value.Value, limit int) ([]NodeID, error) {
	return tx.findNodes("tacdb.FindNodesByLabelProperty", label, key, v, true, limit)
}

// seekable reports whether v can be found by equality, and its runtime
// form.
func seekable(v value.Value) (qv.Value, bool) {
	x := qv.FromValue(v)
	return x, !v.IsNull() && !qv.IsNaN(x)
}

func (tx *Tx) findNodes(op, label, key string, v value.Value, byProp bool, limit int) ([]NodeID, error) {
	want, ok := seekable(v)
	if byProp && !ok {
		return nil, tx.read(op, func(engine.Reader) error { return nil })
	}
	var out []NodeID
	err := tx.read(op, func(r engine.Reader) error {
		s, err := tx.db.g.NodesByLabel(ctx, r, label)
		if err != nil {
			return err
		}
		defer s.Close()
		for s.Next(ctx) && (limit <= 0 || len(out) < limit) {
			id := s.Item().ID
			if byProp {
				have, ok, err := tx.db.g.GetProperty(ctx, r, graph.NodeEntity, id, key)
				if err != nil {
					return err
				}
				if !ok || !qv.Equals(qv.FromValue(have), want) {
					continue
				}
			}
			out = append(out, NodeID(id))
		}
		return s.Err()
	})
	return out, err
}

// FindEdgesByTypeProperty returns the edges of type typ whose property
// key equals v, in ID order, at most limit, with the equality of
// FindNodesByLabelProperty.
func (tx *Tx) FindEdgesByTypeProperty(typ, key string, v value.Value, limit int) ([]EdgeID, error) {
	const op = "tacdb.FindEdgesByTypeProperty"
	want, ok := seekable(v)
	var out []EdgeID
	err := tx.read(op, func(r engine.Reader) error {
		if !ok {
			return nil
		}
		s, err := tx.db.g.EdgesByType(ctx, r, []string{typ})
		if err != nil {
			return err
		}
		defer s.Close()
		for s.Next(ctx) && (limit <= 0 || len(out) < limit) {
			id := s.Item().ID
			have, ok, err := tx.db.g.GetProperty(ctx, r, graph.EdgeEntity, id, key)
			if err != nil {
				return err
			}
			if ok && qv.Equals(qv.FromValue(have), want) {
				out = append(out, EdgeID(id))
			}
		}
		return s.Err()
	})
	return out, err
}

// OutgoingEdges returns the edges leaving node id with one of types, or
// any type, in the order of EdgeCursor: by type, then peer and ID.
func (tx *Tx) OutgoingEdges(id NodeID, types ...string) ([]EdgeRef, error) {
	return tx.edgeList("tacdb.OutgoingEdges", id, graph.Out, types)
}

// IncomingEdges returns the edges reaching node id with one of types, or
// any type, in the order of EdgeCursor.
func (tx *Tx) IncomingEdges(id NodeID, types ...string) ([]EdgeRef, error) {
	return tx.edgeList("tacdb.IncomingEdges", id, graph.In, types)
}

func (tx *Tx) edgeList(op string, id NodeID, dir graph.Direction, types []string) ([]EdgeRef, error) {
	var out []EdgeRef
	err := tx.read(op, func(r engine.Reader) error {
		c, err := tx.db.g.Edges(ctx, r, uint64(id), dir, types)
		if err != nil {
			return err
		}
		defer c.Close()
		for c.Next(ctx) {
			e := c.Edge()
			out = append(out, EdgeRef{ID: EdgeID(e.ID), From: NodeID(e.From), To: NodeID(e.To), Type: e.Type})
		}
		return c.Err()
	})
	return out, err
}

// CreateNodes creates nodes in one statement: all of them, or, on any
// error, none.
func (tx *Tx) CreateNodes(nodes []CreateNodeOptions) ([]Node, error) {
	out := make([]Node, 0, len(nodes))
	err := tx.write("tacdb.CreateNodes", func(s *engine.Stmt) error {
		for _, o := range nodes {
			id, err := tx.db.g.CreateNode(ctx, s, o.Labels, o.Properties)
			if err != nil {
				return err
			}
			out = append(out, Node{ID: NodeID(id), Labels: dedupLabels(o.Labels), Properties: copyProps(o.Properties)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
