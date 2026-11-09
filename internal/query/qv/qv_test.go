package qv

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
	"unsafe"

	"github.com/TacFlow/tacdb/internal/equal"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func lst(items ...Value) Value { return List(items) }

func mapOf(t *testing.T, kv ...any) Value {
	t.Helper()
	var entries []Entry
	for i := 0; i < len(kv); i += 2 {
		entries = append(entries, Entry{Key: kv[i].(string), Value: kv[i+1].(Value)})
	}
	m, err := MapOf(entries)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func path(nodes []uint64, edges []uint64) Value { return PathOf(&Path{Nodes: nodes, Edges: edges}) }

var nan = math.NaN()

// The order of the families, entities and paths (SPEC §5.3).
func TestCompareOrder(t *testing.T) {
	ordered := []Value{
		Bool(false), Bool(true),
		Float(math.Inf(-1)), Int(math.MinInt64), Int(-1), Float(-0.5), Int(0), Float(0.5), Int(1 << 53), Int(1<<53 + 1), Float(math.Inf(1)), Float(nan),
		String(""), String("a"), String("b"),
		FromValue(value.Bytes([]byte{0})),
		FromValue(value.Vector([]float32{1})),
		lst(), lst(Int(1)), lst(Int(1), Null()), lst(Int(2)), lst(Node(1)),
		mapOf(t), mapOf(t, "a", Int(1)), mapOf(t, "b", Int(0)),
		Node(1), Node(2), Edge(1), Edge(2),
		path([]uint64{1}, nil), path([]uint64{1, 3}, []uint64{4}), path([]uint64{1, 2}, []uint64{5}), path([]uint64{2, 1}, []uint64{1}),
		Null(),
	}
	for i := range ordered {
		for j := range ordered {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := Compare(ordered[i], ordered[j]); got != want {
				t.Fatalf("Compare(%v, %v) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	for _, c := range [][2]Value{
		{Int(1), Float(1)}, {Float(0), Float(math.Copysign(0, -1))}, {Float(nan), Float(math.Float64frombits(0x7ff8000000000002))},
		{lst(Int(1)), lst(Float(1))}, {mapOf(t, "a", Int(2)), mapOf(t, "a", Float(2))},
	} {
		if Compare(c[0], c[1]) != 0 || Identical(c[0], c[1]) {
			t.Fatalf("%v and %v should tie without being identical", c[0], c[1])
		}
	}
}

// The = of expressions (SPEC §15.2, §31.3).
func TestEquals(t *testing.T) {
	vec := func(xs ...float32) Value { return FromValue(value.Vector(xs)) }
	for _, c := range []struct {
		a, b Value
		eq   bool
	}{
		{Null(), Null(), true},
		{Null(), Int(0), false},
		{Int(0), Null(), false},
		{Int(1), Float(1), true},
		{Int(1<<53 + 1), Float(1 << 53), false},
		{Float(nan), Float(nan), false},
		{Float(0), Float(math.Copysign(0, -1)), true},
		{String("a"), String("a"), true},
		{String("a"), FromValue(value.Bytes([]byte("a"))), false},
		{Bool(true), Int(1), false},
		{lst(Int(1)), lst(Float(1)), true},
		{lst(Null()), lst(Null()), true},
		{lst(Float(nan)), lst(Float(nan)), false},
		{lst(Int(1)), lst(Int(1), Int(2)), false},
		{mapOf(t, "a", Int(1)), mapOf(t, "a", Float(1)), true},
		{mapOf(t, "a", Int(1)), mapOf(t, "b", Int(1)), false},
		{vec(0), vec(float32(math.Copysign(0, -1))), true},
		{vec(float32(nan)), vec(float32(nan)), false},
		{vec(1), vec(1, 2), false},
		{Node(1), Node(1), true},
		{Node(1), Edge(1), false},
		{Node(1), Int(1), false},
		{path([]uint64{1, 2}, []uint64{3}), path([]uint64{1, 2}, []uint64{3}), true},
		{path([]uint64{1, 2}, []uint64{3}), path([]uint64{1, 2}, []uint64{4}), false},
	} {
		if got := Equals(c.a, c.b); got != c.eq {
			t.Fatalf("Equals(%v, %v) = %v", c.a, c.b, got)
		}
		if got := Equals(c.b, c.a); got != c.eq {
			t.Fatalf("Equals(%v, %v) = %v", c.b, c.a, got)
		}
	}
}

// Truthiness (SPEC §15.2).
func TestTruthy(t *testing.T) {
	for _, c := range []struct {
		v    Value
		want bool
	}{
		{Null(), false}, {Bool(false), false}, {Bool(true), true},
		{Int(0), false}, {Int(-1), true}, {Float(0), false}, {Float(math.Copysign(0, -1)), false}, {Float(nan), true},
		{String(""), false}, {String("x"), true}, {FromValue(value.Bytes(nil)), false},
		{lst(), false}, {lst(Null()), true}, {mapOf(t), false}, {mapOf(t, "a", Null()), true},
		{FromValue(value.Vector([]float32{0})), true},
		{Node(0), true}, {Edge(0), true}, {path([]uint64{1}, nil), true},
	} {
		if got := Truthy(c.v); got != c.want {
			t.Fatalf("Truthy(%v) = %v", c.v, got)
		}
	}
}

// Order is defined only within Bool, number, String and Bytes.
func TestOrder(t *testing.T) {
	for _, c := range []struct {
		a, b Value
		ok   bool
	}{
		{Int(1), Float(2), true}, {String("a"), String("b"), true}, {Bool(false), Bool(true), true},
		{Int(1), String("a"), false}, {lst(), lst(), false}, {Node(1), Node(2), false}, {Null(), Null(), false},
		{FromValue(value.Vector([]float32{1})), FromValue(value.Vector([]float32{1})), false},
	} {
		if _, ok := Order(c.a, c.b); ok != c.ok {
			t.Fatalf("Order(%v, %v) ok = %v", c.a, c.b, ok)
		}
	}
}

func randomValue(r *rand.Rand, depth int) value.Value {
	switch k := r.Intn(9); {
	case k == 0:
		return value.Null()
	case k == 1:
		return value.Bool(r.Intn(2) == 0)
	case k == 2:
		return value.Int(int64(r.Intn(5) - 2))
	case k == 3:
		return value.Float([]float64{0, math.Copysign(0, -1), 1, -2, 0.5, nan, math.Inf(1)}[r.Intn(7)])
	case k == 4:
		return value.String([]string{"", "a", "b"}[r.Intn(3)])
	case k == 5:
		return value.Bytes([]byte("ab")[:r.Intn(3)])
	case k == 6:
		return value.Vector([]float32{float32(r.Intn(3))})
	case k == 7 && depth > 0:
		items := make([]value.Value, r.Intn(3))
		for i := range items {
			items[i] = randomValue(r, depth-1)
		}
		return value.List(items...)
	case depth > 0:
		m := map[string]value.Value{}
		for i := r.Intn(3); i > 0; i-- {
			m[[]string{"a", "b"}[r.Intn(2)]] = randomValue(r, depth-1)
		}
		return value.MapOf(m)
	}
	return value.Int(7)
}

// On property values the key and the order agree with the shared codec
// of equality and value.Compare; equal keys are a class of Compare, and
// Equals implies equal keys.
func TestKeyAgreesWithValues(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		a, b := randomValue(r, 2), randomValue(r, 2)
		x, y := FromValue(a), FromValue(b)
		if !bytes.Equal(AppendKey(nil, x), equal.AppendKey(nil, a)) {
			t.Fatalf("key of %v differs from the codec of equality", a)
		}
		if got, want := Compare(x, y), value.Compare(a, b); got != want {
			t.Fatalf("Compare(%v, %v) = %d, value.Compare = %d", a, b, got, want)
		}
		sameKey := bytes.Equal(AppendKey(nil, x), AppendKey(nil, y))
		if sameKey != equal.Equal(a, b) {
			t.Fatalf("keys of %v and %v disagree with equal.Equal", a, b)
		}
		if Equals(x, y) && !sameKey {
			t.Fatalf("%v = %v but their keys differ", a, b)
		}
		if sameKey != (Compare(x, y) == 0) {
			t.Fatalf("keys of %v and %v disagree with Compare", a, b)
		}
		back, err := ToValue(x)
		if err != nil || !value.Identical(back, a) {
			t.Fatalf("ToValue(FromValue(%v)) = %v, %v", a, back, err)
		}
	}
}

// Entities and paths have keys apart from every property value.
func TestEntityKeys(t *testing.T) {
	keys := map[string]Value{}
	for _, v := range []Value{Node(1), Edge(1), Int(1), path([]uint64{1}, nil), path([]uint64{1, 2}, []uint64{3}), path([]uint64{1, 3}, []uint64{2}), lst(Node(1)), lst(Edge(1))} {
		k := string(AppendKey(nil, v))
		if w, ok := keys[k]; ok {
			t.Fatalf("%v and %v share a key", v, w)
		}
		keys[k] = v
	}
}

// The identity key tells apart what Identical tells apart.
func TestIdentity(t *testing.T) {
	vs := []Value{Int(1), Float(1), Float(0), Float(math.Copysign(0, -1)), Float(nan), Float(math.Float64frombits(0x7ff8000000000002)),
		Bool(false), Int(0), String("a"), FromValue(value.Bytes([]byte("a"))), lst(Int(1)), lst(Float(1)), Node(1), Edge(1),
		mapOf(t, "a", Int(1)), mapOf(t, "a", Float(1)), path([]uint64{1}, nil)}
	for _, a := range vs {
		for _, b := range vs {
			if Identical(a, b) != bytes.Equal(AppendIdentity(nil, a), AppendIdentity(nil, b)) {
				t.Fatalf("identity keys of %v and %v disagree with Identical", a, b)
			}
		}
	}
}

// Shapes count as value.Validate counts, and sizes include strings.
func TestShape(t *testing.T) {
	v := value.List(value.Int(1), value.List(value.String("abc"), value.MapOf(map[string]value.Value{"k": value.Null()})))
	x := FromValue(v)
	if x.Elements() != 2+2+1 || x.Depth() != 4 {
		t.Fatalf("elements %d depth %d", x.Elements(), x.Depth())
	}
	if err := value.Validate(v, value.Limits{MaxDepth: 4, MaxValueBytes: 1 << 20, MaxValueElements: 5, MaxMapKeyBytes: 16, MaxVectorDimensions: 4}); err != nil {
		t.Fatal(err)
	}
	if err := value.Validate(v, value.Limits{MaxDepth: 3, MaxValueBytes: 1 << 20, MaxValueElements: 5, MaxMapKeyBytes: 16, MaxVectorDimensions: 4}); err == nil {
		t.Fatal("depth 4 passes a limit of 3")
	}
	if want := int64(ElementBytes*6 + 3 + 1); x.Size() != want {
		t.Fatalf("size %d, want %d", x.Size(), want)
	}
	if Int(1).Depth() != 1 || lst().Depth() != 1 || lst().Elements() != 0 {
		t.Fatal("scalar or empty list shape")
	}
}

// A reference is not a property value, at any depth (SPEC §31.1).
func TestToValueRefuses(t *testing.T) {
	for _, v := range []Value{Node(1), Edge(1), path([]uint64{1}, nil), lst(Int(1), lst(Node(1))), mapOf(t, "a", Edge(2))} {
		if _, err := ToValue(v); errs.CodeOf(err) != errs.CodeTypeMismatch {
			t.Fatalf("ToValue(%v) = %v", v, err)
		}
		if !v.HasRefs() {
			t.Fatalf("%v has no refs", v)
		}
	}
	if lst(Int(1)).HasRefs() {
		t.Fatal("a list of Ints has refs")
	}
	if _, err := MapOf([]Entry{{Key: "a"}, {Key: "a"}}); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("duplicate key: %v", err)
	}
}

func TestTypes(t *testing.T) {
	if k, ok := (TInt | TNull).Known(); !ok || k != KindInt {
		t.Fatal("Int|Null is a known Int")
	}
	for _, ty := range []Type{TNull, TNumber, TAny, 0} {
		if _, ok := ty.Known(); ok {
			t.Fatalf("%v is known", ty)
		}
	}
	if TAny.String() != "Any" || (TInt|TNull).String() != "Null|Int" {
		t.Fatalf("%s %s", TAny, TInt|TNull)
	}
}

// The budget charges at least what a value takes in memory.
func TestElementBytes(t *testing.T) {
	if unsafe.Sizeof(Value{}) > ElementBytes || unsafe.Sizeof(Entry{}) > ElementBytes || ElementBytes < 64 {
		t.Fatalf("a Value takes %d bytes and an Entry %d, above ElementBytes %d", unsafe.Sizeof(Value{}), unsafe.Sizeof(Entry{}), ElementBytes)
	}
}
