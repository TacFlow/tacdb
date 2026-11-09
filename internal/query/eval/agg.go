package eval

import (
	"bytes"
	"math"
	"math/big"
	"math/bits"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/budget"
	"github.com/TacFlow/tacdb/internal/query/qv"
)

// Accumulator computes one aggregate over the rows of a group (SPEC
// §15.2). Results do not depend on the order of the rows, except for the
// order of collect, which is the order of its input.
type Accumulator interface {
	// Add takes the value of the argument on one row; for count(*) the
	// value is ignored and every row counts.
	Add(v qv.Value) error
	// Result returns the aggregate of the rows added so far.
	Result() (qv.Value, error)
	// Close returns the memory charged to the budget.
	Close()
}

// NewAccumulator returns an accumulator of the aggregate fn. distinct
// keeps one value of each class of the codec of equality; star is
// count(*) (count() is its synonym). b may be nil.
func NewAccumulator(fn *Func, distinct, star bool, b *budget.Budget) Accumulator {
	base := accBase{b: b}
	if distinct {
		base.seen = make(map[string]struct{})
	}
	switch fn.Name {
	case "count":
		return &countAcc{accBase: base, star: star}
	case "sum":
		return &sumAcc{accBase: base}
	case "avg":
		return &sumAcc{accBase: base, avg: true}
	case "min":
		return &extremeAcc{accBase: base, sign: -1}
	case "max":
		return &extremeAcc{accBase: base, sign: 1}
	case "collect":
		return &collectAcc{accBase: base}
	}
	panic("eval: " + fn.Name + " is not an aggregate")
}

// aggError marks an error of an aggregate as an execution error; the
// executor knows where the aggregate is written.
func aggError(e *errs.Error) error {
	e.Stage = errs.StageExecute
	return e
}

type accBase struct {
	b    *budget.Budget
	used int64
	seen map[string]struct{} // DISTINCT
	key  []byte
}

func (a *accBase) grow(n int64) error {
	if a.b == nil {
		return nil
	}
	if err := a.b.Grow(n); err != nil {
		return err
	}
	a.used += n
	return nil
}

func (a *accBase) release(n int64) {
	if a.b != nil {
		a.b.Shrink(n)
	}
	a.used -= n
}

func (a *accBase) Close() {
	if a.b != nil {
		a.b.Shrink(a.used)
	}
	a.used = 0
}

// first reports whether v is the first of its class of equality; it is
// always true without DISTINCT.
func (a *accBase) first(v qv.Value) (bool, error) {
	if a.seen == nil {
		return true, nil
	}
	a.key = qv.AppendKey(a.key[:0], v)
	if _, ok := a.seen[string(a.key)]; ok {
		return false, nil
	}
	if err := a.grow(int64(len(a.key)) + qv.ElementBytes); err != nil {
		return false, err
	}
	a.seen[string(a.key)] = struct{}{}
	return true, nil
}

// countAcc is count(*), which counts rows, and count(x), which counts
// values that are not Null.
type countAcc struct {
	accBase
	star bool
	n    int64
}

func (a *countAcc) Add(v qv.Value) error {
	if !a.star {
		if v.IsNull() {
			return nil
		}
		if ok, err := a.first(v); !ok || err != nil {
			return err
		}
	}
	a.n++
	return nil
}

func (a *countAcc) Result() (qv.Value, error) { return qv.Int(a.n), nil }

// sumAcc is sum and avg. The sum is exact, whatever the order of the rows
// (DET-01): Ints add up in 128 bits, finite Floats in an integer scaled by
// 2^1074, which holds any float64 exactly; the result is rounded once. sum
// of Ints alone is an Int and refuses overflow; any Float makes it a
// Float; no value gives Int 0. avg of no value is Null.
type sumAcc struct {
	accBase
	avg        bool
	hi         int64  // high word of the 128-bit sum of the Ints
	lo         uint64 // low word
	scaled     big.Int
	tmp        big.Int
	floats     bool
	nan        bool
	posInf     bool
	negInf     bool
	n          int64
	sizedFloat bool // the scaled sum is charged
}

func (a *sumAcc) Add(v qv.Value) error {
	if v.IsNull() {
		return nil
	}
	if v.Kind() != qv.KindInt && v.Kind() != qv.KindFloat {
		name := "sum"
		if a.avg {
			name = "avg"
		}
		return aggError(errs.Newf(errs.CodeTypeMismatch, "query", "%s does not accept %s", name, v.Kind()))
	}
	// A Float makes the result a Float even when DISTINCT drops it as
	// equal to an Int already seen, so the type does not depend on the
	// order of the rows.
	if v.Kind() == qv.KindFloat {
		a.floats = true
	}
	if ok, err := a.first(v); !ok || err != nil {
		return err
	}
	a.n++
	if i, ok := v.AsInt(); ok {
		var carry uint64
		a.lo, carry = bits.Add64(a.lo, uint64(i), 0)
		a.hi += int64(carry)
		if i < 0 {
			a.hi-- // sign extension of i to 128 bits
		}
		return nil
	}
	x, _ := v.AsFloat()
	switch {
	case math.IsNaN(x):
		a.nan = true
	case math.IsInf(x, 1):
		a.posInf = true
	case math.IsInf(x, -1):
		a.negInf = true
	case x != 0:
		if !a.sizedFloat {
			// The scaled sum takes at most 2098 + 64 bits.
			if err := a.grow(2 * 300); err != nil {
				return err
			}
			a.sizedFloat = true
		}
		frac, exp := math.Frexp(math.Abs(x))
		m := uint64(math.Ldexp(frac, 53)) // exact
		shift := exp - 53 + 1074
		if shift < 0 { // a subnormal: m has that many trailing zeros
			m >>= uint(-shift)
			shift = 0
		}
		a.tmp.SetUint64(m)
		a.tmp.Lsh(&a.tmp, uint(shift))
		if x < 0 {
			a.scaled.Sub(&a.scaled, &a.tmp)
		} else {
			a.scaled.Add(&a.scaled, &a.tmp)
		}
	}
	return nil
}

// total returns the exact sum of the finite values, scaled by 2^1074.
func (a *sumAcc) total() *big.Int {
	t := new(big.Int).SetInt64(a.hi)
	t.Lsh(t, 64)
	t.Add(t, new(big.Int).SetUint64(a.lo))
	t.Lsh(t, 1074)
	return t.Add(t, &a.scaled)
}

func (a *sumAcc) special() (float64, bool) {
	switch {
	case a.nan || a.posInf && a.negInf:
		return math.NaN(), true
	case a.posInf:
		return math.Inf(1), true
	case a.negInf:
		return math.Inf(-1), true
	}
	return 0, false
}

func (a *sumAcc) Result() (qv.Value, error) {
	if a.avg {
		if a.n == 0 {
			return qv.Value{}, nil
		}
		if x, ok := a.special(); ok {
			return qv.Float(x), nil
		}
		den := new(big.Int).Lsh(big.NewInt(a.n), 1074)
		x, _ := new(big.Rat).SetFrac(a.total(), den).Float64()
		return qv.Float(x), nil
	}
	if x, ok := a.special(); ok {
		return qv.Float(x), nil
	}
	if !a.floats {
		// The 128-bit sum fits an int64 when hi is the sign extension
		// of lo.
		if (a.hi == 0 && a.lo>>63 == 0) || (a.hi == -1 && a.lo>>63 == 1) {
			return qv.Int(int64(a.lo)), nil
		}
		return qv.Value{}, aggError(errs.New(errs.CodeInvalidArgument, "query", "integer overflow in sum"))
	}
	f := new(big.Float).SetInt(a.total()) // exact: the precision is the bit length
	x, _ := f.SetMantExp(f, -1074).Float64()
	return qv.Float(x), nil
}

// extremeAcc is min (sign -1) and max (sign 1) in the order of qv.Compare,
// Null ignored. Values that compare equal but are not identical (Int 1
// and Float 1.0, -0 and +0) are decided by their identity key, so the
// result does not depend on the order of the rows.
type extremeAcc struct {
	accBase
	sign int
	cur  qv.Value
	set  bool
}

func (a *extremeAcc) Add(v qv.Value) error {
	if v.IsNull() {
		return nil
	}
	if !a.set {
		a.cur, a.set = v, true
		return a.grow(v.Size())
	}
	c := qv.Compare(v, a.cur) * a.sign
	if c == 0 && !qv.Identical(v, a.cur) {
		c = -bytes.Compare(qv.AppendIdentity(nil, v), qv.AppendIdentity(nil, a.cur))
	}
	if c > 0 {
		if err := a.grow(v.Size()); err != nil {
			return err
		}
		a.release(a.cur.Size())
		a.cur = v
	}
	return nil
}

func (a *extremeAcc) Result() (qv.Value, error) { return a.cur, nil }

// collectAcc is collect: the values that are not Null, in input order.
type collectAcc struct {
	accBase
	items []qv.Value
	elems int64
	depth int
}

func (a *collectAcc) Add(v qv.Value) error {
	if v.IsNull() {
		return nil
	}
	if ok, err := a.first(v); !ok || err != nil {
		return err
	}
	elems, depth := a.elems+1+v.Elements(), max(a.depth, v.Depth())
	if a.b != nil {
		if err := a.b.CheckShape(elems, depth+1); err != nil {
			return err
		}
	}
	if err := a.grow(v.Size()); err != nil {
		return err
	}
	a.items = append(a.items, v)
	a.elems, a.depth = elems, depth
	return nil
}

func (a *collectAcc) Result() (qv.Value, error) {
	return qv.List(append([]qv.Value(nil), a.items...)), nil
}
