package graph

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

type mNode struct {
	labels []string
	props  map[string]value.Value
}

type mEdge struct {
	from, to uint64
	typ      string
	props    map[string]value.Value
}

// model is the graph in memory.
type model struct {
	nodes   map[uint64]*mNode
	edges   map[uint64]*mEdge
	indexes map[string]IndexDef
}

func newModel() *model {
	return &model{nodes: map[uint64]*mNode{}, edges: map[uint64]*mEdge{}, indexes: map[string]IndexDef{}}
}

func (m *model) clone() *model {
	c := newModel()
	for id, n := range m.nodes {
		c.nodes[id] = &mNode{slices.Clone(n.labels), maps.Clone(n.props)}
	}
	for id, e := range m.edges {
		c.edges[id] = &mEdge{e.from, e.to, e.typ, maps.Clone(e.props)}
	}
	c.indexes = maps.Clone(m.indexes)
	return c
}

// indexed returns the entities in the scope of d with its property, and
// their values.
func (m *model) indexed(d IndexDef) map[uint64]value.Value {
	out := map[uint64]value.Value{}
	if d.Entity == EdgeEntity {
		for id, e := range m.edges {
			if v, ok := e.props[d.Property]; ok && e.typ == d.Scope {
				out[id] = v
			}
		}
		return out
	}
	for id, n := range m.nodes {
		if v, ok := n.props[d.Property]; ok && slices.Contains(n.labels, d.Scope) {
			out[id] = v
		}
	}
	return out
}

// unique reports whether every unique index holds distinct values; the
// generated values have no NaN, so Compare is the equality of uniqueness.
func (m *model) unique() bool {
	for _, d := range m.indexes {
		if !d.Unique || d.State != index.Online {
			continue
		}
		var seen []value.Value
		for _, v := range m.indexed(d) {
			if v.IsNull() {
				continue
			}
			for _, w := range seen {
				if value.Compare(v, w) == 0 {
					return false
				}
			}
			seen = append(seen, v)
		}
	}
	return true
}

func (m *model) dump() string {
	var b strings.Builder
	for _, id := range slices.Sorted(maps.Keys(m.nodes)) {
		n := m.nodes[id]
		b.WriteString("node " + strconv.FormatUint(id, 10) + " [")
		for i, l := range n.labels {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.Quote(l))
		}
		b.WriteByte(']')
		writeProps(&b, n.props)
		b.WriteByte('\n')
	}
	for _, id := range slices.Sorted(maps.Keys(m.edges)) {
		e := m.edges[id]
		fmt.Fprintf(&b, "edge %d %d -> %d %s", id, e.from, e.to, strconv.Quote(e.typ))
		writeProps(&b, e.props)
		b.WriteByte('\n')
	}
	return b.String()
}

// degree counts the edges of n in dir with type typ ("" for any) the way
// the counters do: a self-loop once in Both.
func (m *model) degree(n uint64, dir Direction, typ string) uint64 {
	var d uint64
	for _, e := range m.edges {
		if typ != "" && e.typ != typ {
			continue
		}
		out, in := e.from == n, e.to == n
		switch {
		case dir == Out && out, dir == In && in, dir == Both && (out || in):
			d++
		}
	}
	return d
}

func (m *model) incident(n uint64) []uint64 {
	var ids []uint64
	for id, e := range m.edges {
		if e.from == n || e.to == n {
			ids = append(ids, id)
		}
	}
	return ids
}

var (
	labelNames = []string{"A", "B", "C", "D"}
	propNames  = []string{"p", "q", "r"}
	typeNames  = []string{"R", "S", "T"}
)

func randValue(rng *rand.Rand) value.Value {
	switch rng.IntN(6) {
	case 0:
		return value.Null()
	case 1:
		return value.Int(rng.Int64N(3))
	case 2:
		return value.Float(float64(rng.IntN(4)) / 2)
	case 3:
		return value.String(strings.Repeat("s", rng.IntN(3)))
	case 4:
		return value.List(value.Int(1), value.String("x"))
	}
	return value.Bool(rng.IntN(2) == 0)
}

// pickName returns a name, the first (indexed from the start) half the
// time.
func pickName(rng *rand.Rand, names []string) string {
	if rng.IntN(2) == 0 {
		return names[0]
	}
	return names[rng.IntN(len(names))]
}

// pick returns a live ID of ids, or a missing one now and then.
func pick(rng *rand.Rand, ids []uint64) uint64 {
	if len(ids) == 0 || rng.IntN(10) == 0 {
		return 1000 + rng.Uint64N(10)
	}
	return ids[rng.IntN(len(ids))]
}

// Random operations, savepoints, commits and rollbacks against the model:
// after each step the dump, the counts, sampled degrees and cursors and
// the structural check agree.
func TestAgainstModel(t *testing.T) {
	seen := map[errs.Code]int{}
	for seed := range uint64(8) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runModel(t, seed, 700, seen) })
	}
	// The run must reach the cases it is meant to check.
	for _, c := range []errs.Code{"", errs.CodeUniqueViolation, errs.CodeIndexExistsDifferent, errs.CodeIndexNotFound, errs.CodeNotFound, "builds finished", "builds dropped"} {
		if seen[c] == 0 {
			t.Errorf("no operation ended with %q: %v", c, seen)
		}
	}
	for k, least := range map[errs.Code]int{"index entries": 2000, "unindexed by RemoveLabel": 10, errs.CodeUniqueViolation: 15} {
		if seen[k] < least {
			t.Errorf("%q happened %d times, want %d: %v", k, seen[k], least, seen)
		}
	}
}

func runModel(t *testing.T, seed uint64, steps int, seen map[errs.Code]int) {
	rng := rand.New(rand.NewPCG(seed, 7))
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	g.MaxLabels, g.MaxProperties = 3, 2
	committed, m := newModel(), newModel()
	tx := begin(t, db)
	// Indexes from the start, so that most steps meet them: a unique one
	// over nodes and one over edges.
	for _, d := range []IndexDef{
		{Name: "i0", Scope: "A", Property: "p", Unique: true},
		{Name: "i1", Entity: EdgeEntity, Scope: "R", Property: "q"},
	} {
		do(t, tx, func(s *engine.Stmt) error { return g.CreateIndex(ctx, s, d) })
		d.State = index.Online
		m.indexes[d.Name] = d
	}
	var saves []*model
	for step := range steps {
		nodes, edges := slices.Sorted(maps.Keys(m.nodes)), slices.Sorted(maps.Keys(m.edges))
		op := rng.IntN(20)
		var want errs.Code
		var apply func(id uint64)
		var fn func(s *engine.Stmt) (uint64, error)
		switch op {
		case 0, 1: // CreateNode
			var labels []string
			for range rng.IntN(5) {
				labels = append(labels, pickName(rng, labelNames))
			}
			props := map[string]value.Value{}
			for range rng.IntN(3) {
				props[pickName(rng, propNames)] = randValue(rng)
			}
			var uniq []string
			for _, l := range labels {
				if !slices.Contains(uniq, l) {
					uniq = append(uniq, l)
				}
			}
			if len(uniq) > g.MaxLabels {
				want = errs.CodeLimitExceeded
			}
			fn = func(s *engine.Stmt) (uint64, error) { return g.CreateNode(ctx, s, labels, props) }
			apply = func(id uint64) { m.nodes[id] = &mNode{uniq, maps.Clone(props)} }
		case 2, 3, 4: // CreateEdge
			from, to := pick(rng, nodes), pick(rng, nodes)
			if rng.IntN(5) == 0 {
				to = from
			}
			typ := pickName(rng, typeNames)
			if m.nodes[from] == nil || m.nodes[to] == nil {
				want = errs.CodeNotFound
			}
			props := map[string]value.Value{}
			if rng.IntN(2) == 0 {
				props[pickName(rng, propNames)] = randValue(rng)
			}
			fn = func(s *engine.Stmt) (uint64, error) { return g.CreateEdge(ctx, s, from, to, typ, props) }
			apply = func(id uint64) { m.edges[id] = &mEdge{from, to, typ, maps.Clone(props)} }
		case 5: // DeleteEdge
			id := pick(rng, edges)
			if m.edges[id] == nil {
				want = errs.CodeNotFound
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.DeleteEdge(ctx, s, id) }
			apply = func(uint64) { delete(m.edges, id) }
		case 6, 7: // DeleteNode
			id, detach := pick(rng, nodes), rng.IntN(2) == 0
			switch {
			case m.nodes[id] == nil:
				want = errs.CodeNotFound
			case !detach && len(m.incident(id)) > 0:
				want = errs.CodeInvalidArgument
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.DeleteNode(ctx, s, id, detach) }
			apply = func(uint64) {
				for _, e := range m.incident(id) {
					delete(m.edges, e)
				}
				delete(m.nodes, id)
			}
		case 8: // AddLabel
			id, l := pick(rng, nodes), pickName(rng, labelNames)
			switch n := m.nodes[id]; {
			case n == nil:
				want = errs.CodeNotFound
			case !slices.Contains(n.labels, l) && len(n.labels) >= g.MaxLabels:
				want = errs.CodeLimitExceeded
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.AddLabel(ctx, s, id, l) }
			apply = func(uint64) {
				if n := m.nodes[id]; !slices.Contains(n.labels, l) {
					n.labels = append(n.labels, l)
				}
			}
		case 9: // RemoveLabel
			id, l := pick(rng, nodes), pickName(rng, labelNames)
			if m.nodes[id] == nil {
				want = errs.CodeNotFound
			}
			if n := m.nodes[id]; n != nil && slices.Contains(n.labels, l) {
				for _, d := range m.indexes {
					if _, ok := n.props[d.Property]; ok && d.Entity == NodeEntity && d.Scope == l {
						seen["unindexed by RemoveLabel"]++
					}
				}
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.RemoveLabel(ctx, s, id, l) }
			apply = func(uint64) {
				n := m.nodes[id]
				n.labels = slices.DeleteFunc(n.labels, func(x string) bool { return x == l })
			}
		case 10, 11: // SetProperty
			e, ids := NodeEntity, nodes
			if rng.IntN(2) == 0 {
				e, ids = EdgeEntity, edges
			}
			id, k, v := pick(rng, ids), pickName(rng, propNames), randValue(rng)
			props := m.props(e, id)
			switch _, has := props[k]; {
			case props == nil:
				want = errs.CodeNotFound
			case !has && len(props) >= g.MaxProperties:
				want = errs.CodeLimitExceeded
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.SetProperty(ctx, s, e, id, k, v) }
			apply = func(uint64) { m.props(e, id)[k] = v }
		case 12: // RemoveProperty
			e, ids := NodeEntity, nodes
			if rng.IntN(2) == 0 {
				e, ids = EdgeEntity, edges
			}
			id, k := pick(rng, ids), pickName(rng, propNames)
			if m.props(e, id) == nil {
				want = errs.CodeNotFound
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.RemoveProperty(ctx, s, e, id, k) }
			apply = func(uint64) { delete(m.props(e, id), k) }
		case 13: // savepoint, or roll back to one
			if len(saves) > 0 && rng.IntN(2) == 0 {
				sp := rng.IntN(len(saves))
				if err := tx.RollbackTo(sp); err != nil {
					t.Fatal(err)
				}
				m, saves = saves[sp].clone(), saves[:sp+1]
			} else if _, err := tx.Savepoint(); err != nil {
				t.Fatal(err)
			} else {
				saves = append(saves, m.clone())
			}
		case 14: // commit and begin again
			if _, err := tx.Commit(ctx); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
			committed, saves = m.clone(), nil
			r, err := db.BeginRead(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := dump(t, g, r); got != committed.dump() {
				t.Fatalf("step %d: committed\n%s\nmodel\n%s", step, got, committed.dump())
			}
			r.Close()
			tx = begin(t, db)
		case 15: // roll back
			tx.Rollback()
			m, saves = committed.clone(), nil
			tx = begin(t, db)
		case 16: // CreateIndex
			d := IndexDef{Name: "i" + strconv.Itoa(rng.IntN(4)), Property: pickName(rng, propNames), Unique: rng.IntN(2) == 0}
			if rng.IntN(2) == 0 {
				d.Entity, d.Scope = EdgeEntity, pickName(rng, typeNames)
			} else {
				d.Scope = pickName(rng, labelNames)
			}
			if old, ok := m.indexes[d.Name]; ok && !sameDef(old, d) {
				want = errs.CodeIndexExistsDifferent
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.CreateIndex(ctx, s, d) }
			apply = func(uint64) {
				if _, ok := m.indexes[d.Name]; !ok {
					d.State = index.Online
					m.indexes[d.Name] = d
				}
			}
		case 18: // the first phase of an online build
			d := IndexDef{Name: "b" + strconv.Itoa(rng.IntN(2)), Property: pickName(rng, propNames)}
			if rng.IntN(2) == 0 {
				d.Entity, d.Scope = EdgeEntity, pickName(rng, typeNames)
			} else {
				d.Scope = pickName(rng, labelNames)
			}
			if old, ok := m.indexes[d.Name]; ok && !sameDef(old, d) {
				want = errs.CodeIndexExistsDifferent
			}
			fn = func(s *engine.Stmt) (uint64, error) { _, err := g.StartBuild(ctx, s, d); return 0, err }
			apply = func(uint64) {
				if _, ok := m.indexes[d.Name]; !ok {
					d.State = index.Building
					m.indexes[d.Name] = d
				}
			}
		case 19: // a batch of one entity of a build, and its last phase
			var building []string
			for _, n := range slices.Sorted(maps.Keys(m.indexes)) {
				if m.indexes[n].State == index.Building {
					building = append(building, n)
				}
			}
			if len(building) == 0 {
				break
			}
			name := building[rng.IntN(len(building))]
			var finished bool
			fn = func(s *engine.Stmt) (uint64, error) {
				set, err := g.Indexes.Get(ctx, s)
				if err != nil {
					return 0, err
				}
				ix, _ := set.Named(name)
				_, done, err := g.BuildBatch(ctx, s, ix.ID, 1)
				if err != nil || !done {
					return 0, err
				}
				state, err := g.FinishBuild(ctx, s, ix.ID)
				finished = state == index.Online
				return 0, err
			}
			apply = func(uint64) {
				if finished {
					seen["builds finished"]++
					d := m.indexes[name]
					d.State = index.Online
					m.indexes[name] = d
				}
			}
		case 17: // DropIndex, now and then
			if rng.IntN(4) > 0 {
				break
			}
			name := "i" + strconv.Itoa(rng.IntN(4))
			if rng.IntN(3) == 0 {
				name = "b" + strconv.Itoa(rng.IntN(2)) // cancels a build
			}
			if _, ok := m.indexes[name]; !ok {
				want = errs.CodeIndexNotFound
			}
			fn = func(s *engine.Stmt) (uint64, error) { return 0, g.DropIndex(ctx, s, name) }
			apply = func(uint64) {
				if m.indexes[name].State == index.Building {
					seen["builds dropped"]++
				}
				delete(m.indexes, name)
			}
		}
		if fn != nil {
			var id uint64
			err := tx.Statement("model", func(s *engine.Stmt) error {
				var err error
				id, err = fn(s)
				return err
			})
			if want == "" {
				// The operation fails on uniqueness exactly when its effect
				// would leave two equal values in a unique index.
				saved := m
				m = m.clone()
				apply(1 << 62)
				if !m.unique() {
					want = errs.CodeUniqueViolation
				}
				m = saved
			}
			if errs.CodeOf(err) != want || (want == "" && err != nil) {
				t.Fatalf("step %d op %d: %v, want %q", step, op, err, want)
			}
			if err == nil {
				apply(id)
			}
			seen[errs.CodeOf(err)]++
			for _, d := range m.indexes {
				seen["index entries"] += len(m.indexed(d))
			}
		}
		check(t, g, tx, m, rng, step)
	}
	tx.Rollback()
}

func (m *model) props(e Entity, id uint64) map[string]value.Value {
	if e == EdgeEntity {
		if x := m.edges[id]; x != nil {
			return x.props
		}
		return nil
	}
	if x := m.nodes[id]; x != nil {
		return x.props
	}
	return nil
}

func check(t *testing.T, g *Graph, tx *engine.WriteTx, m *model, rng *rand.Rand, step int) {
	t.Helper()
	if got, want := dump(t, g, tx), m.dump(); got != want {
		t.Fatalf("step %d: dump\n%s\nmodel\n%s", step, got, want)
	}
	if st := tx.State(); st.NodeCount != uint64(len(m.nodes)) || st.EdgeCount != uint64(len(m.edges)) {
		t.Fatalf("step %d: state counts %d %d, model %d %d", step, st.NodeCount, st.EdgeCount, len(m.nodes), len(m.edges))
	}
	if err := g.Verify(ctx, tx); err != nil {
		t.Fatalf("step %d: %v", step, err)
	}
	var online []string
	for _, n := range slices.Sorted(maps.Keys(m.indexes)) {
		if m.indexes[n].State == index.Online {
			online = append(online, n)
		}
	}
	if names := online; len(names) > 0 {
		d := m.indexes[names[rng.IntN(len(names))]]
		set, err := g.Indexes.Get(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		ix, _ := set.Named(d.Name)
		v := randValue(rng)
		var want []uint64
		for id, w := range m.indexed(d) {
			if !v.IsNull() && value.Compare(v, w) == 0 {
				want = append(want, id)
			}
		}
		slices.Sort(want)
		it, err := index.Seek(ctx, tx, ix, index.Query{Op: index.Equal, Value: v})
		if err != nil {
			t.Fatal(err)
		}
		var got []uint64
		for it.Next(ctx) {
			got = append(got, it.Hit().Entity)
		}
		slices.Sort(got)
		if it.Err() != nil || !slices.Equal(got, want) {
			t.Fatalf("step %d: index %q seeks %v: %v, %v; model %v", step, d.Name, v, got, it.Err(), want)
		}
	}
	if len(m.nodes) == 0 {
		return
	}
	nodes := slices.Sorted(maps.Keys(m.nodes))
	n := nodes[rng.IntN(len(nodes))]
	for _, dir := range []Direction{Out, In, Both} {
		typ := ""
		var tid catalog.EdgeTypeID
		if rng.IntN(2) == 0 {
			typ = pickName(rng, typeNames)
			var ok bool
			tid, ok, _ = catalog.Lookup[catalog.EdgeTypeID](ctx, tx, typ)
			if !ok {
				continue
			}
		}
		got, err := Degree(ctx, tx, n, dir, tid)
		if err != nil || got != m.degree(n, dir, typ) {
			t.Fatalf("step %d: degree of %d dir %d type %q: %d, %v; model %d", step, n, dir, typ, got, err, m.degree(n, dir, typ))
		}
		var types []string
		if typ != "" {
			types = []string{typ}
		}
		c, err := g.Edges(ctx, tx, n, dir, types)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uint64
		for c.Next(ctx) {
			e := c.Edge()
			me := m.edges[e.ID]
			if me == nil || me.from != e.From || me.to != e.To || me.typ != e.Type {
				t.Fatalf("step %d: the cursor gives %+v, model %+v", step, e, me)
			}
			ids = append(ids, e.ID)
		}
		if c.Err() != nil || uint64(len(ids)) != got {
			t.Fatalf("step %d: cursor of %d dir %d: %d edges, %v; degree %d", step, n, dir, len(ids), c.Err(), got)
		}
	}
}

// sameDef compares declarations, whatever their states.
func sameDef(a, b IndexDef) bool {
	a.State, b.State = 0, 0
	return a == b
}
