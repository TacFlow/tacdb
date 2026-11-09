package parser

import (
	"math"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/value"
)

// Limits bound the work of the parser (SPEC §15.4).
type Limits struct {
	MaxQueryBytes int // size of the source
	MaxDepth      int // nesting of expressions, patterns, lists and maps
}

// DefaultLimits are the defaults of SPEC §15.4.
func DefaultLimits() Limits { return Limits{MaxQueryBytes: 1 << 20, MaxDepth: 128} }

// Parse reads one statement. It fails with QUERY_LEX, QUERY_PARSE or
// QUERY_UNSUPPORTED, located in the source, or LIMIT_EXCEEDED.
func Parse(src string, lim Limits) (*ast.Query, error) {
	if len(src) > lim.MaxQueryBytes {
		e := errs.Newf(errs.CodeLimitExceeded, "query.Parse", "a query of %d bytes", len(src)).
			With("bytes", int64(len(src))).With("limit", int64(lim.MaxQueryBytes))
		e.Stage = errs.StageLex
		return nil, e
	}
	if err := checkUTF8(src); err != nil {
		return nil, err
	}
	p := &parser{src: src, lx: &lexer{src: src}, lim: lim}
	q, err := p.query()
	if err != nil {
		return nil, err
	}
	return q, nil
}

type parser struct {
	src   string
	lx    *lexer
	toks  []token // read so far; the lexer runs as the parser needs tokens
	i     int
	lim   Limits
	depth int
}

// failure is how the parser unwinds: every error is raised with panic and
// recovered in query, so that the recursive descent stays readable.
type failure struct{ err error }

func (p *parser) fail(code errs.Code, span ast.Span, format string, args ...any) {
	e := errs.Newf(code, "query.Parse", format, args...)
	e.Stage = errs.StageParse
	e.Location = locate(p.src, span)
	panic(failure{e})
}

func (p *parser) errorAt(t token, format string, args ...any) {
	p.fail(errs.CodeQueryParse, t.span, format, args...)
}

func (p *parser) unsupported(span ast.Span, what string) {
	p.fail(errs.CodeQueryUnsupported, span, "%s is not part of tacdb-cypher/1", what)
}

func (p *parser) enter() {
	if p.depth++; p.depth > p.lim.MaxDepth {
		e := errs.Newf(errs.CodeLimitExceeded, "query.Parse", "the query nests deeper than %d", p.lim.MaxDepth).
			With("depth", int64(p.depth)).With("limit", int64(p.lim.MaxDepth))
		e.Stage = errs.StageParse
		e.Location = locate(p.src, p.peek().span)
		panic(failure{e})
	}
}

func (p *parser) leave() { p.depth-- }

// tok returns the token n after the current one, reading from the lexer
// as needed; past the end it is EOF.
func (p *parser) tok(n int) token {
	for len(p.toks) <= p.i+n {
		if len(p.toks) > 0 && p.toks[len(p.toks)-1].kind == tEOF {
			return p.toks[len(p.toks)-1]
		}
		t, err := p.lx.next()
		if err != nil {
			panic(failure{err})
		}
		p.toks = append(p.toks, t)
	}
	return p.toks[p.i+n]
}

func (p *parser) peek() token        { return p.tok(0) }
func (p *parser) peekAt(n int) token { return p.tok(n) }
func (p *parser) advance() token {
	t := p.tok(0)
	if t.kind != tEOF {
		p.i++
	}
	return t
}
func (p *parser) at(k kind) bool { return p.peek().kind == k }
func (p *parser) span(start int) ast.Span {
	end := start
	if p.i > 0 {
		end = p.toks[p.i-1].span.End
	}
	return ast.Span{Start: start, End: max(end, start)}
}

// keyword reports whether the next token is the keyword kw (upper case).
func (p *parser) keyword(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) keywordAt(n int, kw string) bool {
	t := p.peekAt(n)
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) accept(kw string) bool {
	if p.keyword(kw) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw, after string) {
	if !p.accept(kw) {
		p.errorAt(p.peek(), "expected %s %s, found %s", kw, after, describe(p.peek()))
	}
}

func (p *parser) expect(k kind, after string) token {
	t := p.peek()
	if t.kind != k {
		p.errorAt(t, "expected %s %s, found %s", kindText[k], after, describe(t))
	}
	return p.advance()
}

func describe(t token) string {
	switch t.kind {
	case tIdent:
		if ast.IsKeyword(t.text) {
			return strings.ToUpper(t.text)
		}
		return "'" + t.text + "'"
	case tInt, tFloat:
		return "number " + t.text
	case tString:
		return "a string"
	case tParam:
		return "$" + t.text
	}
	return kindText[t.kind]
}

// name reads a name: an identifier that is not a keyword, or a quoted
// name. what says what it names, for the error.
func (p *parser) name(what string) (string, token) {
	t := p.peek()
	switch {
	case t.kind == tQuoted:
	case t.kind == tIdent && !ast.IsKeyword(t.text):
	default:
		p.errorAt(t, "expected %s, found %s", what, describe(t))
	}
	p.advance()
	return t.text, t
}

// symbol reads a label, type or property key: keywords are allowed there.
func (p *parser) symbol(what string) string {
	t := p.peek()
	if t.kind != tQuoted && t.kind != tIdent {
		p.errorAt(t, "expected %s, found %s", what, describe(t))
	}
	p.advance()
	return t.text
}

func (p *parser) query() (q *ast.Query, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(failure)
			if !ok {
				panic(r)
			}
			q, err = nil, f.err
		}
	}()
	start := p.peek().span.Start
	q = &ast.Query{}
	switch {
	case p.accept("EXPLAIN"):
		q.Explain = true
	case p.accept("PROFILE"):
		q.Profile = true
	}
	for !p.at(tEOF) && !p.at(tSemi) {
		q.Clauses = append(q.Clauses, p.clause())
	}
	if p.at(tSemi) {
		p.advance()
		if !p.at(tEOF) {
			p.errorAt(p.peek(), "a query holds one statement; found more after ';'")
		}
	}
	q.Span = p.span(start)
	p.structure(q)
	return q, nil
}

// isUpdate reports whether c changes the graph.
func isUpdate(c ast.Clause) bool {
	switch c.(type) {
	case *ast.Create, *ast.Merge, *ast.Set, *ast.Remove, *ast.Delete:
		return true
	}
	return false
}

// structure checks the order of the clauses: reading clauses, then
// updating ones, separated into parts by WITH, and RETURN last. A query
// ends in RETURN or an update, or is a single CALL.
func (p *parser) structure(q *ast.Query) {
	if len(q.Clauses) == 0 {
		p.fail(errs.CodeQueryParse, ast.Span{}, "the query is empty")
	}
	if _, ok := q.Clauses[0].(*ast.Call); ok && len(q.Clauses) == 1 {
		return // a standalone CALL, with or without YIELD
	}
	updating := false
	for i, c := range q.Clauses {
		last := i == len(q.Clauses)-1
		switch c := c.(type) {
		case *ast.Return:
			if !last {
				p.fail(errs.CodeQueryParse, q.Clauses[i+1].Pos(), "RETURN must be the last clause")
			}
		case *ast.With:
			updating = false
		case *ast.Match, *ast.Unwind, *ast.Call:
			if updating {
				p.fail(errs.CodeQueryParse, c.Pos(), "a reading clause after an update needs WITH between them")
			}
			if call, ok := c.(*ast.Call); ok && len(call.Yield) == 0 {
				p.fail(errs.CodeQueryParse, c.Pos(), "CALL inside a query needs YIELD")
			}
		default:
			updating = true
		}
		if last {
			if _, ok := c.(*ast.Return); !ok && !isUpdate(c) {
				p.fail(errs.CodeQueryParse, c.Pos(), "a query must end with RETURN or an update")
			}
		}
	}
}

func (p *parser) clause() ast.Clause {
	t := p.peek()
	start := t.span.Start
	if t.kind != tIdent {
		p.errorAt(t, "expected a clause, found %s", describe(t))
	}
	switch strings.ToUpper(t.text) {
	case "MATCH":
		p.advance()
		return p.match(start, false)
	case "OPTIONAL":
		p.advance()
		p.expectKeyword("MATCH", "after OPTIONAL")
		return p.match(start, true)
	case "UNWIND":
		p.advance()
		x := p.expr()
		p.expectKeyword("AS", "after the UNWIND expression")
		alias, _ := p.name("a variable after AS")
		return &ast.Unwind{X: x, Alias: alias, Span: p.span(start)}
	case "CALL":
		p.advance()
		return p.call(start)
	case "WITH":
		p.advance()
		proj := p.projection("WITH")
		w := &ast.With{Projection: proj}
		if p.accept("WHERE") {
			w.Where = p.expr()
		}
		w.Span = p.span(start)
		return w
	case "RETURN":
		p.advance()
		proj := p.projection("RETURN")
		return &ast.Return{Projection: proj, Span: p.span(start)}
	case "CREATE":
		if p.keywordAt(1, "INDEX") || p.keywordAt(1, "CONSTRAINT") || p.keywordAt(2, "INDEX") || p.keywordAt(1, "OR") {
			p.unsupported(t.span, "schema commands (use the index API)")
		}
		p.advance()
		return &ast.Create{Patterns: p.patterns(), Span: p.span(start)}
	case "MERGE":
		p.advance()
		return p.merge(start)
	case "SET":
		p.advance()
		return &ast.Set{Items: p.setItems(), Span: p.span(start)}
	case "REMOVE":
		p.advance()
		return p.remove(start)
	case "DELETE":
		p.advance()
		return &ast.Delete{Exprs: p.exprList(), Span: p.span(start)}
	case "DETACH":
		p.advance()
		p.expectKeyword("DELETE", "after DETACH")
		return &ast.Delete{Detach: true, Exprs: p.exprList(), Span: p.span(start)}
	case "WHERE":
		p.errorAt(t, "WHERE must follow MATCH, OPTIONAL MATCH, WITH or CALL")
	case "ORDER", "SKIP", "LIMIT":
		p.errorAt(t, "%s must follow RETURN or WITH", strings.ToUpper(t.text))
	case "UNION":
		p.unsupported(t.span, "UNION")
	case "FOREACH":
		p.unsupported(t.span, "FOREACH")
	case "LOAD":
		p.unsupported(t.span, "LOAD CSV")
	case "USE":
		p.unsupported(t.span, "USE")
	case "DROP", "SHOW", "START", "STOP", "ALTER", "GRANT", "DENY", "REVOKE":
		p.unsupported(t.span, strings.ToUpper(t.text)+" commands")
	}
	p.errorAt(t, "expected a clause, found %s", describe(t))
	return nil
}

func (p *parser) match(start int, optional bool) ast.Clause {
	m := &ast.Match{Optional: optional, Patterns: p.patterns()}
	if p.keyword("USING") {
		u := p.advance()
		if !p.keyword("VECTOR") {
			p.unsupported(u.span, "USING "+describe(p.peek()))
		}
		p.advance()
		hint := &ast.VectorHint{}
		target, ok := p.postfix().(*ast.Property)
		if !ok {
			p.errorAt(p.peek(), "USING VECTOR needs a property, as in n.embedding")
		}
		hint.Target = target
		if p.keyword("OPTIONS") {
			p.advance()
			if !p.at(tLBrace) {
				p.errorAt(p.peek(), "expected a map after OPTIONS, found %s", describe(p.peek()))
			}
			hint.Options = p.mapLit()
		}
		hint.Span = p.span(u.span.Start)
		m.Hint = hint
	}
	if p.accept("WHERE") {
		m.Where = p.expr()
	}
	m.Span = p.span(start)
	return m
}

func (p *parser) call(start int) ast.Clause {
	c := &ast.Call{}
	if p.at(tLParen) {
		p.unsupported(p.peek().span, "CALL subqueries")
	}
	if p.at(tLBrace) {
		p.unsupported(p.peek().span, "CALL subqueries")
	}
	first, _ := p.procName()
	parts := []string{first}
	for p.at(tDot) {
		p.advance()
		part, _ := p.procName()
		parts = append(parts, part)
	}
	c.Procedure = parts
	p.expect(tLParen, "after the procedure name")
	if !p.at(tRParen) {
		c.Args = p.exprList()
	}
	p.expect(tRParen, "after the procedure arguments")
	if p.accept("YIELD") {
		if p.at(tStar) {
			p.unsupported(p.peek().span, "YIELD *")
		}
		for {
			ys := p.peek().span.Start
			n, _ := p.name("a column after YIELD")
			y := &ast.YieldItem{Name: n}
			if p.accept("AS") {
				y.Alias, _ = p.name("a variable after AS")
			}
			y.Span = p.span(ys)
			c.Yield = append(c.Yield, y)
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
		if p.accept("WHERE") {
			c.Where = p.expr()
		}
	}
	c.Span = p.span(start)
	return c
}

func (p *parser) procName() (string, token) {
	t := p.peek()
	if t.kind != tIdent && t.kind != tQuoted {
		p.errorAt(t, "expected a procedure name, found %s", describe(t))
	}
	p.advance()
	return t.text, t
}

func (p *parser) projection(clause string) *ast.Projection {
	proj := &ast.Projection{}
	if p.accept("DISTINCT") {
		proj.Distinct = true
	}
	if p.at(tStar) {
		p.unsupported(p.peek().span, clause+" *")
	}
	for {
		start := p.peek().span.Start
		it := &ast.ProjectionItem{X: p.expr()}
		if p.accept("AS") {
			it.Alias, _ = p.name("an alias after AS")
		}
		it.Span = p.span(start)
		proj.Items = append(proj.Items, it)
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	if p.keyword("ORDER") {
		p.advance()
		p.expectKeyword("BY", "after ORDER")
		for {
			start := p.peek().span.Start
			s := &ast.SortItem{X: p.expr()}
			switch {
			case p.accept("DESC"):
				s.Desc = true
			case p.accept("ASC"):
			}
			s.Span = p.span(start)
			proj.Order = append(proj.Order, s)
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
	}
	if p.accept("SKIP") {
		proj.Skip = p.expr()
	}
	if p.accept("LIMIT") {
		proj.Limit = p.expr()
	}
	return proj
}

func (p *parser) merge(start int) ast.Clause {
	m := &ast.Merge{Pattern: p.pattern()}
	for p.keyword("ON") {
		as := p.advance().span.Start
		a := &ast.MergeAction{}
		switch {
		case p.accept("CREATE"):
			a.OnCreate = true
		case p.accept("MATCH"):
		default:
			p.errorAt(p.peek(), "expected CREATE or MATCH after ON, found %s", describe(p.peek()))
		}
		p.expectKeyword("SET", "after ON CREATE or ON MATCH")
		a.Items = p.setItems()
		a.Span = p.span(as)
		m.Actions = append(m.Actions, a)
	}
	m.Span = p.span(start)
	return m
}

func (p *parser) setItems() []ast.SetItem {
	var items []ast.SetItem
	for {
		start := p.peek().span.Start
		v, vt := p.name("a variable in SET")
		switch {
		case p.at(tDot):
			p.advance()
			key := p.symbol("a property name after '.'")
			if p.at(tDot) {
				p.unsupported(p.peek().span, "SET on a nested property")
			}
			target := &ast.Property{X: &ast.Variable{Name: v, Span: vt.span}, Key: key, Span: p.span(start)}
			p.expect(tEq, "after the property in SET")
			items = append(items, &ast.SetProperty{Target: target, Value: p.expr(), Span: p.span(start)})
		case p.at(tEq):
			p.advance()
			items = append(items, &ast.SetProperties{Var: v, Value: p.expr(), Span: p.span(start)})
		case p.at(tPlusEq):
			p.advance()
			items = append(items, &ast.SetProperties{Var: v, Value: p.expr(), Merge: true, Span: p.span(start)})
		case p.at(tColon):
			items = append(items, &ast.SetLabels{Var: v, Labels: p.labels(), Span: p.span(start)})
		default:
			p.errorAt(p.peek(), "expected '.', '=', '+=' or ':' after the variable in SET, found %s", describe(p.peek()))
		}
		if !p.at(tComma) {
			return items
		}
		p.advance()
	}
}

func (p *parser) remove(start int) ast.Clause {
	r := &ast.Remove{}
	for {
		is := p.peek().span.Start
		v, vt := p.name("a variable in REMOVE")
		switch {
		case p.at(tDot):
			p.advance()
			key := p.symbol("a property name after '.'")
			if p.at(tDot) {
				p.unsupported(p.peek().span, "REMOVE of a nested property")
			}
			target := &ast.Property{X: &ast.Variable{Name: v, Span: vt.span}, Key: key, Span: p.span(is)}
			r.Items = append(r.Items, &ast.RemoveProperty{Target: target, Span: p.span(is)})
		case p.at(tColon):
			r.Items = append(r.Items, &ast.RemoveLabels{Var: v, Labels: p.labels(), Span: p.span(is)})
		default:
			p.errorAt(p.peek(), "expected '.' or ':' after the variable in REMOVE, found %s", describe(p.peek()))
		}
		if !p.at(tComma) {
			break
		}
		p.advance()
	}
	r.Span = p.span(start)
	return r
}

func (p *parser) labels() []string {
	var ls []string
	for p.at(tColon) {
		p.advance()
		ls = append(ls, p.symbol("a label after ':'"))
	}
	return ls
}

func (p *parser) patterns() []*ast.Pattern {
	ps := []*ast.Pattern{p.pattern()}
	for p.at(tComma) {
		p.advance()
		ps = append(ps, p.pattern())
	}
	return ps
}

func (p *parser) pattern() *ast.Pattern {
	p.enter()
	defer p.leave()
	start := p.peek().span.Start
	if (p.at(tIdent) || p.at(tQuoted)) && p.peekAt(1).kind == tEq {
		p.unsupported(p.peek().span, "path variables")
	}
	if p.at(tIdent) && p.peekAt(1).kind == tLParen &&
		(strings.EqualFold(p.peek().text, "shortestPath") || strings.EqualFold(p.peek().text, "allShortestPaths")) {
		p.unsupported(p.peek().span, p.peek().text+" (use CALL tacdb.graph.shortest_path)")
	}
	pat := &ast.Pattern{Nodes: []*ast.NodePattern{p.node()}}
	for p.at(tMinus) || p.at(tArrowLeft) {
		pat.Rels = append(pat.Rels, p.rel())
		if !p.at(tLParen) {
			p.errorAt(p.peek(), "expected a node after the relationship, found %s", describe(p.peek()))
		}
		pat.Nodes = append(pat.Nodes, p.node())
	}
	pat.Span = p.span(start)
	return pat
}

func (p *parser) node() *ast.NodePattern {
	start := p.expect(tLParen, "to start a node").span.Start
	n := &ast.NodePattern{}
	if p.at(tIdent) && !ast.IsKeyword(p.peek().text) || p.at(tQuoted) {
		n.Var, _ = p.name("a variable")
	}
	n.Labels = p.labels()
	n.Properties = p.patternProperties()
	p.expect(tRParen, "to end the node")
	n.Span = p.span(start)
	return n
}

func (p *parser) patternProperties() *ast.MapLit {
	switch {
	case p.at(tLBrace):
		return p.mapLit()
	case p.at(tParam):
		p.unsupported(p.peek().span, "a parameter as the properties of a pattern")
	}
	return nil
}

func (p *parser) rel() *ast.RelPattern {
	start := p.peek().span.Start
	r := &ast.RelPattern{}
	left := p.advance() // - or <-
	if p.at(tLBracket) {
		p.advance()
		if p.at(tIdent) && !ast.IsKeyword(p.peek().text) || p.at(tQuoted) {
			r.Var, _ = p.name("a variable")
		}
		if p.at(tColon) {
			p.advance()
			r.Types = append(r.Types, p.symbol("a relationship type after ':'"))
			for p.at(tPipe) {
				p.advance()
				if p.at(tColon) {
					p.advance()
				}
				r.Types = append(r.Types, p.symbol("a relationship type after '|'"))
			}
			if p.at(tColon) {
				p.errorAt(p.peek(), "a relationship has one type; use '|' for alternatives")
			}
		}
		if p.at(tStar) {
			star := p.peek().span
			r.Length = p.length()
			if r.Var != "" {
				p.unsupported(star, "a variable on a variable-length relationship")
			}
			if p.at(tLBrace) {
				p.unsupported(p.peek().span, "properties on a variable-length relationship")
			}
		}
		r.Properties = p.patternProperties()
		p.expect(tRBracket, "to end the relationship")
	}
	right := p.peek()
	switch right.kind {
	case tArrowRight, tMinus:
		p.advance()
	default:
		p.errorAt(right, "expected '-' or '->' to end the relationship, found %s", describe(right))
	}
	switch {
	case left.kind == tMinus && right.kind == tArrowRight:
		r.Direction = ast.DirOut
	case left.kind == tArrowLeft && right.kind == tMinus:
		r.Direction = ast.DirIn
	default:
		r.Direction = ast.DirBoth // -[]- and <-[]->
	}
	r.Span = p.span(start)
	return r
}

func (p *parser) bound() *uint32 {
	t := p.expect(tInt, "in the range")
	n, err := strconv.ParseUint(t.text, 10, 32)
	if err != nil {
		p.errorAt(t, "a hop count must be at most %d", uint32(math.MaxUint32))
	}
	v := uint32(n)
	return &v
}

func (p *parser) length() *ast.Length {
	p.advance() // *
	l := &ast.Length{}
	if p.at(tInt) {
		l.Min = p.bound()
		if !p.at(tDotDot) {
			l.Max = l.Min
			return l
		}
	}
	if p.at(tDotDot) {
		p.advance()
		if p.at(tInt) {
			l.Max = p.bound()
		}
	}
	return l
}

func (p *parser) exprList() []ast.Expr {
	es := []ast.Expr{p.expr()}
	for p.at(tComma) {
		p.advance()
		es = append(es, p.expr())
	}
	return es
}

// Precedence, from the reference (SPEC §31.3), lowest first: OR; XOR;
// AND; NOT; = <> IN CONTAINS STARTS WITH ENDS WITH IS NULL; < <= > >= <=>
// @@; + -; * / %; ^ (right); unary -; property access.
func (p *parser) expr() ast.Expr {
	p.enter()
	defer p.leave()
	return p.or()
}

func (p *parser) binary(l, r ast.Expr, op ast.BinaryOp) ast.Expr {
	return &ast.Binary{Op: op, L: l, R: r, Span: ast.Span{Start: l.Pos().Start, End: r.Pos().End}}
}

func (p *parser) or() ast.Expr {
	l := p.xor()
	for p.accept("OR") {
		l = p.binary(l, p.xor(), ast.OpOr)
	}
	return l
}

func (p *parser) xor() ast.Expr {
	l := p.and()
	for p.accept("XOR") {
		l = p.binary(l, p.and(), ast.OpXor)
	}
	return l
}

func (p *parser) and() ast.Expr {
	l := p.not()
	for p.accept("AND") {
		l = p.binary(l, p.not(), ast.OpAnd)
	}
	return l
}

func (p *parser) not() ast.Expr {
	if p.keyword("NOT") {
		start := p.advance().span.Start
		p.enter()
		defer p.leave()
		x := p.not()
		return &ast.Unary{Op: ast.OpNot, X: x, Span: ast.Span{Start: start, End: x.Pos().End}}
	}
	return p.equality()
}

func (p *parser) equality() ast.Expr {
	l := p.relational()
	for {
		var op ast.BinaryOp
		switch {
		case p.at(tEq):
			op = ast.OpEq
		case p.at(tNeq):
			op = ast.OpNeq
		case p.keyword("IN"):
			op = ast.OpIn
		case p.keyword("CONTAINS"):
			op = ast.OpContains
		case p.keyword("STARTS"):
			p.advance()
			if !p.keyword("WITH") {
				p.errorAt(p.peek(), "expected WITH after STARTS, found %s", describe(p.peek()))
			}
			op = ast.OpStartsWith
		case p.keyword("ENDS"):
			p.advance()
			if !p.keyword("WITH") {
				p.errorAt(p.peek(), "expected WITH after ENDS, found %s", describe(p.peek()))
			}
			op = ast.OpEndsWith
		case p.keyword("IS"):
			p.advance()
			not := p.accept("NOT")
			nt := p.peek()
			if !p.accept("NULL") {
				p.errorAt(nt, "expected NULL after IS, found %s", describe(nt))
			}
			l = &ast.IsNull{X: l, Not: not, Span: ast.Span{Start: l.Pos().Start, End: nt.span.End}}
			// Tighter operators apply to the result, as in the reference:
			// a IS NULL + b is (a IS NULL) + b.
			l = p.relationalFrom(p.additiveFrom(p.multiplicativeFrom(p.powerFrom(l))))
			continue
		case p.at(tRegex):
			p.unsupported(p.peek().span, "the =~ operator")
		default:
			return l
		}
		p.advance()
		l = p.binary(l, p.relational(), op)
	}
}

func (p *parser) relational() ast.Expr { return p.relationalFrom(p.additive()) }

func (p *parser) relationalFrom(l ast.Expr) ast.Expr {
	for {
		var op ast.BinaryOp
		switch p.peek().kind {
		case tLt:
			op = ast.OpLt
		case tLe:
			op = ast.OpLe
		case tGt:
			op = ast.OpGt
		case tGe:
			op = ast.OpGe
		case tVecDist:
			op = ast.OpVecDist
		case tFTS:
			op = ast.OpFTSMatch
		default:
			return l
		}
		p.advance()
		l = p.binary(l, p.additive(), op)
	}
}

func (p *parser) additive() ast.Expr { return p.additiveFrom(p.multiplicative()) }

func (p *parser) additiveFrom(l ast.Expr) ast.Expr {
	for {
		var op ast.BinaryOp
		switch p.peek().kind {
		case tPlus:
			op = ast.OpAdd
		case tMinus:
			op = ast.OpSub
		default:
			return l
		}
		p.advance()
		l = p.binary(l, p.multiplicative(), op)
	}
}

func (p *parser) multiplicative() ast.Expr { return p.multiplicativeFrom(p.power()) }

func (p *parser) multiplicativeFrom(l ast.Expr) ast.Expr {
	for {
		var op ast.BinaryOp
		switch p.peek().kind {
		case tStar:
			op = ast.OpMul
		case tSlash:
			op = ast.OpDiv
		case tPercent:
			op = ast.OpMod
		default:
			return l
		}
		p.advance()
		l = p.binary(l, p.power(), op)
	}
}

// power is right-associative, and its operands are unary: -2^2 is
// (-2)^2, as in the reference.
func (p *parser) power() ast.Expr { return p.powerFrom(p.unary()) }

func (p *parser) powerFrom(l ast.Expr) ast.Expr {
	if p.at(tCaret) {
		p.advance()
		p.enter()
		defer p.leave()
		l = p.binary(l, p.power(), ast.OpPow)
	}
	return l
}

func (p *parser) unary() ast.Expr {
	if p.keyword("NOT") {
		// As in the reference, NOT may start any operand; its own
		// operand reaches down to the equality level.
		return p.not()
	}
	switch p.peek().kind {
	case tPlus:
		p.advance()
		p.enter()
		defer p.leave()
		return p.unary() // a no-op, as in the reference
	case tMinus:
		start := p.advance().span.Start
		if t := p.peek(); (t.kind == tInt || t.kind == tFloat) && p.peekAt(1).kind != tDot {
			// A literal takes its sign, so that -9223372036854775808 is an
			// Int and -0.0 keeps its sign.
			p.advance()
			lit := p.number(t, true)
			lit.Span.Start = start
			return p.postfixFrom(lit)
		}
		p.enter()
		defer p.leave()
		x := p.unary()
		return &ast.Unary{Op: ast.OpNeg, X: x, Span: ast.Span{Start: start, End: x.Pos().End}}
	}
	return p.postfix()
}

func (p *parser) number(t token, negative bool) *ast.Literal {
	text := t.text
	if negative {
		text = "-" + text
	}
	if t.kind == tInt {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			panic(failure{lexError(p.src, t.span, "the integer %s is out of range", text)})
		}
		return &ast.Literal{Value: value.Int(n), Span: t.span}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || f == 0 && strings.ContainsAny(mantissa(text), "123456789") {
		panic(failure{lexError(p.src, t.span, "the float %s is out of range", text)})
	}
	return &ast.Literal{Value: value.Float(f), Span: t.span}
}

// mantissa is the digits of a float literal before its exponent.
func mantissa(text string) string {
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		return text[:i]
	}
	return text
}

func (p *parser) postfix() ast.Expr { return p.postfixFrom(p.primary()) }

func (p *parser) postfixFrom(x ast.Expr) ast.Expr {
	for {
		switch {
		case p.at(tDot):
			p.advance()
			key := p.symbol("a property name after '.'")
			x = &ast.Property{X: x, Key: key, Span: ast.Span{Start: x.Pos().Start, End: p.toks[p.i-1].span.End}}
		case p.at(tLBracket):
			p.unsupported(p.peek().span, "list indexing and slicing")
		case p.at(tColon):
			p.unsupported(p.peek().span, "label predicates in expressions")
		default:
			return x
		}
	}
}

func (p *parser) primary() ast.Expr {
	t := p.peek()
	switch t.kind {
	case tInt, tFloat:
		p.advance()
		return p.number(t, false)
	case tString:
		p.advance()
		return &ast.Literal{Value: value.String(t.text), Span: t.span}
	case tParam:
		p.advance()
		return &ast.Parameter{Name: t.text, Span: t.span}
	case tLBracket:
		return p.list()
	case tLBrace:
		return p.mapLit()
	case tLParen:
		p.advance()
		if p.at(tRParen) || p.at(tColon) {
			// (), (:L): a node, so a pattern predicate
			p.unsupported(t.span, "pattern predicates")
		}
		x := p.expr()
		p.expect(tRParen, "to close the parenthesis")
		if _, ok := x.(*ast.Variable); ok && p.patternFollows() {
			p.unsupported(t.span, "pattern predicates")
		}
		return x
	case tQuoted:
		p.advance()
		return p.afterName(t)
	case tIdent:
		switch strings.ToUpper(t.text) {
		case "TRUE":
			p.advance()
			return &ast.Literal{Value: value.Bool(true), Span: t.span}
		case "FALSE":
			p.advance()
			return &ast.Literal{Value: value.Bool(false), Span: t.span}
		case "NULL":
			p.advance()
			return &ast.Literal{Value: value.Null(), Span: t.span}
		case "CASE":
			p.unsupported(t.span, "CASE")
		case "EXISTS", "COUNT", "COLLECT":
			if p.peekAt(1).kind == tLBrace {
				p.unsupported(t.span, strings.ToUpper(t.text)+" subqueries")
			}
		case "REDUCE", "ALL", "ANY", "NONE", "SINGLE":
			if p.peekAt(1).kind == tLParen {
				p.unsupported(t.span, strings.ToUpper(t.text)+"(...)")
			}
		}
		if ast.IsKeyword(t.text) {
			p.errorAt(t, "expected an expression, found %s", describe(t))
		}
		p.advance()
		return p.afterName(t)
	}
	p.errorAt(t, "expected an expression, found %s", describe(t))
	return nil
}

// patternFollows reports whether a relationship starts at the next token.
func (p *parser) patternFollows() bool {
	switch p.peek().kind {
	case tArrowLeft:
		return true
	case tMinus:
		switch p.peekAt(1).kind {
		case tLBracket, tArrowRight:
			return true
		case tMinus:
			return p.peekAt(2).kind == tLParen // (a)--(b); (x) - -1 is arithmetic
		}
	}
	return false
}

// afterName reads what follows a name in an expression: a function call
// or a variable.
func (p *parser) afterName(t token) ast.Expr {
	switch p.peek().kind {
	case tLParen:
		return p.funcCall(t)
	case tDot:
		// A dotted function name, as in tacdb.x(), is a procedure.
		if k := p.peekAt(1).kind; (k == tIdent || k == tQuoted) && p.peekAt(2).kind == tLParen {
			p.unsupported(t.span, "namespaced functions")
		}
	case tLBrace:
		p.unsupported(p.peek().span, "map projection")
	}
	return &ast.Variable{Name: t.text, Span: t.span}
}

func (p *parser) funcCall(name token) ast.Expr {
	p.advance() // (
	f := &ast.FuncCall{Name: name.text}
	switch {
	case p.at(tStar):
		star := p.advance()
		if !strings.EqualFold(name.text, "count") {
			p.errorAt(star, "only count accepts *")
		}
		f.Star = true
	case p.at(tRParen):
	default:
		if p.accept("DISTINCT") {
			f.Distinct = true
		}
		f.Args = p.exprList()
	}
	p.expect(tRParen, "to close the call of "+name.text)
	f.Span = p.span(name.span.Start)
	return f
}

func (p *parser) list() ast.Expr {
	start := p.advance().span.Start
	p.enter()
	defer p.leave()
	l := &ast.ListLit{}
	if !p.at(tRBracket) {
		for {
			l.Elems = append(l.Elems, p.expr())
			if p.at(tPipe) || p.keyword("WHERE") {
				p.unsupported(p.peek().span, "list comprehensions")
			}
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
	}
	p.expect(tRBracket, "to close the list")
	l.Span = p.span(start)
	return l
}

func (p *parser) mapLit() *ast.MapLit {
	start := p.advance().span.Start
	p.enter()
	defer p.leave()
	m := &ast.MapLit{}
	if !p.at(tRBrace) {
		for {
			es := p.peek().span.Start
			key := p.symbol("a map key")
			p.expect(tColon, "after the map key")
			m.Entries = append(m.Entries, &ast.MapEntry{Key: key, Value: p.expr(), Span: p.span(es)})
			if !p.at(tComma) {
				break
			}
			p.advance()
		}
	}
	p.expect(tRBrace, "to close the map")
	m.Span = p.span(start)
	return m
}
