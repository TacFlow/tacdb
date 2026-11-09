package exec

import (
	"maps"
	"slices"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// Mutations counts the effects of an execution. PropertiesSet counts
// every property written with a value and every existing property
// removed; LabelsAdded counts the labels of new nodes and the labels SET
// adds to a node without them; NodesDeleted and EdgesDeleted count each
// entity once, with the relationships DETACH DELETE removes.
type Mutations struct {
	NodesCreated, NodesDeleted uint64
	EdgesCreated, EdgesDeleted uint64
	PropertiesSet              uint64
	LabelsAdded, LabelsRemoved uint64
}

// barrier is an operator of a clause that writes (design §4.4): on its
// first call it reads every row of its input, then applies the clause to
// each row, and only then yields the rows that result.
type barrier struct {
	x     *Exec
	in    iter
	span  ast.Span
	apply func(rows []Row) ([]Row, error)
	out   []Row
	sizes []int64 // what each row of out is charged
	held  int64
	ran   bool
}

func (b *barrier) next() (Row, bool, error) {
	if !b.ran {
		b.ran = true
		if err := b.run(); err != nil {
			return nil, false, err
		}
	}
	if len(b.out) == 0 {
		return nil, false, nil
	}
	r := b.out[0]
	b.x.shrink(b.sizes[0])
	b.held -= b.sizes[0]
	b.out[0], b.out, b.sizes = nil, b.out[1:], b.sizes[1:]
	return r, true, nil
}

// run reads every row, applies the clause and charges the rows it yields
// one by one, each released as it goes to the next operator.
func (b *barrier) run() error {
	var rows []Row
	for {
		r, ok, err := b.in.next()
		if err != nil {
			return err // an error of the clauses before keeps its place
		}
		if !ok {
			break
		}
		n := rowBytes(r)
		if err := b.x.grow(n); err != nil {
			return b.at(err)
		}
		b.held += n
		rows = append(rows, r)
	}
	out, err := b.apply(rows)
	if err != nil {
		return b.at(err)
	}
	b.x.shrink(b.held)
	b.held = 0
	b.sizes = make([]int64, len(out))
	for i, r := range out {
		n := rowBytes(r)
		if err := b.x.grow(n); err != nil {
			return b.at(err)
		}
		b.held += n
		b.sizes[i] = n
	}
	b.out = out
	return nil
}

// at places an error of the clause at its text.
func (b *barrier) at(err error) error {
	if e, ok := err.(*errs.Error); ok && e.Location == nil {
		c := *e
		c.Stage, c.Location = errs.StageExecute, ast.Locate(b.x.plan.Src, b.span)
		return &c
	}
	return err
}

func (b *barrier) close() {
	b.out, b.sizes = nil, nil
	b.x.shrink(b.held)
	b.held = 0
	b.in.close()
}

// drop releases row i of rows, which the barrier no longer holds.
func (b *barrier) drop(rows []Row, i int) {
	n := rowBytes(rows[i])
	b.x.shrink(n)
	b.held -= n
	rows[i] = nil
}

// keep charges rows a barrier holds besides the rows it read.
func (b *barrier) keep(r Row) error {
	n := rowBytes(r)
	if err := b.x.grow(n); err != nil {
		return err
	}
	b.held += n
	return nil
}

// buildWrite instantiates the operator of a clause that writes.
func (x *Exec) buildWrite(op plan.Op, arg Row) iter {
	switch o := op.(type) {
	case *plan.Create:
		b := &barrier{x: x, in: x.build(o.In, arg), span: o.Span}
		b.apply = func(rows []Row) ([]Row, error) {
			for _, r := range rows {
				if err := x.work(1); err != nil {
					return nil, err
				}
				if err := x.create(o.Steps, r); err != nil {
					return nil, err
				}
			}
			return rows, nil
		}
		return b
	case *plan.Merge:
		b := &barrier{x: x, in: x.build(o.In, arg), span: o.Span}
		b.apply = func(rows []Row) ([]Row, error) { return x.merge(b, o, rows) }
		return b
	case *plan.Set:
		b := &barrier{x: x, in: x.build(o.In, arg), span: o.Span}
		b.apply = func(rows []Row) ([]Row, error) { return rows, x.update(o.Items, rows) }
		return b
	case *plan.Remove:
		b := &barrier{x: x, in: x.build(o.In, arg), span: o.Span}
		b.apply = func(rows []Row) ([]Row, error) { return rows, x.update(o.Items, rows) }
		return b
	case *plan.Delete:
		b := &barrier{x: x, in: x.build(o.In, arg), span: o.Span}
		b.apply = func(rows []Row) ([]Row, error) { return rows, x.delete(o, rows) }
		return b
	}
	return nil
}

// propValues evaluates the property map of a new element; a Null value
// writes nothing, and a reference anywhere in a value is TYPE_MISMATCH.
func (x *Exec) propValues(props []plan.PropTest, r Row) (map[string]value.Value, error) {
	out := make(map[string]value.Value, len(props))
	for _, p := range props {
		v, err := x.eval(p.Value, r)
		if err != nil {
			return nil, err
		}
		if v.Kind() == qv.KindNull {
			continue
		}
		pv, err := qv.ToValue(v)
		if err != nil {
			return nil, err
		}
		out[p.Key] = pv
	}
	return out, nil
}

// create creates the elements of steps for row r, into its slots.
func (x *Exec) create(steps []plan.CreateStep, r Row) error {
	ctx, s := x.env.Ctx, x.src.W
	for _, st := range steps {
		props, err := x.propValues(st.Props, r)
		if err != nil {
			return err
		}
		if !st.Rel {
			id, err := x.src.G.CreateNode(ctx, s, st.Labels, props)
			if err != nil {
				return err
			}
			r[st.Slot] = qv.Node(id)
			x.stats.NodesCreated++
			x.stats.LabelsAdded += uint64(len(dedup(st.Labels)))
			x.stats.PropertiesSet += uint64(len(props))
			continue
		}
		ends := [2]uint64{}
		for i, slot := range []int{st.From, st.To} {
			v := r[slot]
			if v.Kind() != qv.KindNode {
				return errs.Newf(errs.CodeTypeMismatch, "query", "an endpoint of a new relationship is %s, not a node", v.Kind())
			}
			ends[i] = v.ID()
		}
		id, err := x.src.G.CreateEdge(ctx, s, ends[0], ends[1], st.Type, props)
		if err != nil {
			return err
		}
		r[st.Slot] = qv.Edge(id)
		x.stats.EdgesCreated++
		x.stats.PropertiesSet += uint64(len(props))
	}
	return nil
}

func dedup(labels []string) []string {
	var out []string
	for _, l := range labels {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// merge matches the pattern of o for each row, with a plan that sees what
// the rows before created, and creates it when nothing matches.
func (x *Exec) merge(b *barrier, o *plan.Merge, rows []Row) ([]Row, error) {
	var out []Row
	for i, r := range rows {
		if err := x.work(1); err != nil {
			return nil, err
		}
		for _, e := range o.Values {
			v, err := x.eval(e, r)
			if err != nil {
				return nil, err
			}
			if v.Kind() == qv.KindNull {
				return nil, errs.New(errs.CodeInvalidArgument, "query", "MERGE cannot match a null property value")
			}
		}
		m := x.build(o.Match, r)
		var matches []Row
		for {
			mr, ok, err := m.next()
			if err != nil {
				m.close()
				return nil, err
			}
			if !ok {
				break
			}
			if err := b.keep(mr); err != nil {
				m.close()
				return nil, err
			}
			matches = append(matches, mr)
		}
		m.close()
		if len(matches) > 0 {
			b.drop(rows, i) // the matches take the place of the row
			for _, mr := range matches {
				if err := x.actions(o.Actions, false, mr); err != nil {
					return nil, err
				}
			}
			out = append(out, matches...)
			continue
		}
		if err := x.create(o.Steps, r); err != nil {
			return nil, err
		}
		if err := x.actions(o.Actions, true, r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// actions applies the ON CREATE or the ON MATCH actions of a MERGE, in
// order.
func (x *Exec) actions(acts []plan.MergeAction, onCreate bool, r Row) error {
	for _, a := range acts {
		if a.OnCreate == onCreate {
			if err := x.updateRow(a.Items, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// update applies the items of a SET or a REMOVE to each row.
func (x *Exec) update(items []plan.SetItem, rows []Row) error {
	for _, r := range rows {
		if err := x.work(1); err != nil {
			return err
		}
		if err := x.updateRow(items, r); err != nil {
			return err
		}
	}
	return nil
}

// target returns the entity an item of SET or REMOVE changes; ok is false
// for Null, which changes nothing.
func target(v qv.Value, labels bool) (graph.Entity, uint64, bool, error) {
	switch v.Kind() {
	case qv.KindNull:
		return 0, 0, false, nil
	case qv.KindNode:
		return graph.NodeEntity, v.ID(), true, nil
	case qv.KindEdge:
		if !labels {
			return graph.EdgeEntity, v.ID(), true, nil
		}
		return 0, 0, false, errs.New(errs.CodeTypeMismatch, "query", "only nodes have labels")
	}
	if labels {
		return 0, 0, false, errs.Newf(errs.CodeTypeMismatch, "query", "only nodes have labels, not %s", v.Kind())
	}
	return 0, 0, false, errs.Newf(errs.CodeTypeMismatch, "query", "only nodes and relationships have properties, not %s", v.Kind())
}

func (x *Exec) updateRow(items []plan.SetItem, r Row) error {
	for _, it := range items {
		labels := it.Kind == plan.AddLabels || it.Kind == plan.RemoveLabels
		e, id, ok, err := target(r[it.Slot], labels)
		if err != nil || !ok {
			if err != nil {
				return err
			}
			continue
		}
		switch it.Kind {
		case plan.SetProperty, plan.SetProperties, plan.MergeProperties:
			var v qv.Value
			if v, err = x.eval(*it.Value, r); err != nil {
				return err
			}
			if it.Kind == plan.SetProperty {
				err = x.setProperty(e, id, it.Key, v)
			} else {
				err = x.setProperties(e, id, v, it.Kind == plan.SetProperties)
			}
		case plan.RemoveProperty:
			err = x.removeProperty(e, id, it.Key)
		case plan.AddLabels, plan.RemoveLabels:
			err = x.relabel(id, it.Labels, it.Kind == plan.AddLabels)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// setProperty writes one property; Null removes it.
func (x *Exec) setProperty(e graph.Entity, id uint64, key string, v qv.Value) error {
	if v.Kind() == qv.KindNull {
		return x.removeProperty(e, id, key)
	}
	pv, err := qv.ToValue(v)
	if err != nil {
		return err
	}
	if err := x.src.G.SetProperty(x.env.Ctx, x.src.W, e, id, key, pv); err != nil {
		return err
	}
	x.stats.PropertiesSet++
	return nil
}

// removeProperty removes one property, counting it when it was there.
func (x *Exec) removeProperty(e graph.Entity, id uint64, key string) error {
	_, had, err := x.src.G.GetProperty(x.env.Ctx, x.src.W, e, id, key)
	if err != nil || !had {
		return err
	}
	if err := x.src.G.RemoveProperty(x.env.Ctx, x.src.W, e, id, key); err != nil {
		return err
	}
	x.stats.PropertiesSet++
	return nil
}

// setProperties applies a map: = replaces every property, += only the
// keys of the map. A Null value removes its key; a Null map changes
// nothing. Every value is converted before the first write.
func (x *Exec) setProperties(e graph.Entity, id uint64, v qv.Value, replace bool) error {
	switch v.Kind() {
	case qv.KindNull:
		return x.exists(e, id) // changes nothing, but not on a deleted entity
	case qv.KindMap:
	default:
		return errs.Newf(errs.CodeTypeMismatch, "query", "SET needs a map of properties, not %s", v.Kind())
	}
	if err := x.exists(e, id); err != nil {
		return err
	}
	set := map[string]value.Value{}
	var drop []string
	for i := 0; i < v.Len(); i++ {
		k, it := v.EntryAt(i)
		if it.Kind() == qv.KindNull {
			drop = append(drop, k)
			continue
		}
		pv, err := qv.ToValue(it)
		if err != nil {
			return err
		}
		set[k] = pv
	}
	if replace {
		have, err := x.src.G.Properties(x.env.Ctx, x.src.W, e, id)
		if err != nil {
			return err
		}
		for k := range have {
			if _, ok := set[k]; !ok {
				drop = append(drop, k)
			}
		}
	}
	slices.Sort(drop)
	for _, k := range slices.Compact(drop) {
		if err := x.removeProperty(e, id, k); err != nil {
			return err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(set)) {
		if err := x.setProperty(e, id, k, qv.FromValue(set[k])); err != nil {
			return err
		}
	}
	return nil
}

// exists fails with NOT_FOUND when the entity is gone, deleted earlier in
// the query.
func (x *Exec) exists(e graph.Entity, id uint64) error {
	if e == graph.EdgeEntity {
		_, err := x.src.G.EdgeEnds(x.env.Ctx, x.src.W, id)
		return err
	}
	_, err := x.src.G.HasLabels(x.env.Ctx, x.src.W, id, nil)
	return err
}

// relabel adds or removes labels of node id, counting those it changes.
func (x *Exec) relabel(id uint64, labels []string, add bool) error {
	ctx, s := x.env.Ctx, x.src.W
	have, err := x.src.G.Labels(ctx, s, id)
	if err != nil {
		return err
	}
	for _, l := range dedup(labels) {
		if slices.Contains(have, l) == add {
			continue
		}
		if add {
			if err := x.src.G.AddLabel(ctx, s, id, l); err != nil {
				return err
			}
			x.stats.LabelsAdded++
			continue
		}
		if err := x.src.G.RemoveLabel(ctx, s, id, l); err != nil {
			return err
		}
		x.stats.LabelsRemoved++
	}
	return nil
}

// delete deletes the entities of a DELETE over every row: relationships,
// then nodes. An entity deleted already, or Null, is skipped.
func (x *Exec) delete(o *plan.Delete, rows []Row) error {
	// An entity listed twice is NOT_FOUND the second time, and skipped.
	var nodes, edges []uint64
	add := func(k qv.Kind, id uint64) {
		if k == qv.KindNode {
			nodes = append(nodes, id)
		} else {
			edges = append(edges, id)
		}
	}
	for _, r := range rows {
		if err := x.work(1); err != nil {
			return err
		}
		for _, e := range o.Exprs {
			v, err := x.eval(e, r)
			if err != nil {
				return err
			}
			switch v.Kind() {
			case qv.KindNull:
			case qv.KindNode, qv.KindEdge:
				add(v.Kind(), v.ID())
			case qv.KindPath:
				p := v.Path()
				for _, id := range p.Edges {
					add(qv.KindEdge, id)
				}
				for _, id := range p.Nodes {
					add(qv.KindNode, id)
				}
			default:
				return errs.Newf(errs.CodeTypeMismatch, "query", "DELETE needs a node, a relationship or a path, not %s", v.Kind())
			}
		}
	}
	ctx, s := x.env.Ctx, x.src.W
	for _, id := range edges {
		err := x.src.G.DeleteEdge(ctx, s, id)
		if errs.CodeOf(err) == errs.CodeNotFound {
			continue
		}
		if err != nil {
			return err
		}
		x.stats.EdgesDeleted++
	}
	for _, id := range nodes {
		before := s.State().EdgeCount
		err := x.src.G.DeleteNode(ctx, s, id, o.Detach)
		if errs.CodeOf(err) == errs.CodeNotFound {
			continue
		}
		if err != nil {
			return err
		}
		x.stats.NodesDeleted++
		x.stats.EdgesDeleted += before - s.State().EdgeCount
	}
	return nil
}
