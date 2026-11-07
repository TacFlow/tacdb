package index

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"slices"
	"strings"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Op is the kind of a seek.
type Op uint8

// Seeks.
const (
	// Equal finds the values equal to Query.Value by the semantics of
	// expressions: Int 1 meets Float 1.0, NaN and Null meet nothing.
	Equal Op = iota
	// Range finds the values between the bounds that are present, which
	// must be of one family (numbers count as one); a bound of another
	// family than a value excludes it, as a comparison would.
	Range
	// Prefix finds the strings that start with Query.Value, a String.
	Prefix
	// All gives every indexed value in the order of SPEC §5.3, Null last
	// in ascending order and first in descending order.
	All
)

// Query is a seek over one index.
type Query struct {
	Op             Op
	Value          value.Value // Equal and Prefix
	Lo, Hi         value.Value // Range
	HasLo, HasHi   bool
	LoIncl, HiIncl bool
	Desc           bool
}

// ErrUnordered is the answer of a seek the index cannot order: values
// kept only as hashes, or bounds of a family without an order here. The
// caller reads the full scope instead.
var ErrUnordered = errs.New(errs.CodeQueryUnsupported, "index.Seek", "the index cannot order these values")

// Hit is an entity found by a seek, with the value of its property.
type Hit struct {
	Entity uint64
	Value  value.Value
}

// Iterator walks the hits of a seek. Every hit satisfies the predicate on
// the full value, read from the base record: keys only name candidates.
// Hits come ordered by value, then entity, ascending or descending.
type Iterator struct {
	r      engine.Reader
	ix     *Index
	desc   bool
	stages [][]*source // walked in order; the sources of a stage are merged
	cur    Hit
	err    error
}

// Seek starts a seek of q over ix, whose entries r must be able to read.
func Seek(ctx context.Context, r engine.Reader, ix *Index, q Query) (*Iterator, error) {
	switch ix.State {
	case Building:
		return nil, errs.Newf(errs.CodeIndexBuilding, "index.Seek", "index %q is still building", ix.Name)
	case Failed:
		return nil, errs.Newf(errs.CodeIndexBuildFailed, "index.Seek", "the build of index %q failed", ix.Name)
	}
	it := &Iterator{r: r, ix: ix, desc: q.Desc}
	p := indexPrefix(ix.ID)
	fam := func(tag byte) []byte { return append(slices.Clone(p), tag) }
	whole := func(tag byte, keep func(value.Value) bool) *source {
		return it.source(fam(tag), codec.PrefixEnd(fam(tag)), keep)
	}
	switch q.Op {
	case Equal:
		v := q.Value
		if v.IsNull() || isNaN(v) {
			return it, nil
		}
		keep := func(w value.Value) bool { return exprEqual(v, w) }
		var stage []*source
		if f, ok := v.AsFloat(); !ok || !math.IsInf(f, 0) { // infinities are never stored
			k, _, err := appendSortable(slices.Clone(p), v, ix.PrefixBytes)
			if err != nil {
				return nil, err
			}
			stage = append(stage, it.source(k, codec.PrefixEnd(k), keep))
		}
		if sib, ok := sibling(v); ok {
			k, _, _ := appendSortable(slices.Clone(p), sib, ix.PrefixBytes)
			stage = append(stage, it.source(k, codec.PrefixEnd(k), keep))
		}
		it.stages = [][]*source{stage}
	case Prefix:
		s, ok := q.Value.AsString()
		if !ok {
			return nil, errs.Newf(errs.CodeInvalidArgument, "index.Seek", "a prefix seek takes a String, not %v", q.Value.Kind())
		}
		lo := textBound(fam(tagString), s, ix.PrefixBytes)
		keep := func(w value.Value) bool { x, ok := w.AsString(); return ok && strings.HasPrefix(x, s) }
		it.stages = [][]*source{{it.source(lo, codec.PrefixEnd(lo), keep)}}
	case Range:
		stage, err := it.rangeSources(p, q)
		if err != nil {
			return nil, err
		}
		it.stages = [][]*source{stage}
	case All:
		empty, err := it.emptyRange(ctx, fam(tagHash), codec.PrefixEnd(fam(tagHash)))
		if err != nil {
			return nil, err
		}
		if !empty {
			return nil, ErrUnordered
		}
		any := func(value.Value) bool { return true }
		it.stages = [][]*source{
			{whole(tagBool, any)},
			{whole(tagInt, any), whole(tagFloat, any)},
			{whole(tagString, any)},
			{whole(tagBytes, any)},
			{whole(tagNull, any)},
		}
		if q.Desc {
			slices.Reverse(it.stages)
		}
	default:
		return nil, errs.Newf(errs.CodeInvalidArgument, "index.Seek", "seek %d", q.Op)
	}
	return it, nil
}

func isNaN(v value.Value) bool {
	f, ok := v.AsFloat()
	return ok && math.IsNaN(f)
}

func (it *Iterator) emptyRange(ctx context.Context, lo, hi []byte) (bool, error) {
	empty := true
	err := it.r.Range(ctx, it.ix.Entity.Slot(), lo, hi, func(_, _ []byte) error { empty = false; return errStop })
	if err == errStop {
		err = nil
	}
	return empty, err
}

// textBound is a key at or below the key of every text at or above s,
// and whose PrefixEnd is above the key of every text at or below s: the
// escaped s cut as a truncated key would be. A truncated key cut short of
// an escape belongs to a text whose next byte is NUL, below s whenever s
// itself was not cut there.
func textBound(fam []byte, s string, prefixBytes int) []byte {
	esc := codec.AppendKeyStringPrefix(nil, s)
	return append(fam, esc[:cutPoint(esc, prefixBytes)]...)
}

// rangeSources returns the sources of a range seek: one family, or the
// two numeric ones, with key bounds that hold every candidate and a
// residual with the exact comparison.
func (it *Iterator) rangeSources(p []byte, q Query) ([]*source, error) {
	if !q.HasLo && !q.HasHi {
		return nil, errs.New(errs.CodeInvalidArgument, "index.Seek", "a range seek needs a bound")
	}
	var bounds []value.Value
	if q.HasLo {
		bounds = append(bounds, q.Lo)
	}
	if q.HasHi {
		bounds = append(bounds, q.Hi)
	}
	family := rangeFamily(bounds[0].Kind())
	for _, b := range bounds {
		switch {
		case b.IsNull() || isNaN(b):
			return nil, nil // comparisons with them are never true
		case rangeFamily(b.Kind()) < 0:
			return nil, ErrUnordered
		case rangeFamily(b.Kind()) != family:
			return nil, nil
		}
	}
	keep := func(w value.Value) bool { // the key ranges hold only the family
		if q.HasLo {
			c := value.Compare(w, q.Lo)
			if c < 0 || c == 0 && !q.LoIncl {
				return false
			}
		}
		if q.HasHi {
			c := value.Compare(w, q.Hi)
			if c > 0 || c == 0 && !q.HiIncl {
				return false
			}
		}
		return true
	}
	fam := func(tag byte) []byte { return append(slices.Clone(p), tag) }
	switch family {
	case familyText, familyBytes:
		tag := byte(tagString)
		if bounds[0].Kind() == value.KindBytes {
			tag = tagBytes
		}
		lo, hi := fam(tag), codec.PrefixEnd(fam(tag))
		if q.HasLo {
			lo = textBound(fam(tag), text(q.Lo), it.ix.PrefixBytes)
		}
		if q.HasHi {
			hi = codec.PrefixEnd(textBound(fam(tag), text(q.Hi), it.ix.PrefixBytes))
		}
		return []*source{it.source(lo, hi, keep)}, nil
	case familyBool:
		return []*source{it.source(fam(tagBool), codec.PrefixEnd(fam(tagBool)), keep)}, nil
	}
	var out []*source
	ilo, ihi, iok := intBounds(q)
	if iok {
		lo := binary.BigEndian.AppendUint64(fam(tagInt), uint64(ilo)^1<<63)
		hi := codec.PrefixEnd(binary.BigEndian.AppendUint64(fam(tagInt), uint64(ihi)^1<<63))
		out = append(out, it.source(lo, hi, keep))
	}
	flo, fhi := floatBounds(q)
	lo := binary.BigEndian.AppendUint64(fam(tagFloat), floatBits(flo))
	hi := codec.PrefixEnd(binary.BigEndian.AppendUint64(fam(tagFloat), floatBits(fhi)))
	return append(out, it.source(lo, hi, keep)), nil
}

// Families of range seeks; -1 has no order in an index.
const (
	familyBool = iota
	familyNumber
	familyText
	familyBytes
)

func rangeFamily(k value.Kind) int {
	switch k {
	case value.KindBool:
		return familyBool
	case value.KindInt, value.KindFloat:
		return familyNumber
	case value.KindString:
		return familyText
	case value.KindBytes:
		return familyBytes
	}
	return -1
}

func text(v value.Value) string {
	if s, ok := v.AsString(); ok {
		return s
	}
	b, _ := v.AsBytes()
	return string(b)
}

// intBounds are Int bounds that hold every Int within the range: a Float
// bound is rounded outwards. ok is false when no Int can be in range.
func intBounds(q Query) (lo, hi int64, ok bool) {
	lo, hi = math.MinInt64, math.MaxInt64
	if q.HasLo {
		if i, isInt := q.Lo.AsInt(); isInt {
			lo = i
		} else {
			f, _ := q.Lo.AsFloat()
			switch {
			case f >= 1<<63:
				return 0, 0, false
			case f > -(1 << 63):
				lo = int64(math.Floor(f))
			}
		}
	}
	if q.HasHi {
		if i, isInt := q.Hi.AsInt(); isInt {
			hi = i
		} else {
			f, _ := q.Hi.AsFloat()
			switch {
			case f < -(1 << 63):
				return 0, 0, false
			case f < 1<<63:
				hi = int64(math.Ceil(f))
			}
		}
	}
	return lo, hi, lo <= hi
}

// floatBounds are finite Float bounds that hold every Float within the
// range. An Int bound converts to its nearest Float: no Float lies between
// the two, so none is lost.
func floatBounds(q Query) (lo, hi float64) {
	lo, hi = -math.MaxFloat64, math.MaxFloat64
	if q.HasLo {
		if i, isInt := q.Lo.AsInt(); isInt {
			lo = float64(i)
		} else if f, _ := q.Lo.AsFloat(); f > lo {
			lo = f
		}
	}
	if q.HasHi {
		if i, isInt := q.Hi.AsInt(); isInt {
			hi = float64(i)
		} else if f, _ := q.Hi.AsFloat(); f < hi {
			hi = f
		}
	}
	return lo, hi
}

// exprEqual is equality as an expression sees it (SPEC §15.2): numbers by
// value across Int and Float, NaN and Null never equal, recursively.
func exprEqual(a, b value.Value) bool {
	na, nb := rangeFamily(a.Kind()) == familyNumber, rangeFamily(b.Kind()) == familyNumber
	if na || nb {
		return na && nb && !isNaN(a) && !isNaN(b) && value.Compare(a, b) == 0
	}
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case value.KindNull:
		return false
	case value.KindVector:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if a.VectorAt(i) != b.VectorAt(i) {
				return false
			}
		}
		return true
	case value.KindList:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !exprEqual(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case value.KindMap:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			ka, va := a.EntryAt(i)
			kb, vb := b.EntryAt(i)
			if ka != kb || !exprEqual(va, vb) {
				return false
			}
		}
		return true
	}
	return value.Compare(a, b) == 0
}

// Next moves to the next hit; at the end or on error it returns false and
// Err tells which.
func (it *Iterator) Next(ctx context.Context) bool {
	for it.err == nil && len(it.stages) > 0 {
		var best *source
		for _, s := range it.stages[0] {
			h, ok, err := s.peek(ctx)
			if err != nil {
				it.err = err
				return false
			}
			if ok && (best == nil || it.before(h, best.head)) {
				best = s
			}
		}
		if best == nil {
			it.stages = it.stages[1:]
			continue
		}
		it.cur = best.head
		best.has = false
		return true
	}
	return false
}

// before reports whether a comes before b in the order of the walk.
func (it *Iterator) before(a, b Hit) bool {
	c := value.Compare(a.Value, b.Value)
	if c == 0 {
		c = cmpUint(a.Entity, b.Entity)
	}
	if it.desc {
		return c > 0
	}
	return c < 0
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Hit returns the current hit.
func (it *Iterator) Hit() Hit { return it.cur }

// Err returns the error that ended the walk, if any.
func (it *Iterator) Err() error { return it.err }

// seekBatch is how many keys a source reads per operation of the
// transaction.
const seekBatch = 64

// source walks the entries of one key range, in key order or its
// reverse, and turns them into hits: it reads each candidate's value,
// keeps those the residual accepts, and sorts each group of truncated
// keys that share a prefix by the full value.
type source struct {
	it      *Iterator
	lo, hi  []byte // the range left to read
	keep    func(value.Value) bool
	keys    [][]byte // read, not yet turned into hits
	done    bool     // the range is read
	pending []Hit    // hits of a sorted group, in walk order
	head    Hit
	has     bool
}

func (it *Iterator) source(lo, hi []byte, keep func(value.Value) bool) *source {
	return &source{it: it, lo: lo, hi: hi, keep: keep}
}

// peek returns the next hit of the source without taking it.
func (s *source) peek(ctx context.Context) (Hit, bool, error) {
	for !s.has {
		if len(s.pending) > 0 {
			s.head, s.pending, s.has = s.pending[0], s.pending[1:], true
			break
		}
		k, ok, err := s.nextKey(ctx)
		if err != nil || !ok {
			return Hit{}, false, err
		}
		group := truncGroup(k)
		keys := [][]byte{k}
		for group != nil {
			n, ok, err := s.peekKey(ctx)
			if err != nil {
				return Hit{}, false, err
			}
			if !ok || !bytes.Equal(truncGroup(n), group) {
				break
			}
			s.keys = s.keys[1:]
			keys = append(keys, n)
		}
		for _, k := range keys {
			h, err := s.hit(ctx, k)
			if err != nil {
				return Hit{}, false, err
			}
			if s.keep(h.Value) {
				s.pending = append(s.pending, h)
			}
		}
		if group != nil {
			slices.SortFunc(s.pending, func(a, b Hit) int {
				if s.it.before(a, b) {
					return -1
				}
				if s.it.before(b, a) {
					return 1
				}
				return 0
			})
		}
	}
	return s.head, true, nil
}

func (s *source) hit(ctx context.Context, k []byte) (Hit, error) {
	if len(k) < 8+1+8 {
		return Hit{}, errs.Newf(errs.CodeCorrupt, "index", "index entry %x", k)
	}
	e := binary.BigEndian.Uint64(k[len(k)-8:])
	v, err := baseValue(ctx, s.it.r, s.it.ix, e)
	return Hit{Entity: e, Value: v}, err
}

// truncGroup returns the bytes a truncated text key shares with the keys
// of its group, up to its marker 00 01; nil for any other key.
func truncGroup(k []byte) []byte {
	if len(k) < 8+1+2+truncHashBytes+8 || (k[8] != tagString && k[8] != tagBytes) {
		return nil
	}
	m := len(k) - 8 - truncHashBytes - 2
	if k[m] != 0x00 || k[m+1] != 0x01 {
		return nil
	}
	// An exact key ends with 00 00 before the entity; the marker of a
	// truncated key stands where the hash would follow. Confirm it is
	// the first unescaped 00 of the text.
	for i := 9; i < m; i++ {
		if k[i] == 0x00 {
			if k[i+1] != 0xFF {
				return nil
			}
			i++
		}
	}
	return k[:m+2]
}

func (s *source) peekKey(ctx context.Context) ([]byte, bool, error) {
	if len(s.keys) == 0 {
		if err := s.fill(ctx); err != nil {
			return nil, false, err
		}
	}
	if len(s.keys) == 0 {
		return nil, false, nil
	}
	return s.keys[0], true, nil
}

func (s *source) nextKey(ctx context.Context) ([]byte, bool, error) {
	k, ok, err := s.peekKey(ctx)
	if ok {
		s.keys = s.keys[1:]
	}
	return k, ok, err
}

// fill reads the next batch of keys of the range, each batch one
// operation of the transaction, resuming after the last key read.
func (s *source) fill(ctx context.Context) error {
	if s.done {
		return nil
	}
	n := 0
	var last []byte
	collect := func(k, _ []byte) error {
		if n == seekBatch {
			return errStop
		}
		n++
		last = append([]byte(nil), k...)
		s.keys = append(s.keys, last)
		return nil
	}
	var err error
	if s.it.desc {
		err = s.it.r.RangeReverse(ctx, s.it.ix.Entity.Slot(), s.lo, s.hi, collect)
	} else {
		err = s.it.r.Range(ctx, s.it.ix.Entity.Slot(), s.lo, s.hi, collect)
	}
	switch {
	case err == errStop:
		if s.it.desc {
			s.hi = last
		} else {
			s.lo = append(last, 0)
		}
	case err != nil:
		return err
	default:
		s.done = true
	}
	return nil
}
