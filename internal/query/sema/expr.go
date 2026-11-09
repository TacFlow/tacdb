package sema

import (
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// projection checks the body of a WITH (with true) or a RETURN and
// returns its columns. A WITH replaces the scope by its items.
func (a *analyzer) projection(p *ast.Projection, with bool) ([]Column, error) {
	pr := &Projection{OrderItems: map[ast.Expr]int{}}
	a.info.Projections[p] = pr
	in := a.sc
	texts := make([]string, len(p.Items))
	names := map[string]bool{}
	cols := make([]Column, len(p.Items))
	for i, it := range p.Items {
		start := len(pr.Aggregates)
		a.proj = pr
		t, err := a.expr(it.X)
		a.proj = nil
		if err != nil {
			return nil, err
		}
		item := &Item{Type: t, Aggregate: len(pr.Aggregates) > start}
		switch {
		case it.Alias != "":
			item.Name = it.Alias
		case isVariable(it.X):
			item.Name = it.X.(*ast.Variable).Name
		case with:
			return nil, a.fail(it.Span, "an expression in WITH needs an alias (AS name)")
		default:
			item.Name = columnName(a.src, it.Span) // the item without alias, parentheses included
		}
		if names[item.Name] {
			return nil, a.fail(it.Span, "column %s is projected twice", ast.Name(item.Name))
		}
		names[item.Name] = true
		pr.Items = append(pr.Items, item)
		pr.Aggregating = pr.Aggregating || item.Aggregate
		texts[i] = ast.FormatExpr(it.X)
		cols[i] = Column{Name: item.Name, Type: t}
	}
	keyTexts := map[string]bool{}
	keyVars := map[string]bool{}
	if pr.Aggregating {
		for i, it := range p.Items {
			if !pr.Items[i].Aggregate {
				pr.Keys = append(pr.Keys, i)
				keyTexts[texts[i]] = true
				if v, ok := it.X.(*ast.Variable); ok {
					keyVars[v.Name] = true
				}
			}
		}
		for i, it := range p.Items {
			if pr.Items[i].Aggregate {
				if err := a.grouped(it.X, keyTexts, keyVars); err != nil {
					return nil, err
				}
			}
		}
	}

	// The scope after the projection: its items, with new symbols. A
	// RETURN declares them too, for its ORDER BY.
	out := newScope()
	a.sc = out
	for i, it := range p.Items {
		pr.Items[i].Symbol = a.declare(pr.Items[i].Name, pr.Items[i].Type, it)
	}
	a.sc = in

	// ORDER BY sees the items by name and by text. Without aggregation or
	// DISTINCT it also sees the variables before the projection;
	// aggregates are allowed when the projection aggregates.
	order := out
	if !pr.Aggregating && !p.Distinct {
		order = in.clone()
		for _, name := range out.names {
			order.add(out.syms[name])
		}
	}
	oc := &orderCtx{texts: map[string]int{}, pr: pr, in: in, shadow: map[string]bool{}}
	for i, it := range p.Items {
		// A name projected from anything but the variable of that name
		// hides the variable: text that uses it is not that item's text.
		if v, ok := it.X.(*ast.Variable); !ok || v.Name != pr.Items[i].Name {
			oc.shadow[pr.Items[i].Name] = true
		}
	}
	for i, t := range texts {
		if _, ok := oc.texts[t]; !ok {
			oc.texts[t] = i
		}
	}
	saved := a.sc
	a.sc, a.order = order, oc
	for _, s := range p.Order {
		if _, err := a.expr(s.X); err != nil {
			a.sc, a.order = saved, nil
			return nil, err
		}
	}
	a.sc, a.order = saved, nil
	for _, x := range []ast.Expr{p.Skip, p.Limit} {
		if err := a.skipLimit(x); err != nil {
			return nil, err
		}
	}
	if with {
		a.sc = out
	}
	return cols, nil
}

func isVariable(e ast.Expr) bool {
	_, ok := e.(*ast.Variable)
	return ok
}

// grouped checks that an aggregating item uses the variables of the rows
// only inside its aggregates or through a grouping key.
func (a *analyzer) grouped(e ast.Expr, keyTexts, keyVars map[string]bool) error {
	if keyTexts[ast.FormatExpr(e)] {
		return nil
	}
	switch e := e.(type) {
	case *ast.FuncCall:
		if fn, ok := eval.Lookup(e.Name); ok && fn.Aggregate {
			return nil
		}
		for _, x := range e.Args {
			if err := a.grouped(x, keyTexts, keyVars); err != nil {
				return err
			}
		}
	case *ast.Variable:
		if !keyVars[e.Name] {
			return a.fail(e.Span, "%s must be a grouping key or inside an aggregate", ast.Name(e.Name))
		}
	case *ast.Property:
		return a.grouped(e.X, keyTexts, keyVars)
	case *ast.ListLit:
		for _, x := range e.Elems {
			if err := a.grouped(x, keyTexts, keyVars); err != nil {
				return err
			}
		}
	case *ast.MapLit:
		for _, en := range e.Entries {
			if err := a.grouped(en.Value, keyTexts, keyVars); err != nil {
				return err
			}
		}
	case *ast.Unary:
		return a.grouped(e.X, keyTexts, keyVars)
	case *ast.IsNull:
		return a.grouped(e.X, keyTexts, keyVars)
	case *ast.Binary:
		if err := a.grouped(e.L, keyTexts, keyVars); err != nil {
			return err
		}
		return a.grouped(e.R, keyTexts, keyVars)
	}
	return nil
}

// orderCtx is the context of the expressions of an ORDER BY: a
// subexpression written as an item is that item, and an aggregate reads
// the rows before the projection.
type orderCtx struct {
	texts  map[string]int
	pr     *Projection
	in     *scope
	shadow map[string]bool // item names that hide a variable of the rows
}

// uses reports whether e reads a variable named in names.
func uses(e ast.Expr, names map[string]bool) bool {
	switch e := e.(type) {
	case *ast.Variable:
		return names[e.Name]
	case *ast.Property:
		return uses(e.X, names)
	case *ast.ListLit:
		for _, x := range e.Elems {
			if uses(x, names) {
				return true
			}
		}
	case *ast.MapLit:
		for _, en := range e.Entries {
			if uses(en.Value, names) {
				return true
			}
		}
	case *ast.Unary:
		return uses(e.X, names)
	case *ast.IsNull:
		return uses(e.X, names)
	case *ast.Binary:
		return uses(e.L, names) || uses(e.R, names)
	case *ast.FuncCall:
		for _, x := range e.Args {
			if uses(x, names) {
				return true
			}
		}
	}
	return false
}

// skipLimit checks SKIP and LIMIT: an Int literal >= 0, or a parameter
// whose value is checked before execution.
func (a *analyzer) skipLimit(x ast.Expr) error {
	switch x := x.(type) {
	case nil:
		return nil
	case *ast.Literal:
		if i, ok := x.Value.AsInt(); ok && i >= 0 {
			return nil
		}
	case *ast.Parameter:
		a.param(x)
		a.info.nonNegInt[x.Name] = true
		return nil
	}
	return a.fail(x.Pos(), "SKIP and LIMIT need an integer literal >= 0 or a parameter")
}

func (a *analyzer) param(p *ast.Parameter) {
	if _, ok := a.info.params[p.Name]; !ok {
		a.info.params[p.Name] = p.Span
	}
}

// columnName is the text of an expression as written, with runs of
// spaces and comments made one space outside of quotes.
func columnName(src string, span ast.Span) string {
	s := src[span.Start:span.End]
	var b strings.Builder
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			j := i + 1
			for j < len(s) && s[j] != c {
				if s[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			j = min(j+1, len(s))
			b.WriteString(s[i:j])
			i = j - 1
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			space = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteByte(c)
		}
	}
	return b.String()
}

// expr checks an expression in the current scope and returns its static
// type. Aggregates are allowed only while a projection is analyzed, and
// never inside one another.
func (a *analyzer) expr(e ast.Expr) (qv.Type, error) {
	if o := a.order; o != nil && !uses(e, o.shadow) {
		if i, ok := o.texts[ast.FormatExpr(e)]; ok {
			o.pr.OrderItems[e] = i
			return o.pr.Items[i].Type, nil
		}
	}
	switch e := e.(type) {
	case *ast.Literal:
		return qv.TypeOf(qv.Kind(e.Value.Kind())), nil
	case *ast.Parameter:
		a.param(e)
		return qv.TAny, nil
	case *ast.Variable:
		sym, ok := a.sc.lookup(e.Name)
		if !ok {
			return 0, a.fail(e.Span, "variable %s is not defined", ast.Name(e.Name))
		}
		a.info.Refs[e] = sym
		return sym.Type, nil
	case *ast.Property:
		t, err := a.expr(e.X)
		if err != nil {
			return 0, err
		}
		if k, ok := t.Known(); ok && k != qv.KindNode && k != qv.KindEdge && k != qv.KindMap {
			return 0, a.fail(e.Span, "cannot read a property of %s", k)
		}
		return qv.TAny, nil
	case *ast.ListLit:
		for _, x := range e.Elems {
			if _, err := a.expr(x); err != nil {
				return 0, err
			}
		}
		return qv.TList, nil
	case *ast.MapLit:
		seen := map[string]bool{}
		for _, en := range e.Entries {
			if seen[en.Key] {
				return 0, a.fail(en.Span, "duplicate key %s in a map", ast.Name(en.Key))
			}
			seen[en.Key] = true
			if _, err := a.expr(en.Value); err != nil {
				return 0, err
			}
		}
		return qv.TMap, nil
	case *ast.Unary:
		t, err := a.expr(e.X)
		if err != nil {
			return 0, err
		}
		if e.Op == ast.OpNot {
			return qv.TBool, nil
		}
		if k, ok := t.Known(); ok && qv.TypeOf(k)&qv.TNumber == 0 {
			return 0, a.fail(e.Span, "cannot negate %s", k)
		}
		if t&qv.TNumber == 0 {
			return qv.TNumber | qv.TNull, nil
		}
		return t & (qv.TNumber | qv.TNull), nil
	case *ast.Binary:
		return a.binary(e)
	case *ast.IsNull:
		if _, err := a.expr(e.X); err != nil {
			return 0, err
		}
		return qv.TBool, nil
	case *ast.FuncCall:
		return a.funcCall(e)
	}
	panic("sema: unknown expression node")
}

func (a *analyzer) funcCall(e *ast.FuncCall) (qv.Type, error) {
	fn, ok := eval.Lookup(e.Name)
	if !ok {
		return 0, a.fail(e.Span, "unknown function %s", ast.Name(e.Name))
	}
	if err := eval.CheckCall(fn, e, a.src); err != nil {
		return 0, err
	}
	if fn.Aggregate {
		if a.inAgg {
			return 0, a.fail(e.Span, "aggregates cannot be nested")
		}
		pr, sc, order := a.proj, a.sc, a.order
		if order != nil {
			if !order.pr.Aggregating {
				return 0, a.fail(e.Span, "aggregate %s in ORDER BY needs a projection that aggregates", fn.Name)
			}
			pr, a.sc = order.pr, order.in // the arguments read the rows before the projection
		}
		if pr == nil {
			return 0, a.fail(e.Span, "aggregate %s is allowed only in WITH, RETURN and their ORDER BY", fn.Name)
		}
		a.proj, a.order, a.inAgg = nil, nil, true
		var err error
		for _, x := range e.Args {
			if _, err = a.expr(x); err != nil {
				break
			}
		}
		a.proj, a.sc, a.order, a.inAgg = pr, sc, order, false
		if order != nil {
			a.proj = nil
		}
		if err != nil {
			return 0, err
		}
		pr.Aggregates = append(pr.Aggregates, e)
		return fn.Result, nil
	}
	for _, x := range e.Args {
		if _, err := a.expr(x); err != nil {
			return 0, err
		}
	}
	return fn.Result, nil
}

// binary checks an operator; an operation that cannot succeed for the
// known types of its operands is QUERY_SEMANTIC (SPEC §31.3).
func (a *analyzer) binary(e *ast.Binary) (qv.Type, error) {
	l, err := a.expr(e.L)
	if err != nil {
		return 0, err
	}
	r, err := a.expr(e.R)
	if err != nil {
		return 0, err
	}
	lk, lok := l.Known()
	rk, rok := r.Known()
	both := lok && rok
	num := func(k qv.Kind) bool { return k == qv.KindInt || k == qv.KindFloat }
	impossible := func() error {
		return a.fail(e.Span, "%s %s %s cannot succeed", l, e.Op, r)
	}
	switch e.Op {
	case ast.OpOr, ast.OpAnd, ast.OpEq, ast.OpNeq:
		return qv.TBool, nil
	case ast.OpXor:
		return qv.TBool | qv.TNull, nil
	case ast.OpIn:
		if both && rk != qv.KindList {
			return 0, impossible()
		}
		return qv.TBool | qv.TNull, nil
	case ast.OpContains, ast.OpStartsWith, ast.OpEndsWith:
		if both && (lk != qv.KindString || rk != qv.KindString) {
			return 0, impossible()
		}
		return qv.TBool | qv.TNull, nil
	case ast.OpLt, ast.OpLe, ast.OpGt, ast.OpGe:
		if both && !(lk == rk && lk <= qv.KindBytes || num(lk) && num(rk)) {
			return 0, impossible()
		}
		return qv.TBool | qv.TNull, nil
	case ast.OpVecDist:
		vec := func(k qv.Kind) bool { return k == qv.KindVector || k == qv.KindList }
		if both && (!vec(lk) || !vec(rk)) {
			return 0, impossible()
		}
		return qv.TFloat | qv.TNull, nil
	case ast.OpFTSMatch:
		if both && rk != qv.KindString {
			return 0, impossible()
		}
		return qv.TBool | qv.TNull, nil
	case ast.OpAdd:
		switch {
		case !both:
			return qv.TAny, nil
		case lk == qv.KindList || rk == qv.KindList:
			return qv.TList, nil
		case lk == qv.KindString && rk == qv.KindString:
			return qv.TString | qv.TNull, nil
		}
	}
	// - * / % ^, and + of numbers
	if !both {
		return qv.TNumber | qv.TNull, nil
	}
	if !num(lk) || !num(rk) {
		return 0, impossible()
	}
	if e.Op == ast.OpPow || lk == qv.KindFloat || rk == qv.KindFloat {
		return qv.TFloat | qv.TNull, nil
	}
	return qv.TInt | qv.TNull, nil
}

// CheckParams checks the parameters of an execution before it starts:
// their count, that every parameter of the query is given, and the values
// of SKIP, LIMIT and MERGE.
func (in *Info) CheckParams(params map[string]value.Value, maxParams int) error {
	if maxParams <= 0 {
		maxParams = DefaultMaxParams
	}
	if len(params) > maxParams {
		e := errs.Newf(errs.CodeLimitExceeded, "query", "%d parameters, above the limit", len(params)).
			With("limit", int64(maxParams)).With("count", int64(len(params)))
		e.Stage = errs.StageSemantic
		return e
	}
	for _, name := range in.Params {
		v, ok := params[name]
		fail := func(code errs.Code, format string) error {
			e := errs.Newf(code, "query", format, name)
			e.Stage = errs.StageSemantic
			e.Location = ast.Locate(in.src, in.params[name])
			return e
		}
		switch {
		case !ok:
			return fail(errs.CodeParamMissing, "parameter $%s is missing")
		case in.nonNegInt[name]:
			if i, ok := v.AsInt(); !ok || i < 0 {
				return fail(errs.CodeParamType, "parameter $%s of SKIP or LIMIT must be an integer >= 0")
			}
		}
		if in.notNull[name] && v.IsNull() {
			return fail(errs.CodeParamType, "parameter $%s of a MERGE property cannot be null")
		}
	}
	return nil
}
