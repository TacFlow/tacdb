package plan

import (
	"slices"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/query/ast"
)

// clauseState is the planning of one MATCH or OPTIONAL MATCH.
type clauseState struct {
	bound    map[int]bool // slots bound so far, before the clause or by it
	before   map[int]bool // slots bound before the clause
	slots    map[*ast.NodePattern]int
	rels     map[*ast.RelPattern]int
	edges    []int // edge slots of the clause bound so far
	lists    []int // edge lists of the clause bound so far
	deferred []ast.Expr
	fresh    []int // slots the clause declares
}

// match plans a MATCH, or an OPTIONAL MATCH as a left outer apply (SPEC
// §15.5): entry by a bound variable, then by an ID seek, then by the node
// with the most labels and properties, ties by position; expansions
// follow the text from the entry (design §4.3).
func (p *planner) match(c *ast.Match) error {
	if c.Hint != nil {
		return failf(p.src, c.Hint.Span, errs.CodeQueryUnsupported, errs.StagePlan, "USING VECTOR is not available in this version")
	}
	outer := p.op
	if c.Optional {
		p.op, p.single = &Argument{Part: p.part}, false
	}
	st, err := p.matchPatterns(c)
	if err != nil {
		return err
	}
	if c.Optional {
		p.op = &OptionalApply{In: outer, Inner: p.op, Slots: st.fresh}
		p.single = false
	}
	return nil
}

// matchPatterns plans the patterns and the WHERE of a clause over p.op and
// returns the slots it gave each element.
func (p *planner) matchPatterns(c *ast.Match) (*clauseState, error) {
	st := &clauseState{bound: map[int]bool{}, before: map[int]bool{}, slots: map[*ast.NodePattern]int{}, rels: map[*ast.RelPattern]int{}}
	for _, s := range p.names {
		st.bound[s], st.before[s] = true, true
	}
	// Slots: a variable bound before keeps its slot; a new one, or an
	// anonymous element, gets one.
	for _, pat := range c.Patterns {
		for i, n := range pat.Nodes {
			st.slots[n] = p.elementSlot(st, n.Var)
			if i < len(pat.Rels) {
				r := pat.Rels[i]
				st.rels[r] = p.elementSlot(st, r.Var)
			}
		}
	}
	if err := p.checkLengths(c); err != nil {
		return nil, err
	}
	done := make([]bool, len(c.Patterns))
	for {
		pi, start := p.entry(c, st, done)
		if pi < 0 {
			break
		}
		done[pi] = true
		if err := p.chain(c.Patterns[pi], start, st, c.Where); err != nil {
			return nil, err
		}
	}
	for _, x := range st.deferred {
		pred, err := p.compile(x, p.scope())
		if err != nil {
			return nil, err
		}
		p.op = &Filter{In: p.op, Pred: pred}
	}
	if c.Where != nil {
		pred, err := p.compile(c.Where, p.scope())
		if err != nil {
			return nil, err
		}
		p.op = &Filter{In: p.op, Pred: pred}
	}
	return st, nil
}

// elementSlot returns the slot of a pattern element: the slot of its
// variable, bound before or earlier in the clause, or a new one.
func (p *planner) elementSlot(st *clauseState, name string) int {
	if name != "" {
		if s, ok := p.names[name]; ok {
			return s
		}
	}
	if name == "" {
		// A hidden name, which no query can write (a NUL is not allowed
		// in a name), so that a deferred filter can read the element.
		name = hiddenPrefix + strconv.Itoa(p.part.Width)
	}
	s := p.declare(name)
	st.fresh = append(st.fresh, s)
	return s
}

// checkLengths refuses bounds of variable-length relationships that
// cannot be planned: a minimum above the maximum, or a maximum above
// MaxHops.
func (p *planner) checkLengths(c *ast.Match) error {
	for _, pat := range c.Patterns {
		for _, r := range pat.Rels {
			if r.Length == nil {
				continue
			}
			lo, hi := bounds(r.Length)
			if r.Length.Max != nil && lo > hi {
				return failf(p.src, r.Span, errs.CodeQuerySemantic, errs.StageSemantic, "variable-length relationship with a minimum of %d above its maximum of %d", lo, hi)
			}
			if r.Length.Max != nil && hi > p.cfg.MaxHops {
				e := errs.Newf(errs.CodeResourceLimit, "query", "variable-length relationship of up to %d hops, above the limit of hops", hi).
					With("requested", int64(hi)).With("limit", int64(p.cfg.MaxHops))
				e.Stage, e.Location = errs.StagePlan, ast.Locate(p.src, r.Span)
				return e
			}
			if r.Length.Max == nil && lo > p.cfg.MaxHops {
				e := errs.Newf(errs.CodeResourceLimit, "query", "variable-length relationship of at least %d hops, above the limit of hops", lo).
					With("requested", int64(lo)).With("limit", int64(p.cfg.MaxHops))
				e.Stage, e.Location = errs.StagePlan, ast.Locate(p.src, r.Span)
				return e
			}
		}
	}
	return nil
}

func bounds(l *ast.Length) (lo, hi int) {
	lo = 1
	if l.Min != nil {
		lo = int(*l.Min)
	}
	hi = -1
	if l.Max != nil {
		hi = int(*l.Max)
		if l.Min == nil && hi < 1 {
			lo = hi // *..0 is *0
		}
	}
	return lo, hi
}

// entry chooses the next pattern of the clause and the node it starts
// from; -1 when every pattern is planned.
func (p *planner) entry(c *ast.Match, st *clauseState, done []bool) (int, int) {
	// A pattern that touches a bound node starts there.
	for pi, pat := range c.Patterns {
		if done[pi] {
			continue
		}
		for i, n := range pat.Nodes {
			if st.bound[st.slots[n]] {
				return pi, i
			}
		}
	}
	// Then a node with an ID seek, then the node with the most labels
	// and properties; ties by position.
	best, bestPos, bestScore := -1, -1, -1
	for pi, pat := range c.Patterns {
		if done[pi] {
			continue
		}
		for i, n := range pat.Nodes {
			score := len(n.Labels)
			if n.Properties != nil {
				score += len(n.Properties.Entries)
			}
			if p.idSeek(n, c.Where, st) != nil {
				score = 1 << 30
			}
			if score > bestScore {
				best, bestPos, bestScore = pi, i, score
			}
		}
	}
	return best, bestPos
}

// idSeek returns the expression x of a conjunct id(n) = x or x = id(n) of
// where, when x reads nothing the clause binds.
func (p *planner) idSeek(n *ast.NodePattern, where ast.Expr, st *clauseState) ast.Expr {
	if n.Var == "" || where == nil {
		return nil
	}
	for _, conj := range conjuncts(where) {
		b, ok := conj.(*ast.Binary)
		if !ok || b.Op != ast.OpEq {
			continue
		}
		for _, side := range [][2]ast.Expr{{b.L, b.R}, {b.R, b.L}} {
			f, ok := side[0].(*ast.FuncCall)
			if !ok || !strings.EqualFold(f.Name, "id") || len(f.Args) != 1 {
				continue
			}
			v, ok := f.Args[0].(*ast.Variable)
			if !ok || v.Name != n.Var {
				continue
			}
			if p.readsOnly(side[1], st.before) {
				return side[1]
			}
		}
	}
	return nil
}

func conjuncts(e ast.Expr) []ast.Expr {
	if b, ok := e.(*ast.Binary); ok && b.Op == ast.OpAnd {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []ast.Expr{e}
}

// readsOnly reports whether e reads only variables whose slots are in
// slots.
func (p *planner) readsOnly(e ast.Expr, slots map[int]bool) bool {
	ok := true
	walk(e, func(v *ast.Variable) {
		s, found := p.names[v.Name]
		if !found || !slots[s] {
			ok = false
		}
	})
	return ok
}

// walk calls fn on each variable e reads.
func walk(e ast.Expr, fn func(*ast.Variable)) {
	switch e := e.(type) {
	case *ast.Variable:
		fn(e)
	case *ast.Property:
		walk(e.X, fn)
	case *ast.ListLit:
		for _, x := range e.Elems {
			walk(x, fn)
		}
	case *ast.MapLit:
		for _, en := range e.Entries {
			walk(en.Value, fn)
		}
	case *ast.Unary:
		walk(e.X, fn)
	case *ast.IsNull:
		walk(e.X, fn)
	case *ast.Binary:
		walk(e.L, fn)
		walk(e.R, fn)
	case *ast.FuncCall:
		for _, x := range e.Args {
			walk(x, fn)
		}
	}
}

// props compiles the property map of an element bound now. An entry that
// reads an element of the clause not bound yet is checked after the
// clause instead.
func (p *planner) props(m *ast.MapLit, slot int, st *clauseState) ([]PropTest, error) {
	if m == nil {
		return nil, nil
	}
	var out []PropTest
	for _, en := range m.Entries {
		if !p.readsOnly(en.Value, st.bound) {
			st.deferred = append(st.deferred, &ast.Binary{Op: ast.OpEq,
				L:    &ast.Property{X: &ast.Variable{Name: p.part.Names[slot], Span: en.Span}, Key: en.Key, Span: en.Span},
				R:    en.Value,
				Span: en.Span})
			continue
		}
		x, err := p.compile(en.Value, p.scope())
		if err != nil {
			return nil, err
		}
		out = append(out, PropTest{Key: en.Key, Value: x})
	}
	return out, nil
}

// chain plans one pattern from its node start: the entry, the
// relationships to its right in order, then those to its left.
func (p *planner) chain(pat *ast.Pattern, start int, st *clauseState, where ast.Expr) error {
	n := pat.Nodes[start]
	slot := st.slots[n]
	if st.bound[slot] {
		if len(n.Labels) > 0 || n.Properties != nil || len(pat.Rels) == 0 {
			props, err := p.props(n.Properties, slot, st)
			if err != nil {
				return err
			}
			p.op = &NodeFilter{In: p.op, Slot: slot, Labels: n.Labels, Props: props}
		}
	} else {
		st.bound[slot] = true
		props, err := p.props(n.Properties, slot, st)
		if err != nil {
			return err
		}
		single := p.single
		switch x := p.idSeek(n, where, st); {
		case x != nil:
			id, err := p.compile(x, p.scope())
			if err != nil {
				return err
			}
			p.op = &IDSeek{In: p.op, Slot: slot, ID: id, Labels: n.Labels, Props: props}
		case len(n.Labels) > 0:
			p.op = &LabelScan{In: p.op, Slot: slot, Label: n.Labels[0], Labels: n.Labels[1:], Props: props}
		default:
			p.op = &NodeScan{In: p.op, Slot: slot, Props: props}
		}
		if single {
			p.cluster = slot
		}
		p.single = false
	}
	for i := start; i < len(pat.Rels); i++ {
		if err := p.expand(pat.Rels[i], pat.Nodes[i], pat.Nodes[i+1], false, st); err != nil {
			return err
		}
	}
	for i := start - 1; i >= 0; i-- {
		if err := p.expand(pat.Rels[i], pat.Nodes[i+1], pat.Nodes[i], true, st); err != nil {
			return err
		}
	}
	return nil
}

// expand plans relationship r from node a, bound, to node b; reverse is
// set when the walk goes against the text.
func (p *planner) expand(r *ast.RelPattern, a, b *ast.NodePattern, reverse bool, st *clauseState) error {
	dir := graph.Both
	switch {
	case r.Direction == ast.DirOut && !reverse, r.Direction == ast.DirIn && reverse:
		dir = graph.Out
	case r.Direction == ast.DirIn && !reverse, r.Direction == ast.DirOut && reverse:
		dir = graph.In
	}
	from, to := st.slots[a], st.slots[b]
	into := st.bound[to]
	st.bound[to] = true
	toProps, err := p.props(b.Properties, to, st)
	if err != nil {
		return err
	}
	toLabels := b.Labels
	unique, lists := slices.Clone(st.edges), slices.Clone(st.lists)
	if r.Length != nil {
		lo, hi := bounds(r.Length)
		list := st.rels[r]
		p.op = &VarExpand{In: p.op, From: from, To: to, List: list, Dir: dir, Types: r.Types,
			Min: lo, Max: hi, Unbounded: r.Length.Max == nil, Into: into,
			ToLabels: toLabels, ToProps: toProps, Unique: unique, Lists: lists, Span: r.Span}
		st.lists = append(st.lists, list)
		return nil
	}
	rel := st.rels[r]
	relBound := st.bound[rel]
	st.bound[rel] = true
	relProps, err := p.props(r.Properties, rel, st)
	if err != nil {
		return err
	}
	p.op = &Expand{In: p.op, From: from, To: to, Rel: rel, Dir: dir, Types: r.Types, Into: into,
		RelBound: relBound, RelProps: relProps, ToLabels: toLabels, ToProps: toProps, Unique: unique, Lists: lists}
	st.edges = append(st.edges, rel)
	return nil
}
