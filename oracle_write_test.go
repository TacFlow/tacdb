package tacdb

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// The oracle of writes applies the clauses that write by brute force to
// the graph in memory, row after row, with the semantics of D-017: every
// clause reads all its rows before it writes. Random write queries run on
// the database with Exec and on the oracle; the rows, the counters and
// the graph after them must be the same, and a query that fails must
// leave both graphs as they were (QRY-05). The queries never make a
// result depend on the order of rows, which the plan chooses: values come
// from constants or from the entity they change.

func (g *oGraph) clone() *oGraph {
	c := &oGraph{}
	for _, n := range g.nodes {
		c.nodes = append(c.nodes, &oNode{id: n.id, labels: slices.Clone(n.labels), props: maps.Clone(n.props)})
	}
	for _, e := range g.edges {
		c.edges = append(c.edges, &oEdge{id: e.id, from: e.from, to: e.to, typ: e.typ, props: maps.Clone(e.props)})
	}
	return c
}

// dump renders the graph without IDs: each node by its labels and
// properties, each edge by its endpoints, type and properties, sorted.
func (g *oGraph) dump() []string {
	text := map[uint64]string{}
	var out []string
	for _, n := range g.nodes {
		text[n.id] = nodeText(n.labels, n.props)
		out = append(out, text[n.id])
	}
	for _, e := range g.edges {
		out = append(out, edgeText(text[e.from], e.typ, e.props, text[e.to]))
	}
	sort.Strings(out)
	return out
}

func (o *oracle) write(tb *table, c ast.Clause) (*table, error) {
	switch c := c.(type) {
	case *ast.Create:
		return o.create(tb, c)
	case *ast.Merge:
		return o.merge(tb, c)
	case *ast.Set:
		sets := o.setItems(c.Items, tb)
		return tb, o.each(tb, sets)
	case *ast.Remove:
		sets := o.removeItems(c.Items, tb)
		return tb, o.each(tb, sets)
	case *ast.Delete:
		return tb, o.delete(tb, c)
	}
	o.t.Fatalf("%s: clause %T", o.src, c)
	return nil, nil
}

func (o *oracle) each(tb *table, items []func(eval.Slots) error) error {
	for _, r := range tb.rows {
		for _, it := range items {
			if err := it(r); err != nil {
				return err
			}
		}
	}
	return nil
}

type oProp struct {
	key string
	x   *eval.Expr
}

func (o *oracle) props(m *ast.MapLit, sc *table) []oProp {
	if m == nil {
		return nil
	}
	var out []oProp
	for _, en := range m.Entries {
		out = append(out, oProp{en.Key, o.compile(en.Value, sc)})
	}
	return out
}

// values evaluates the property map of a new element: Null writes
// nothing.
func (o *oracle) values(ps []oProp, r eval.Slots) (map[string]value.Value, error) {
	out := map[string]value.Value{}
	for _, p := range ps {
		v, err := o.eval(p.x, r)
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
		out[p.key] = pv
	}
	return out, nil
}

func (o *oracle) newNode(labels []string, props map[string]value.Value) uint64 {
	o.next++
	o.g.nodes = append(o.g.nodes, &oNode{id: o.next, labels: dedupLabels(labels), props: props})
	o.stats.NodesCreated++
	o.stats.LabelsAdded += uint64(len(dedupLabels(labels)))
	o.stats.PropertiesSet += uint64(len(props))
	return o.next
}

func (o *oracle) newEdge(from, to qv.Value, typ string, props map[string]value.Value) (uint64, error) {
	for _, v := range []qv.Value{from, to} {
		if v.Kind() != qv.KindNode {
			return 0, errs.New(errs.CodeTypeMismatch, "oracle", "endpoint")
		}
		if o.g.node(v.ID()) == nil {
			return 0, errs.New(errs.CodeNotFound, "oracle", "endpoint")
		}
	}
	o.next++
	o.g.edges = append(o.g.edges, &oEdge{id: o.next, from: from.ID(), to: to.ID(), typ: typ, props: props})
	o.stats.EdgesCreated++
	o.stats.PropertiesSet += uint64(len(props))
	return o.next, nil
}

// create creates the elements of the patterns for each row.
func (o *oracle) create(tb *table, c *ast.Create) (*table, error) {
	out := &table{names: slices.Clone(tb.names)}
	type step struct {
		slot, from, to int
		labels         []string
		typ            string
		props          []oProp
	}
	var steps []step
	anon := 0
	slot := func(v string) (int, bool) {
		if v != "" {
			if i, ok := out.Variable(v); ok {
				return i, false
			}
		} else {
			anon++
			v = fmt.Sprintf("\x00c%d", anon)
		}
		out.names = append(out.names, v)
		return len(out.names) - 1, true
	}
	for _, p := range c.Patterns {
		slots := make([]int, len(p.Nodes))
		for i, n := range p.Nodes {
			s, fresh := slot(n.Var)
			slots[i] = s
			if fresh {
				steps = append(steps, step{slot: s, labels: n.Labels, props: o.props(n.Properties, out), from: -1})
			}
			if i == 0 {
				continue
			}
			r := p.Rels[i-1]
			s, _ = slot(r.Var)
			from, to := slots[i-1], slots[i]
			if r.Direction == ast.DirIn {
				from, to = to, from
			}
			steps = append(steps, step{slot: s, typ: r.Types[0], from: from, to: to, props: o.props(r.Properties, out)})
		}
	}
	for _, r := range tb.rows {
		row := make(eval.Slots, len(out.names))
		copy(row, r)
		for _, st := range steps {
			props, err := o.values(st.props, row)
			if err != nil {
				return nil, err
			}
			if st.from < 0 {
				row[st.slot] = qv.Node(o.newNode(st.labels, props))
				continue
			}
			id, err := o.newEdge(row[st.from], row[st.to], st.typ, props)
			if err != nil {
				return nil, err
			}
			row[st.slot] = qv.Edge(id)
		}
		out.rows = append(out.rows, row)
	}
	return out, nil
}

// merge matches the whole pattern for each row, after the rows before;
// without a match it creates the elements not bound before.
func (o *oracle) merge(tb *table, c *ast.Merge) (*table, error) {
	names, chains := elements(tb.names, []*ast.Pattern{c.Pattern})
	out := &table{names: names}
	var checks []*eval.Expr
	for _, el := range chains[0] {
		var m *ast.MapLit
		if el.node != nil {
			m = el.node.Properties
		} else {
			m = el.rel.Properties
		}
		for _, p := range o.props(m, tb) {
			checks = append(checks, p.x)
		}
	}
	var onCreate, onMatch []func(eval.Slots) error
	for _, a := range c.Actions {
		items := o.setItems(a.Items, out)
		if a.OnCreate {
			onCreate = append(onCreate, items...)
		} else {
			onMatch = append(onMatch, items...)
		}
	}
	for _, r := range tb.rows {
		for _, x := range checks {
			v, err := o.eval(x, r)
			if err != nil {
				return nil, err
			}
			if v.Kind() == qv.KindNull {
				return nil, errs.New(errs.CodeInvalidArgument, "oracle", "MERGE with null")
			}
		}
		m, err := o.match(&table{names: tb.names, rows: []eval.Slots{r}}, &ast.Match{Patterns: []*ast.Pattern{c.Pattern}})
		if err != nil {
			return nil, err
		}
		if len(m.rows) > 0 {
			for _, mr := range m.rows {
				for _, f := range onMatch {
					if err := f(mr); err != nil {
						return nil, err
					}
				}
			}
			out.rows = append(out.rows, m.rows...)
			continue
		}
		row := make(eval.Slots, len(names))
		copy(row, r)
		made := map[string]bool{}
		ch := chains[0]
		for i, el := range ch {
			slot, _ := out.Variable(el.name)
			if el.node != nil {
				if el.fresh && !made[el.name] {
					made[el.name] = true
					props, err := o.values(o.props(el.node.Properties, out), row)
					if err != nil {
						return nil, err
					}
					row[slot] = qv.Node(o.newNode(el.node.Labels, props))
				}
				continue
			}
			a, _ := out.Variable(ch[i-1].name)
			b, _ := out.Variable(ch[i+1].name)
			// The node after the relationship comes first if it is new.
			if bn := ch[i+1]; bn.fresh && !made[bn.name] {
				made[bn.name] = true
				props, err := o.values(o.props(bn.node.Properties, out), row)
				if err != nil {
					return nil, err
				}
				row[b] = qv.Node(o.newNode(bn.node.Labels, props))
			}
			if el.rel.Direction == ast.DirIn {
				a, b = b, a
			}
			props, err := o.values(o.props(el.rel.Properties, out), row)
			if err != nil {
				return nil, err
			}
			id, err := o.newEdge(row[a], row[b], el.rel.Types[0], props)
			if err != nil {
				return nil, err
			}
			row[slot] = qv.Edge(id)
		}
		for _, f := range onCreate {
			if err := f(row); err != nil {
				return nil, err
			}
		}
		out.rows = append(out.rows, row)
	}
	return out, nil
}

// entity returns the properties of the entity an item changes; ok is
// false for Null.
func (o *oracle) entity(v qv.Value, labels bool) (*oNode, map[string]value.Value, bool, error) {
	switch v.Kind() {
	case qv.KindNull:
		return nil, nil, false, nil
	case qv.KindNode:
		n := o.g.node(v.ID())
		if n == nil {
			return nil, nil, false, errs.New(errs.CodeNotFound, "oracle", "deleted")
		}
		return n, n.props, true, nil
	case qv.KindEdge:
		if labels {
			return nil, nil, false, errs.New(errs.CodeTypeMismatch, "oracle", "labels of an edge")
		}
		e := o.g.edge(v.ID())
		if e == nil {
			return nil, nil, false, errs.New(errs.CodeNotFound, "oracle", "deleted")
		}
		return nil, e.props, true, nil
	}
	return nil, nil, false, errs.New(errs.CodeTypeMismatch, "oracle", "not an entity")
}

func (o *oracle) setProp(props map[string]value.Value, k string, v qv.Value) error {
	if v.Kind() == qv.KindNull {
		if _, ok := props[k]; ok {
			delete(props, k)
			o.stats.PropertiesSet++
		}
		return nil
	}
	pv, err := qv.ToValue(v)
	if err != nil {
		return err
	}
	props[k] = pv
	o.stats.PropertiesSet++
	return nil
}

func (o *oracle) setItems(items []ast.SetItem, sc *table) []func(eval.Slots) error {
	var out []func(eval.Slots) error
	for _, it := range items {
		switch it := it.(type) {
		case *ast.SetProperty:
			slot, _ := sc.Variable(it.Target.X.(*ast.Variable).Name)
			x, key := o.compile(it.Value, sc), it.Target.Key
			out = append(out, func(r eval.Slots) error {
				_, props, ok, err := o.entity(r[slot], false)
				if err != nil || !ok {
					return err
				}
				v, err := o.eval(x, r)
				if err != nil {
					return err
				}
				return o.setProp(props, key, v)
			})
		case *ast.SetProperties:
			slot, _ := sc.Variable(it.Var)
			x, replace := o.compile(it.Value, sc), !it.Merge
			out = append(out, func(r eval.Slots) error {
				if r[slot].Kind() == qv.KindNull {
					return nil
				}
				v, err := o.eval(x, r)
				if err != nil {
					return err
				}
				if v.Kind() == qv.KindNull {
					_, _, _, err := o.entity(r[slot], false)
					return err
				}
				if v.Kind() != qv.KindMap {
					return errs.New(errs.CodeTypeMismatch, "oracle", "not a map")
				}
				_, props, _, err := o.entity(r[slot], false)
				if err != nil {
					return err
				}
				for i := 0; i < v.Len(); i++ {
					if _, it := v.EntryAt(i); it.Kind() != qv.KindNull {
						if _, err := qv.ToValue(it); err != nil {
							return err
						}
					}
				}
				keep := map[string]bool{}
				for i := 0; i < v.Len(); i++ {
					k, it := v.EntryAt(i)
					keep[k] = it.Kind() != qv.KindNull
				}
				for _, k := range slices.Sorted(maps.Keys(props)) {
					if !keep[k] && (replace || !keep[k] && hasKey(v, k)) {
						delete(props, k)
						o.stats.PropertiesSet++
					}
				}
				for i := 0; i < v.Len(); i++ {
					if k, it := v.EntryAt(i); it.Kind() != qv.KindNull {
						if err := o.setProp(props, k, it); err != nil {
							return err
						}
					}
				}
				return nil
			})
		case *ast.SetLabels:
			slot, _ := sc.Variable(it.Var)
			labels := it.Labels
			out = append(out, func(r eval.Slots) error {
				n, _, ok, err := o.entity(r[slot], true)
				if err != nil || !ok {
					return err
				}
				for _, l := range dedupLabels(labels) {
					if !slices.Contains(n.labels, l) {
						n.labels = append(n.labels, l)
						o.stats.LabelsAdded++
					}
				}
				return nil
			})
		}
	}
	return out
}

func hasKey(m qv.Value, k string) bool {
	for i := 0; i < m.Len(); i++ {
		if key, _ := m.EntryAt(i); key == k {
			return true
		}
	}
	return false
}

func (o *oracle) removeItems(items []ast.RemoveItem, sc *table) []func(eval.Slots) error {
	var out []func(eval.Slots) error
	for _, it := range items {
		switch it := it.(type) {
		case *ast.RemoveProperty:
			slot, _ := sc.Variable(it.Target.X.(*ast.Variable).Name)
			key := it.Target.Key
			out = append(out, func(r eval.Slots) error {
				_, props, ok, err := o.entity(r[slot], false)
				if err != nil || !ok {
					return err
				}
				return o.setProp(props, key, qv.Null())
			})
		case *ast.RemoveLabels:
			slot, _ := sc.Variable(it.Var)
			labels := it.Labels
			out = append(out, func(r eval.Slots) error {
				n, _, ok, err := o.entity(r[slot], true)
				if err != nil || !ok {
					return err
				}
				for _, l := range dedupLabels(labels) {
					if i := slices.Index(n.labels, l); i >= 0 {
						n.labels = slices.Delete(n.labels, i, i+1)
						o.stats.LabelsRemoved++
					}
				}
				return nil
			})
		}
	}
	return out
}

// delete deletes, over every row, the relationships, then the nodes; a
// node with relationships left needs DETACH.
func (o *oracle) delete(tb *table, c *ast.Delete) error {
	var xs []*eval.Expr
	for _, e := range c.Exprs {
		xs = append(xs, o.compile(e, tb))
	}
	var nodes, edges []uint64
	for _, r := range tb.rows {
		for _, x := range xs {
			v, err := o.eval(x, r)
			if err != nil {
				return err
			}
			switch v.Kind() {
			case qv.KindNull:
			case qv.KindNode:
				nodes = append(nodes, v.ID())
			case qv.KindEdge:
				edges = append(edges, v.ID())
			default:
				return errs.New(errs.CodeTypeMismatch, "oracle", "not an entity")
			}
		}
	}
	dropEdge := func(id uint64) {
		o.g.edges = slices.DeleteFunc(o.g.edges, func(e *oEdge) bool { return e.id == id })
		o.stats.EdgesDeleted++
	}
	for _, id := range edges {
		if o.g.edge(id) != nil {
			dropEdge(id)
		}
	}
	for _, id := range nodes {
		if o.g.node(id) == nil {
			continue
		}
		var inc []uint64
		for _, e := range o.g.edges {
			if e.from == id || e.to == id {
				inc = append(inc, e.id)
			}
		}
		if len(inc) > 0 && !c.Detach {
			return errs.New(errs.CodeInvalidArgument, "oracle", "node with edges")
		}
		for _, e := range inc {
			dropEdge(e)
		}
		o.g.nodes = slices.DeleteFunc(o.g.nodes, func(n *oNode) bool { return n.id == id })
		o.stats.NodesDeleted++
	}
	return nil
}

// wgen writes random queries that write over the graphs of randomGraph.
type wgen struct{ gen }

func (g *wgen) k() int { return g.rng.IntN(4) }

func (g *wgen) setItem(v string, rel bool) string {
	if rel {
		return g.pick([]string{
			fmt.Sprintf("%s.p = %d", v, g.k()), v + ".p = " + v + ".p + 1", v + ".q = null",
			v + " += {p: 3, q: null}", v + " = {q: 's0'}"})
	}
	return g.pick([]string{
		fmt.Sprintf("%s.p = %d", v, g.k()), v + ".p = " + v + ".p + 1", v + ".q = null", v + ".q = 's2'",
		v + " += {q: 's1', p: null}", fmt.Sprintf("%s = {p: %d}", v, g.k()), v + ":B", v + ":A:B",
		v + ".l = [" + v + ".p, 'x']", v + " = {}", v + ".bad = " + v, v + " += {o: [" + v + "]}"})
}

func (g *wgen) removeItem(v string, rel bool) string {
	if rel {
		return v + ".p"
	}
	return g.pick([]string{v + ".p", v + ".q", v + ":A", v + ":B", v + ":A:B"})
}

// matchClause writes a MATCH without predicates on IDs.
func (g *wgen) matchClause() {
	g.noID, g.noVarLen = true, true
	g.match(g.rng.IntN(6) == 0)
	if len(g.nodes) == 0 {
		g.b.WriteString("MATCH (z) ")
		g.nodes = []string{"z"}
	}
}

func (g *wgen) target() (string, bool) {
	if len(g.rels) > 0 && g.rng.IntN(4) == 0 {
		return g.pick(g.rels), true
	}
	return g.pick(g.nodes), false
}

func (g *wgen) write() {
	switch g.rng.IntN(12) {
	case 0: // CREATE alone
		fmt.Fprintf(&g.b, "CREATE (x:A {p: %d})", g.k())
		if g.rng.IntN(2) == 0 {
			fmt.Fprintf(&g.b, "-[e:R {p: %d}]->(y:B)", g.k())
			g.rels = append(g.rels, "e")
			g.nodes = append(g.nodes, "y")
		}
		g.nodes = append(g.nodes, "x")
	case 1: // CREATE over matched rows
		g.matchClause()
		v := g.pick(g.nodes)
		switch g.rng.IntN(3) {
		case 0:
			g.b.WriteString("CREATE (" + v + ")-[e:S]->(x:A {p: " + v + ".p})")
			g.nodes, g.rels = append(g.nodes, "x"), append(g.rels, "e")
		case 1:
			g.b.WriteString("CREATE (" + v + ")<-[e:R]-(" + v + ")")
			g.rels = append(g.rels, "e")
		default:
			g.b.WriteString("CREATE (x:B {q: 's0'})")
			g.nodes = append(g.nodes, "x")
		}
	case 2, 3: // SET
		g.matchClause()
		// The items of one SET change one variable: two variables may be
		// the same entity in different rows, and then the order of the
		// rows would decide its value.
		g.b.WriteString("SET ")
		v, rel := g.target()
		for i := range 1 + g.rng.IntN(2) {
			if i > 0 {
				g.b.WriteString(", ")
			}
			g.b.WriteString(g.setItem(v, rel))
		}
	case 4: // REMOVE
		g.matchClause()
		v, rel := g.target()
		g.b.WriteString("REMOVE " + g.removeItem(v, rel))
	case 5: // DELETE
		g.matchClause()
		if len(g.rels) > 0 && g.rng.IntN(2) == 0 {
			r := g.pick(g.rels)
			if g.rng.IntN(2) == 0 {
				g.b.WriteString("DELETE " + r)
				g.forget(r)
				return
			}
			v := g.pick(g.nodes)
			g.b.WriteString("DELETE " + r + ", " + v)
			g.forget(r, v)
			return
		}
		v := g.pick(g.nodes)
		g.b.WriteString(g.pick([]string{"DETACH DELETE ", "DELETE "}) + v)
		g.forget(v)
	case 6: // MERGE of a node
		if g.rng.IntN(2) == 0 {
			g.matchClause()
			v := g.pick(g.nodes)
			g.b.WriteString("MERGE (x:A {p: " + v + ".p})")
		} else {
			fmt.Fprintf(&g.b, "MERGE (x:%s {p: %d})", g.pick([]string{"A", "B", "A:B"}), g.k())
		}
		g.nodes = append(g.nodes, "x")
		g.actions("x")
	case 7: // MERGE of a relationship between matched nodes
		fmt.Fprintf(&g.b, "MATCH (a:A), (b:B) WHERE a.p <= %d ", g.k())
		g.nodes = []string{"a", "b"}
		g.b.WriteString("MERGE (a)-[e:" + g.pick([]string{"R", "S"}) + "]->(b)")
		g.rels = []string{"e"}
		g.actions("e")
	case 8: // MERGE of a whole pattern
		fmt.Fprintf(&g.b, "MERGE (x:A {p: %d})-[e:R]->(y:B {p: %d})", g.k(), g.k())
		g.nodes, g.rels = []string{"x", "y"}, []string{"e"}
		g.actions("x")
	case 9: // CREATE of a path with mixed directions
		fmt.Fprintf(&g.b, "CREATE (x:A {p: %d})<-[e:R]-(y:B)-[f:S {p: %d}]->(z)<-[h:R]-(x)", g.k(), g.k())
		g.nodes, g.rels = []string{"x", "y", "z"}, []string{"e", "f", "h"}
	case 10: // MERGE of a pattern against the text or without direction
		fmt.Fprintf(&g.b, "MERGE (x:A {p: %d})%s(y:B {p: %d})%s(z:A {p: %d})", g.k(),
			g.pick([]string{"<-[e:R]-", "-[e:S]-", "-[e:R]->"}), g.k(), g.pick([]string{"<-[f:S]-", "-[f:R]->"}), g.k())
		g.nodes, g.rels = []string{"x", "y", "z"}, []string{"e", "f"}
	case 11: // ON MATCH changes the key the next rows match
		g.b.WriteString("UNWIND [1, 2, 3] AS i MERGE (x:A {p: 0}) ON MATCH SET x.p = i")
		g.nodes = []string{"x"}
	}
}

// actions writes the ON CREATE and ON MATCH of a MERGE, which change
// what no pattern reads, so that the order of rows does not matter.
func (g *wgen) actions(v string) {
	if g.rng.IntN(2) == 0 {
		g.b.WriteString(" ON CREATE SET " + v + ".m = 1")
		if !strings.HasPrefix(v, "e") && g.rng.IntN(2) == 0 {
			g.b.WriteString(", " + v + ":C")
		}
	}
	if g.rng.IntN(2) == 0 {
		g.b.WriteString(" ON MATCH SET " + v + ".c = coalesce(" + v + ".c, 0) + 1")
	}
}

func (g *wgen) forget(vs ...string) {
	for _, v := range vs {
		g.nodes = slices.DeleteFunc(g.nodes, func(x string) bool { return x == v })
		g.rels = slices.DeleteFunc(g.rels, func(x string) bool { return x == v })
	}
}

func (g *wgen) scalar() string {
	if len(g.rels) > 0 && g.rng.IntN(4) == 0 {
		r := g.pick(g.rels)
		return g.pick([]string{"type(" + r + ")", r + ".p"})
	}
	v := g.pick(g.nodes)
	return g.pick([]string{v + ".p", "labels(" + v + ")", v + ".q", v + ".c", v + ".m"})
}

func (g *wgen) query() string {
	g.b.Reset()
	g.nodes, g.rels, g.n = nil, nil, 0
	g.write()
	if g.rng.IntN(4) == 0 && len(g.nodes) > 0 && !strings.Contains(g.b.String(), "DELETE") {
		v, rel := g.target()
		g.b.WriteString(" SET " + g.setItem(v, rel))
	}
	switch g.rng.IntN(5) {
	case 0:
		return g.b.String()
	case 1:
		g.b.WriteString(" RETURN count(*) AS c0")
	case 2:
		fmt.Fprintf(&g.b, " WITH count(*) AS k MATCH (m:%s) RETURN k AS c0, count(m) AS c1", g.pick([]string{"A", "B", "C"}))
	case 3:
		if len(g.nodes) == 0 {
			g.b.WriteString(" RETURN count(*) AS c0 LIMIT 0")
			break
		}
		fmt.Fprintf(&g.b, " RETURN %s AS c0 ORDER BY c0 LIMIT %d", g.scalar(), g.rng.IntN(3))
	default:
		if len(g.nodes) == 0 {
			g.b.WriteString(" RETURN count(*) AS c0")
			break
		}
		g.b.WriteString(" RETURN " + g.scalar() + " AS c0, " + g.scalar() + " AS c1")
	}
	return g.b.String()
}

// writeOracleRuns counts the queries of TestOracleWrites by outcome.
var writeOracleRuns = map[Code]int{}

func TestOracleWrites(t *testing.T) {
	seeds, queries := 150, 20
	if testing.Short() {
		seeds = 30
	}
	if raceDetector {
		seeds = 15
	}
	for seed := range uint64(seeds) {
		rng := rand.New(rand.NewPCG(seed, 11))
		db, err := OpenMemory(DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		g := randomGraph(t, db, rng)
		next := uint64(1) << 40
		wg := &wgen{gen: gen{rng: rng}}
		for range queries {
			// A CREATE over a cross product multiplies the graph; past a
			// few hundred entities the brute force of the oracle is slow.
			if len(g.nodes)+len(g.edges) > 300 {
				break
			}
			src := wg.query()
			g, next = checkWriteOracle(t, db, g, next, src, rng.IntN(4) == 0)
		}
		db.Close()
	}
	t.Logf("queries by outcome: %v", writeOracleRuns)
}

// checkWriteOracle runs src on db and on the oracle over g and returns
// the graph of the oracle after it. inTx runs it with Tx.Exec after a
// statement that creates a node: a failure undoes only src.
func checkWriteOracle(t *testing.T, db *DB, g *oGraph, next uint64, src string, inTx bool) (*oGraph, uint64) {
	t.Helper()
	q, err := parser.Parse(src, parser.DefaultLimits())
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	var res Result
	if inTx {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "CREATE (:T {k: 1})", nil); err != nil {
			t.Fatal(err)
		}
		next++
		g.nodes = append(g.nodes, &oNode{id: next, labels: []string{"T"}, props: map[string]value.Value{"k": value.Int(1)}})
		res, err = tx.Exec(ctx, src, nil)
		if _, cerr := tx.Commit(ctx); cerr != nil {
			t.Fatal(cerr)
		}
		return compareWrite(t, db, g, next, src, q, res, err)
	}
	res, err = db.Exec(ctx, src, nil)
	return compareWrite(t, db, g, next, src, q, res, err)
}

func compareWrite(t *testing.T, db *DB, g *oGraph, next uint64, src string, q *ast.Query, res Result, err error) (*oGraph, uint64) {
	t.Helper()
	before := g.clone()
	writeOracleRuns[code(err)]++
	if st := res.Stats; err == nil && st.NodesCreated+st.NodesDeleted+st.EdgesCreated+st.EdgesDeleted+
		st.PropertiesSet+st.LabelsAdded+st.LabelsRemoved > 0 {
		writeOracleRuns["changed"]++
	}
	o := &oracle{t: t, g: g, src: src, env: &eval.Env{Ctx: ctx, Graph: g}, next: next}
	want, oerr := o.run(q)
	if err != nil || oerr != nil {
		// The oracle has no limits of resources: a query over them must
		// only leave the graph as it was.
		if code(err) != code(oerr) && code(err) != CodeResourceLimit {
			t.Fatalf("%s: exec %v, oracle %v", src, err, oerr)
		}
		if got, exp := dumpDB(t, db), before.dump(); !slices.Equal(got, exp) {
			t.Fatalf("%s: failed with %v and changed the graph:\n got  %q\n want %q", src, err, got, exp)
		}
		return before, o.next
	}
	var got, exp []string
	for _, r := range res.Rows {
		var row []qv.Value
		for _, v := range r {
			row = append(row, fromQuery(v))
		}
		got = append(got, rowString(row))
	}
	if want != nil && res.Columns != nil {
		for _, r := range want.rows {
			exp = append(exp, rowString(r[:len(res.Columns)]))
		}
	}
	if !strings.Contains(src, "ORDER BY") {
		sort.Strings(got)
		sort.Strings(exp)
	}
	if !slices.Equal(got, exp) {
		t.Fatalf("%s:\n got  %q\n want %q", src, got, exp)
	}
	st := res.Stats
	gotStats := [7]uint64{st.NodesCreated, st.NodesDeleted, st.EdgesCreated, st.EdgesDeleted, st.PropertiesSet, st.LabelsAdded, st.LabelsRemoved}
	m := o.stats
	wantStats := [7]uint64{m.NodesCreated, m.NodesDeleted, m.EdgesCreated, m.EdgesDeleted, m.PropertiesSet, m.LabelsAdded, m.LabelsRemoved}
	if gotStats != wantStats {
		t.Fatalf("%s: counters %v, want %v", src, gotStats, wantStats)
	}
	if got, exp := dumpDB(t, db), g.dump(); !slices.Equal(got, exp) {
		t.Fatalf("%s: graph\n got  %q\n want %q", src, got, exp)
	}
	return g, o.next
}
