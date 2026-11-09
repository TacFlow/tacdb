package ast

import (
	"math"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/value"
)

// Format writes q as canonical text: keywords in upper case, every
// operation parenthesized, names quoted when they need it. Parsing the
// text gives q again, spans aside.
func Format(q *Query) string {
	var f formatter
	f.query(q)
	return f.b.String()
}

// FormatExpr writes one expression as Format does.
func FormatExpr(e Expr) string {
	var f formatter
	f.expr(e)
	return f.b.String()
}

type formatter struct{ b strings.Builder }

func (f *formatter) w(s ...string) {
	for _, x := range s {
		f.b.WriteString(x)
	}
}

func (f *formatter) query(q *Query) {
	switch {
	case q.Explain:
		f.w("EXPLAIN ")
	case q.Profile:
		f.w("PROFILE ")
	}
	for i, c := range q.Clauses {
		if i > 0 {
			f.w(" ")
		}
		f.clause(c)
	}
}

func (f *formatter) clause(c Clause) {
	switch c := c.(type) {
	case *Match:
		if c.Optional {
			f.w("OPTIONAL ")
		}
		f.w("MATCH ")
		f.patterns(c.Patterns)
		if c.Hint != nil {
			f.w(" USING VECTOR ")
			f.expr(c.Hint.Target)
			if c.Hint.Options != nil {
				f.w(" OPTIONS ")
				f.expr(c.Hint.Options)
			}
		}
		f.where(c.Where)
	case *Unwind:
		f.w("UNWIND ")
		f.expr(c.X)
		f.w(" AS ", Name(c.Alias))
	case *Call:
		f.w("CALL ")
		for i, part := range c.Procedure {
			if i > 0 {
				f.w(".")
			}
			f.w(Name(part))
		}
		f.w("(")
		f.exprs(c.Args)
		f.w(")")
		if len(c.Yield) > 0 {
			f.w(" YIELD ")
			for i, y := range c.Yield {
				if i > 0 {
					f.w(", ")
				}
				f.w(Name(y.Name))
				if y.Alias != "" {
					f.w(" AS ", Name(y.Alias))
				}
			}
		}
		f.where(c.Where)
	case *With:
		f.w("WITH ")
		f.projection(c.Projection)
		f.where(c.Where)
	case *Return:
		f.w("RETURN ")
		f.projection(c.Projection)
	case *Create:
		f.w("CREATE ")
		f.patterns(c.Patterns)
	case *Merge:
		f.w("MERGE ")
		f.pattern(c.Pattern)
		for _, a := range c.Actions {
			if a.OnCreate {
				f.w(" ON CREATE SET ")
			} else {
				f.w(" ON MATCH SET ")
			}
			f.setItems(a.Items)
		}
	case *Set:
		f.w("SET ")
		f.setItems(c.Items)
	case *Remove:
		f.w("REMOVE ")
		for i, it := range c.Items {
			if i > 0 {
				f.w(", ")
			}
			switch it := it.(type) {
			case *RemoveProperty:
				f.expr(it.Target)
			case *RemoveLabels:
				f.w(Name(it.Var))
				f.labels(it.Labels)
			}
		}
	case *Delete:
		if c.Detach {
			f.w("DETACH ")
		}
		f.w("DELETE ")
		f.exprs(c.Exprs)
	}
}

func (f *formatter) where(e Expr) {
	if e != nil {
		f.w(" WHERE ")
		f.expr(e)
	}
}

func (f *formatter) projection(p *Projection) {
	if p.Distinct {
		f.w("DISTINCT ")
	}
	for i, it := range p.Items {
		if i > 0 {
			f.w(", ")
		}
		f.expr(it.X)
		if it.Alias != "" {
			f.w(" AS ", Name(it.Alias))
		}
	}
	if len(p.Order) > 0 {
		f.w(" ORDER BY ")
		for i, s := range p.Order {
			if i > 0 {
				f.w(", ")
			}
			f.expr(s.X)
			if s.Desc {
				f.w(" DESC")
			}
		}
	}
	if p.Skip != nil {
		f.w(" SKIP ")
		f.expr(p.Skip)
	}
	if p.Limit != nil {
		f.w(" LIMIT ")
		f.expr(p.Limit)
	}
}

func (f *formatter) setItems(items []SetItem) {
	for i, it := range items {
		if i > 0 {
			f.w(", ")
		}
		switch it := it.(type) {
		case *SetProperty:
			f.expr(it.Target)
			f.w(" = ")
			f.expr(it.Value)
		case *SetProperties:
			f.w(Name(it.Var))
			if it.Merge {
				f.w(" += ")
			} else {
				f.w(" = ")
			}
			f.expr(it.Value)
		case *SetLabels:
			f.w(Name(it.Var))
			f.labels(it.Labels)
		}
	}
}

func (f *formatter) labels(ls []string) {
	for _, l := range ls {
		f.w(":", Name(l))
	}
}

func (f *formatter) patterns(ps []*Pattern) {
	for i, p := range ps {
		if i > 0 {
			f.w(", ")
		}
		f.pattern(p)
	}
}

func (f *formatter) pattern(p *Pattern) {
	f.node(p.Nodes[0])
	for i, r := range p.Rels {
		f.rel(r)
		f.node(p.Nodes[i+1])
	}
}

func (f *formatter) node(n *NodePattern) {
	f.w("(")
	if n.Var != "" {
		f.w(Name(n.Var))
	}
	f.labels(n.Labels)
	if n.Properties != nil {
		f.w(" ")
		f.expr(n.Properties)
	}
	f.w(")")
}

func (f *formatter) rel(r *RelPattern) {
	if r.Direction == DirIn {
		f.w("<-[")
	} else {
		f.w("-[")
	}
	if r.Var != "" {
		f.w(Name(r.Var))
	}
	for i, t := range r.Types {
		if i == 0 {
			f.w(":")
		} else {
			f.w("|")
		}
		f.w(Name(t))
	}
	if r.Length != nil {
		f.w("*")
		if r.Length.Min != nil {
			f.w(strconv.FormatUint(uint64(*r.Length.Min), 10))
		}
		if r.Length.Min == nil || r.Length.Max == nil || *r.Length.Min != *r.Length.Max {
			f.w("..")
			if r.Length.Max != nil {
				f.w(strconv.FormatUint(uint64(*r.Length.Max), 10))
			}
		}
	}
	if r.Properties != nil {
		f.w(" ")
		f.expr(r.Properties)
	}
	if r.Direction == DirOut {
		f.w("]->")
	} else {
		f.w("]-")
	}
}

func (f *formatter) exprs(es []Expr) {
	for i, e := range es {
		if i > 0 {
			f.w(", ")
		}
		f.expr(e)
	}
}

var binaryText = [...]string{
	OpOr: "OR", OpXor: "XOR", OpAnd: "AND", OpEq: "=", OpNeq: "<>", OpIn: "IN", OpContains: "CONTAINS",
	OpStartsWith: "STARTS WITH", OpEndsWith: "ENDS WITH", OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">=",
	OpVecDist: "<=>", OpFTSMatch: "@@", OpAdd: "+", OpSub: "-", OpMul: "*", OpDiv: "/", OpMod: "%", OpPow: "^",
}

// String returns the operator as written.
func (op BinaryOp) String() string { return binaryText[op] }

func (f *formatter) expr(e Expr) {
	switch e := e.(type) {
	case *Literal:
		f.w(literal(e.Value))
	case *Parameter:
		f.w("$", e.Name)
	case *Variable:
		f.w(Name(e.Name))
	case *Property:
		switch e.X.(type) {
		case *Variable, *Property, *Parameter:
			f.expr(e.X)
		default:
			f.w("(")
			f.expr(e.X)
			f.w(")")
		}
		f.w(".", Name(e.Key))
	case *ListLit:
		f.w("[")
		f.exprs(e.Elems)
		f.w("]")
	case *MapLit:
		f.w("{")
		for i, en := range e.Entries {
			if i > 0 {
				f.w(", ")
			}
			f.w(Name(en.Key), ": ")
			f.expr(en.Value)
		}
		f.w("}")
	case *Unary:
		if e.Op == OpNot {
			f.w("(NOT ")
			f.expr(e.X)
			f.w(")")
			return
		}
		f.w("(- ")
		if lit, ok := e.X.(*Literal); ok && (lit.Value.Kind() == value.KindInt || lit.Value.Kind() == value.KindFloat) {
			// "- 1" would read back as the literal -1.
			f.w("(")
			f.expr(e.X)
			f.w(")")
		} else {
			f.expr(e.X)
		}
		f.w(")")
	case *Binary:
		f.w("(")
		f.expr(e.L)
		f.w(" ", e.Op.String(), " ")
		f.expr(e.R)
		f.w(")")
	case *IsNull:
		f.w("(")
		f.expr(e.X)
		if e.Not {
			f.w(" IS NOT NULL)")
		} else {
			f.w(" IS NULL)")
		}
	case *FuncCall:
		f.w(Name(e.Name), "(")
		if e.Star {
			f.w("*")
		} else {
			if e.Distinct {
				f.w("DISTINCT ")
			}
			f.exprs(e.Args)
		}
		f.w(")")
	}
}

// literal writes a literal so that the lexer reads the same value back.
func literal(v value.Value) string {
	switch v.Kind() {
	case value.KindNull:
		return "NULL"
	case value.KindBool:
		if b, _ := v.AsBool(); b {
			return "TRUE"
		}
		return "FALSE"
	case value.KindInt:
		i, _ := v.AsInt()
		return strconv.FormatInt(i, 10)
	case value.KindFloat:
		x, _ := v.AsFloat()
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".e") && !math.IsInf(x, 0) && !math.IsNaN(x) {
			s += ".0"
		}
		return s
	case value.KindString:
		s, _ := v.AsString()
		return Quote(s)
	}
	return "NULL"
}

// Quote writes s as a single-quoted string literal.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// Name writes an identifier, between backticks when it is not a plain
// identifier or is a keyword; a backtick inside is doubled.
func Name(s string) string {
	if Plain(s) {
		return s
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// Plain reports whether s can be written without backticks: an ASCII
// identifier that is not a keyword.
func Plain(s string) bool {
	if s == "" || IsKeyword(s) || contextual[strings.ToUpper(s)] {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// keywords of the dialect, in upper case. Names that are keywords need
// backticks.
var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`MATCH OPTIONAL WHERE RETURN CREATE DELETE DETACH SET REMOVE MERGE WITH
		UNWIND ON AS ORDER BY LIMIT SKIP AND OR NOT XOR NULL TRUE FALSE IN IS CONTAINS STARTS ENDS
		DISTINCT ASC DESC CALL YIELD EXPLAIN PROFILE USING`) {
		keywords[k] = true
	}
}

// contextual are names that start a construction outside the dialect in
// some positions (CASE, all(...), ...): written bare they would not read
// back as names.
var contextual = map[string]bool{"CASE": true, "ALL": true, "ANY": true, "NONE": true, "SINGLE": true, "REDUCE": true}

// IsKeyword reports whether s is a keyword, in any letter case.
func IsKeyword(s string) bool { return len(s) <= 16 && keywords[strings.ToUpper(s)] }
