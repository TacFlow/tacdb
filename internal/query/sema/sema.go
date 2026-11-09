// Package sema is the semantic analysis of tacdb-cypher/1 (SPEC §15.2,
// §15.3, §31.3): it resolves variables and parameters against their
// scopes, classifies a query as read or write, places aggregates, names
// the columns, and refuses impossible operations whose operand types are
// known. Every error is QUERY_SEMANTIC (or LIMIT_EXCEEDED for bindings
// and parameters) and comes before any effect.
package sema

import (
	"slices"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/qv"
)

// Defaults and maxima of SPEC §15.4 and §31.4.
const (
	DefaultMaxBindings = 256
	MaxMaxBindings     = 4096
	DefaultMaxParams   = 1024
)

// Config holds the limits of the analysis and the registry of
// procedures.
type Config struct {
	MaxBindings int // variables in scope at once; 0 is the default
	MaxParams   int // distinct parameters; 0 is the default
	// Procedure returns the signature of a procedure by the parts of its
	// name; nil knows none.
	Procedure func(name []string) (*Procedure, bool)
}

// Procedure is the signature of a procedure of the closed registry (SPEC
// §15.5).
type Procedure struct {
	Name   string
	Args   []qv.Type // static type accepted by each argument
	Yields []Column
	Write  bool
}

// Column is a column of a result or of a procedure.
type Column struct {
	Name string
	Type qv.Type
}

// Symbol is a variable. Each declaration makes a new Symbol; a WITH makes
// new Symbols for the names it projects.
type Symbol struct {
	Name string
	ID   int     // order of declaration in the query
	Type qv.Type // static type
}

// Projection is the analysis of the body of a WITH or a RETURN.
type Projection struct {
	Items       []*Item
	Aggregating bool
	// Aggregates are the aggregate calls of the items and of ORDER BY, in
	// order; the executor computes them into slots.
	Aggregates []*ast.FuncCall
	// Keys are the indexes of the items that group the rows when the
	// projection aggregates.
	Keys []int
	// OrderItems maps the subexpressions of ORDER BY that are an item,
	// written the same way, to the index of the item.
	OrderItems map[ast.Expr]int
}

// Item is a projected expression.
type Item struct {
	Name      string
	Symbol    *Symbol // the name it binds for ORDER BY and, after a WITH, for the next clauses
	Type      qv.Type
	Aggregate bool // holds an aggregate
}

// Info is the result of the analysis. It refers to the nodes of the
// analyzed tree, which must not change afterwards.
type Info struct {
	Write   bool
	Columns []Column // nil for a write that returns counters only
	Params  []string // the parameters of the query, sorted
	Symbols []*Symbol
	// Refs resolves every variable reference.
	Refs map[*ast.Variable]*Symbol
	// Decls gives the symbol declared by a pattern element, an UNWIND, a
	// YIELD item or a projection item.
	Decls map[any]*Symbol
	// Bound marks the pattern elements of CREATE and MERGE whose variable
	// was bound before, so they are reused rather than created.
	Bound       map[any]bool
	Projections map[*ast.Projection]*Projection
	// Procedures gives the signature of each CALL.
	Procedures map[*ast.Call]*Procedure

	src       string
	params    map[string]ast.Span // first reference of each parameter
	nonNegInt map[string]bool     // SKIP and LIMIT: an Int >= 0
	notNull   map[string]bool     // property values of MERGE
}

// analyzer walks one query.
type analyzer struct {
	src  string
	cfg  Config
	info *Info
	sc   *scope
	// proj is the projection whose items accept aggregates, while they are
	// analyzed.
	proj *Projection
	// order is set while an ORDER BY is analyzed.
	order *orderCtx
	inAgg bool // inside the arguments of an aggregate
}

// scope is the set of variables visible at a point, in order.
type scope struct {
	names []string
	syms  map[string]*Symbol
}

func newScope() *scope { return &scope{syms: map[string]*Symbol{}} }

func (s *scope) lookup(name string) (*Symbol, bool) {
	sym, ok := s.syms[name]
	return sym, ok
}

func (s *scope) add(sym *Symbol) {
	if _, ok := s.syms[sym.Name]; !ok {
		s.names = append(s.names, sym.Name)
	}
	s.syms[sym.Name] = sym
}

func (s *scope) clone() *scope {
	c := &scope{names: slices.Clone(s.names), syms: make(map[string]*Symbol, len(s.syms))}
	for k, v := range s.syms {
		c.syms[k] = v
	}
	return c
}

// Analyze checks q, whose source is src, and returns what the planner
// and the executor need.
func Analyze(q *ast.Query, src string, cfg Config) (*Info, error) {
	if cfg.MaxBindings <= 0 {
		cfg.MaxBindings = DefaultMaxBindings
	}
	cfg.MaxBindings = min(cfg.MaxBindings, MaxMaxBindings)
	if cfg.MaxParams <= 0 {
		cfg.MaxParams = DefaultMaxParams
	}
	a := &analyzer{src: src, cfg: cfg, sc: newScope(), info: &Info{
		src:         src,
		Refs:        map[*ast.Variable]*Symbol{},
		Decls:       map[any]*Symbol{},
		Bound:       map[any]bool{},
		Projections: map[*ast.Projection]*Projection{},
		Procedures:  map[*ast.Call]*Procedure{},
		params:      map[string]ast.Span{},
		nonNegInt:   map[string]bool{},
		notNull:     map[string]bool{},
	}}
	for _, c := range q.Clauses {
		if err := a.clause(c, len(q.Clauses) == 1); err != nil {
			return nil, err
		}
		if n := len(a.sc.names); n > a.cfg.MaxBindings {
			e := errs.Newf(errs.CodeLimitExceeded, "query", "%d variables in scope, above the limit of bindings", n).
				With("limit", int64(a.cfg.MaxBindings)).With("count", int64(n))
			e.Stage = errs.StageSemantic
			e.Location = ast.Locate(src, c.Pos())
			return nil, e
		}
	}
	for name := range a.info.params {
		a.info.Params = append(a.info.Params, name)
	}
	slices.Sort(a.info.Params)
	if n := len(a.info.Params); n > cfg.MaxParams {
		e := errs.Newf(errs.CodeLimitExceeded, "query", "%d parameters, above the limit", n).
			With("limit", int64(cfg.MaxParams)).With("count", int64(n))
		e.Stage = errs.StageSemantic
		e.Location = ast.Locate(src, a.info.params[a.info.Params[cfg.MaxParams]])
		return nil, e
	}
	return a.info, nil
}

func (a *analyzer) fail(span ast.Span, format string, args ...any) error {
	e := errs.Newf(errs.CodeQuerySemantic, "query", format, args...)
	e.Stage = errs.StageSemantic
	e.Location = ast.Locate(a.src, span)
	return e
}

// declare binds a new variable in the current scope.
func (a *analyzer) declare(name string, t qv.Type, decl any) *Symbol {
	sym := &Symbol{Name: name, ID: len(a.info.Symbols), Type: t}
	a.info.Symbols = append(a.info.Symbols, sym)
	a.sc.add(sym)
	if decl != nil {
		a.info.Decls[decl] = sym
	}
	return sym
}

func (a *analyzer) clause(c ast.Clause, alone bool) error {
	switch c := c.(type) {
	case *ast.Match:
		return a.match(c)
	case *ast.Unwind:
		if _, err := a.expr(c.X); err != nil {
			return err
		}
		if _, ok := a.sc.lookup(c.Alias); ok {
			return a.fail(c.Span, "variable %s is already bound", ast.Name(c.Alias))
		}
		a.declare(c.Alias, qv.TAny, c)
	case *ast.Call:
		return a.call(c, alone)
	case *ast.With:
		if _, err := a.projection(c.Projection, true); err != nil {
			return err
		}
		return a.where(c.Where)
	case *ast.Return:
		cols, err := a.projection(c.Projection, false)
		if err != nil {
			return err
		}
		a.info.Columns = cols
	case *ast.Create:
		a.info.Write = true
		for _, p := range c.Patterns {
			if err := a.writePattern(p, false); err != nil {
				return err
			}
		}
	case *ast.Merge:
		a.info.Write = true
		if err := a.writePattern(c.Pattern, true); err != nil {
			return err
		}
		for _, act := range c.Actions {
			if err := a.setItems(act.Items); err != nil {
				return err
			}
		}
	case *ast.Set:
		a.info.Write = true
		return a.setItems(c.Items)
	case *ast.Remove:
		a.info.Write = true
		for _, item := range c.Items {
			switch item := item.(type) {
			case *ast.RemoveProperty:
				if err := a.propertyTarget(item.Target); err != nil {
					return err
				}
			case *ast.RemoveLabels:
				if err := a.labelTarget(item.Var, item.Span); err != nil {
					return err
				}
			}
		}
	case *ast.Delete:
		a.info.Write = true
		for _, x := range c.Exprs {
			t, err := a.expr(x)
			if err != nil {
				return err
			}
			if k, ok := t.Known(); ok && k != qv.KindNode && k != qv.KindEdge && k != qv.KindPath {
				return a.fail(x.Pos(), "DELETE needs a node, a relationship or a path, not %s", k)
			}
		}
	}
	return nil
}

func (a *analyzer) where(w ast.Expr) error {
	if w == nil {
		return nil
	}
	_, err := a.expr(w)
	return err
}

// match declares the variables of a MATCH or OPTIONAL MATCH. A variable
// bound before is the same entity; a relationship variable may appear only
// once in a clause, whose relationships are all distinct (SPEC §33.3).
func (a *analyzer) match(c *ast.Match) error {
	extra := qv.Type(0)
	if c.Optional {
		extra = qv.TNull
	}
	rels := map[string]bool{}
	for _, p := range c.Patterns {
		for i, n := range p.Nodes {
			if err := a.matchVar(n.Var, qv.TNode, extra, n, n.Span); err != nil {
				return err
			}
			if i < len(p.Rels) {
				r := p.Rels[i]
				if r.Var != "" {
					if rels[r.Var] {
						return a.fail(r.Span, "relationship variable %s is used twice in one MATCH", ast.Name(r.Var))
					}
					rels[r.Var] = true
				}
				if err := a.matchVar(r.Var, qv.TEdge, extra, r, r.Span); err != nil {
					return err
				}
			}
		}
	}
	for _, p := range c.Patterns {
		if err := a.patternProps(p, false); err != nil {
			return err
		}
	}
	if c.Hint != nil {
		if err := a.hint(c.Hint); err != nil {
			return err
		}
	}
	return a.where(c.Where)
}

func (a *analyzer) matchVar(name string, want, extra qv.Type, decl any, span ast.Span) error {
	if name == "" {
		return nil
	}
	if sym, ok := a.sc.lookup(name); ok {
		if sym.Type&want == 0 {
			return a.fail(span, "%s is bound to %s, not %s", ast.Name(name), sym.Type, want)
		}
		a.info.Decls[decl] = sym
		return nil
	}
	a.declare(name, want|extra, decl)
	return nil
}

// patternProps checks the property maps of a pattern. In MERGE a Null
// literal cannot be matched, and a parameter there must not be Null.
func (a *analyzer) patternProps(p *ast.Pattern, merge bool) error {
	check := func(m *ast.MapLit) error {
		if m == nil {
			return nil
		}
		if merge {
			for _, e := range m.Entries {
				if lit, ok := e.Value.(*ast.Literal); ok && lit.Value.IsNull() {
					return a.fail(e.Span, "MERGE cannot match a null property")
				}
				if par, ok := e.Value.(*ast.Parameter); ok {
					a.info.notNull[par.Name] = true
				}
			}
		}
		_, err := a.expr(m)
		return err
	}
	for i, n := range p.Nodes {
		if err := check(n.Properties); err != nil {
			return err
		}
		if i < len(p.Rels) {
			if err := check(p.Rels[i].Properties); err != nil {
				return err
			}
		}
	}
	return nil
}

// hint checks USING VECTOR: its target is a property of a node of the
// clause, and its options a map.
func (a *analyzer) hint(h *ast.VectorHint) error {
	v, ok := h.Target.X.(*ast.Variable)
	if !ok {
		return a.fail(h.Span, "USING VECTOR needs a property of a node variable")
	}
	t, err := a.expr(v)
	if err != nil {
		return err
	}
	if t&qv.TNode == 0 {
		return a.fail(h.Span, "USING VECTOR needs a node variable, not %s", t)
	}
	if h.Options != nil {
		_, err = a.expr(h.Options)
	}
	return err
}

// writePattern declares the elements of a CREATE or MERGE pattern. A
// bound node is reused and cannot get labels or properties; a
// relationship is always new, with one type, and in CREATE a direction.
func (a *analyzer) writePattern(p *ast.Pattern, merge bool) error {
	verb := "CREATE"
	if merge {
		verb = "MERGE"
	}
	if n := p.Nodes[0]; len(p.Rels) == 0 && n.Var != "" && len(n.Labels) == 0 && n.Properties == nil {
		if _, ok := a.sc.lookup(n.Var); ok {
			return a.fail(p.Span, "%s of the bound variable %s creates nothing", verb, ast.Name(n.Var))
		}
	}
	if merge {
		// MERGE matches the whole pattern: its expressions see only what
		// was bound before it.
		if err := a.patternProps(p, true); err != nil {
			return err
		}
	}
	for i, n := range p.Nodes {
		if err := a.writeNode(n, verb, merge); err != nil {
			return err
		}
		if i == len(p.Rels) {
			break
		}
		r := p.Rels[i]
		switch {
		case len(r.Types) != 1:
			return a.fail(r.Span, "%s needs exactly one relationship type", verb)
		case r.Length != nil:
			return a.fail(r.Span, "%s cannot use a variable-length relationship", verb)
		case !merge && r.Direction == ast.DirBoth:
			return a.fail(r.Span, "CREATE needs a relationship direction")
		}
		if r.Var != "" {
			if _, ok := a.sc.lookup(r.Var); ok {
				return a.fail(r.Span, "relationship variable %s is already bound", ast.Name(r.Var))
			}
		}
		if !merge && r.Properties != nil {
			if _, err := a.expr(r.Properties); err != nil {
				return err
			}
		}
		if r.Var != "" {
			a.declare(r.Var, qv.TEdge, r)
		}
	}
	return nil
}

func (a *analyzer) writeNode(n *ast.NodePattern, verb string, merge bool) error {
	if n.Var != "" {
		if sym, ok := a.sc.lookup(n.Var); ok {
			if len(n.Labels) > 0 || n.Properties != nil {
				return a.fail(n.Span, "%s cannot add labels or properties to the bound variable %s", verb, ast.Name(n.Var))
			}
			if sym.Type&qv.TNode == 0 {
				return a.fail(n.Span, "%s is bound to %s, not a node", ast.Name(n.Var), sym.Type)
			}
			a.info.Decls[n] = sym
			a.info.Bound[n] = true
			return nil
		}
	}
	if !merge && n.Properties != nil {
		if _, err := a.expr(n.Properties); err != nil {
			return err
		}
	}
	if n.Var != "" {
		a.declare(n.Var, qv.TNode, n)
	}
	return nil
}

func (a *analyzer) setItems(items []ast.SetItem) error {
	for _, item := range items {
		switch item := item.(type) {
		case *ast.SetProperty:
			if err := a.propertyTarget(item.Target); err != nil {
				return err
			}
			if _, err := a.expr(item.Value); err != nil {
				return err
			}
		case *ast.SetProperties:
			if err := a.entityVar(item.Var, item.Span); err != nil {
				return err
			}
			t, err := a.expr(item.Value)
			if err != nil {
				return err
			}
			if k, ok := t.Known(); ok && k != qv.KindMap {
				return a.fail(item.Value.Pos(), "SET needs a map of properties, not %s", k)
			}
		case *ast.SetLabels:
			if err := a.labelTarget(item.Var, item.Span); err != nil {
				return err
			}
		}
	}
	return nil
}

// propertyTarget checks x.key of SET and REMOVE: x is a node or a
// relationship.
func (a *analyzer) propertyTarget(p *ast.Property) error {
	v, ok := p.X.(*ast.Variable)
	if !ok {
		return a.fail(p.Span, "SET and REMOVE need a property of a variable")
	}
	return a.entityVar(v.Name, p.Span, v)
}

func (a *analyzer) entityVar(name string, span ast.Span, ref ...*ast.Variable) error {
	sym, ok := a.sc.lookup(name)
	if !ok {
		return a.fail(span, "variable %s is not defined", ast.Name(name))
	}
	for _, r := range ref {
		a.info.Refs[r] = sym
	}
	if sym.Type&(qv.TNode|qv.TEdge) == 0 {
		return a.fail(span, "%s is %s; only nodes and relationships have properties", ast.Name(name), sym.Type)
	}
	return nil
}

func (a *analyzer) labelTarget(name string, span ast.Span) error {
	sym, ok := a.sc.lookup(name)
	if !ok {
		return a.fail(span, "variable %s is not defined", ast.Name(name))
	}
	if sym.Type&qv.TNode == 0 {
		return a.fail(span, "%s is %s; only nodes have labels", ast.Name(name), sym.Type)
	}
	return nil
}

// call checks a CALL against the registry. A standalone CALL without
// YIELD returns every column of the procedure.
func (a *analyzer) call(c *ast.Call, alone bool) error {
	name := strings.Join(c.Procedure, ".")
	var proc *Procedure
	ok := false
	if a.cfg.Procedure != nil {
		proc, ok = a.cfg.Procedure(c.Procedure)
	}
	if !ok {
		return a.fail(c.Span, "unknown procedure %s", name)
	}
	a.info.Procedures[c] = proc
	if proc.Write {
		a.info.Write = true
	}
	if len(c.Args) != len(proc.Args) {
		return a.fail(c.Span, "%s takes %d arguments, got %d", proc.Name, len(proc.Args), len(c.Args))
	}
	for i, x := range c.Args {
		t, err := a.expr(x)
		if err != nil {
			return err
		}
		if k, ok := t.Known(); ok && qv.TypeOf(k)&proc.Args[i] == 0 {
			return a.fail(x.Pos(), "argument %d of %s cannot be %s", i+1, proc.Name, k)
		}
	}
	if c.Yield == nil {
		if alone {
			a.info.Columns = slices.Clone(proc.Yields)
		}
		return nil
	}
	var cols []Column
	for _, y := range c.Yield {
		i := slices.IndexFunc(proc.Yields, func(col Column) bool { return col.Name == y.Name })
		if i < 0 {
			return a.fail(y.Span, "%s does not yield %s", proc.Name, ast.Name(y.Name))
		}
		alias := y.Name
		if y.Alias != "" {
			alias = y.Alias
		}
		if _, ok := a.sc.lookup(alias); ok {
			return a.fail(y.Span, "variable %s is already bound", ast.Name(alias))
		}
		a.declare(alias, proc.Yields[i].Type, y)
		cols = append(cols, Column{Name: alias, Type: proc.Yields[i].Type})
	}
	if alone {
		a.info.Columns = cols
	}
	return a.where(c.Where)
}
