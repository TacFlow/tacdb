package tacdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/exec"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/internal/query/sema"
	"github.com/TacFlow/tacdb/value"
)

// The oracle answers read queries by brute force over a graph in memory:
// patterns by enumeration of every node and edge, clauses one after the
// other over a table of bindings, with the same evaluator of expressions.
// It checks plans and operators, not expressions (design §7). Random
// queries from a grammar of the dialect run on the database and on the
// oracle, and their results must be the same multiset, or the same
// sequence when ORDER BY orders every column.

type oNode struct {
	id     uint64
	labels []string
	props  map[string]value.Value
}

type oEdge struct {
	id, from, to uint64
	typ          string
	props        map[string]value.Value
}

type oGraph struct {
	nodes []*oNode
	edges []*oEdge
}

func (g *oGraph) node(id uint64) *oNode {
	for _, n := range g.nodes {
		if n.id == id {
			return n
		}
	}
	return nil
}

func (g *oGraph) edge(id uint64) *oEdge {
	for _, e := range g.edges {
		if e.id == id {
			return e
		}
	}
	return nil
}

// props returns the properties of an entity; one deleted by a query that
// writes is NOT_FOUND, as in the database.
func (g *oGraph) props(k qv.Kind, id uint64) (map[string]value.Value, error) {
	if k == qv.KindEdge {
		if e := g.edge(id); e != nil {
			return e.props, nil
		}
	} else if n := g.node(id); n != nil {
		return n.props, nil
	}
	return nil, errs.Newf(errs.CodeNotFound, "oracle", "%s %d does not exist", k, id)
}

func (g *oGraph) Property(_ context.Context, k qv.Kind, id uint64, key string) (value.Value, bool, error) {
	props, err := g.props(k, id)
	v, ok := props[key]
	return v, ok, err
}

func (g *oGraph) Properties(_ context.Context, k qv.Kind, id uint64) (map[string]value.Value, error) {
	return g.props(k, id)
}

func (g *oGraph) Labels(_ context.Context, id uint64) ([]string, error) {
	if _, err := g.props(qv.KindNode, id); err != nil {
		return nil, err
	}
	return g.node(id).labels, nil
}

func (g *oGraph) EdgeType(_ context.Context, id uint64) (string, error) {
	if _, err := g.props(qv.KindEdge, id); err != nil {
		return "", err
	}
	return g.edge(id).typ, nil
}

// randomGraph builds a small graph in db and in memory.
func randomGraph(t *testing.T, db *DB, rng *rand.Rand) *oGraph {
	g := &oGraph{}
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	props := func() map[string]value.Value {
		p := map[string]value.Value{}
		if rng.IntN(4) > 0 {
			p["p"] = value.Int(int64(rng.IntN(4)))
		}
		if rng.IntN(3) == 0 {
			p["q"] = value.String(fmt.Sprintf("s%d", rng.IntN(3)))
		}
		return p
	}
	for range 2 + rng.IntN(6) {
		var labels []string
		for _, l := range []string{"A", "B"} {
			if rng.IntN(2) == 0 {
				labels = append(labels, l)
			}
		}
		p := props()
		n, err := tx.CreateNode(CreateNodeOptions{Labels: labels, Properties: p})
		if err != nil {
			t.Fatal(err)
		}
		g.nodes = append(g.nodes, &oNode{id: uint64(n.ID), labels: labels, props: p})
	}
	for range rng.IntN(10) {
		a, b := g.nodes[rng.IntN(len(g.nodes))], g.nodes[rng.IntN(len(g.nodes))]
		typ := []string{"R", "S"}[rng.IntN(2)]
		p := props()
		e, err := tx.CreateEdge(NodeID(a.id), NodeID(b.id), typ, p)
		if err != nil {
			t.Fatal(err)
		}
		g.edges = append(g.edges, &oEdge{id: uint64(e.ID), from: a.id, to: b.id, typ: typ, props: p})
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return g
}

// table is a table of bindings: every row has a value per name.
type table struct {
	names []string
	rows  []eval.Slots
}

func (t *table) Variable(name string) (int, bool) {
	i := slices.Index(t.names, name)
	return i, i >= 0
}

func (*table) Aggregate(*ast.FuncCall) (int, bool) { return 0, false }

type oracle struct {
	t     *testing.T
	g     *oGraph
	src   string
	env   *eval.Env
	stats exec.Mutations // the effects of the clauses that write
	next  uint64         // the last ID the oracle gave a new entity
}

func (o *oracle) compile(e ast.Expr, sc eval.Scope) *eval.Expr {
	x, err := eval.Compile(e, sc, o.src)
	if err != nil {
		o.t.Fatalf("%s: %v", o.src, err)
	}
	return x
}

func (o *oracle) eval(x *eval.Expr, row eval.Row) (qv.Value, error) { return x.Eval(o.env, row) }

// run answers q.
func (o *oracle) run(q *ast.Query) (*table, error) {
	tb := &table{rows: []eval.Slots{{}}}
	var err error
	for _, c := range q.Clauses {
		switch c := c.(type) {
		case *ast.Match:
			tb, err = o.match(tb, c)
		case *ast.Unwind:
			tb, err = o.unwind(tb, c)
		case *ast.With:
			tb, err = o.project(tb, c.Projection)
			if err == nil && c.Where != nil {
				tb, err = o.filter(tb, c.Where)
			}
		case *ast.Return:
			tb, err = o.project(tb, c.Projection)
		default:
			tb, err = o.write(tb, c)
		}
		if err != nil {
			return nil, err
		}
	}
	return tb, nil
}

func (o *oracle) filter(tb *table, where ast.Expr) (*table, error) {
	x := o.compile(where, tb)
	out := &table{names: tb.names}
	for _, r := range tb.rows {
		v, err := o.eval(x, r)
		if err != nil {
			return nil, err
		}
		if qv.Truthy(v) {
			out.rows = append(out.rows, r)
		}
	}
	return out, nil
}

func (o *oracle) unwind(tb *table, c *ast.Unwind) (*table, error) {
	x := o.compile(c.X, tb)
	out := &table{names: append(slices.Clone(tb.names), c.Alias)}
	for _, r := range tb.rows {
		v, err := o.eval(x, r)
		if err != nil {
			return nil, err
		}
		var items []qv.Value
		switch v.Kind() {
		case qv.KindNull:
		case qv.KindList:
			items = v.Items()
		default:
			items = []qv.Value{v}
		}
		for _, it := range items {
			out.rows = append(out.rows, append(slices.Clone(r), it))
		}
	}
	return out, nil
}

// element is a pattern element and the name the oracle gives it.
type element struct {
	name  string
	node  *ast.NodePattern
	rel   *ast.RelPattern
	fresh bool
}

// elements names the elements of patterns after the names in scope: a
// variable keeps its name, an anonymous element gets a hidden one. It
// returns the names in scope after the patterns and each pattern as a
// chain of elements.
func elements(scope []string, pats []*ast.Pattern) ([]string, [][]*element) {
	names := slices.Clone(scope)
	var chains [][]*element
	anon := 0
	name := func(v string) (string, bool) {
		if v == "" {
			anon++
			return fmt.Sprintf("\x00%d", anon), true
		}
		if slices.Contains(names, v) {
			return v, false
		}
		return v, true
	}
	for _, p := range pats {
		var chain []*element
		for i, n := range p.Nodes {
			nm, fresh := name(n.Var)
			if fresh {
				names = append(names, nm)
			}
			chain = append(chain, &element{name: nm, node: n, fresh: fresh})
			if i < len(p.Rels) {
				nm, fresh := name(p.Rels[i].Var)
				if fresh {
					names = append(names, nm)
				}
				chain = append(chain, &element{name: nm, rel: p.Rels[i], fresh: fresh})
			}
		}
		chains = append(chains, chain)
	}
	return names, chains
}

// match enumerates every assignment of the elements of the clause, for
// each row.
func (o *oracle) match(tb *table, c *ast.Match) (*table, error) {
	names, chains := elements(tb.names, c.Patterns)
	out := &table{names: names}
	var where *eval.Expr
	if c.Where != nil {
		where = o.compile(c.Where, out)
	}
	var tests []func(eval.Slots) (bool, error)
	for _, ch := range chains {
		for _, el := range ch {
			var m *ast.MapLit
			if el.node != nil {
				m = el.node.Properties
			} else {
				m = el.rel.Properties
			}
			if m == nil {
				continue
			}
			slot, _ := out.Variable(el.name)
			kind := qv.KindNode
			if el.rel != nil {
				kind = qv.KindEdge
			}
			for _, en := range m.Entries {
				x := o.compile(en.Value, out)
				key := en.Key
				tests = append(tests, func(r eval.Slots) (bool, error) {
					want, err := o.eval(x, r)
					if err != nil {
						return false, err
					}
					have, _, _ := o.g.Property(nil, kind, r[slot].ID(), key)
					return qv.Equals(qv.FromValue(have), want), nil
				})
			}
		}
	}
	for _, r := range tb.rows {
		row := make(eval.Slots, len(out.names))
		copy(row, r)
		found := false
		var err error
		var assign func(ci, ei int, used []uint64) bool
		emit := func() bool {
			for _, test := range tests {
				ok, e := test(row)
				if e != nil {
					err = e
					return false
				}
				if !ok {
					return true
				}
			}
			if where != nil {
				v, e := o.eval(where, row)
				if e != nil {
					err = e
					return false
				}
				if !qv.Truthy(v) {
					return true
				}
			}
			found = true
			out.rows = append(out.rows, slices.Clone(row))
			return true
		}
		assign = func(ci, ei int, used []uint64) bool {
			if ci == len(chains) {
				return emit()
			}
			ch := chains[ci]
			if ei == len(ch) {
				return assign(ci+1, 0, used)
			}
			el := ch[ei]
			slot, _ := out.Variable(el.name)
			if el.node != nil {
				var cands []uint64
				if ei > 0 {
					cands = []uint64{row[slot].ID()} // set by the relationship before
				} else if !el.fresh {
					if row[slot].Kind() != qv.KindNode {
						return true
					}
					cands = []uint64{row[slot].ID()}
				} else {
					for _, n := range o.g.nodes {
						cands = append(cands, n.id)
					}
				}
				for _, id := range cands {
					n := o.g.node(id)
					if !hasAll(n.labels, el.node.Labels) {
						continue
					}
					saved := row[slot]
					row[slot] = qv.Node(id)
					if !assign(ci, ei+1, used) {
						return false
					}
					row[slot] = saved
				}
				return true
			}
			// A relationship from the node before to the node after.
			fromSlot, _ := out.Variable(ch[ei-1].name)
			toEl := ch[ei+1]
			toSlot, _ := out.Variable(toEl.name)
			from := row[fromSlot].ID()
			toFixed := !toEl.fresh
			if toFixed && row[toSlot].Kind() != qv.KindNode {
				return true
			}
			try := func(to uint64, edges []uint64) bool {
				if toFixed && row[toSlot].ID() != to {
					return true
				}
				savedTo, savedRel := row[toSlot], row[slot]
				row[toSlot] = qv.Node(to)
				if el.rel.Length != nil {
					items := make([]qv.Value, len(edges))
					for i, id := range edges {
						items[i] = qv.Edge(id)
					}
					row[slot] = qv.List(items)
				} else {
					if !el.fresh && (row[slot].Kind() != qv.KindEdge || row[slot].ID() != edges[0]) {
						row[toSlot], row[slot] = savedTo, savedRel
						return true
					}
					row[slot] = qv.Edge(edges[0])
				}
				ok := assign(ci, ei+1, append(slices.Clone(used), edges...))
				row[toSlot], row[slot] = savedTo, savedRel
				return ok
			}
			if el.rel.Length == nil {
				for _, e := range o.g.edges {
					to, ok := o.step(e, from, el.rel)
					if !ok || slices.Contains(used, e.id) {
						continue
					}
					if !try(to, []uint64{e.id}) {
						return false
					}
				}
				return true
			}
			lo, hi := 1, 64
			if l := el.rel.Length; l.Min != nil {
				lo = int(*l.Min)
			}
			if l := el.rel.Length; l.Max != nil {
				hi = int(*l.Max)
				if l.Min == nil && hi < 1 {
					lo = hi
				}
			}
			var walk func(at uint64, path []uint64) bool
			walk = func(at uint64, path []uint64) bool {
				if len(path) >= lo {
					if !try(at, path) {
						return false
					}
				}
				if len(path) == hi {
					return true
				}
				for _, e := range o.g.edges {
					to, ok := o.step(e, at, el.rel)
					if !ok || slices.Contains(used, e.id) || slices.Contains(path, e.id) {
						continue
					}
					if !walk(to, append(slices.Clone(path), e.id)) {
						return false
					}
				}
				return true
			}
			return walk(from, nil)
		}
		assign(0, 0, nil)
		if err != nil {
			return nil, err
		}
		if !found && c.Optional {
			out.rows = append(out.rows, make(eval.Slots, len(out.names)))
			copy(out.rows[len(out.rows)-1], r)
		}
	}
	return out, nil
}

// step reports whether edge e goes from node at along r, and where to.
func (o *oracle) step(e *oEdge, at uint64, r *ast.RelPattern) (uint64, bool) {
	if len(r.Types) > 0 && !slices.Contains(r.Types, e.typ) {
		return 0, false
	}
	switch r.Direction {
	case ast.DirOut:
		return e.to, e.from == at
	case ast.DirIn:
		return e.from, e.to == at
	}
	if e.from == at {
		return e.to, true
	}
	return e.from, e.to == at
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// project computes a projection whose aggregates, if any, are whole
// items, and whose ORDER BY names items.
func (o *oracle) project(tb *table, p *ast.Projection) (*table, error) {
	out := &table{}
	type item struct {
		x   *eval.Expr
		agg *ast.FuncCall
		fn  *eval.Func
	}
	items := make([]item, len(p.Items))
	aggregating := false
	for i, it := range p.Items {
		name := it.Alias
		if name == "" {
			name = it.X.(*ast.Variable).Name
		}
		out.names = append(out.names, name)
		if f, ok := it.X.(*ast.FuncCall); ok {
			if fn, ok := eval.Lookup(f.Name); ok && fn.Aggregate {
				items[i] = item{agg: f, fn: fn}
				if len(f.Args) == 1 {
					items[i].x = o.compile(f.Args[0], tb)
				}
				aggregating = true
				continue
			}
		}
		items[i] = item{x: o.compile(it.X, tb)}
	}
	if !aggregating {
		for _, r := range tb.rows {
			row := make(eval.Slots, len(items))
			for i, it := range items {
				v, err := o.eval(it.x, r)
				if err != nil {
					return nil, err
				}
				row[i] = v
			}
			out.rows = append(out.rows, row)
		}
	} else {
		type grp struct {
			keys eval.Slots
			accs []eval.Accumulator
		}
		groups := map[string]*grp{}
		var order []*grp
		for _, r := range tb.rows {
			keys := make(eval.Slots, len(items))
			var key []byte
			for i, it := range items {
				if it.agg == nil {
					v, err := o.eval(it.x, r)
					if err != nil {
						return nil, err
					}
					keys[i] = v
					key = qv.AppendKey(key, v)
				}
			}
			g := groups[string(key)]
			if g == nil {
				g = &grp{keys: keys, accs: make([]eval.Accumulator, len(items))}
				for i, it := range items {
					if it.agg != nil {
						g.accs[i] = eval.NewAccumulator(it.fn, it.agg.Distinct, it.agg.Star || len(it.agg.Args) == 0, nil)
					}
				}
				groups[string(key)] = g
				order = append(order, g)
			}
			for i, it := range items {
				if it.agg == nil {
					continue
				}
				var v qv.Value
				if it.x != nil {
					var err error
					if v, err = o.eval(it.x, r); err != nil {
						return nil, err
					}
				}
				if err := g.accs[i].Add(v); err != nil {
					return nil, err
				}
			}
		}
		keyed := slices.ContainsFunc(items, func(it item) bool { return it.agg == nil })
		if len(order) == 0 && !keyed {
			g := &grp{keys: make(eval.Slots, len(items)), accs: make([]eval.Accumulator, len(items))}
			for i, it := range items {
				g.accs[i] = eval.NewAccumulator(it.fn, it.agg.Distinct, it.agg.Star || len(it.agg.Args) == 0, nil)
			}
			order = append(order, g)
		}
		for _, g := range order {
			row := slices.Clone(g.keys)
			for i, acc := range g.accs {
				if acc != nil {
					v, err := acc.Result()
					if err != nil {
						return nil, err
					}
					row[i] = v
				}
			}
			out.rows = append(out.rows, row)
		}
	}
	if p.Distinct {
		seen := map[string]bool{}
		var rows []eval.Slots
		for _, r := range out.rows {
			var key []byte
			for _, v := range r {
				key = qv.AppendKey(key, v)
			}
			if !seen[string(key)] {
				seen[string(key)] = true
				rows = append(rows, r)
			}
		}
		out.rows = rows
	}
	if len(p.Order) > 0 {
		sort.SliceStable(out.rows, func(i, j int) bool {
			for _, s := range p.Order {
				k := slices.Index(out.names, s.X.(*ast.Variable).Name)
				c := qv.Compare(out.rows[i][k], out.rows[j][k])
				if s.Desc {
					c = -c
				}
				if c != 0 {
					return c < 0
				}
			}
			return false
		})
	}
	n := func(x ast.Expr) int {
		i, _ := x.(*ast.Literal).Value.AsInt()
		return int(i)
	}
	if p.Skip != nil {
		out.rows = out.rows[min(n(p.Skip), len(out.rows)):]
	}
	if p.Limit != nil {
		out.rows = out.rows[:min(n(p.Limit), len(out.rows))]
	}
	return out, nil
}

// gen writes random read queries over the graphs of randomGraph.
type gen struct {
	rng   *rand.Rand
	b     strings.Builder
	nodes []string // node variables in scope
	rels  []string // relationship variables in scope
	n     int
	noID  bool // no predicate on IDs, which differ in the oracle of writes
	// noVarLen leaves out variable-length relationships, which the oracle
	// of reads covers and whose paths grow fast with the graphs of writes.
	noVarLen bool
}

func (g *gen) pick(xs []string) string { return xs[g.rng.IntN(len(xs))] }

func (g *gen) node(clause map[string]bool) {
	g.b.WriteByte('(')
	switch r := g.rng.IntN(10); {
	case r < 3 && len(g.nodes) > 0:
		v := g.pick(g.nodes)
		g.b.WriteString(v)
		g.b.WriteByte(')')
		return
	case r < 8:
		g.n++
		v := fmt.Sprintf("n%d", g.n)
		g.nodes = append(g.nodes, v)
		g.b.WriteString(v)
	}
	for _, l := range []string{"A", "B"} {
		if g.rng.IntN(3) == 0 {
			g.b.WriteString(":" + l)
		}
	}
	if g.rng.IntN(5) == 0 {
		fmt.Fprintf(&g.b, " {p: %d}", g.rng.IntN(4))
	}
	g.b.WriteByte(')')
}

func (g *gen) rel(clause map[string]bool) {
	left, right := "-", "-"
	switch g.rng.IntN(3) {
	case 0:
		right = "->"
	case 1:
		left = "<-"
	}
	g.b.WriteString(left + "[")
	varLen := !g.noVarLen && g.rng.IntN(5) == 0
	if !varLen && g.rng.IntN(2) == 0 {
		g.n++
		v := fmt.Sprintf("r%d", g.n)
		g.rels = append(g.rels, v)
		g.b.WriteString(v)
	}
	switch g.rng.IntN(4) {
	case 1:
		g.b.WriteString(":R")
	case 2:
		g.b.WriteString(":S")
	case 3:
		g.b.WriteString(":R|S")
	}
	if varLen {
		g.b.WriteString(g.pick([]string{"*1..2", "*0..1", "*2", "*..2", "*", "*0"}))
	} else if g.rng.IntN(8) == 0 {
		fmt.Fprintf(&g.b, " {p: %d}", g.rng.IntN(4))
	}
	g.b.WriteString("]" + right)
}

func (g *gen) match(optional bool) {
	if optional {
		g.b.WriteString("OPTIONAL ")
	}
	g.b.WriteString("MATCH ")
	clause := map[string]bool{}
	for i := range 1 + g.rng.IntN(2) {
		if i > 0 {
			g.b.WriteString(", ")
		}
		g.node(clause)
		for range g.rng.IntN(3) {
			g.rel(clause)
			g.node(clause)
		}
	}
	if g.rng.IntN(2) == 0 {
		g.b.WriteString(" WHERE " + g.pred())
	}
	g.b.WriteByte(' ')
}

func (g *gen) pred() string {
	if len(g.nodes) == 0 {
		return "true"
	}
	v := g.pick(g.nodes)
	k := g.rng.IntN(4)
	switch g.rng.IntN(6) {
	case 0:
		return fmt.Sprintf("%s.p > %d", v, k)
	case 1:
		return fmt.Sprintf("%s.p = %d", v, k)
	case 2:
		return v + ".p IS NULL"
	case 3:
		if g.noID {
			return fmt.Sprintf("%s.p < %d", v, k)
		}
		if g.rng.IntN(3) == 0 {
			return fmt.Sprintf("id(%s) = %d.%d", v, 1+g.rng.IntN(6), g.rng.IntN(2)*5)
		}
		return fmt.Sprintf("id(%s) = %d", v, 1+g.rng.IntN(6))
	case 4:
		return fmt.Sprintf("%s.p <> %d OR %s.q = 's1'", v, k, g.pick(g.nodes))
	}
	return fmt.Sprintf("NOT %s.q IS NULL", v)
}

// scalar is an expression of one variable in scope.
func (g *gen) scalar() string {
	if len(g.rels) > 0 && g.rng.IntN(4) == 0 {
		r := g.pick(g.rels)
		return g.pick([]string{r, "type(" + r + ")", r + ".p"})
	}
	v := g.pick(g.nodes)
	return g.pick([]string{v, v + ".p", "id(" + v + ")", "labels(" + v + ")", v + ".q"})
}

func (g *gen) query() string {
	g.b.Reset()
	g.nodes, g.rels, g.n = nil, nil, 0
	g.match(false)
	if g.rng.IntN(3) == 0 {
		g.match(g.rng.IntN(2) == 0)
	}
	if g.rng.IntN(5) == 0 && len(g.nodes) > 0 {
		keep := []string{g.pick(g.nodes)}
		if v := g.pick(g.nodes); v != keep[0] {
			keep = append(keep, v)
		}
		g.b.WriteString("WITH " + strings.Join(keep, ", "))
		g.nodes, g.rels = keep, nil
		if g.rng.IntN(2) == 0 {
			g.b.WriteString(" WHERE " + g.pred())
		}
		g.b.WriteByte(' ')
		if g.rng.IntN(2) == 0 {
			g.match(g.rng.IntN(2) == 0)
		}
	}
	if g.rng.IntN(5) == 0 {
		g.b.WriteString("UNWIND [1, 2] AS u ")
	}
	if len(g.nodes) == 0 {
		g.b.WriteString("RETURN count(*) AS c0")
		return g.b.String()
	}
	g.b.WriteString("RETURN ")
	var aliases []string
	item := func(x string) {
		a := fmt.Sprintf("c%d", len(aliases))
		if len(aliases) > 0 {
			g.b.WriteString(", ")
		}
		g.b.WriteString(x + " AS " + a)
		aliases = append(aliases, a)
	}
	if g.rng.IntN(3) == 0 {
		for range g.rng.IntN(3) {
			item(g.scalar())
		}
		for range 1 + g.rng.IntN(2) {
			v := g.pick(g.nodes)
			item(g.pick([]string{"count(*)", "count(" + v + ".p)", "sum(" + v + ".p)", "min(" + v + ".p)",
				"max(" + v + ".q)", "count(DISTINCT " + v + ".p)", "avg(" + v + ".p)", "count(" + v + ")"}))
		}
	} else {
		if g.rng.IntN(3) == 0 {
			s := g.b.String()
			g.b.Reset()
			g.b.WriteString(s + "DISTINCT ")
		}
		for range 1 + g.rng.IntN(3) {
			item(g.scalar())
		}
	}
	if g.rng.IntN(2) == 0 {
		g.b.WriteString(" ORDER BY ")
		for i, a := range aliases {
			if i > 0 {
				g.b.WriteString(", ")
			}
			g.b.WriteString(a + g.pick([]string{"", " DESC"}))
		}
		if g.rng.IntN(3) == 0 {
			fmt.Fprintf(&g.b, " SKIP %d", g.rng.IntN(3))
		}
		if g.rng.IntN(2) == 0 {
			fmt.Fprintf(&g.b, " LIMIT %d", g.rng.IntN(4))
		}
	}
	return g.b.String()
}

// fromQuery turns a value of a result back into a runtime value, for
// comparison.
func fromQuery(v QueryValue) qv.Value {
	switch v.Type() {
	case TypeNode:
		n, _ := v.Node()
		return qv.Node(uint64(n.ID))
	case TypeEdge:
		e, _ := v.Edge()
		return qv.Edge(uint64(e.ID))
	case TypeList:
		l, _ := v.List()
		items := make([]qv.Value, len(l))
		for i, it := range l {
			items[i] = fromQuery(it)
		}
		return qv.List(items)
	}
	x, _ := v.Value()
	return qv.FromValue(x)
}

func rowString(r []qv.Value) string {
	s := make([]string, len(r))
	for i, v := range r {
		s[i] = v.String()
	}
	return strings.Join(s, " | ")
}

func TestOracle(t *testing.T) {
	seeds, queries := 200, 30
	if testing.Short() || raceDetector {
		seeds = 40
	}
	for seed := range uint64(seeds) {
		rng := rand.New(rand.NewPCG(seed, 7))
		db, err := OpenMemory(DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		g := randomGraph(t, db, rng)
		gn := &gen{rng: rng}
		for range queries {
			src := gn.query()
			checkOracle(t, db, g, src)
		}
		db.Close()
	}
}

func checkOracle(t *testing.T, db *DB, g *oGraph, src string) {
	t.Helper()
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	_, serr := sema.Analyze(q, src, sema.Config{})
	rows, err := db.Query(ctx, src, nil)
	if serr != nil {
		if err == nil || code(err) != code(serr) {
			t.Fatalf("%s: analysis %v, query %v", src, serr, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	var got []string
	for rows.Next() {
		var r []qv.Value
		for _, v := range rows.Values() {
			r = append(r, fromQuery(v))
		}
		got = append(got, rowString(r))
	}
	o := &oracle{t: t, g: g, src: src, env: &eval.Env{Ctx: ctx, Graph: g}}
	want, oerr := o.run(q)
	if rows.Err() != nil || oerr != nil {
		if code(rows.Err()) != code(oerr) {
			t.Fatalf("%s: query %v, oracle %v", src, rows.Err(), oerr)
		}
		return
	}
	var exp []string
	for _, r := range want.rows {
		exp = append(exp, rowString(r))
	}
	ordered := strings.Contains(src, "ORDER BY")
	if !ordered {
		sort.Strings(got)
		sort.Strings(exp)
	}
	if !slices.Equal(got, exp) {
		t.Fatalf("%s:\n got  %q\n want %q", src, got, exp)
	}
}
