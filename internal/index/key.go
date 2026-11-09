// Package index holds the property indexes of the G2 format (SPEC §12.2):
// their declarations in the Catalog tree, the sortable keys of their
// entries, the maintenance of entries and uniqueness inside an engine
// statement, and the seeks that read them.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/equal"
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
	sum := sha256.Sum256(equal.AppendKey(nil, v))
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
