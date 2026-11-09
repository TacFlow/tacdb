// Package equal is the codec of equality (SPEC §12.2, §31.3): a canonical
// key such that two values equal for uniqueness, grouping and DISTINCT
// have the same bytes. Property indexes and the query executor share it.
package equal

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/TacFlow/tacdb/value"
)

// Tags of the equality key. The query executor extends the key with the
// entity and path tags.
const (
	TagNull   = 0x00
	TagBool   = 0x01
	TagNumber = 0x02 // a finite non-zero number as sign, exponent and odd mantissa
	TagZero   = 0x03
	TagNaN    = 0x04
	TagPosInf = 0x05
	TagNegInf = 0x06
	TagString = 0x07
	TagBytes  = 0x08
	TagVector = 0x09
	TagList   = 0x0A
	TagMap    = 0x0B
	TagNode   = 0x0C
	TagEdge   = 0x0D
	TagPath   = 0x0E
)

// AppendKey appends the canonical equality key of v: equal values by the
// codec of equality give equal bytes. A finite number is its exact
// reduced binary fraction, so Int 1, Float 1.0 and -0, +0 meet; every NaN
// is one marker; lists and maps recurse, maps in key order.
func AppendKey(dst []byte, v value.Value) []byte {
	switch v.Kind() {
	case value.KindNull:
		return append(dst, TagNull)
	case value.KindBool:
		b, _ := v.AsBool()
		if b {
			return append(dst, TagBool, 1)
		}
		return append(dst, TagBool, 0)
	case value.KindInt:
		i, _ := v.AsInt()
		return AppendInt(dst, i)
	case value.KindFloat:
		f, _ := v.AsFloat()
		return AppendFloat(dst, f)
	case value.KindString:
		s, _ := v.AsString()
		return append(binary.BigEndian.AppendUint32(append(dst, TagString), uint32(len(s))), s...)
	case value.KindBytes:
		return v.AppendBytes(binary.BigEndian.AppendUint32(append(dst, TagBytes), uint32(v.Len())))
	case value.KindVector:
		dst = binary.BigEndian.AppendUint32(append(dst, TagVector), uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = AppendFloat(dst, float64(v.VectorAt(i)))
		}
		return dst
	case value.KindList:
		dst = AppendHeader(dst, TagList, v.Len())
		for i := 0; i < v.Len(); i++ {
			dst = AppendKey(dst, v.Index(i))
		}
		return dst
	case value.KindMap:
		dst = AppendHeader(dst, TagMap, v.Len())
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			dst = AppendKey(AppendMapKey(dst, k), item)
		}
		return dst
	}
	panic("equal: unknown value kind")
}

// AppendHeader appends the tag and the element count of a List, a Map or
// a path.
func AppendHeader(dst []byte, tag byte, n int) []byte {
	return binary.BigEndian.AppendUint32(append(dst, tag), uint32(n))
}

// AppendMapKey appends a key of a Map entry, before its value.
func AppendMapKey(dst []byte, k string) []byte {
	return append(binary.BigEndian.AppendUint32(dst, uint32(len(k))), k...)
}

// AppendID appends the tag and the ID of an entity.
func AppendID(dst []byte, tag byte, id uint64) []byte {
	return binary.BigEndian.AppendUint64(append(dst, tag), id)
}

// AppendInt appends the key of an Int.
func AppendInt(dst []byte, i int64) []byte {
	m := uint64(i)
	if i < 0 {
		m = -m // two's complement: also right for MinInt64
	}
	return appendFraction(dst, i < 0, m, 0)
}

// AppendFloat appends the key of a Float.
func AppendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, TagNaN)
	case math.IsInf(f, 1):
		return append(dst, TagPosInf)
	case math.IsInf(f, -1):
		return append(dst, TagNegInf)
	}
	frac, exp := math.Frexp(math.Abs(f)) // |f| = frac * 2^exp, frac in [0.5, 1)
	m := uint64(math.Ldexp(frac, 53))    // exact: 53 bits of mantissa
	return appendFraction(dst, f < 0, m, exp-53)
}

// appendFraction appends ±m * 2^e reduced to an odd mantissa.
func appendFraction(dst []byte, neg bool, m uint64, e int) []byte {
	if m == 0 {
		return append(dst, TagZero)
	}
	for m&1 == 0 {
		m >>= 1
		e++
	}
	sign := byte(0)
	if neg {
		sign = 1
	}
	dst = append(dst, TagNumber, sign)
	dst = binary.BigEndian.AppendUint32(dst, uint32(int32(e)))
	return binary.BigEndian.AppendUint64(dst, m)
}

// Equal reports whether a and b are equal by the codec of equality, the
// equality of uniqueness and grouping.
func Equal(a, b value.Value) bool {
	return bytes.Equal(AppendKey(nil, a), AppendKey(nil, b))
}
