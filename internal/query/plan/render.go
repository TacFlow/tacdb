package plan

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/graph"
)

// RenderVersion is the version of the JSON of a rendered plan.
const RenderVersion = 1

// node is an operator in the rendered plan.
type node struct {
	Op      string            `json:"op"`
	Details map[string]string `json:"details,omitempty"`
	Inputs  []node            `json:"inputs,omitempty"`
}

// Render returns the plan as JSON: the version, the columns and the tree
// of operators, each with its details as text. Equal plans give equal
// bytes; it is internal until EXPLAIN publishes it.
func Render(p *Plan) []byte {
	cols := make([]string, len(p.Columns))
	for i, c := range p.Columns {
		cols[i] = c.Name
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		Version int      `json:"version"`
		Columns []string `json:"columns"`
		Plan    node     `json:"plan"`
	}{RenderVersion, cols, render(p.Root)})
	if err != nil {
		panic(err) // only strings and slices
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// render renders op and its inputs.
func render(op Op) node {
	n := node{Op: op.Name(), Details: map[string]string{}}
	part := partOf(op)
	d := n.Details
	name := func(slot int) string {
		if part == nil {
			return "#" + strconv.Itoa(slot)
		}
		return part.String(slot)
	}
	props := func(key string, ps []PropTest) {
		if len(ps) == 0 {
			return
		}
		s := "{"
		for i, p := range ps {
			if i > 0 {
				s += ", "
			}
			s += p.Key + ": " + p.Value.Text
		}
		d[key] = s + "}"
	}
	labels := func(key string, ls []string) {
		if len(ls) > 0 {
			d[key] = strings.Join(ls, "|")
		}
	}
	slots := func(key string, ss []int) {
		if len(ss) == 0 {
			return
		}
		s := ""
		for i, x := range ss {
			if i > 0 {
				s += ", "
			}
			s += name(x)
		}
		d[key] = s
	}
	dir := func(x graph.Direction) { d["direction"] = [...]string{"out", "in", "both"}[x] }
	switch o := op.(type) {
	case *NodeScan:
		d["variable"] = name(o.Slot)
		labels("labels", o.Labels)
		props("properties", o.Props)
	case *LabelScan:
		d["variable"], d["label"] = name(o.Slot), o.Label
		labels("labels", o.Labels)
		props("properties", o.Props)
	case *IDSeek:
		d["variable"], d["id"] = name(o.Slot), o.ID.Text
		labels("labels", o.Labels)
		props("properties", o.Props)
	case *NodeFilter:
		d["variable"] = name(o.Slot)
		labels("labels", o.Labels)
		props("properties", o.Props)
	case *Expand:
		d["from"], d["to"], d["relationship"] = name(o.From), name(o.To), name(o.Rel)
		dir(o.Dir)
		labels("types", o.Types)
		labels("toLabels", o.ToLabels)
		props("properties", o.RelProps)
		props("toProperties", o.ToProps)
		slots("distinctFrom", append(append([]int{}, o.Unique...), o.Lists...))
		if o.RelBound {
			d["bound"] = "true"
		}
	case *VarExpand:
		d["from"], d["to"], d["edges"] = name(o.From), name(o.To), name(o.List)
		dir(o.Dir)
		labels("types", o.Types)
		labels("toLabels", o.ToLabels)
		props("toProperties", o.ToProps)
		slots("distinctFrom", append(append([]int{}, o.Unique...), o.Lists...))
		d["min"] = strconv.Itoa(o.Min)
		if o.Unbounded {
			d["max"] = "MaxHops"
		} else {
			d["max"] = strconv.Itoa(o.Max)
		}
		if o.Into {
			d["into"] = "true"
		}
	case *Filter:
		d["predicate"] = o.Pred.Text
	case *Unwind:
		d["expression"], d["variable"] = o.X.Text, name(o.Slot)
	case *OptionalApply:
		slots("nullable", o.Slots)
	case *Project:
		s := ""
		for i, x := range o.Items {
			if i > 0 {
				s += ", "
			}
			s += x.Text + " AS " + o.Out.String(i)
		}
		d["items"] = s
		for i, k := range o.Keys {
			d["sortKey"+strconv.Itoa(i)] = k.Text
		}
	case *Aggregate:
		s := ""
		for i, k := range o.Keys {
			if i > 0 {
				s += ", "
			}
			s += k.Text
		}
		if s != "" {
			d["keys"] = s
		}
		s = ""
		for i, a := range o.Aggs {
			if i > 0 {
				s += ", "
			}
			s += a.Text
		}
		d["aggregates"] = s
	case *Distinct:
		d["columns"] = strconv.Itoa(o.N)
	case *Sort:
		sortKeys(d, o.Keys)
	case *TopK:
		sortKeys(d, o.Keys)
		if o.Skip != nil {
			d["skip"] = o.Skip.Text
		}
		d["limit"] = o.Limit.Text
		if o.Exhaustive {
			d["exhaustive"] = "true"
		}
	case *Skip:
		d["count"] = o.X.Text
	case *Limit:
		d["count"] = o.X.Text
	case *Create:
		steps(d, "create", o.Steps, name)
	case *Merge:
		steps(d, "create", o.Steps, name)
		for i, a := range o.Actions {
			k := "onMatch"
			if a.OnCreate {
				k = "onCreate"
			}
			d[k+strconv.Itoa(i)] = setItems(a.Items, name)
		}
	case *Set:
		d["items"] = setItems(o.Items, name)
	case *Remove:
		d["items"] = setItems(o.Items, name)
	case *Delete:
		s := ""
		for i, x := range o.Exprs {
			if i > 0 {
				s += ", "
			}
			s += x.Text
		}
		d["entities"] = s
		if o.Detach {
			d["detach"] = "true"
		}
	}
	if len(d) == 0 {
		n.Details = nil
	}
	for _, x := range op.Inputs() {
		n.Inputs = append(n.Inputs, render(x))
	}
	return n
}

// steps renders the elements a Create or a Merge creates, in order.
func steps(d map[string]string, key string, ss []CreateStep, name func(int) string) {
	s := ""
	for i, st := range ss {
		if i > 0 {
			s += ", "
		}
		if st.Rel {
			s += "(" + name(st.From) + ")-[" + name(st.Slot) + ":" + st.Type + propText(st.Props) + "]->(" + name(st.To) + ")"
			continue
		}
		s += "(" + name(st.Slot)
		for _, l := range st.Labels {
			s += ":" + l
		}
		s += propText(st.Props) + ")"
	}
	d[key] = s
}

func propText(ps []PropTest) string {
	if len(ps) == 0 {
		return ""
	}
	s := " {"
	for i, p := range ps {
		if i > 0 {
			s += ", "
		}
		s += p.Key + ": " + p.Value.Text
	}
	return s + "}"
}

// setItems renders the items of SET, REMOVE or a MERGE action.
func setItems(items []SetItem, name func(int) string) string {
	s := ""
	for i, it := range items {
		if i > 0 {
			s += ", "
		}
		v := name(it.Slot)
		switch it.Kind {
		case SetProperty:
			s += v + "." + it.Key + " = " + it.Value.Text
		case SetProperties:
			s += v + " = " + it.Value.Text
		case MergeProperties:
			s += v + " += " + it.Value.Text
		case AddLabels, RemoveLabels:
			s += v + ":" + strings.Join(it.Labels, ":")
		case RemoveProperty:
			s += v + "." + it.Key
		}
	}
	return s
}

func sortKeys(d map[string]string, keys []SortKey) {
	for i, k := range keys {
		s := k.Text
		if k.Desc {
			s += " DESC"
		}
		d["key"+strconv.Itoa(i)] = s
	}
}

// partOf returns the layout of the rows op yields, found at its leaf or
// at the projection that made them.
func partOf(op Op) *Part {
	for {
		switch o := op.(type) {
		case *SingleRow:
			return o.Part
		case *Argument:
			return o.Part
		case *Project:
			return o.Out
		case *Aggregate:
			return o.Part
		}
		ins := op.Inputs()
		if len(ins) == 0 {
			return nil
		}
		op = ins[0]
	}
}
