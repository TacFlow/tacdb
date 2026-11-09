package plan

import (
	"strconv"

	"github.com/TacFlow/tacdb/internal/query/ast"
)

// The operators of the clauses that write (design §4.4). Each one reads
// every row of its input before its first effect and applies its effects
// to all of them before it yields its first row: a full barrier of
// materialization, so that no read runs while a clause writes and the
// clauses after it read every effect.
type (
	// Create creates, for each row, the elements of Steps in order.
	Create struct {
		In    Op
		Steps []CreateStep
		Span  ast.Span // of the clause, for the location of its errors
	}
	// Merge matches its pattern, Match, for each row, with a fresh plan
	// that sees what earlier rows created. Each match is a row, after the
	// ON MATCH actions; without one, the row gets the elements of Steps
	// and the ON CREATE actions. Values are the property values of the
	// pattern, which must not be Null.
	Merge struct {
		In      Op
		Match   Op
		Values  []Expr
		Steps   []CreateStep
		Actions []MergeAction
		Span    ast.Span
	}
	// Set applies the items of SET in order, row by row.
	Set struct {
		In    Op
		Items []SetItem
		Span  ast.Span
	}
	// Remove applies the items of REMOVE in order, row by row.
	Remove struct {
		In    Op
		Items []SetItem
		Span  ast.Span
	}
	// Delete deletes the entities of Exprs over every row: the
	// relationships first, then the nodes, so that a node whose
	// relationships the clause deletes too has none left when its turn
	// comes. Detach deletes the other relationships of the nodes too.
	Delete struct {
		In     Op
		Exprs  []Expr
		Detach bool
		Span   ast.Span
	}
)

// CreateStep creates one element of a pattern into Slot: a node with
// Labels, or a relationship of Type from the node in From to the node in
// To. Props are evaluated on the row, and a Null value writes nothing.
type CreateStep struct {
	Rel      bool
	Slot     int
	Labels   []string
	Type     string
	From, To int
	Props    []PropTest
}

// MergeAction is ON CREATE SET or ON MATCH SET.
type MergeAction struct {
	OnCreate bool
	Items    []SetItem
}

// SetKind is what an item of SET or REMOVE does.
type SetKind uint8

// Items of SET and REMOVE.
const (
	SetProperty     SetKind = iota // Slot.Key = Value
	SetProperties                  // Slot = Value, a map
	MergeProperties                // Slot += Value, a map
	AddLabels                      // SET Slot:Labels
	RemoveProperty                 // REMOVE Slot.Key
	RemoveLabels                   // REMOVE Slot:Labels
)

// SetItem is an item of SET, of REMOVE or of the actions of MERGE.
type SetItem struct {
	Kind   SetKind
	Slot   int
	Key    string
	Value  *Expr
	Labels []string
}

func (*Create) Name() string { return "Create" }
func (*Merge) Name() string  { return "Merge" }
func (*Set) Name() string    { return "Set" }
func (*Remove) Name() string { return "Remove" }
func (*Delete) Name() string { return "Delete" }

func (o *Create) Inputs() []Op { return in(o.In) }
func (o *Merge) Inputs() []Op  { return []Op{o.In, o.Match} }
func (o *Set) Inputs() []Op    { return in(o.In) }
func (o *Remove) Inputs() []Op { return in(o.In) }
func (o *Delete) Inputs() []Op { return in(o.In) }

// written ends a clause that writes: what follows is no longer one row
// of SingleRow nor clustered, and a LIMIT after it must not cut effects.
func (p *planner) written(op Op) {
	p.op, p.single, p.cluster, p.write = op, false, -1, true
}

// create plans CREATE: the elements of each pattern in order, a node
// before the relationships that reach it. A node bound before, or earlier
// in the clause, is reused.
func (p *planner) create(c *ast.Create) error {
	var steps []CreateStep
	for _, pat := range c.Patterns {
		slots := make([]int, len(pat.Nodes))
		for i, n := range pat.Nodes {
			if p.info.Bound[n] {
				slots[i] = p.names[n.Var]
			} else {
				props, err := p.writeProps(n.Properties)
				if err != nil {
					return err
				}
				slots[i] = p.writeSlot(n.Var)
				steps = append(steps, CreateStep{Slot: slots[i], Labels: n.Labels, Props: props})
			}
			if i == 0 {
				continue
			}
			r := pat.Rels[i-1]
			props, err := p.writeProps(r.Properties)
			if err != nil {
				return err
			}
			from, to := slots[i-1], slots[i]
			if r.Direction == ast.DirIn {
				from, to = to, from
			}
			steps = append(steps, CreateStep{Rel: true, Slot: p.writeSlot(r.Var), Type: r.Types[0], From: from, To: to, Props: props})
		}
	}
	p.written(&Create{In: p.op, Steps: steps, Span: c.Span})
	return nil
}

// writeSlot gives a new slot to an element of CREATE or MERGE; an
// anonymous one gets a hidden name.
func (p *planner) writeSlot(name string) int {
	if name == "" {
		name = hiddenPrefix + strconv.Itoa(p.part.Width)
	}
	return p.declare(name)
}

func (p *planner) writeProps(m *ast.MapLit) ([]PropTest, error) {
	if m == nil {
		return nil, nil
	}
	out := make([]PropTest, len(m.Entries))
	for i, en := range m.Entries {
		x, err := p.compile(en.Value, p.scope())
		if err != nil {
			return nil, err
		}
		out[i] = PropTest{Key: en.Key, Value: x}
	}
	return out, nil
}

// merge plans MERGE: its pattern matched as a MATCH clause over the row,
// then what a row without a match creates, with the slots the match
// declared.
func (p *planner) merge(c *ast.Merge) error {
	var values []Expr
	for i, n := range c.Pattern.Nodes {
		props, err := p.writeProps(n.Properties)
		if err != nil {
			return err
		}
		for _, x := range props {
			values = append(values, x.Value)
		}
		if i < len(c.Pattern.Rels) {
			if props, err = p.writeProps(c.Pattern.Rels[i].Properties); err != nil {
				return err
			}
			for _, x := range props {
				values = append(values, x.Value)
			}
		}
	}
	outer := p.op
	p.op = &Argument{Part: p.part}
	st, err := p.matchPatterns(&ast.Match{Patterns: []*ast.Pattern{c.Pattern}, Span: c.Span})
	if err != nil {
		return err
	}
	match := p.op
	var steps []CreateStep
	made := map[int]bool{}
	pat := c.Pattern
	for i, n := range pat.Nodes {
		if s := st.slots[n]; !st.before[s] && !made[s] {
			made[s] = true
			props, err := p.writeProps(n.Properties)
			if err != nil {
				return err
			}
			steps = append(steps, CreateStep{Slot: s, Labels: n.Labels, Props: props})
		}
		if i == 0 {
			continue
		}
		r := pat.Rels[i-1]
		props, err := p.writeProps(r.Properties)
		if err != nil {
			return err
		}
		from, to := st.slots[pat.Nodes[i-1]], st.slots[n]
		if r.Direction == ast.DirIn {
			from, to = to, from
		}
		steps = append(steps, CreateStep{Rel: true, Slot: st.rels[r], Type: r.Types[0], From: from, To: to, Props: props})
	}
	m := &Merge{In: outer, Match: match, Values: values, Steps: steps, Span: c.Span}
	for _, a := range c.Actions {
		items, err := p.setItems(a.Items)
		if err != nil {
			return err
		}
		m.Actions = append(m.Actions, MergeAction{OnCreate: a.OnCreate, Items: items})
	}
	p.written(m)
	return nil
}

func (p *planner) setItems(items []ast.SetItem) ([]SetItem, error) {
	out := make([]SetItem, 0, len(items))
	for _, it := range items {
		var s SetItem
		switch it := it.(type) {
		case *ast.SetProperty:
			s = SetItem{Kind: SetProperty, Slot: p.names[it.Target.X.(*ast.Variable).Name], Key: it.Target.Key}
			x, err := p.compile(it.Value, p.scope())
			if err != nil {
				return nil, err
			}
			s.Value = &x
		case *ast.SetProperties:
			s = SetItem{Kind: SetProperties, Slot: p.names[it.Var]}
			if it.Merge {
				s.Kind = MergeProperties
			}
			x, err := p.compile(it.Value, p.scope())
			if err != nil {
				return nil, err
			}
			s.Value = &x
		case *ast.SetLabels:
			s = SetItem{Kind: AddLabels, Slot: p.names[it.Var], Labels: it.Labels}
		}
		out = append(out, s)
	}
	return out, nil
}

func (p *planner) removeItems(items []ast.RemoveItem) []SetItem {
	out := make([]SetItem, 0, len(items))
	for _, it := range items {
		switch it := it.(type) {
		case *ast.RemoveProperty:
			out = append(out, SetItem{Kind: RemoveProperty, Slot: p.names[it.Target.X.(*ast.Variable).Name], Key: it.Target.Key})
		case *ast.RemoveLabels:
			out = append(out, SetItem{Kind: RemoveLabels, Slot: p.names[it.Var], Labels: it.Labels})
		}
	}
	return out
}

func (p *planner) delete(c *ast.Delete) error {
	d := &Delete{In: p.op, Detach: c.Detach, Span: c.Span}
	for _, e := range c.Exprs {
		x, err := p.compile(e, p.scope())
		if err != nil {
			return err
		}
		d.Exprs = append(d.Exprs, x)
	}
	p.written(d)
	return nil
}
