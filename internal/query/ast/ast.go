// Package ast is the syntax tree of the tacdb-cypher/1 dialect (SPEC
// §15.1). A tree is immutable once the parser returns it; every node keeps
// the span of source it came from.
package ast

import "github.com/TacFlow/tacdb/value"

// Span is a range of bytes [Start, End) of the source.
type Span struct{ Start, End int }

// Query is one statement.
type Query struct {
	Explain bool
	Profile bool
	Clauses []Clause
	Span    Span
}

// Clause is a clause of a query.
type Clause interface {
	clause()
	Pos() Span
}

// Match is MATCH or OPTIONAL MATCH.
type Match struct {
	Optional bool
	Patterns []*Pattern
	Hint     *VectorHint // USING VECTOR, right after the patterns
	Where    Expr        // nil when absent
	Span     Span
}

// VectorHint is USING VECTOR target OPTIONS {...} (SPEC §14.3).
type VectorHint struct {
	Target  *Property
	Options *MapLit // nil when absent
	Span    Span
}

// Unwind is UNWIND x AS alias.
type Unwind struct {
	X     Expr
	Alias string
	Span  Span
}

// Call is CALL proc(args) YIELD items [WHERE cond].
type Call struct {
	Procedure []string // the parts of the dotted name, as written
	Args      []Expr
	Yield     []*YieldItem
	Where     Expr
	Span      Span
}

// YieldItem is a column of a procedure, with an optional alias.
type YieldItem struct {
	Name  string
	Alias string // empty when absent
	Span  Span
}

// With is WITH projection [WHERE cond].
type With struct {
	Projection *Projection
	Where      Expr
	Span       Span
}

// Return is RETURN projection.
type Return struct {
	Projection *Projection
	Span       Span
}

// Projection is the shared body of WITH and RETURN.
type Projection struct {
	Distinct bool
	Items    []*ProjectionItem
	Order    []*SortItem
	Skip     Expr
	Limit    Expr
}

// ProjectionItem is expr [AS alias].
type ProjectionItem struct {
	X     Expr
	Alias string // empty when absent
	Span  Span
}

// SortItem is expr [ASC|DESC].
type SortItem struct {
	X    Expr
	Desc bool
	Span Span
}

// Create is CREATE patterns.
type Create struct {
	Patterns []*Pattern
	Span     Span
}

// Merge is MERGE pattern with its ON CREATE / ON MATCH actions, in order.
type Merge struct {
	Pattern *Pattern
	Actions []*MergeAction
	Span    Span
}

// MergeAction is ON CREATE SET items or ON MATCH SET items.
type MergeAction struct {
	OnCreate bool
	Items    []SetItem
	Span     Span
}

// Set is SET items.
type Set struct {
	Items []SetItem
	Span  Span
}

// Remove is REMOVE items.
type Remove struct {
	Items []RemoveItem
	Span  Span
}

// Delete is [DETACH] DELETE exprs.
type Delete struct {
	Detach bool
	Exprs  []Expr
	Span   Span
}

func (*Match) clause()  {}
func (*Unwind) clause() {}
func (*Call) clause()   {}
func (*With) clause()   {}
func (*Return) clause() {}
func (*Create) clause() {}
func (*Merge) clause()  {}
func (*Set) clause()    {}
func (*Remove) clause() {}
func (*Delete) clause() {}

func (c *Match) Pos() Span  { return c.Span }
func (c *Unwind) Pos() Span { return c.Span }
func (c *Call) Pos() Span   { return c.Span }
func (c *With) Pos() Span   { return c.Span }
func (c *Return) Pos() Span { return c.Span }
func (c *Create) Pos() Span { return c.Span }
func (c *Merge) Pos() Span  { return c.Span }
func (c *Set) Pos() Span    { return c.Span }
func (c *Remove) Pos() Span { return c.Span }
func (c *Delete) Pos() Span { return c.Span }

// SetItem is an item of SET.
type SetItem interface {
	setItem()
	Pos() Span
}

// SetProperty is target.key = value.
type SetProperty struct {
	Target *Property
	Value  Expr
	Span   Span
}

// SetProperties is var = map (Merge false) or var += map (Merge true).
type SetProperties struct {
	Var   string
	Value Expr
	Merge bool
	Span  Span
}

// SetLabels is var:L1:L2.
type SetLabels struct {
	Var    string
	Labels []string
	Span   Span
}

func (*SetProperty) setItem()   {}
func (*SetProperties) setItem() {}
func (*SetLabels) setItem()     {}

func (i *SetProperty) Pos() Span   { return i.Span }
func (i *SetProperties) Pos() Span { return i.Span }
func (i *SetLabels) Pos() Span     { return i.Span }

// RemoveItem is an item of REMOVE.
type RemoveItem interface {
	removeItem()
	Pos() Span
}

// RemoveProperty is REMOVE target.key.
type RemoveProperty struct {
	Target *Property
	Span   Span
}

// RemoveLabels is REMOVE var:L1:L2.
type RemoveLabels struct {
	Var    string
	Labels []string
	Span   Span
}

func (*RemoveProperty) removeItem() {}
func (*RemoveLabels) removeItem()   {}

func (i *RemoveProperty) Pos() Span { return i.Span }
func (i *RemoveLabels) Pos() Span   { return i.Span }

// Pattern is a chain of nodes joined by relationships: Nodes has one more
// element than Rels.
type Pattern struct {
	Nodes []*NodePattern
	Rels  []*RelPattern
	Span  Span
}

// NodePattern is (var:L1:L2 {props}).
type NodePattern struct {
	Var        string // empty when anonymous
	Labels     []string
	Properties *MapLit // nil when absent
	Span       Span
}

// Direction of a relationship pattern.
type Direction uint8

// Directions, as written from the left node.
const (
	DirBoth Direction = iota // -[]- or <-[]->
	DirOut                   // -[]->
	DirIn                    // <-[]-
)

// RelPattern is -[var:T1|T2 *min..max {props}]->.
type RelPattern struct {
	Var        string
	Types      []string // alternatives
	Direction  Direction
	Length     *Length // nil for a single hop
	Properties *MapLit
	Span       Span
}

// Length is the range of a variable-length relationship; nil bounds are
// open.
type Length struct {
	Min, Max *uint32
}

// Expr is an expression.
type Expr interface {
	expr()
	Pos() Span
}

// Literal is an Int, Float, String, Bool or Null literal.
type Literal struct {
	Value value.Value
	Span  Span
}

// Parameter is $name.
type Parameter struct {
	Name string
	Span Span
}

// Variable is a name in scope.
type Variable struct {
	Name string
	Span Span
}

// Property is x.key.
type Property struct {
	X    Expr
	Key  string
	Span Span
}

// ListLit is [e1, e2, ...].
type ListLit struct {
	Elems []Expr
	Span  Span
}

// MapLit is {k1: e1, ...}, entries in the order written.
type MapLit struct {
	Entries []*MapEntry
	Span    Span
}

// MapEntry is an entry of a map literal.
type MapEntry struct {
	Key   string
	Value Expr
	Span  Span
}

// UnaryOp is a prefix operator.
type UnaryOp uint8

// Prefix operators. Unary plus is dropped by the parser.
const (
	OpNot UnaryOp = iota
	OpNeg
)

// Unary is a prefix operation.
type Unary struct {
	Op   UnaryOp
	X    Expr
	Span Span
}

// BinaryOp is an infix operator.
type BinaryOp uint8

// Infix operators.
const (
	OpOr BinaryOp = iota
	OpXor
	OpAnd
	OpEq
	OpNeq
	OpIn
	OpContains
	OpStartsWith
	OpEndsWith
	OpLt
	OpLe
	OpGt
	OpGe
	OpVecDist  // <=>
	OpFTSMatch // @@
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpMod
	OpPow
)

// Binary is an infix operation.
type Binary struct {
	Op   BinaryOp
	L, R Expr
	Span Span
}

// IsNull is x IS NULL, or x IS NOT NULL when Not.
type IsNull struct {
	X    Expr
	Not  bool
	Span Span
}

// FuncCall is name([DISTINCT] args) or count(*).
type FuncCall struct {
	Name     string // as written
	Distinct bool
	Star     bool // count(*)
	Args     []Expr
	Span     Span
}

func (*Literal) expr()   {}
func (*Parameter) expr() {}
func (*Variable) expr()  {}
func (*Property) expr()  {}
func (*ListLit) expr()   {}
func (*MapLit) expr()    {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*IsNull) expr()    {}
func (*FuncCall) expr()  {}

func (e *Literal) Pos() Span   { return e.Span }
func (e *Parameter) Pos() Span { return e.Span }
func (e *Variable) Pos() Span  { return e.Span }
func (e *Property) Pos() Span  { return e.Span }
func (e *ListLit) Pos() Span   { return e.Span }
func (e *MapLit) Pos() Span    { return e.Span }
func (e *Unary) Pos() Span     { return e.Span }
func (e *Binary) Pos() Span    { return e.Span }
func (e *IsNull) Pos() Span    { return e.Span }
func (e *FuncCall) Pos() Span  { return e.Span }
