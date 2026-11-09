package eval

import (
	"math"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/qv"
)

func infix(e *ast.Binary, l, r node) node {
	switch e.Op {
	case ast.OpAnd, ast.OpOr:
		// Short-circuit on truthiness; the result is always a Bool
		// (SPEC §15.2).
		stop := e.Op == ast.OpOr
		return func(f *frame) (qv.Value, error) {
			a, err := l(f)
			if err != nil {
				return qv.Value{}, err
			}
			if qv.Truthy(a) == stop {
				return qv.Bool(stop), nil
			}
			b, err := r(f)
			if err != nil {
				return qv.Value{}, err
			}
			return qv.Bool(qv.Truthy(b)), nil
		}
	}
	return func(f *frame) (qv.Value, error) {
		a, err := l(f)
		if err != nil {
			return qv.Value{}, err
		}
		b, err := r(f)
		if err != nil {
			return qv.Value{}, err
		}
		v, err := apply(f, e, a, b)
		if err != nil {
			return qv.Value{}, f.at(e.Span, err)
		}
		return v, nil
	}
}

// apply computes a binary operator other than AND and OR on evaluated
// operands.
func apply(f *frame, e *ast.Binary, a, b qv.Value) (qv.Value, error) {
	switch e.Op {
	case ast.OpEq:
		return qv.Bool(qv.Equals(a, b)), nil
	case ast.OpNeq:
		return qv.Bool(!qv.Equals(a, b)), nil
	}
	// Every other operator gives Null when an operand is Null (SPEC §15.2).
	if a.IsNull() || b.IsNull() {
		return qv.Value{}, nil
	}
	switch e.Op {
	case ast.OpXor:
		return qv.Bool(qv.Truthy(a) != qv.Truthy(b)), nil
	case ast.OpIn:
		if b.Kind() != qv.KindList {
			return qv.Value{}, nil
		}
		for _, item := range b.Items() {
			if !item.IsNull() && qv.Equals(a, item) {
				return qv.Bool(true), nil
			}
		}
		return qv.Bool(false), nil
	case ast.OpContains, ast.OpStartsWith, ast.OpEndsWith:
		x, ok1 := a.AsString()
		y, ok2 := b.AsString()
		if !ok1 || !ok2 {
			return qv.Value{}, nil
		}
		switch e.Op {
		case ast.OpContains:
			return qv.Bool(strings.Contains(x, y)), nil
		case ast.OpStartsWith:
			return qv.Bool(strings.HasPrefix(x, y)), nil
		}
		return qv.Bool(strings.HasSuffix(x, y)), nil
	case ast.OpLt, ast.OpLe, ast.OpGt, ast.OpGe:
		c, ok := qv.Order(a, b)
		if !ok {
			return qv.Value{}, nil
		}
		if qv.IsNaN(a) || qv.IsNaN(b) {
			return qv.Bool(false), nil
		}
		switch e.Op {
		case ast.OpLt:
			return qv.Bool(c < 0), nil
		case ast.OpLe:
			return qv.Bool(c <= 0), nil
		case ast.OpGt:
			return qv.Bool(c > 0), nil
		}
		return qv.Bool(c >= 0), nil
	case ast.OpVecDist, ast.OpFTSMatch:
		return qv.Value{}, f.fail(e.Span, errs.CodeQueryUnsupported, "%s is not available in this version", opName(e.Op))
	case ast.OpAdd:
		return add(f, e, a, b)
	}
	return arith(f, e, a, b)
}

func opName(op ast.BinaryOp) string {
	if op == ast.OpVecDist {
		return "vector distance (<=>)"
	}
	return "full-text match (@@)"
}

// add is +: numbers, String + String, and Lists with Lists or elements;
// any other mix is Null.
func add(f *frame, e *ast.Binary, a, b qv.Value) (qv.Value, error) {
	al, bl := a.Kind() == qv.KindList, b.Kind() == qv.KindList
	if al || bl {
		// Charge the result before it is allocated: a list operand gives
		// its items, any other operand is one item.
		elems, depth, bytes := int64(0), 1, int64(qv.ElementBytes)
		for _, x := range []struct {
			v    qv.Value
			list bool
		}{{a, al}, {b, bl}} {
			if x.list {
				elems += x.v.Elements()
				depth = max(depth, x.v.Depth())
				bytes += x.v.Size() - qv.ElementBytes
			} else {
				elems += 1 + x.v.Elements()
				depth = max(depth, x.v.Depth()+1)
				bytes += x.v.Size()
			}
		}
		if err := f.build(elems, depth, bytes); err != nil {
			return qv.Value{}, err
		}
		n := 2
		if al {
			n += a.Len() - 1
		}
		if bl {
			n += b.Len() - 1
		}
		items := make([]qv.Value, 0, n)
		for _, x := range []qv.Value{a, b} {
			if x.Kind() == qv.KindList {
				items = append(items, x.Items()...)
			} else {
				items = append(items, x)
			}
		}
		return qv.List(items), nil
	}
	switch {
	case a.Kind() == qv.KindString && b.Kind() == qv.KindString:
		x, _ := a.AsString()
		y, _ := b.AsString()
		if err := f.grow(int64(len(x) + len(y))); err != nil {
			return qv.Value{}, err
		}
		return qv.String(x + y), nil
	}
	return arith(f, e, a, b)
}

func isNumber(v qv.Value) bool { return v.Kind() == qv.KindInt || v.Kind() == qv.KindFloat }

func isZero(v qv.Value) bool {
	if i, ok := v.AsInt(); ok {
		return i == 0
	}
	x, ok := v.AsFloat()
	return ok && x == 0
}

func toFloat(v qv.Value) float64 {
	if i, ok := v.AsInt(); ok {
		return float64(i)
	}
	x, _ := v.AsFloat()
	return x
}

// arith is + - * / % ^ on numbers. Int with Int stays Int and refuses
// overflow; a Float operand makes the result Float; ^ is always Float. A
// zero divisor is an error before the types are looked at, Float
// included (SPEC §15.2).
func arith(f *frame, e *ast.Binary, a, b qv.Value) (qv.Value, error) {
	if (e.Op == ast.OpDiv || e.Op == ast.OpMod) && isZero(b) {
		return qv.Value{}, f.fail(e.Span, errs.CodeInvalidArgument, "division by zero")
	}
	if !isNumber(a) || !isNumber(b) {
		return qv.Value{}, nil
	}
	if e.Op == ast.OpPow {
		return float(pow(toFloat(a), toFloat(b))), nil
	}
	x, ok1 := a.AsInt()
	y, ok2 := b.AsInt()
	if ok1 && ok2 {
		r, ok := intOp(e.Op, x, y)
		if !ok {
			return qv.Value{}, f.fail(e.Span, errs.CodeInvalidArgument, "integer overflow")
		}
		return qv.Int(r), nil
	}
	p, q := toFloat(a), toFloat(b)
	switch e.Op {
	case ast.OpAdd:
		return float(p + q), nil
	case ast.OpSub:
		return float(p - q), nil
	case ast.OpMul:
		return float(float64(p * q)), nil
	case ast.OpDiv:
		return float(p / q), nil
	}
	return float(floatMod(p, q)), nil
}

// float returns a computed Float. The sign and payload of a NaN made by
// an operation depend on the processor (amd64 and arm64 differ), so every
// computed NaN becomes the one NaN of math.NaN (DET-01).
func float(x float64) qv.Value {
	if x != x {
		x = math.NaN()
	}
	return qv.Float(x)
}

// intOp computes an Int operator; ok is false on overflow.
func intOp(op ast.BinaryOp, x, y int64) (int64, bool) {
	switch op {
	case ast.OpAdd:
		r := x + y
		return r, (x >= 0) != (y >= 0) || (r >= 0) == (x >= 0)
	case ast.OpSub:
		r := x - y
		return r, (x >= 0) == (y >= 0) || (r >= 0) == (x >= 0)
	case ast.OpMul:
		if x == 0 || y == 0 {
			return 0, true
		}
		r := x * y
		if (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64) || r/y != x {
			return 0, false
		}
		return r, true
	case ast.OpDiv:
		if x == math.MinInt64 && y == -1 {
			return 0, false
		}
		return x / y, true // truncated toward zero
	}
	// %: floored, with the sign of the divisor; MinInt64 % -1 is 0.
	r := x % y
	if r != 0 && (r < 0) != (y < 0) {
		r += y
	}
	return r, true
}

// floatMod is the floored remainder of floats, with the sign of the
// divisor like the Int %.
func floatMod(x, y float64) float64 {
	r := math.Mod(x, y)
	switch {
	case r == 0:
		return math.Copysign(0, y)
	case (r < 0) != (y < 0):
		r += y
	}
	return r
}
