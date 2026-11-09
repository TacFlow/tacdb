package exec

import (
	"container/heap"
	"slices"
	"sort"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/qv"
)

// varExpand enumerates paths of distinct edges depth first (SPEC §33.5),
// one row per path whose length is in [Min, Max].
type varExpand struct {
	x     *Exec
	in    iter
	op    *plan.VarExpand
	cur   Row
	max   int
	stack []*vframe
	path  []uint64        // edge IDs of the current path
	used  map[uint64]bool // edges of the path and of the other relationships of the clause
	// pending is a node reached and not yet yielded; leaf marks that its
	// edge has no frame and leaves the path once it is yielded.
	pending    uint64
	hasPending bool
	leaf       bool
}

type vframe struct {
	node uint64
	c    *graph.EdgeCursor
	edge bool // reached by an edge of the path
}

// expansion counts one edge read by a variable-length relationship.
func (x *Exec) expansion() error {
	x.expanded++
	if x.expanded > x.cfg.MaxExpansions {
		return errs.New(errs.CodeResourceLimit, "query", "variable-length relationships read more edges than the limit of expansions").
			With("requested", x.expanded).With("limit", x.cfg.MaxExpansions)
	}
	return x.examine()
}

func (v *varExpand) start(r Row) error {
	o := v.op
	v.cur, v.path, v.stack = r, v.path[:0], v.stack[:0]
	v.used = map[uint64]bool{}
	for _, u := range o.Unique {
		if e := r[u]; e.Kind() == qv.KindEdge {
			v.used[e.ID()] = true
		}
	}
	for _, l := range o.Lists {
		if e := r[l]; e.Kind() == qv.KindList {
			for _, it := range e.Items() {
				v.used[it.ID()] = true
			}
		}
	}
	v.max = o.Max
	if o.Unbounded {
		v.max = v.x.plan.MaxHops
	}
	return v.visit(r[o.From].ID(), false)
}

// visit reaches node: it becomes pending, and gets a frame when the path
// may grow from it.
func (v *varExpand) visit(node uint64, byEdge bool) error {
	v.pending, v.hasPending, v.leaf = node, true, false
	if len(v.path) < v.max {
		c, err := v.x.src.G.Edges(v.x.env.Ctx, v.x.src.R, node, v.op.Dir, v.op.Types)
		if err != nil {
			return err
		}
		v.stack = append(v.stack, &vframe{node: node, c: c, edge: byEdge})
		return nil
	}
	v.leaf = byEdge
	if v.op.Unbounded {
		return v.beyond(node)
	}
	return nil
}

// beyond fails when a path of MaxHops edges could go on: an unbounded
// relationship never stops short in silence (SPEC §15.4).
func (v *varExpand) beyond(node uint64) error {
	c, err := v.x.src.G.Edges(v.x.env.Ctx, v.x.src.R, node, v.op.Dir, v.op.Types)
	if err != nil {
		return err
	}
	defer c.Close()
	for c.Next(v.x.env.Ctx) {
		if err := v.x.expansion(); err != nil {
			return v.at(err)
		}
		if !v.used[c.Edge().ID] {
			return v.at(errs.New(errs.CodeResourceLimit, "query", "a variable-length relationship goes on past the limit of hops").
				With("requested", int64(v.max)+1).With("limit", int64(v.max)))
		}
	}
	return c.Err()
}

func (v *varExpand) next() (Row, bool, error) {
	x, o := v.x, v.op
	for {
		if v.hasPending {
			v.hasPending = false
			node := v.pending
			row, ok, err := v.emit(node)
			if v.leaf {
				v.pop()
				v.leaf = false
			}
			if err != nil || ok {
				return row, ok, err
			}
			continue
		}
		if len(v.stack) == 0 {
			r, ok, err := v.in.next()
			if err != nil || !ok {
				return nil, false, err
			}
			if r[o.From].Kind() != qv.KindNode || (o.Into && r[o.To].Kind() != qv.KindNode) {
				continue
			}
			if err := v.start(r); err != nil {
				return nil, false, err
			}
			continue
		}
		top := v.stack[len(v.stack)-1]
		if !top.c.Next(x.env.Ctx) {
			err := top.c.Err()
			top.c.Close()
			v.stack = v.stack[:len(v.stack)-1]
			if top.edge {
				v.pop()
			}
			if err != nil {
				return nil, false, err
			}
			continue
		}
		if err := x.expansion(); err != nil {
			return nil, false, v.at(err)
		}
		e := top.c.Edge()
		if v.used[e.ID] {
			continue
		}
		peer := e.From
		if e.From == top.node {
			peer = e.To
		}
		v.used[e.ID] = true
		v.path = append(v.path, e.ID)
		if err := v.visit(peer, true); err != nil {
			return nil, false, err
		}
	}
}

// at places an error of the relationship at its text, as an execution
// error.
func (v *varExpand) at(err error) error {
	if e, ok := err.(*errs.Error); ok && e.Location == nil {
		c := *e
		c.Stage, c.Location = errs.StageExecute, ast.Locate(v.x.plan.Src, v.op.Span)
		return &c
	}
	return err
}

// pop removes the last edge of the path.
func (v *varExpand) pop() {
	last := v.path[len(v.path)-1]
	v.path = v.path[:len(v.path)-1]
	delete(v.used, last)
}

// emit yields the row of the path that reached node, if it qualifies.
func (v *varExpand) emit(node uint64) (Row, bool, error) {
	o := v.op
	if len(v.path) < o.Min {
		return nil, false, nil
	}
	if o.Into && v.cur[o.To].ID() != node {
		return nil, false, nil
	}
	row := slices.Clone(v.cur)
	edges := make([]qv.Value, len(v.path))
	for i, id := range v.path {
		edges[i] = qv.Edge(id)
	}
	row[o.List] = qv.List(edges)
	row[o.To] = qv.Node(node)
	ok, err := v.x.nodeOK(node, o.ToLabels, o.ToProps, row)
	if err != nil || !ok {
		return nil, false, err
	}
	return row, true, v.x.work(1)
}

func (v *varExpand) close() {
	for _, f := range v.stack {
		f.c.Close()
	}
	v.stack = nil
	v.in.close()
}

// group is a group of an aggregation.
type group struct {
	keys  []qv.Value
	accs  []eval.Accumulator
	bytes int64
}

// aggregate groups rows by their keys, with the codec of equality.
type aggregate struct {
	x      *Exec
	in     iter
	op     *plan.Aggregate
	groups []*group
	byKey  map[string]*group
	cur    *group // Ordered: the group being filled
	curKey []byte
	read   bool
	held   int64
}

func (a *aggregate) newGroup(keys []qv.Value, key []byte) (*group, error) {
	g := &group{keys: keys, bytes: rowBytes(keys) + int64(len(key)) + 64}
	if err := a.x.grow(g.bytes); err != nil {
		return nil, err
	}
	a.held += g.bytes
	for _, ag := range a.op.Aggs {
		g.accs = append(g.accs, eval.NewAccumulator(ag.Func, ag.Distinct, ag.Star, a.x.b))
	}
	return g, nil
}

// add evaluates the arguments of the aggregates on r into g.
func (a *aggregate) add(g *group, r Row) error {
	for i, ag := range a.op.Aggs {
		var v qv.Value
		if ag.Arg != nil {
			var err error
			if v, err = a.x.eval(*ag.Arg, r); err != nil {
				return err
			}
		}
		if err := g.accs[i].Add(v); err != nil {
			return err
		}
	}
	return nil
}

// result turns g into a row and releases it.
func (a *aggregate) result(g *group) (Row, error) {
	row := make(Row, a.op.Part.Width)
	copy(row, g.keys)
	var err error
	for i, acc := range g.accs {
		if err == nil {
			row[len(g.keys)+i], err = acc.Result()
		}
		acc.Close()
	}
	g.accs = nil
	a.x.shrink(g.bytes)
	a.held -= g.bytes
	if err == nil {
		err = a.x.work(1)
	}
	return row, err
}

// keysOf evaluates the grouping keys on r.
func (a *aggregate) keysOf(r Row) ([]qv.Value, []byte, error) {
	keys := make([]qv.Value, len(a.op.Keys))
	var key []byte
	for i, k := range a.op.Keys {
		v, err := a.x.eval(k, r)
		if err != nil {
			return nil, nil, err
		}
		keys[i] = v
		key = qv.AppendKey(key, v)
	}
	return keys, key, nil
}

func (a *aggregate) next() (Row, bool, error) {
	if a.op.Ordered {
		return a.nextOrdered()
	}
	if !a.read {
		a.read = true
		a.byKey = map[string]*group{}
		for {
			r, ok, err := a.in.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			keys, key, err := a.keysOf(r)
			if err != nil {
				return nil, false, err
			}
			g := a.byKey[string(key)]
			if g == nil {
				if g, err = a.newGroup(keys, key); err != nil {
					return nil, false, err
				}
				a.byKey[string(key)] = g
				a.groups = append(a.groups, g)
			}
			if err := a.add(g, r); err != nil {
				return nil, false, err
			}
		}
		a.byKey = nil
		if len(a.groups) == 0 && len(a.op.Keys) == 0 {
			g, err := a.newGroup(nil, nil)
			if err != nil {
				return nil, false, err
			}
			a.groups = append(a.groups, g)
		}
	}
	if len(a.groups) == 0 {
		return nil, false, nil
	}
	g := a.groups[0]
	a.groups = a.groups[1:]
	row, err := a.result(g)
	return row, err == nil, err
}

// nextOrdered keeps only the current group: its input comes clustered by
// the keys, so a group ends when the key changes.
func (a *aggregate) nextOrdered() (Row, bool, error) {
	for !a.read {
		r, ok, err := a.in.next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			a.read = true
			break
		}
		keys, key, err := a.keysOf(r)
		if err != nil {
			return nil, false, err
		}
		var done *group
		if a.cur != nil && string(key) != string(a.curKey) {
			done, a.cur = a.cur, nil
		}
		if a.cur == nil {
			if a.cur, err = a.newGroup(keys, key); err != nil {
				return nil, false, err
			}
			a.curKey = key
		}
		if err := a.add(a.cur, r); err != nil {
			return nil, false, err
		}
		if done != nil {
			row, err := a.result(done)
			return row, err == nil, err
		}
	}
	if a.cur == nil {
		return nil, false, nil
	}
	g := a.cur
	a.cur = nil
	row, err := a.result(g)
	return row, err == nil, err
}

func (a *aggregate) close() {
	for _, g := range append(a.groups, a.cur) {
		if g != nil {
			for _, acc := range g.accs {
				acc.Close()
			}
		}
	}
	for _, g := range a.byKey {
		for _, acc := range g.accs {
			acc.Close()
		}
	}
	a.groups, a.cur, a.byKey = nil, nil, nil
	a.x.shrink(a.held)
	a.held = 0
	a.in.close()
}

// distinct keeps the first row of each class of its first n slots.
type distinct struct {
	x    *Exec
	in   iter
	n    int
	seen map[string]struct{}
	held int64
}

func (d *distinct) next() (Row, bool, error) {
	for {
		r, ok, err := d.in.next()
		if err != nil || !ok {
			return nil, false, err
		}
		var key []byte
		for _, v := range r[:d.n] {
			key = qv.AppendKey(key, v)
		}
		if _, dup := d.seen[string(key)]; dup {
			continue
		}
		n := int64(len(key)) + 64
		if err := d.x.grow(n); err != nil {
			return nil, false, err
		}
		d.held += n
		d.seen[string(key)] = struct{}{}
		return r, true, nil
	}
}

func (d *distinct) close() {
	d.seen = nil
	d.x.shrink(d.held)
	d.held = 0
	d.in.close()
}

// compareRows orders two rows by keys: the total order of values, Null
// last ascending and first descending.
func compareRows(keys []plan.SortKey, a, b Row) int {
	for _, k := range keys {
		c := qv.Compare(a[k.Slot], b[k.Slot])
		if k.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// sorter orders every row of its input, stably.
type sorter struct {
	x    *Exec
	in   iter
	keys []plan.SortKey
	rows []Row
	read bool
	held int64
}

func (s *sorter) next() (Row, bool, error) {
	if !s.read {
		s.read = true
		for {
			r, ok, err := s.in.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			n := rowBytes(r)
			if err := s.x.grow(n); err != nil {
				return nil, false, err
			}
			s.held += n
			s.rows = append(s.rows, r)
		}
		sort.SliceStable(s.rows, func(i, j int) bool { return compareRows(s.keys, s.rows[i], s.rows[j]) < 0 })
	}
	if len(s.rows) == 0 {
		return nil, false, nil
	}
	r := s.rows[0]
	s.rows[0] = nil
	s.rows = s.rows[1:]
	return r, true, s.x.work(1)
}

func (s *sorter) close() {
	s.rows = nil
	s.x.shrink(s.held)
	s.held = 0
	s.in.close()
}

// ranked is a row kept by TopK with its position in the input.
type ranked struct {
	row Row
	seq uint64
}

// topHeap keeps the worst kept row on top.
type topHeap struct {
	keys []plan.SortKey
	rows []ranked
}

func (h *topHeap) less(a, b ranked) bool {
	if c := compareRows(h.keys, a.row, b.row); c != 0 {
		return c < 0
	}
	return a.seq < b.seq
}

func (h *topHeap) Len() int           { return len(h.rows) }
func (h *topHeap) Less(i, j int) bool { return h.less(h.rows[j], h.rows[i]) }
func (h *topHeap) Swap(i, j int)      { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *topHeap) Push(x any)         { h.rows = append(h.rows, x.(ranked)) }
func (h *topHeap) Pop() any {
	r := h.rows[len(h.rows)-1]
	h.rows = h.rows[:len(h.rows)-1]
	return r
}

// topK keeps the first Skip+Limit rows of the order in a heap and yields
// those after Skip (SPEC §33.4). LIMIT 0 reads nothing.
type topK struct {
	x    *Exec
	in   iter
	op   *plan.TopK
	out  []ranked
	read bool
	held int64
}

func (t *topK) next() (Row, bool, error) {
	if !t.read {
		t.read = true
		if err := t.fill(); err != nil {
			return nil, false, err
		}
	}
	if len(t.out) == 0 {
		return nil, false, nil
	}
	r := t.out[0].row
	t.out = t.out[1:]
	return r, true, t.x.work(1)
}

func (t *topK) fill() error {
	x := t.x
	k, err := x.count(*t.op.Limit)
	if err != nil {
		return err
	}
	var s int64
	if t.op.Skip != nil {
		if s, err = x.count(*t.op.Skip); err != nil {
			return err
		}
	}
	if k == 0 {
		if t.op.Exhaustive {
			return drain(t.in)
		}
		return nil
	}
	n := s + k
	if n < 0 { // overflow: every row
		n = 1<<63 - 1
	}
	h := &topHeap{keys: t.op.Keys}
	var seq uint64
	for {
		r, ok, err := t.in.next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		it := ranked{row: r, seq: seq}
		seq++
		if int64(h.Len()) < n {
			b := rowBytes(r)
			if err := x.grow(b); err != nil {
				return err
			}
			t.held += b
			heap.Push(h, it)
			continue
		}
		if h.less(it, h.rows[0]) {
			old := h.rows[0]
			b := rowBytes(r)
			if err := x.grow(b); err != nil {
				return err
			}
			t.held += b
			ob := rowBytes(old.row)
			x.shrink(ob)
			t.held -= ob
			h.rows[0] = it
			heap.Fix(h, 0)
		}
	}
	rows := h.rows
	sort.Slice(rows, func(i, j int) bool { return h.less(rows[i], rows[j]) })
	if int64(len(rows)) > s {
		t.out = rows[s:]
	}
	return nil
}

func (t *topK) close() {
	t.out = nil
	t.x.shrink(t.held)
	t.held = 0
	t.in.close()
}
