package plan

import (
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/sema"
)

// projection plans the body of a WITH or a RETURN: Project (after
// Aggregate when it aggregates), Distinct, then ORDER BY, SKIP and LIMIT
// in that order (D-010), as TopK when ORDER BY has a LIMIT. The rows of
// the next part hold the items, then the sort keys.
func (p *planner) projection(ap *ast.Projection) error {
	pr := p.info.Projections[ap]
	n := len(pr.Items)
	out := &Part{}
	for _, it := range pr.Items {
		out.slot(it.Name)
	}
	keySlots := make([]int, len(ap.Order))
	for i := range ap.Order {
		keySlots[i] = out.slot("")
	}
	proj := &Project{Out: out}
	cluster := -1
	if pr.Aggregating {
		agg, sc, err := p.aggregate(ap, pr)
		if err != nil {
			return err
		}
		proj.In = agg
		for _, it := range ap.Items {
			x, err := p.compile(it.X, sc)
			if err != nil {
				return err
			}
			proj.Items = append(proj.Items, x)
		}
		osc := &orderScope{base: sc, width: agg.Part.Width, pr: pr}
		for _, s := range ap.Order {
			x, err := p.compile(s.X, osc)
			if err != nil {
				return err
			}
			proj.Keys = append(proj.Keys, x)
		}
	} else {
		proj.In = p.op
		for i, it := range ap.Items {
			x, err := p.compile(it.X, p.scope())
			if err != nil {
				return err
			}
			proj.Items = append(proj.Items, x)
			if v, ok := it.X.(*ast.Variable); ok && p.cluster >= 0 && p.names[v.Name] == p.cluster {
				cluster = i
			}
		}
		osc := &orderScope{base: p.scope(), width: p.part.Width, pr: pr}
		for _, s := range ap.Order {
			x, err := p.compile(s.X, osc)
			if err != nil {
				return err
			}
			proj.Keys = append(proj.Keys, x)
		}
	}
	p.op, p.part, p.single, p.cluster = proj, out, false, cluster
	p.names = map[string]int{}
	for i, it := range pr.Items {
		p.names[it.Name] = i
	}
	if ap.Distinct {
		p.op = &Distinct{In: p.op, N: n}
	}
	var skip, limit *Expr
	for _, x := range []struct {
		e   ast.Expr
		dst **Expr
	}{{ap.Skip, &skip}, {ap.Limit, &limit}} {
		if x.e != nil {
			c, err := p.compile(x.e, names{})
			if err != nil {
				return err
			}
			*x.dst = &c
		}
	}
	if len(ap.Order) > 0 {
		keys := make([]SortKey, len(ap.Order))
		for i, s := range ap.Order {
			keys[i] = SortKey{Slot: keySlots[i], Desc: s.Desc, Text: ast.FormatExpr(s.X)}
		}
		p.cluster = -1
		if limit != nil && !p.cfg.NoTopK {
			p.op = &TopK{In: p.op, Keys: keys, Skip: skip, Limit: limit, Exhaustive: p.write}
			return nil
		}
		p.op = &Sort{In: p.op, Keys: keys}
	}
	if skip != nil {
		p.op = &Skip{In: p.op, X: *skip}
	}
	if limit != nil {
		p.op = &Limit{In: p.op, X: *limit, Exhaustive: p.write}
	}
	return nil
}

// aggregate plans the grouping of an aggregating projection. The rows it
// yields hold the grouping keys, then the aggregates; sc resolves the
// items over them.
func (p *planner) aggregate(ap *ast.Projection, pr *sema.Projection) (*Aggregate, eval.Scope, error) {
	part := &Part{}
	agg := &Aggregate{In: p.op, Part: part}
	sc := &aggScope{texts: map[string]int{}, vars: map[string]int{}, aggs: map[*ast.FuncCall]int{}}
	ordered := p.cluster >= 0 && !p.cfg.NoOrderedAggregate && len(pr.Keys) > 0
	hasCluster := false
	for _, k := range pr.Keys {
		it := ap.Items[k]
		x, err := p.compile(it.X, p.scope())
		if err != nil {
			return nil, nil, err
		}
		slot := part.slot("")
		agg.Keys = append(agg.Keys, x)
		sc.texts[ast.FormatExpr(it.X)] = slot
		if v, ok := it.X.(*ast.Variable); ok {
			sc.vars[v.Name] = slot
			if p.names[v.Name] == p.cluster {
				hasCluster = true
			}
		}
		// The input is clustered by the keys when every key reads only
		// the clustered variable and one of them is that variable.
		walk(it.X, func(v *ast.Variable) {
			if p.names[v.Name] != p.cluster {
				ordered = false
			}
		})
	}
	agg.Ordered = ordered && hasCluster
	for _, call := range pr.Aggregates {
		fn, _ := eval.Lookup(call.Name)
		a := Agg{Func: fn, Distinct: call.Distinct, Star: call.Star || len(call.Args) == 0, Text: ast.FormatExpr(call)}
		if !a.Star {
			x, err := p.compile(call.Args[0], p.scope())
			if err != nil {
				return nil, nil, err
			}
			a.Arg = &x
		}
		sc.aggs[call] = part.slot("")
		agg.Aggs = append(agg.Aggs, a)
	}
	return agg, sc, nil
}

// aggScope resolves the items of an aggregating projection over the rows
// of Aggregate: a grouping key, written as one or as its variable, and
// each aggregate.
type aggScope struct {
	texts map[string]int
	vars  map[string]int
	aggs  map[*ast.FuncCall]int
}

func (s *aggScope) Variable(name string) (int, bool) {
	slot, ok := s.vars[name]
	return slot, ok
}

func (s *aggScope) Aggregate(call *ast.FuncCall) (int, bool) {
	slot, ok := s.aggs[call]
	return slot, ok
}

func (s *aggScope) Expr(e ast.Expr) (int, bool) {
	if _, ok := e.(*ast.Literal); ok {
		return 0, false
	}
	slot, ok := s.texts[ast.FormatExpr(e)]
	return slot, ok
}

// orderScope resolves ORDER BY over a row made of the projection's input
// row, of width width, followed by its items: an item by name or by the
// text sema matched, then what base resolves.
type orderScope struct {
	base  eval.Scope
	width int
	pr    *sema.Projection
}

func (s *orderScope) Variable(name string) (int, bool) {
	for i, it := range s.pr.Items {
		if it.Name == name {
			return s.width + i, true
		}
	}
	return s.base.Variable(name)
}

func (s *orderScope) Aggregate(call *ast.FuncCall) (int, bool) { return s.base.Aggregate(call) }

func (s *orderScope) Expr(e ast.Expr) (int, bool) {
	if i, ok := s.pr.OrderItems[e]; ok {
		return s.width + i, true
	}
	if es, ok := s.base.(eval.ExprScope); ok {
		return es.Expr(e)
	}
	return 0, false
}
