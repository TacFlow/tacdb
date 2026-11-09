package equal

import (
	"bytes"
	"math"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

// The equality of uniqueness and grouping (SPEC §12.2, §15.2).
func TestEqual(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		a, b value.Value
		eq   bool
	}{
		{value.Int(1), value.Float(1), true},
		{value.Float(0), value.Float(math.Copysign(0, -1)), true},
		{value.Int(0), value.Float(math.Copysign(0, -1)), true},
		{value.Int(1<<53 + 1), value.Float(1 << 53), false},
		{value.Int(math.MinInt64), value.Float(-(1 << 63)), true},
		{value.Int(math.MaxInt64), value.Float(1 << 63), false},
		{value.Float(0.5), value.Float(0.25), false},
		{value.Int(-1), value.Int(1), false},
		{value.Float(-2), value.Int(2), false},
		{value.Float(nan), value.Float(math.Float64frombits(0x7ff8000000000002)), true},
		{value.Float(math.Float64frombits(0xfff8000000000003)), value.Float(nan), true},
		{value.Float(nan), value.Float(math.Inf(1)), false},
		{value.Float(nan), value.Float(0), false},
		{value.Float(nan), value.Int(0), false},
		{value.Int(-3), value.Float(-3), true},
		{value.Int(-3), value.Int(3), false},
		{value.Int(-6), value.Float(-6), true},
		{value.List(value.Int(1)), value.List(value.Float(1)), true},
		{value.List(value.Float(nan)), value.List(value.Float(math.Float64frombits(0x7ff8000000000001))), true},
		{value.List(value.Int(1)), value.Vector([]float32{1}), false},
		{value.Vector([]float32{0.5}), value.Vector([]float32{0.5}), true},
		{value.Vector([]float32{0}), value.Vector([]float32{float32(math.Copysign(0, -1))}), true},
		{value.MapOf(map[string]value.Value{"a": value.Int(2)}), value.MapOf(map[string]value.Value{"a": value.Float(2)}), true},
		{value.MapOf(map[string]value.Value{"a": value.Int(2)}), value.MapOf(map[string]value.Value{"b": value.Int(2)}), false},
		{value.String("a"), value.Bytes([]byte("a")), false},
		{value.String("ab"), value.String("a"), false},
		{value.Null(), value.Null(), true},
		{value.Null(), value.Int(0), false},
		{value.Bool(false), value.Int(0), false},
		{value.List(), value.MapOf(nil), false},
		{value.List(value.List(), value.Int(1)), value.List(value.List(value.Int(1))), false},
		{value.Float(math.Inf(1)), value.Float(math.Inf(-1)), false},
	} {
		if got := Equal(c.a, c.b); got != c.eq {
			t.Fatalf("Equal(%v, %v) = %v", c.a, c.b, got)
		}
	}
}

// Entity and path keys never meet a property value.
func TestEntityKeys(t *testing.T) {
	node := AppendID(nil, TagNode, 1)
	edge := AppendID(nil, TagEdge, 1)
	if bytes.Equal(node, edge) || bytes.Equal(node, AppendKey(nil, value.Int(1))) {
		t.Fatal("an entity key meets another kind")
	}
	if !bytes.Equal(AppendID(nil, TagNode, 1), node) {
		t.Fatal("entity keys are not canonical")
	}
}
