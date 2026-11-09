package qv

import (
	"cmp"
	"encoding/binary"
	"math"
	"strings"

	"github.com/TacFlow/tacdb/internal/equal"
	"github.com/TacFlow/tacdb/value"
)

// family orders kinds for Compare (SPEC §5.3): Bool < number < String <
// Bytes < Vector < List < Map < entity < Path < Null.
func family(k Kind) int {
	switch k {
	case KindBool:
		return 0
	case KindInt, KindFloat:
		return 1
	case KindString:
		return 2
	case KindBytes:
		return 3
	case KindVector:
		return 4
	case KindList:
		return 5
	case KindMap:
		return 6
	case KindNode, KindEdge:
		return 7
	case KindPath:
		return 8
	}
	return 9 // Null
}

// Compare returns -1, 0 or +1 ordering a before, equal to or after b in the
// total order shared by ORDER BY, min, max and DISTINCT (SPEC §5.3):
// property values as value.Compare orders them, then entities (nodes
// before edges, by ID), then paths (by length, then by their IDs in
// order), then Null. Lists and Maps compare item by item with Compare.
// Compare == 0 does not imply Identical: Int 1 and Float 1.0 compare
// equal.
func Compare(a, b Value) int {
	fa, fb := family(a.kind), family(b.kind)
	if fa != fb {
		return cmp.Compare(fa, fb)
	}
	switch a.kind {
	case KindNull:
		return 0
	case KindList:
		x, y := a.ref.(*list).items, b.ref.(*list).items
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := Compare(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmp.Compare(len(x), len(y))
	case KindMap:
		x, y := a.ref.(*mapv).entries, b.ref.(*mapv).entries
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := strings.Compare(x[i].Key, y[i].Key); c != 0 {
				return c
			}
			if c := Compare(x[i].Value, y[i].Value); c != 0 {
				return c
			}
		}
		return cmp.Compare(len(x), len(y))
	case KindNode, KindEdge:
		if a.kind != b.kind {
			return cmp.Compare(a.kind, b.kind)
		}
		return cmp.Compare(a.id, b.id)
	case KindPath:
		p, q := a.ref.(*Path), b.ref.(*Path)
		if c := cmp.Compare(len(p.Edges), len(q.Edges)); c != 0 {
			return c
		}
		for i := range p.Edges {
			if c := cmp.Compare(p.Nodes[i], q.Nodes[i]); c != 0 {
				return c
			}
			if c := cmp.Compare(p.Edges[i], q.Edges[i]); c != 0 {
				return c
			}
		}
		return cmp.Compare(p.Nodes[len(p.Nodes)-1], q.Nodes[len(q.Nodes)-1])
	}
	return value.Compare(a.sc, b.sc)
}

// Identical reports whether a and b have the same kind and the same bits,
// recursively: unlike Compare, it tells Int 1 from Float 1.0 and -0 from
// +0.
func Identical(a, b Value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case KindList:
		x, y := a.ref.(*list).items, b.ref.(*list).items
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Identical(x[i], y[i]) {
				return false
			}
		}
		return true
	case KindMap:
		x, y := a.ref.(*mapv).entries, b.ref.(*mapv).entries
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !Identical(x[i].Value, y[i].Value) {
				return false
			}
		}
		return true
	case KindNode, KindEdge:
		return a.id == b.id
	case KindPath:
		p, q := a.ref.(*Path), b.ref.(*Path)
		return slicesEqual(p.Nodes, q.Nodes) && slicesEqual(p.Edges, q.Edges)
	}
	return value.Identical(a.sc, b.sc)
}

func slicesEqual(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Equals is the = of expressions (SPEC §15.2, §31.3): Null = Null is true
// and Null = x is false; numbers compare by exact value, and NaN equals
// nothing, itself included; Vectors compare component by component with
// IEEE equality; Lists and Maps compare item by item with Equals; an
// entity equals the same kind with the same ID, and never a property
// value.
func Equals(a, b Value) bool {
	if a.kind == KindNull || b.kind == KindNull {
		return a.kind == b.kind
	}
	if family(a.kind) != family(b.kind) {
		return false
	}
	switch a.kind {
	case KindInt, KindFloat:
		if IsNaN(a) || IsNaN(b) {
			return false
		}
		return value.Compare(a.sc, b.sc) == 0
	case KindVector:
		if a.sc.Len() != b.sc.Len() {
			return false
		}
		for i := 0; i < a.sc.Len(); i++ {
			if a.sc.VectorAt(i) != b.sc.VectorAt(i) {
				return false
			}
		}
		return true
	case KindList:
		x, y := a.ref.(*list).items, b.ref.(*list).items
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equals(x[i], y[i]) {
				return false
			}
		}
		return true
	case KindMap:
		x, y := a.ref.(*mapv).entries, b.ref.(*mapv).entries
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !Equals(x[i].Value, y[i].Value) {
				return false
			}
		}
		return true
	case KindNode, KindEdge, KindPath:
		return Identical(a, b)
	}
	return value.Compare(a.sc, b.sc) == 0 // Bool, String, Bytes
}

// IsNaN reports whether v is a Float NaN.
func IsNaN(v Value) bool {
	f, ok := v.AsFloat()
	return ok && math.IsNaN(f)
}

// Order compares a and b for < <= > >=: it returns ok false, and the
// operator gives Null, unless both are Bools, numbers, Strings or Bytes of
// the same family. NaN is ordered here; the operators make it false.
func Order(a, b Value) (c int, ok bool) {
	fa := family(a.kind)
	if fa != family(b.kind) || fa > 3 {
		return 0, false
	}
	return value.Compare(a.sc, b.sc), true
}

// Truthy is the truthiness of SPEC §15.2: Null, false, zero and empty
// values are false; NaN, entities and paths are true.
func Truthy(v Value) bool {
	switch v.kind {
	case KindNull:
		return false
	case KindBool:
		b, _ := v.sc.AsBool()
		return b
	case KindInt:
		i, _ := v.sc.AsInt()
		return i != 0
	case KindFloat:
		f, _ := v.sc.AsFloat()
		return f != 0 // NaN != 0
	case KindString, KindBytes, KindVector, KindList, KindMap:
		return v.Len() > 0
	}
	return true // Node, Edge, Path
}

// AppendKey appends the key of v in the codec of equality: values equal
// for grouping, DISTINCT and IN have equal keys (SPEC §31.3). It extends
// the key of property values with entities and paths.
func AppendKey(dst []byte, v Value) []byte {
	switch v.kind {
	case KindNull:
		return append(dst, equal.TagNull)
	case KindNode:
		return equal.AppendID(dst, equal.TagNode, v.id)
	case KindEdge:
		return equal.AppendID(dst, equal.TagEdge, v.id)
	case KindPath:
		p := v.ref.(*Path)
		dst = equal.AppendHeader(dst, equal.TagPath, len(p.Edges))
		for i, n := range p.Nodes {
			if i > 0 {
				dst = binary.BigEndian.AppendUint64(dst, p.Edges[i-1])
			}
			dst = binary.BigEndian.AppendUint64(dst, n)
		}
		return dst
	case KindList:
		items := v.ref.(*list).items
		dst = equal.AppendHeader(dst, equal.TagList, len(items))
		for _, item := range items {
			dst = AppendKey(dst, item)
		}
		return dst
	case KindMap:
		entries := v.ref.(*mapv).entries
		dst = equal.AppendHeader(dst, equal.TagMap, len(entries))
		for _, e := range entries {
			dst = AppendKey(equal.AppendMapKey(dst, e.Key), e.Value)
		}
		return dst
	}
	return equal.AppendKey(dst, v.sc)
}

// AppendIdentity appends bytes that tell apart the values Identical tells
// apart: the kind, then the bits. Aggregates use it to break ties between
// values that compare equal, so that the result does not depend on the
// order of the rows.
func AppendIdentity(dst []byte, v Value) []byte {
	dst = append(dst, byte(v.kind))
	switch v.kind {
	case KindBool, KindInt:
		i, _ := v.sc.AsInt()
		if v.kind == KindBool {
			if b, _ := v.sc.AsBool(); b {
				i = 1
			}
		}
		return binary.BigEndian.AppendUint64(dst, uint64(i))
	case KindFloat:
		f, _ := v.sc.AsFloat()
		return binary.BigEndian.AppendUint64(dst, math.Float64bits(f))
	case KindString, KindBytes:
		return v.sc.AppendBytes(binary.BigEndian.AppendUint32(dst, uint32(v.sc.Len())))
	case KindVector:
		dst = binary.BigEndian.AppendUint32(dst, uint32(v.sc.Len()))
		for i := 0; i < v.sc.Len(); i++ {
			dst = binary.BigEndian.AppendUint32(dst, math.Float32bits(v.sc.VectorAt(i)))
		}
		return dst
	case KindList:
		items := v.ref.(*list).items
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(items)))
		for _, item := range items {
			dst = AppendIdentity(dst, item)
		}
		return dst
	case KindMap:
		entries := v.ref.(*mapv).entries
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(entries)))
		for _, e := range entries {
			dst = AppendIdentity(equal.AppendMapKey(dst, e.Key), e.Value)
		}
		return dst
	case KindNode, KindEdge:
		return binary.BigEndian.AppendUint64(dst, v.id)
	case KindPath:
		return AppendKey(dst, v)
	}
	return dst
}
