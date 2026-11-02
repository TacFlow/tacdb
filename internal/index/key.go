// Package index holds the property indexes of the G2 format (SPEC §12.2):
// their declarations in the Catalog tree, the sortable keys of their
// entries, the maintenance of entries and uniqueness inside an engine
// statement, and the seeks that read them.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Tags of the sortable value of an entry. The order of the tags is the
// order of the families (SPEC §5.3), Null apart.
const (
	tagNull   = 0x01
	tagBool   = 0x10
	tagInt    = 0x20
	tagFloat  = 0x21
	tagString = 0x30
	tagBytes  = 0x40
	tagHash   = 0xF0 // List, Map and Vector: no order, equality only
)

// Bounds of IndexKeyPrefixBytes: an entry key is the index ID, a tag, the
// escaped prefix, a marker, a 16-byte hash and the entity ID, and it must
// fit codec.MaxKeyLen.
const (
	DefaultPrefixBytes = 896
	MinPrefixBytes     = 16
	MaxPrefixBytes     = codec.MaxKeyLen - 8 - 1 - 2 - truncHashBytes - 8
)

const truncHashBytes = 16

// form says how much of a value its sortable key holds.
type form uint8

const (
	exact     form = iota // the key determines the value, up to its family's equality
	truncated             // an escaped prefix and a hash: confirm on the base record
	hashed                // a hash of the equality key: confirm on the base record
)

// appendSortable appends the sortable form of v, without the entity ID.
// Ordered families refuse non-finite floats (SPEC §5.1).
func appendSortable(dst []byte, v value.Value, prefixBytes int) ([]byte, form, error) {
	switch v.Kind() {
	case value.KindNull:
		return append(dst, tagNull), exact, nil
	case value.KindBool:
		b, _ := v.AsBool()
		x := byte(0)
		if b {
			x = 1
		}
		return append(dst, tagBool, x), exact, nil
	case value.KindInt:
		i, _ := v.AsInt()
		return binary.BigEndian.AppendUint64(append(dst, tagInt), uint64(i)^1<<63), exact, nil
	case value.KindFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return dst, exact, errs.New(errs.CodeInvalidArgument, "index", "an ordered index refuses NaN and infinities")
		}
		return binary.BigEndian.AppendUint64(append(dst, tagFloat), floatBits(f)), exact, nil
	case value.KindString:
		s, _ := v.AsString()
		return appendText(dst, tagString, s, prefixBytes)
	case value.KindBytes:
		b, _ := v.AsBytes()
		return appendText(dst, tagBytes, string(b), prefixBytes)
	}
	sum := sha256.Sum256(equalKey(nil, v))
	return append(append(dst, tagHash), sum[:]...), hashed, nil
}

// floatBits orders finite floats as unsigned integers: negative ones have
// every bit flipped, the others their sign bit; -0 is +0.
func floatBits(f float64) uint64 {
	if f == 0 {
		f = 0
	}
	b := math.Float64bits(f)
	if b>>63 == 1 {
		return ^b
	}
	return b | 1<<63
}

// appendText appends a String or Bytes value: its escaped form and the
// terminator 00 00 when the escaped form fits prefixBytes; otherwise the
// longest escape-aligned prefix that fits, the marker 00 01 and the first
// bytes of the SHA-256 of the value (SPEC §12.2). 00 01 sorts after 00 00,
// so a truncated key follows the string equal to its prefix and precedes
// every greater string.
func appendText(dst []byte, tag byte, s string, prefixBytes int) ([]byte, form, error) {
	dst = append(dst, tag)
	start := len(dst)
	dst = codec.AppendKeyStringPrefix(dst, s)
	if len(dst)-start <= prefixBytes {
		return append(dst, 0x00, 0x00), exact, nil
	}
	cut := cutPoint(dst[start:], prefixBytes)
	dst = append(dst[:start+cut], 0x00, 0x01)
	sum := sha256.Sum256([]byte(s))
	return append(dst, sum[:truncHashBytes]...), truncated, nil
}

// cutPoint is the length of the longest prefix of esc, at most limit
// bytes, that does not split an escape 00 FF.
func cutPoint(esc []byte, limit int) int {
	if limit > len(esc) {
		limit = len(esc)
	}
	// Escapes are 00 FF and never overlap: count the escapes that start
	// before the cut to know whether the byte before it opens one.
	for i := 0; i < limit; i++ {
		if esc[i] == 0x00 {
			if i == limit-1 {
				return i
			}
			i++ // its FF
		}
	}
	return limit
}

// siblings returns the values of the other numeric family that equal v
// exactly: Int n and Float n when n converts both ways without loss
// (SPEC §12.2: Int 1 equals Float 1.0, never through a rounding).
func sibling(v value.Value) (value.Value, bool) {
	switch v.Kind() {
	case value.KindInt:
		i, _ := v.AsInt()
		f := float64(i)
		if f >= -(1<<63) && f < 1<<63 && int64(f) == i {
			return value.Float(f), true
		}
	case value.KindFloat:
		f, _ := v.AsFloat()
		if f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
			return value.Int(int64(f)), true
		}
	}
	return value.Value{}, false
}

// Tags of the equality key.
const (
	eqNull   = 0x00
	eqBool   = 0x01
	eqNumber = 0x02 // a finite non-zero number as sign, exponent and odd mantissa
	eqZero   = 0x03
	eqNaN    = 0x04
	eqPosInf = 0x05
	eqNegInf = 0x06
	eqString = 0x07
	eqBytes  = 0x08
	eqVector = 0x09
	eqList   = 0x0A
	eqMap    = 0x0B
)

// equalKey appends the canonical equality key of v (SPEC §12.2): equal
// values by the codec of equality give equal bytes. A finite number is
// its exact reduced binary fraction, so Int 1, Float 1.0 and -0, +0 meet;
// every NaN is one marker; lists and maps recurse, maps in key order.
func equalKey(dst []byte, v value.Value) []byte {
	switch v.Kind() {
	case value.KindNull:
		return append(dst, eqNull)
	case value.KindBool:
		b, _ := v.AsBool()
		if b {
			return append(dst, eqBool, 1)
		}
		return append(dst, eqBool, 0)
	case value.KindInt:
		i, _ := v.AsInt()
		neg := i < 0
		m := uint64(i)
		if neg {
			m = -m // two's complement: also right for MinInt64
		}
		return appendFraction(dst, neg, m, 0)
	case value.KindFloat:
		f, _ := v.AsFloat()
		return appendFloat(dst, f)
	case value.KindString:
		s, _ := v.AsString()
		return append(binary.BigEndian.AppendUint32(append(dst, eqString), uint32(len(s))), s...)
	case value.KindBytes:
		b, _ := v.AsBytes()
		return append(binary.BigEndian.AppendUint32(append(dst, eqBytes), uint32(len(b))), b...)
	case value.KindVector:
		dst = binary.BigEndian.AppendUint32(append(dst, eqVector), uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = appendFloat(dst, float64(v.VectorAt(i)))
		}
		return dst
	case value.KindList:
		dst = binary.BigEndian.AppendUint32(append(dst, eqList), uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = equalKey(dst, v.Index(i))
		}
		return dst
	case value.KindMap:
		dst = binary.BigEndian.AppendUint32(append(dst, eqMap), uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			dst = append(binary.BigEndian.AppendUint32(dst, uint32(len(k))), k...)
			dst = equalKey(dst, item)
		}
		return dst
	}
	panic("index: unknown value kind")
}

func appendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, eqNaN)
	case math.IsInf(f, 1):
		return append(dst, eqPosInf)
	case math.IsInf(f, -1):
		return append(dst, eqNegInf)
	}
	frac, exp := math.Frexp(math.Abs(f)) // |f| = frac * 2^exp, frac in [0.5, 1)
	m := uint64(math.Ldexp(frac, 53))    // exact: 53 bits of mantissa
	return appendFraction(dst, f < 0, m, exp-53)
}

// appendFraction appends ±m * 2^e reduced to an odd mantissa.
func appendFraction(dst []byte, neg bool, m uint64, e int) []byte {
	if m == 0 {
		return append(dst, eqZero)
	}
	for m&1 == 0 {
		m >>= 1
		e++
	}
	sign := byte(0)
	if neg {
		sign = 1
	}
	dst = append(dst, eqNumber, sign)
	dst = binary.BigEndian.AppendUint32(dst, uint32(int32(e)))
	return binary.BigEndian.AppendUint64(dst, m)
}

// equal reports whether a and b are equal by the codec of equality, the
// equality of uniqueness.
func equal(a, b value.Value) bool {
	return slices.Equal(equalKey(nil, a), equalKey(nil, b))
}
