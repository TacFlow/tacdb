package eval

import "math"

// pow is the ^ of the dialect. math.Pow is not bit-for-bit the same on
// amd64 and arm64: math.Exp and math.Log have assembly versions, and the
// compiler may fuse a product into an addition. pow is the portable Go
// algorithm of the standard library with every product rounded by an
// explicit conversion before it is added (SPEC §5.3), so ^ gives the same
// bits on every architecture (DET-01). Frexp, Ldexp, Modf, Sqrt and Abs
// are exact, so their assembly versions are safe; a NaN, whose sign
// depends on the processor, becomes the NaN of math.NaN.
func pow(x, y float64) float64 {
	if r := powf(x, y); r == r {
		return r
	}
	return math.NaN()
}

func powf(x, y float64) float64 {
	switch {
	case y == 0 || x == 1:
		return 1
	case y == 1:
		return x
	case math.IsNaN(x) || math.IsNaN(y):
		return math.NaN()
	case x == 0:
		switch {
		case y < 0:
			if math.Signbit(x) && isOddInt(y) {
				return math.Inf(-1)
			}
			return math.Inf(1)
		case y > 0:
			if math.Signbit(x) && isOddInt(y) {
				return x
			}
			return 0
		}
	case math.IsInf(y, 0):
		switch {
		case x == -1:
			return 1
		case (math.Abs(x) < 1) == math.IsInf(y, 1):
			return 0
		default:
			return math.Inf(1)
		}
	case math.IsInf(x, 0):
		if math.IsInf(x, -1) {
			return powf(1/x, -y) // pow(-0, -y)
		}
		switch {
		case y < 0:
			return 0
		case y > 0:
			return math.Inf(1)
		}
	case y == 0.5:
		return math.Sqrt(x)
	case y == -0.5:
		return 1 / math.Sqrt(x)
	}

	yi, yf := math.Modf(math.Abs(y))
	if yf != 0 && x < 0 {
		return math.NaN()
	}
	if yi >= 1<<63 {
		// yi is a large even int that overflows or underflows for every x
		// except -1 (x == 1 was handled above).
		switch {
		case x == -1:
			return 1
		case (math.Abs(x) < 1) == (y > 0):
			return 0
		default:
			return math.Inf(1)
		}
	}

	// ans = a1 * 2**ae (= 1 for now).
	a1 := 1.0
	ae := 0

	// ans *= x**yf
	if yf != 0 {
		if yf > 0.5 {
			yf--
			yi++
		}
		a1 = exp(float64(yf * logf(x)))
	}

	// ans *= x**yi by successive squarings of x, accumulating powers of
	// two into ae.
	x1, xe := math.Frexp(x)
	for i := int64(yi); i != 0; i >>= 1 {
		if xe < -1<<12 || 1<<12 < xe {
			// xe would overflow the shift below; ae += xe is already
			// beyond the exponent of a float64, so Ldexp gives 0 or Inf.
			ae += xe
			break
		}
		if i&1 == 1 {
			a1 = float64(a1 * x1)
			ae += xe
		}
		x1 = float64(x1 * x1)
		xe <<= 1
		if x1 < .5 {
			x1 += x1
			xe--
		}
	}

	// ans = a1*2**ae; if y < 0 { ans = 1 / ans }, in the opposite order.
	if y < 0 {
		a1 = 1 / a1
		ae = -ae
	}
	return math.Ldexp(a1, ae)
}

func isOddInt(x float64) bool {
	if math.Abs(x) >= 1<<53 {
		return false // every float64 this large is even
	}
	xi, xf := math.Modf(x)
	return xf == 0 && int64(xi)&1 == 1
}

// exp is the portable exponential of the standard library.
func exp(x float64) float64 {
	const (
		ln2Hi = 6.93147180369123816490e-01
		ln2Lo = 1.90821492927058770002e-10
		log2e = 1.44269504088896338700e+00

		overflow  = 7.09782712893383973096e+02
		underflow = -7.45133219101941108420e+02
		nearZero  = 1.0 / (1 << 28) // 2**-28
	)
	switch {
	case math.IsNaN(x):
		return x
	case x > overflow: // also +Inf
		return math.Inf(1)
	case x < underflow: // also -Inf
		return 0
	case -nearZero < x && x < nearZero:
		return 1 + x
	}
	// reduce; computed as r = hi - lo for extra precision.
	var k int
	switch {
	case x < 0:
		k = int(float64(log2e*x) - 0.5)
	case x > 0:
		k = int(float64(log2e*x) + 0.5)
	}
	hi := x - float64(float64(k)*ln2Hi)
	lo := float64(float64(k) * ln2Lo)
	return expmulti(hi, lo, k)
}

func expmulti(hi, lo float64, k int) float64 {
	const (
		p1 = 1.66666666666666657415e-01  /* 0x3FC55555; 0x55555555 */
		p2 = -2.77777777770155933842e-03 /* 0xBF66C16C; 0x16BEBD93 */
		p3 = 6.61375632143793436117e-05  /* 0x3F11566A; 0xAF25DE2C */
		p4 = -1.65339022054652515390e-06 /* 0xBEBBBD41; 0xC5D26BF1 */
		p5 = 4.13813679705723846039e-08  /* 0x3E663769; 0x72BEA4D0 */
	)
	r := hi - lo
	t := float64(r * r)
	c := r - float64(t*(p1+float64(t*(p2+float64(t*(p3+float64(t*(p4+float64(t*p5)))))))))
	y := 1 - ((lo - float64(r*c)/(2-c)) - hi)
	return math.Ldexp(y, k)
}

// logf is the portable natural logarithm of the standard library.
func logf(x float64) float64 {
	const (
		ln2Hi = 6.93147180369123816490e-01 /* 3fe62e42 fee00000 */
		ln2Lo = 1.90821492927058770002e-10 /* 3dea39ef 35793c76 */
		l1    = 6.666666666666735130e-01   /* 3FE55555 55555593 */
		l2    = 3.999999999940941908e-01   /* 3FD99999 9997FA04 */
		l3    = 2.857142874366239149e-01   /* 3FD24924 94229359 */
		l4    = 2.222219843214978396e-01   /* 3FCC71C5 1D8E78AF */
		l5    = 1.818357216161805012e-01   /* 3FC74664 96CB03DE */
		l6    = 1.531383769920937332e-01   /* 3FC39A09 D078C69F */
		l7    = 1.479819860511658591e-01   /* 3FC2F112 DF3E5244 */
	)
	switch {
	case math.IsNaN(x) || math.IsInf(x, 1):
		return x
	case x < 0:
		return math.NaN()
	case x == 0:
		return math.Inf(-1)
	}
	// reduce
	f1, ki := math.Frexp(x)
	if f1 < math.Sqrt2/2 {
		f1 *= 2
		ki--
	}
	f := f1 - 1
	k := float64(ki)
	// compute
	s := f / (2 + f)
	s2 := float64(s * s)
	s4 := float64(s2 * s2)
	t1 := float64(s2 * (l1 + float64(s4*(l3+float64(s4*(l5+float64(s4*l7)))))))
	t2 := float64(s4 * (l2 + float64(s4*(l4+float64(s4*l6)))))
	R := t1 + t2
	hfsq := float64(float64(0.5*f) * f)
	return float64(k*ln2Hi) - ((hfsq - (float64(s*(hfsq+R)) + float64(k*ln2Lo))) - f)
}
