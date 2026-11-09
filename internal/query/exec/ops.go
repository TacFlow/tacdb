package exec

import (
	"math"
	"slices"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/qv"
)

// build instantiates op; arg is the row an Argument leaf yields.
func (x *Exec) build(op plan.Op, arg Row) iter {
	switch o := op.(type) {
	case *plan.SingleRow:
		return &once{row: make(Row, o.Part.Width)}
	case *plan.Argument:
		return &once{row: slices.Clone(arg)}
	case *plan.NodeScan:
		return &scan{x: x, in: x.build(o.In, arg), slot: o.Slot, labels: o.Labels, props: o.Props}
	case *plan.LabelScan:
		return &scan{x: x, in: x.build(o.In, arg), slot: o.Slot, label: o.Label, byLabel: true, labels: o.Labels, props: o.Props}
	case *plan.IDSeek:
		return &idSeek{x: x, in: x.build(o.In, arg), op: o}
	case *plan.NodeFilter:
		return &filter{in: x.build(o.In, arg), keep: func(r Row) (bool, error) {
			if r[o.Slot].Kind() != qv.KindNode {
				return false, nil
			}
			return x.nodeOK(r[o.Slot].ID(), o.Labels, o.Props, r)
		}}
	case *plan.Expand:
		return &expand{x: x, in: x.build(o.In, arg), op: o}
	case *plan.VarExpand:
		return &varExpand{x: x, in: x.build(o.In, arg), op: o}
	case *plan.Filter:
		return &filter{in: x.build(o.In, arg), keep: func(r Row) (bool, error) {
			v, err := x.eval(o.Pred, r)
			return qv.Truthy(v), err
		}}
	case *plan.Unwind:
		return &unwind{x: x, in: x.build(o.In, arg), op: o}
	case *plan.OptionalApply:
		return &optional{x: x, in: x.build(o.In, arg), op: o}
	case *plan.Project:
		return &project{x: x, in: x.build(o.In, arg), op: o}
	case *plan.Aggregate:
		return &aggregate{x: x, in: x.build(o.In, arg), op: o}
	case *plan.Distinct:
		return &distinct{x: x, in: x.build(o.In, arg), n: o.N, seen: map[string]struct{}{}}
	case *plan.Sort:
		return &sorter{x: x, in: x.build(o.In, arg), keys: o.Keys}
	case *plan.TopK:
		return &topK{x: x, in: x.build(o.In, arg), op: o}
	case *plan.Skip:
		return &skip{x: x, in: x.build(o.In, arg), count: o.X}
	case *plan.Limit:
		return &limit{x: x, in: x.build(o.In, arg), count: o.X, exhaustive: o.Exhaustive}
	}
	if it := x.buildWrite(op, arg); it != nil {
		return it
	}
	panic("exec: unknown operator " + op.Name())
}

// once yields one row.
type once struct {
	row  Row
	done bool
}

func (o *once) next() (Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	return o.row, true, nil
}

func (o *once) close() { o.done = true }

// labelIDs resolves labels; ok is false when one was never used, so no
// node has it.
func (x *Exec) labelIDs(labels []string) ([]catalog.LabelID, bool, error) {
	ids := make([]catalog.LabelID, len(labels))
	for i, l := range labels {
		id, ok, err := x.src.G.LabelID(x.env.Ctx, x.src.R, l)
		if err != nil || !ok {
			return nil, false, err
		}
		ids[i] = id
	}
	return ids, true, nil
}

// nodeOK reports whether node id has labels and the properties of props,
// whose values are evaluated on row.
func (x *Exec) nodeOK(id uint64, labels []string, props []plan.PropTest, row Row) (bool, error) {
	if len(labels) > 0 {
		ids, ok, err := x.labelIDs(labels)
		if err != nil || !ok {
			return false, err
		}
		if ok, err = x.src.G.HasLabels(x.env.Ctx, x.src.R, id, ids); err != nil || !ok {
			return false, err
		}
	}
	return x.propsOK(qv.KindNode, id, props, row)
}

// propsOK reports whether every property of props equals, by the = of
// expressions, the property of the entity; an absent property is Null.
func (x *Exec) propsOK(k qv.Kind, id uint64, props []plan.PropTest, row Row) (bool, error) {
	for _, p := range props {
		want, err := x.eval(p.Value, row)
		if err != nil {
			return false, err
		}
		have, _, err := x.src.Property(x.env.Ctx, k, id, p.Key)
		if err != nil {
			return false, err
		}
		if !qv.Equals(qv.FromValue(have), want) {
			return false, nil
		}
	}
	return true, nil
}

// examine counts one entity scanned or edge read.
func (x *Exec) examine() error {
	x.stats.Examined++
	return x.work(1)
}

// scan is NodeScan and LabelScan: for each input row, every node.
type scan struct {
	x       *Exec
	in      iter
	slot    int
	label   string
	byLabel bool
	labels  []string
	props   []plan.PropTest
	cur     Row
	sc      *graph.Scan
}

func (s *scan) next() (Row, bool, error) {
	x := s.x
	for {
		if s.sc == nil {
			r, ok, err := s.in.next()
			if err != nil || !ok {
				return nil, false, err
			}
			s.cur = r
			if s.byLabel {
				if s.sc, err = x.src.G.NodesByLabel(x.env.Ctx, x.src.R, s.label); err != nil {
					return nil, false, err
				}
			} else {
				s.sc = x.src.G.Nodes(x.env.Ctx, x.src.R)
			}
		}
		if !s.sc.Next(x.env.Ctx) {
			err := s.sc.Err()
			s.sc.Close()
			s.sc = nil
			if err != nil {
				return nil, false, err
			}
			continue
		}
		if err := x.examine(); err != nil {
			return nil, false, err
		}
		row := slices.Clone(s.cur)
		id := s.sc.Item().ID
		row[s.slot] = qv.Node(id)
		ok, err := x.nodeOK(id, s.labels, s.props, row)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return row, true, x.work(1)
		}
	}
}

func (s *scan) close() {
	if s.sc != nil {
		s.sc.Close()
	}
	s.in.close()
}

// idSeek binds the node whose ID an expression gives.
type idSeek struct {
	x  *Exec
	in iter
	op *plan.IDSeek
}

func (s *idSeek) next() (Row, bool, error) {
	x := s.x
	for {
		r, ok, err := s.in.next()
		if err != nil || !ok {
			return nil, false, err
		}
		v, err := x.eval(s.op.ID, r)
		if err != nil {
			return nil, false, err
		}
		id, ok := seekID(v)
		if !ok {
			continue
		}
		if err := x.examine(); err != nil {
			return nil, false, err
		}
		if _, err := x.src.G.HasLabels(x.env.Ctx, x.src.R, uint64(id), nil); err != nil {
			if errs.CodeOf(err) == errs.CodeNotFound {
				continue
			}
			return nil, false, err
		}
		row := slices.Clone(r)
		row[s.op.Slot] = qv.Node(uint64(id))
		if ok, err := x.nodeOK(uint64(id), s.op.Labels, s.op.Props, row); err != nil || !ok {
			if err != nil {
				return nil, false, err
			}
			continue
		}
		return row, true, x.work(1)
	}
}

func (s *idSeek) close() { s.in.close() }

// seekID returns the node ID that equals v by the = of expressions: an
// Int, or a Float with an exact integer value (D-011); IDs are positive.
func seekID(v qv.Value) (int64, bool) {
	id, ok := v.AsInt()
	if f, isFloat := v.AsFloat(); isFloat {
		ok = f == math.Trunc(f) && f >= 1 && f < 1<<63
		id = int64(f)
	}
	return id, ok && id > 0
}

// filter keeps the rows keep accepts.
type filter struct {
	in   iter
	keep func(Row) (bool, error)
}

func (f *filter) next() (Row, bool, error) {
	for {
		r, ok, err := f.in.next()
		if err != nil || !ok {
			return nil, false, err
		}
		if ok, err := f.keep(r); err != nil || ok {
			return r, err == nil, err
		}
	}
}

func (f *filter) close() { f.in.close() }

// usedEdge reports whether edge id is bound in one of the slots of
// unique, or in one of the edge lists of lists, of row.
func usedEdge(row Row, unique, lists []int, id uint64) bool {
	for _, u := range unique {
		if v := row[u]; v.Kind() == qv.KindEdge && v.ID() == id {
			return true
		}
	}
	for _, l := range lists {
		if v := row[l]; v.Kind() == qv.KindList {
			for _, e := range v.Items() {
				if e.ID() == id {
					return true
				}
			}
		}
	}
	return false
}

func reverse(d graph.Direction) graph.Direction {
	switch d {
	case graph.Out:
		return graph.In
	case graph.In:
		return graph.Out
	}
	return d
}

// expand is ExpandAll and ExpandInto.
type expand struct {
	x   *Exec
	in  iter
	op  *plan.Expand
	cur Row
	c   *graph.EdgeCursor
	src uint64 // the node the cursor walks
}

func (e *expand) open(r Row) error {
	x, o := e.x, e.op
	from := r[o.From]
	if from.Kind() != qv.KindNode {
		return nil
	}
	if o.RelBound && r[o.Rel].Kind() != qv.KindEdge {
		return nil
	}
	ctx := x.env.Ctx
	var err error
	if !o.Into {
		e.src = from.ID()
		e.c, err = x.src.G.Edges(ctx, x.src.R, from.ID(), o.Dir, o.Types)
		return err
	}
	to := r[o.To]
	if to.Kind() != qv.KindNode {
		return nil
	}
	a, b, dir := from.ID(), to.ID(), o.Dir
	if len(o.Types) == 0 && a != b {
		// Walk the side with fewer edges (SPEC §33.3).
		da, err := x.src.G.DegreeOf(ctx, x.src.R, a, dir, nil)
		if err != nil {
			return err
		}
		db, err := x.src.G.DegreeOf(ctx, x.src.R, b, reverse(dir), nil)
		if err != nil {
			return err
		}
		if db < da {
			a, b, dir = b, a, reverse(dir)
		}
	}
	e.src = a
	e.c, err = x.src.G.EdgesBetween(ctx, x.src.R, a, dir, o.Types, b)
	return err
}

func (e *expand) next() (Row, bool, error) {
	x, o := e.x, e.op
	for {
		if e.c == nil {
			r, ok, err := e.in.next()
			if err != nil || !ok {
				return nil, false, err
			}
			e.cur = r
			if err := e.open(r); err != nil {
				return nil, false, err
			}
			if e.c == nil {
				continue
			}
		}
		if !e.c.Next(x.env.Ctx) {
			err := e.c.Err()
			e.c.Close()
			e.c = nil
			if err != nil {
				return nil, false, err
			}
			continue
		}
		if err := x.examine(); err != nil {
			return nil, false, err
		}
		edge := e.c.Edge()
		if o.RelBound && e.cur[o.Rel].ID() != edge.ID {
			continue
		}
		if usedEdge(e.cur, o.Unique, o.Lists, edge.ID) {
			continue
		}
		row := slices.Clone(e.cur)
		row[o.Rel] = qv.Edge(edge.ID)
		if !o.Into {
			peer := edge.From
			if edge.From == e.src {
				peer = edge.To
			}
			row[o.To] = qv.Node(peer)
		}
		ok, err := x.propsOK(qv.KindEdge, edge.ID, o.RelProps, row)
		if err == nil && ok {
			ok, err = x.nodeOK(row[o.To].ID(), o.ToLabels, o.ToProps, row)
		}
		if err != nil {
			return nil, false, err
		}
		if ok {
			return row, true, x.work(1)
		}
	}
}

func (e *expand) close() {
	if e.c != nil {
		e.c.Close()
	}
	e.in.close()
}

// unwind yields a row per element of a List.
type unwind struct {
	x     *Exec
	in    iter
	op    *plan.Unwind
	cur   Row
	items []qv.Value
}

func (u *unwind) next() (Row, bool, error) {
	for {
		if len(u.items) > 0 {
			row := slices.Clone(u.cur)
			row[u.op.Slot], u.items = u.items[0], u.items[1:]
			return row, true, u.x.work(1)
		}
		r, ok, err := u.in.next()
		if err != nil || !ok {
			return nil, false, err
		}
		v, err := u.x.eval(u.op.X, r)
		if err != nil {
			return nil, false, err
		}
		switch v.Kind() {
		case qv.KindNull:
		case qv.KindList:
			u.cur, u.items = r, v.Items()
		default:
			u.cur, u.items = r, []qv.Value{v}
		}
	}
}

func (u *unwind) close() { u.in.close() }

// optional is the left outer apply of OPTIONAL MATCH.
type optional struct {
	x     *Exec
	in    iter
	op    *plan.OptionalApply
	cur   Row
	inner iter
	found bool
}

func (o *optional) next() (Row, bool, error) {
	for {
		if o.inner == nil {
			r, ok, err := o.in.next()
			if err != nil || !ok {
				return nil, false, err
			}
			o.cur, o.found = r, false
			o.inner = o.x.build(o.op.Inner, r)
		}
		r, ok, err := o.inner.next()
		if err != nil {
			return nil, false, err
		}
		if ok {
			o.found = true
			return r, true, nil
		}
		o.inner.close()
		o.inner = nil
		if !o.found {
			// The slots of the optional pattern were never set: Null.
			return slices.Clone(o.cur), true, o.x.work(1)
		}
	}
}

func (o *optional) close() {
	if o.inner != nil {
		o.inner.close()
	}
	o.in.close()
}

// joined is a row made of a row followed by another, for sort keys.
type joined struct{ a, b Row }

func (j joined) Slot(i int) qv.Value {
	if i < len(j.a) {
		return j.a[i]
	}
	return j.b[i-len(j.a)]
}

// project computes the items and sort keys of a projection.
type project struct {
	x  *Exec
	in iter
	op *plan.Project
}

func (p *project) next() (Row, bool, error) {
	r, ok, err := p.in.next()
	if err != nil || !ok {
		return nil, false, err
	}
	out := make(Row, p.op.Out.Width)
	for i, it := range p.op.Items {
		if out[i], err = p.x.eval(it, r); err != nil {
			return nil, false, err
		}
	}
	n := len(p.op.Items)
	for i, k := range p.op.Keys {
		if out[n+i], err = p.x.eval(k, joined{r, out[:n]}); err != nil {
			return nil, false, err
		}
	}
	return out, true, p.x.work(1)
}

func (p *project) close() { p.in.close() }

// skip drops the first rows.
type skip struct {
	x     *Exec
	in    iter
	count plan.Expr
	left  int64
	init  bool
}

// count evaluates SKIP or LIMIT: an Int >= 0, as the analysis and the
// check of parameters made sure.
func (x *Exec) count(e plan.Expr) (int64, error) {
	v, err := x.eval(e, Row{})
	if err != nil {
		return 0, err
	}
	n, ok := v.AsInt()
	if !ok || n < 0 {
		return 0, errs.Newf(errs.CodeParamType, "query", "SKIP and LIMIT need an integer >= 0, not %v", v)
	}
	return n, nil
}

func (s *skip) next() (Row, bool, error) {
	if !s.init {
		n, err := s.x.count(s.count)
		if err != nil {
			return nil, false, err
		}
		s.left, s.init = n, true
	}
	for s.left > 0 {
		_, ok, err := s.in.next()
		if err != nil || !ok {
			return nil, false, err
		}
		s.left--
	}
	return s.in.next()
}

func (s *skip) close() { s.in.close() }

// limit yields the first rows; LIMIT 0 reads nothing, unless the limit
// is exhaustive: then it reads its whole input, which runs the effects of
// the clauses that write before it.
type limit struct {
	x          *Exec
	in         iter
	count      plan.Expr
	left       int64
	init       bool
	exhaustive bool
}

func (l *limit) next() (Row, bool, error) {
	if !l.init {
		n, err := l.x.count(l.count)
		if err != nil {
			return nil, false, err
		}
		l.left, l.init = n, true
	}
	if l.left == 0 {
		if l.exhaustive {
			l.exhaustive = false
			return nil, false, drain(l.in)
		}
		return nil, false, nil
	}
	l.left--
	return l.in.next()
}

// drain reads the rest of in.
func drain(in iter) error {
	for {
		_, ok, err := in.next()
		if err != nil || !ok {
			return err
		}
	}
}

func (l *limit) close() { l.in.close() }
