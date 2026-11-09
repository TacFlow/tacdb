package tacdb

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/eval"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// The corpus of read queries (design §7): cases transcribed from the
// inventory of the reference with the results tacdb gives, each marked
// parity or divergence, and cases of tacdb's own. Setups are CREATE
// statements, which the runner reads with the parser and runs through the
// typed graph calls, so that reads are tested before the executor writes.

type corpusCase struct {
	Name     string                     `json:"name"`
	Source   string                     `json:"source"`
	Kind     string                     `json:"kind"`
	Decision string                     `json:"decision,omitempty"`
	Note     string                     `json:"note,omitempty"`
	Setup    []string                   `json:"setup"`
	Query    string                     `json:"query"`
	Params   map[string]json.RawMessage `json:"params,omitempty"`
	Expect   corpusExpect               `json:"expect"`
}

type corpusExpect struct {
	Rows     [][]json.RawMessage `json:"rows,omitempty"`
	Ordered  bool                `json:"ordered,omitempty"`
	RowCount *int                `json:"rowCount,omitempty"`
	Columns  []string            `json:"columns,omitempty"`
	Error    string              `json:"error,omitempty"`
}

// typed is a value of the corpus.
type typed struct {
	Type       string                     `json:"type"`
	Value      json.RawMessage            `json:"value,omitempty"`
	Labels     []string                   `json:"labels,omitempty"`
	Properties map[string]json.RawMessage `json:"properties,omitempty"`
	EdgeType   string                     `json:"edgeType,omitempty"`
}

func decodeTyped(raw json.RawMessage) (typed, error) {
	var t typed
	err := json.Unmarshal(raw, &t)
	return t, err
}

// toValue converts a property value of the corpus.
func toValue(raw json.RawMessage) (value.Value, error) {
	t, err := decodeTyped(raw)
	if err != nil {
		return value.Value{}, err
	}
	switch t.Type {
	case "Null":
		return value.Null(), nil
	case "Bool":
		var b bool
		err := json.Unmarshal(t.Value, &b)
		return value.Bool(b), err
	case "Int":
		var i int64
		err := json.Unmarshal(t.Value, &i)
		return value.Int(i), err
	case "Float":
		f, err := corpusFloat(t.Value)
		return value.Float(f), err
	case "String":
		var s string
		err := json.Unmarshal(t.Value, &s)
		return value.String(s), err
	case "List":
		var items []json.RawMessage
		if err := json.Unmarshal(t.Value, &items); err != nil {
			return value.Value{}, err
		}
		vs := make([]value.Value, len(items))
		for i, it := range items {
			if vs[i], err = toValue(it); err != nil {
				return value.Value{}, err
			}
		}
		return value.List(vs...), nil
	case "Map":
		var m map[string]json.RawMessage
		if err := json.Unmarshal(t.Value, &m); err != nil {
			return value.Value{}, err
		}
		out := map[string]value.Value{}
		for k, it := range m {
			if out[k], err = toValue(it); err != nil {
				return value.Value{}, err
			}
		}
		return value.MapOf(out), nil
	}
	return value.Value{}, fmt.Errorf("type %q is not a property value", t.Type)
}

// corpusFloat reads a Float: a JSON number, a decimal string, or the bits
// as "0x" and 16 hex digits.
func corpusFloat(raw json.RawMessage) (float64, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.HasPrefix(s, "0x") {
			b, err := strconv.ParseUint(s[2:], 16, 64)
			return math.Float64frombits(b), err
		}
		return strconv.ParseFloat(s, 64)
	}
	var f float64
	err := json.Unmarshal(raw, &f)
	return f, err
}

// encodeResult renders a value of a result in the typed form of the
// corpus: nodes and edges by labels, type and properties, not IDs.
func encodeResult(v QueryValue) any {
	switch v.Type() {
	case TypeNull:
		return map[string]any{"type": "Null"}
	case TypeNode:
		n, _ := v.Node()
		return map[string]any{"type": "Node", "labels": nonNil(n.Labels), "properties": encodeProps(n.Properties)}
	case TypeEdge:
		e, _ := v.Edge()
		return map[string]any{"type": "Edge", "edgeType": e.Type, "properties": encodeProps(e.Properties)}
	case TypeList:
		l, _ := v.List()
		items := make([]any, len(l))
		for i, it := range l {
			items[i] = encodeResult(it)
		}
		return map[string]any{"type": "List", "value": items}
	case TypeMap:
		m, _ := v.Map()
		out := map[string]any{}
		for k, it := range m {
			out[k] = encodeResult(it)
		}
		return map[string]any{"type": "Map", "value": out}
	}
	x, _ := v.Value()
	return encodeValue(x)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func encodeProps(p map[string]value.Value) map[string]any {
	out := map[string]any{}
	for k, v := range p {
		out[k] = encodeValue(v)
	}
	return out
}

func encodeValue(v value.Value) any {
	switch v.Kind() {
	case value.KindNull:
		return map[string]any{"type": "Null"}
	case value.KindBool:
		b, _ := v.AsBool()
		return map[string]any{"type": "Bool", "value": b}
	case value.KindInt:
		i, _ := v.AsInt()
		return map[string]any{"type": "Int", "value": i}
	case value.KindFloat:
		f, _ := v.AsFloat()
		return map[string]any{"type": "Float", "value": fmt.Sprintf("0x%016x", math.Float64bits(f))}
	case value.KindString:
		s, _ := v.AsString()
		return map[string]any{"type": "String", "value": s}
	case value.KindList:
		items := make([]any, v.Len())
		for i := range items {
			items[i] = encodeValue(v.Index(i))
		}
		return map[string]any{"type": "List", "value": items}
	case value.KindMap:
		out := map[string]any{}
		for i := 0; i < v.Len(); i++ {
			k, it := v.EntryAt(i)
			out[k] = encodeValue(it)
		}
		return map[string]any{"type": "Map", "value": out}
	}
	return map[string]any{"type": v.Kind().String(), "value": v.String()}
}

// canonical renders a typed value of the corpus so that equal values give
// equal text: floats by bits, maps by key.
func canonical(raw json.RawMessage) string {
	t, err := decodeTyped(raw)
	if err != nil {
		return "invalid " + string(raw)
	}
	switch t.Type {
	case "Float":
		f, err := corpusFloat(t.Value)
		if err != nil {
			return "invalid float"
		}
		return fmt.Sprintf("Float(%016x)", math.Float64bits(f))
	case "List":
		var items []json.RawMessage
		json.Unmarshal(t.Value, &items)
		s := make([]string, len(items))
		for i, it := range items {
			s[i] = canonical(it)
		}
		return "[" + strings.Join(s, ", ") + "]"
	case "Map":
		var m map[string]json.RawMessage
		json.Unmarshal(t.Value, &m)
		return canonicalMap(m)
	case "Node":
		return "Node(" + strings.Join(t.Labels, ":") + " " + canonicalMap(t.Properties) + ")"
	case "Edge":
		return "Edge(" + t.EdgeType + " " + canonicalMap(t.Properties) + ")"
	case "Null":
		return "null"
	}
	return t.Type + "(" + string(t.Value) + ")"
}

func canonicalMap(m map[string]json.RawMessage) string {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
	s := make([]string, len(keys))
	for i, k := range keys {
		s[i] = k + ": " + canonical(m[k])
	}
	return "{" + strings.Join(s, ", ") + "}"
}

// runSetup creates what the CREATE statements of a setup describe, with
// the typed calls of the graph.
func runSetup(t *testing.T, db *DB, setup []string) {
	t.Helper()
	tx, err := db.BeginWrite(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, src := range setup {
		q, err := parser.Parse(src, parser.DefaultLimits())
		if err != nil {
			t.Fatalf("setup %s: %v", src, err)
		}
		vars := map[string]NodeID{}
		literal := func(m *ast.MapLit) map[string]value.Value {
			if m == nil {
				return nil
			}
			out := map[string]value.Value{}
			for _, en := range m.Entries {
				x, err := eval.Compile(en.Value, &table{}, src)
				if err != nil {
					t.Fatalf("setup %s: %v", src, err)
				}
				v, err := x.Eval(&eval.Env{Ctx: bg}, eval.Slots{})
				if err != nil {
					t.Fatalf("setup %s: %v", src, err)
				}
				pv, err := qv.ToValue(v)
				if err != nil {
					t.Fatalf("setup %s: %v", src, err)
				}
				if !pv.IsNull() {
					out[en.Key] = pv
				}
			}
			return out
		}
		node := func(n *ast.NodePattern) NodeID {
			if id, ok := vars[n.Var]; ok && n.Var != "" {
				return id
			}
			created, err := tx.CreateNode(CreateNodeOptions{Labels: n.Labels, Properties: literal(n.Properties)})
			if err != nil {
				t.Fatalf("setup %s: %v", src, err)
			}
			if n.Var != "" {
				vars[n.Var] = created.ID
			}
			return created.ID
		}
		for _, c := range q.Clauses {
			cr, ok := c.(*ast.Create)
			if !ok {
				t.Fatalf("setup %s: only CREATE", src)
			}
			for _, p := range cr.Patterns {
				prev := node(p.Nodes[0])
				for i, r := range p.Rels {
					next := node(p.Nodes[i+1])
					from, to := prev, next
					if r.Direction == ast.DirIn {
						from, to = next, prev
					}
					if _, err := tx.CreateEdge(from, to, r.Types[0], literal(r.Properties)); err != nil {
						t.Fatalf("setup %s: %v", src, err)
					}
					prev = next
				}
			}
		}
	}
	if _, err := tx.Commit(bg); err != nil {
		t.Fatal(err)
	}
}

// corpusResult runs a case and returns its rows in typed form, or its
// error code.
func corpusResult(t *testing.T, c corpusCase) (rows [][]any, cols []string, errCode Code) {
	t.Helper()
	db, err := OpenMemory(options())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runSetup(t, db, c.Setup)
	params := Params{}
	for k, raw := range c.Params {
		if params[k], err = toValue(raw); err != nil {
			t.Fatalf("%s: parameter %s: %v", c.Name, k, err)
		}
	}
	rs, err := db.Query(bg, c.Query, params)
	if err != nil {
		return nil, nil, code(err)
	}
	for _, col := range rs.Columns() {
		cols = append(cols, col.Name)
	}
	for rs.Next() {
		var r []any
		for _, v := range rs.Values() {
			r = append(r, encodeResult(v))
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return nil, cols, code(err)
	}
	return rows, cols, ""
}

func rowTexts(rows [][]json.RawMessage) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		s := make([]string, len(r))
		for j, v := range r {
			s[j] = canonical(v)
		}
		out[i] = strings.Join(s, " | ")
	}
	return out
}

func loadCorpus(t *testing.T, path string) []corpusCase {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestReadCorpus(t *testing.T) {
	cases := loadCorpus(t, "testdata/corpus/read.json")
	dump := os.Getenv("TACDB_CORPUS_DUMP")
	var dumped []map[string]any
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("case %q twice", c.Name)
		}
		seen[c.Name] = true
		if c.Kind != "parity" && c.Kind != "divergence" || c.Source != "reference" && c.Source != "tacdb" ||
			(c.Kind == "divergence") != (c.Decision != "") {
			t.Fatalf("%s: kind %q, source %q, decision %q", c.Name, c.Kind, c.Source, c.Decision)
		}
		rows, cols, errCode := corpusResult(t, c)
		if dump != "" {
			raw, _ := json.Marshal(rows)
			dumped = append(dumped, map[string]any{"name": c.Name, "rows": json.RawMessage(raw), "columns": cols, "error": errCode})
			continue
		}
		if c.Expect.Error != "" || errCode != "" {
			if string(errCode) != c.Expect.Error {
				t.Errorf("%s: %s: error %q, want %q", c.Name, c.Query, errCode, c.Expect.Error)
			}
			continue
		}
		if c.Expect.RowCount != nil && len(rows) != *c.Expect.RowCount {
			t.Errorf("%s: %s: %d rows, want %d", c.Name, c.Query, len(rows), *c.Expect.RowCount)
		}
		if c.Expect.Columns != nil && !slices.Equal(cols, c.Expect.Columns) {
			t.Errorf("%s: columns %q, want %q", c.Name, cols, c.Expect.Columns)
		}
		if c.Expect.Rows == nil {
			continue
		}
		var gotRaw [][]json.RawMessage
		for _, r := range rows {
			var rr []json.RawMessage
			for _, v := range r {
				b, _ := json.Marshal(v)
				rr = append(rr, b)
			}
			gotRaw = append(gotRaw, rr)
		}
		got, want := rowTexts(gotRaw), rowTexts(c.Expect.Rows)
		if !c.Expect.Ordered {
			sort.Strings(got)
			sort.Strings(want)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %s:\n got  %q\n want %q", c.Name, c.Query, got, want)
		}
	}
	if dump != "" {
		b, _ := json.MarshalIndent(dumped, "", " ")
		if err := os.WriteFile(dump, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
