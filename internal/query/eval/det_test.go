package eval

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/rand"
	"testing"

	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/qv"
)

// The goldens below were computed on arm64 and checked on amd64; the CI
// runs them on both (DET-01). Inputs are built from integers with exact
// operations only, so the compiler cannot fuse them.
const (
	powGolden = "a7dc372166fe1c598f64285ca21025b5adbf240ca3ef16f9926ee21815ae288b"
	sumGolden = "f1379067ae7234ce8ce6be0a350f8fc7c907a7c568ae2a2d6f7f46950d69f7ea"
)

func powInputs(yield func(x, y float64)) {
	special := []float64{0, math.Copysign(0, -1), 1, -1, 0.5, -0.5, 2, -2, 3, math.Inf(1), math.Inf(-1), math.NaN(), 1e-300, 1e300, 5e-324}
	for _, x := range special {
		for _, y := range special {
			yield(x, y)
		}
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		switch i % 4 {
		case 0:
			yield(math.Ldexp(float64(r.Int63n(1<<40)), -33), math.Ldexp(float64(r.Int63n(1<<40)), -36)-8)
		case 1:
			yield(math.Float64frombits(r.Uint64()), math.Float64frombits(r.Uint64()))
		case 2:
			yield(float64(r.Int63n(1<<50)-(1<<49))/1e9, float64(r.Intn(60)-30)+math.Ldexp(float64(r.Int63n(1<<20)), -20))
		case 3:
			yield(1+math.Ldexp(float64(r.Int63n(1<<30)), -50), math.Ldexp(float64(r.Int63n(1<<40)), -13))
		}
	}
}

// ^ gives the same bits on every architecture.
func TestPowGolden(t *testing.T) {
	h := sha256.New()
	var b [8]byte
	powInputs(func(x, y float64) {
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(pow(x, y)))
		h.Write(b[:])
	})
	if got := hex.EncodeToString(h.Sum(nil)); got != powGolden {
		t.Fatalf("pow golden %s, want %s", got, powGolden)
	}
	for _, c := range [][3]float64{{2, 10, 1024}, {2, -1, 0.5}, {9, 0.5, 3}, {10, 2, 100}, {-2, 3, -8}, {0, -1, math.Inf(1)}} {
		if got := pow(c[0], c[1]); got != c[2] {
			t.Fatalf("pow(%v, %v) = %v", c[0], c[1], got)
		}
	}
}

// sum and avg give the same bits in any order and on every architecture.
func TestSumGolden(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	h := sha256.New()
	var b [8]byte
	for set := 0; set < 2000; set++ {
		vals := make([]qv.Value, 1+r.Intn(40))
		for i := range vals {
			switch r.Intn(4) {
			case 0:
				vals[i] = qv.Int(r.Int63() - r.Int63())
			case 1:
				vals[i] = qv.Float(math.Ldexp(float64(r.Int63n(1<<53)-(1<<52)), r.Intn(200)-100))
			case 2:
				vals[i] = qv.Float(math.Ldexp(float64(r.Int63n(1<<53)), r.Intn(2000)-1074))
			default:
				vals[i] = qv.Float(float64(r.Intn(1000)) / 10)
			}
		}
		for _, name := range []string{"sum", "avg"} {
			fn, _ := Lookup(name)
			var first qv.Value
			for order := 0; order < 3; order++ {
				acc := NewAccumulator(fn, false, false, nil)
				for _, v := range vals {
					if err := acc.Add(v); err != nil {
						t.Fatal(err)
					}
				}
				got, err := acc.Result()
				if err != nil {
					t.Fatal(err)
				}
				if order == 0 {
					first = got
					x, _ := got.AsFloat()
					if i, ok := got.AsInt(); ok {
						x = math.Float64frombits(uint64(i))
					}
					binary.LittleEndian.PutUint64(b[:], math.Float64bits(x))
					h.Write(b[:])
				} else if !qv.Identical(got, first) {
					t.Fatalf("%s of %v: %v then %v", name, vals, first, got)
				}
				r.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
			}
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sumGolden {
		t.Fatalf("sum golden %s, want %s", got, sumGolden)
	}
}

type emptyScope struct{}

func (emptyScope) Variable(string) (int, bool)         { return 0, false }
func (emptyScope) Aggregate(*ast.FuncCall) (int, bool) { return 0, false }

// A NaN made by an operation has the bits of math.NaN on every processor,
// whatever sign the hardware gives it.
func TestComputedNaNIsCanonical(t *testing.T) {
	for _, expr := range []string{"toFloat('inf') - toFloat('inf')", "0.0 * toFloat('inf')", "(-1) ^ 0.5",
		"toFloat('inf') % 2", "toFloat('-inf') + toFloat('inf')", "toFloat('inf') / toFloat('-inf')", "(-8) ^ (1.0 / 3)",
		"-toFloat('nan')", "-(toFloat('inf') - toFloat('inf'))"} {
		src := "RETURN " + expr
		q, err := parser.Parse(src, parser.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		e, err := Compile(q.Clauses[0].(*ast.Return).Projection.Items[0].X, emptyScope{}, src)
		if err != nil {
			t.Fatal(err)
		}
		v, err := e.Eval(&Env{}, nil)
		x, ok := v.AsFloat()
		if err != nil || !ok || math.Float64bits(x) != math.Float64bits(math.NaN()) {
			t.Fatalf("%s = %v (%x), %v", expr, v, math.Float64bits(x), err)
		}
	}
}
