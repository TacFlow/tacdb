// Package qv is the runtime value of the query executor: a property value,
// a reference to a node or an edge, a path, or a List or Map of runtime
// values, which may hold references at any depth. It owns the shared
// comparator, the equality of expressions and the key of the codec of
// equality used by grouping, DISTINCT and IN (SPEC §5.3, §15.2, §31.3).
package qv

import (
	"sort"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Kind is the type of a runtime value. The kinds of property values keep
// the numbers of value.Kind.
type Kind uint8

// Kinds.
const (
	KindNull        = Kind(value.KindNull)
	KindBool        = Kind(value.KindBool)
	KindInt         = Kind(value.KindInt)
	KindFloat       = Kind(value.KindFloat)
	KindString      = Kind(value.KindString)
	KindBytes       = Kind(value.KindBytes)
	KindVector      = Kind(value.KindVector)
	KindList        = Kind(value.KindList)
	KindMap         = Kind(value.KindMap)
	KindNode   Kind = 9
	KindEdge   Kind = 10
	KindPath   Kind = 11
)

var kindNames = [...]string{"Null", "Bool", "Int", "Float", "String", "Bytes", "Vector", "List", "Map", "Node", "Edge", "Path"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// ElementBytes is what one value costs in a budget, besides the bytes of
// its strings and vectors: at least the 64 bytes per element of SPEC §5.3,
// and at least what a Value, or an Entry of a Map, takes in memory, so
// that the budget does not count less than the heap holds.
const ElementBytes = 96

// Value is an immutable runtime value. The zero Value is Null.
type Value struct {
	_    [0]func() // makes == a compile error
	kind Kind
	sc   value.Value // Bool, Int, Float, String, Bytes, Vector
	id   uint64      // Node, Edge
	ref  any         // *list (List), *mapv (Map) or *Path
}

// Entry is a key/value pair of a Map.
type Entry struct {
	Key   string
	Value Value
}

// Path is an alternating sequence of nodes and edges: Nodes has one more
// element than Edges.
type Path struct {
	Nodes []uint64
	Edges []uint64
}

// shape caches what the budget needs to know of a List or a Map without
// walking it again.
type shape struct {
	elems int64 // items and entries summed over all levels, as value.Validate counts
	depth int   // a scalar has depth 1
	bytes int64 // what Size returns
}

type list struct {
	items []Value
	shape
}

type mapv struct {
	entries []Entry
	shape
}

// Null returns the Null value.
func Null() Value { return Value{} }

// Bool returns a Bool value.
func Bool(b bool) Value { return Value{kind: KindBool, sc: value.Bool(b)} }

// Int returns an Int value.
func Int(i int64) Value { return Value{kind: KindInt, sc: value.Int(i)} }

// Float returns a Float value.
func Float(f float64) Value { return Value{kind: KindFloat, sc: value.Float(f)} }

// String returns a String value.
func String(s string) Value { return Value{kind: KindString, sc: value.String(s)} }

// Node returns a reference to a node.
func Node(id uint64) Value { return Value{kind: KindNode, id: id} }

// Edge returns a reference to an edge.
func Edge(id uint64) Value { return Value{kind: KindEdge, id: id} }

// PathOf returns a path; it takes ownership of p.
func PathOf(p *Path) Value { return Value{kind: KindPath, ref: p} }

// FromValue returns the runtime form of a property value.
func FromValue(v value.Value) Value {
	switch v.Kind() {
	case value.KindNull:
		return Value{}
	case value.KindList:
		items := make([]Value, v.Len())
		for i := range items {
			items[i] = FromValue(v.Index(i))
		}
		return List(items)
	case value.KindMap:
		entries := make([]Entry, v.Len())
		for i := range entries {
			k, item := v.EntryAt(i)
			entries[i] = Entry{Key: k, Value: FromValue(item)}
		}
		return Value{kind: KindMap, ref: newMap(entries)} // already sorted and unique
	}
	return Value{kind: Kind(v.Kind()), sc: v}
}

// ShapeOf returns what Elements, Depth and Size of FromValue(v) return,
// without building it, so that a budget is charged before the copy.
func ShapeOf(v value.Value) (elems int64, depth int, bytes int64) {
	switch v.Kind() {
	case value.KindString, value.KindBytes:
		return 0, 1, ElementBytes + int64(v.Len())
	case value.KindVector:
		return 0, 1, ElementBytes + 4*int64(v.Len())
	case value.KindList, value.KindMap:
		elems, depth, bytes = int64(v.Len()), 1, ElementBytes
		for i := 0; i < v.Len(); i++ {
			item := value.Value{}
			if v.Kind() == value.KindList {
				item = v.Index(i)
			} else {
				var k string
				k, item = v.EntryAt(i)
				bytes += int64(len(k))
			}
			e, d, b := ShapeOf(item)
			elems, depth, bytes = elems+e, max(depth, d+1), bytes+b
		}
		return elems, depth, bytes
	}
	return 0, 1, ElementBytes
}

// List returns a List; it takes ownership of items.
func List(items []Value) Value {
	l := &list{items: items}
	l.elems, l.depth, l.bytes = int64(len(items)), 1, ElementBytes
	for i := range items {
		l.add(&items[i])
	}
	return Value{kind: KindList, ref: l}
}

func (s *shape) add(v *Value) {
	s.elems += v.Elements()
	s.depth = max(s.depth, v.Depth()+1)
	s.bytes += v.Size()
}

// MapOf returns a Map with the entries sorted by key; it takes ownership
// of entries. A duplicate key is an INVALID_ARGUMENT error.
func MapOf(entries []Entry) (Value, error) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	for i := 1; i < len(entries); i++ {
		if entries[i].Key == entries[i-1].Key {
			return Value{}, errs.New(errs.CodeInvalidArgument, "query", "duplicate key in a map")
		}
	}
	return Value{kind: KindMap, ref: newMap(entries)}, nil
}

func newMap(entries []Entry) *mapv {
	m := &mapv{entries: entries}
	m.elems, m.depth, m.bytes = int64(len(entries)), 1, ElementBytes
	for i := range entries {
		m.add(&entries[i].Value)
		m.bytes += int64(len(entries[i].Key))
	}
	return m
}

// Kind returns the type of v.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether v is Null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// Scalar returns the property value of a Null, Bool, Int, Float, String,
// Bytes or Vector.
func (v Value) Scalar() value.Value { return v.sc }

// AsInt returns the integer and true if v is an Int.
func (v Value) AsInt() (int64, bool) {
	i, _ := v.sc.AsInt()
	return i, v.kind == KindInt
}

// AsFloat returns the float and true if v is a Float.
func (v Value) AsFloat() (float64, bool) {
	f, _ := v.sc.AsFloat()
	return f, v.kind == KindFloat
}

// AsString returns the string and true if v is a String.
func (v Value) AsString() (string, bool) {
	s, _ := v.sc.AsString()
	return s, v.kind == KindString
}

// ID returns the ID of a node or an edge.
func (v Value) ID() uint64 { return v.id }

// Path returns the path of a Path value; it must not be modified.
func (v Value) Path() *Path {
	p, _ := v.ref.(*Path)
	return p
}

// Len returns the number of items of a List or entries of a Map, and the
// Len of the property value otherwise.
func (v Value) Len() int {
	switch v.kind {
	case KindList:
		return len(v.ref.(*list).items)
	case KindMap:
		return len(v.ref.(*mapv).entries)
	}
	return v.sc.Len()
}

// Index returns item i of a List.
func (v Value) Index(i int) Value { return v.ref.(*list).items[i] }

// Items returns the items of a List; they must not be modified.
func (v Value) Items() []Value { return v.ref.(*list).items }

// EntryAt returns entry i of a Map, in key order.
func (v Value) EntryAt(i int) (string, Value) {
	e := v.ref.(*mapv).entries[i]
	return e.Key, e.Value
}

// Lookup returns the value of key in a Map.
func (v Value) Lookup(key string) (Value, bool) {
	if v.kind != KindMap {
		return Value{}, false
	}
	entries := v.ref.(*mapv).entries
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Key >= key })
	if i < len(entries) && entries[i].Key == key {
		return entries[i].Value, true
	}
	return Value{}, false
}

// Elements returns the List items and Map entries of v, summed over all
// levels, as value.Validate counts them.
func (v Value) Elements() int64 {
	switch v.kind {
	case KindList:
		return v.ref.(*list).elems
	case KindMap:
		return v.ref.(*mapv).elems
	}
	return 0
}

// Depth returns the nesting depth of v; a scalar has depth 1.
func (v Value) Depth() int {
	switch v.kind {
	case KindList:
		return v.ref.(*list).depth
	case KindMap:
		return v.ref.(*mapv).depth
	}
	return 1
}

// Size returns what v costs in a budget: ElementBytes per value plus the
// bytes of strings, keys, vectors and paths.
func (v Value) Size() int64 {
	switch v.kind {
	case KindString, KindBytes:
		return ElementBytes + int64(v.sc.Len())
	case KindVector:
		return ElementBytes + 4*int64(v.sc.Len())
	case KindList:
		return v.ref.(*list).bytes
	case KindMap:
		return v.ref.(*mapv).bytes
	case KindPath:
		p := v.ref.(*Path)
		return ElementBytes + 8*int64(len(p.Nodes)+len(p.Edges))
	}
	return ElementBytes
}

// HasRefs reports whether v holds a node, an edge or a path at any depth.
func (v Value) HasRefs() bool {
	switch v.kind {
	case KindNode, KindEdge, KindPath:
		return true
	case KindList:
		for _, item := range v.ref.(*list).items {
			if item.HasRefs() {
				return true
			}
		}
	case KindMap:
		for _, e := range v.ref.(*mapv).entries {
			if e.Value.HasRefs() {
				return true
			}
		}
	}
	return false
}

// ToValue converts v to a property value. A reference at any depth is a
// TYPE_MISMATCH error: entities are not property values (SPEC §31.1).
func ToValue(v Value) (value.Value, error) {
	switch v.kind {
	case KindNode, KindEdge, KindPath:
		return value.Value{}, errs.Newf(errs.CodeTypeMismatch, "query", "a %s is not a property value", v.kind)
	case KindList:
		items := v.ref.(*list).items
		return value.MakeList(len(items), func(i int) (value.Value, error) { return ToValue(items[i]) })
	case KindMap:
		entries := v.ref.(*mapv).entries
		return value.MakeMap(len(entries), func(i int) (value.Entry, error) {
			item, err := ToValue(entries[i].Value)
			return value.Entry{Key: entries[i].Key, Value: item}, err
		})
	}
	return v.sc, nil
}

// String returns a debugging representation of v. It may contain user
// data, so it must not be used in logs or error messages.
func (v Value) String() string {
	var b strings.Builder
	v.format(&b)
	return b.String()
}

func (v Value) format(b *strings.Builder) {
	switch v.kind {
	case KindNode:
		b.WriteString("node(" + strconv.FormatUint(v.id, 10) + ")")
	case KindEdge:
		b.WriteString("edge(" + strconv.FormatUint(v.id, 10) + ")")
	case KindPath:
		p := v.ref.(*Path)
		b.WriteString("path(")
		for i, n := range p.Nodes {
			if i > 0 {
				b.WriteString(" -" + strconv.FormatUint(p.Edges[i-1], 10) + "- ")
			}
			b.WriteString(strconv.FormatUint(n, 10))
		}
		b.WriteByte(')')
	case KindList:
		b.WriteByte('[')
		for i, item := range v.ref.(*list).items {
			if i > 0 {
				b.WriteString(", ")
			}
			item.format(b)
		}
		b.WriteByte(']')
	case KindMap:
		b.WriteByte('{')
		for i, e := range v.ref.(*mapv).entries {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Quote(e.Key) + ": ")
			e.Value.format(b)
		}
		b.WriteByte('}')
	default:
		b.WriteString(v.sc.String())
	}
}
