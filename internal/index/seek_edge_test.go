package index

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/value"
)

// rat is the exact value of a finite number, independent of value.Compare.
func rat(v value.Value) *big.Rat {
	if i, ok := v.AsInt(); ok {
		return new(big.Rat).SetInt64(i)
	}
	f, _ := v.AsFloat()
	return new(big.Rat).SetFloat64(f)
}

func extremeNumbers() []value.Value {
	two53 := int64(1) << 53
	fs := []float64{0, math.Copysign(0, -1), 1, -1, 0.5, -0.5,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 2.2250738585072014e-308,
		float64(two53), float64(two53) + 2, -float64(two53),
		math.Ldexp(1, 63), -math.Ldexp(1, 63), math.Nextafter(math.Ldexp(1, 63), 0), -math.Nextafter(math.Ldexp(1, 63), 0),
		math.Ldexp(1, 64), -math.Ldexp(1, 64), math.MaxFloat64, -math.MaxFloat64, 1e300, -1e300, 123.25}
	is := []int64{0, 1, -1, two53, two53 + 1, two53 - 1, -two53 - 1, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
		math.MaxInt64 - 511, math.MaxInt64 - 512, 1<<62 + 1, 123}
	var out []value.Value
	for _, f := range fs {
		out = append(out, value.Float(f))
	}
	for _, i := range is {
		out = append(out, value.Int(i))
	}
	return out
}

// Number seeks over extreme Int and Float values, with many duplicates so
// that sources cross batch boundaries, against an oracle in exact
// rational arithmetic.
func TestNumberSeeksExact(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	ix := declare(t, tx, "n", false, MinPrefixBytes)
	nums := extremeNumbers()
	vals := map[uint64]value.Value{}
	r := rand.New(rand.NewPCG(3, 4))
	do(t, tx, func(s *engine.Stmt) error {
		for e := uint64(1); e <= 900; e++ {
			v := nums[r.IntN(len(nums))]
			vals[e] = v
			putBase(t, s, ix, e, v)
			if err := Insert(ctx, s, ix, e, v); err != nil {
				return err
			}
		}
		return nil
	})
	cmpExact := func(a, b value.Value) int { return rat(a).Cmp(rat(b)) }
	order := func(desc bool) func(a, b Hit) int {
		return func(a, b Hit) int {
			c := cmpExact(a.Value, b.Value)
			if c == 0 {
				c = cmpUint(a.Entity, b.Entity)
			}
			if desc {
				c = -c
			}
			return c
		}
	}
	run := func(q Query, keep func(value.Value) bool) {
		t.Helper()
		var want []Hit
		for e, v := range vals {
			if keep(v) {
				want = append(want, Hit{e, v})
			}
		}
		slices.SortFunc(want, order(q.Desc))
		got, err := seekAll(tx, ix, q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%v: %d hits, want %d", q, len(got), len(want))
		}
		for i := range got {
			if got[i].Entity != want[i].Entity {
				t.Fatalf("%v: hit %d is %v, want %v", q, i, got[i], want[i])
			}
		}
	}
	for _, desc := range []bool{false, true} {
		for _, b := range nums {
			b := b
			run(Query{Op: Equal, Value: b, Desc: desc}, func(v value.Value) bool { return cmpExact(v, b) == 0 })
			for _, incl := range []bool{false, true} {
				run(Query{Op: Range, Lo: b, HasLo: true, LoIncl: incl, Desc: desc}, func(v value.Value) bool {
					c := cmpExact(v, b)
					return c > 0 || incl && c == 0
				})
				run(Query{Op: Range, Hi: b, HasHi: true, HiIncl: incl, Desc: desc}, func(v value.Value) bool {
					c := cmpExact(v, b)
					return c < 0 || incl && c == 0
				})
			}
		}
		for range 300 {
			lo, hi := nums[r.IntN(len(nums))], nums[r.IntN(len(nums))]
			li, hi2 := r.IntN(2) == 0, r.IntN(2) == 0
			run(Query{Op: Range, Lo: lo, Hi: hi, HasLo: true, HasHi: true, LoIncl: li, HiIncl: hi2, Desc: desc}, func(v value.Value) bool {
				a, b := cmpExact(v, lo), cmpExact(v, hi)
				return (a > 0 || li && a == 0) && (b < 0 || hi2 && b == 0)
			})
		}
		// Infinite bounds.
		run(Query{Op: Range, Lo: value.Float(math.Inf(-1)), HasLo: true, Desc: desc}, func(value.Value) bool { return true })
		run(Query{Op: Range, Hi: value.Float(math.Inf(1)), HasHi: true, Desc: desc}, func(value.Value) bool { return true })
		run(Query{Op: Range, Lo: value.Float(math.Inf(1)), HasLo: true, Desc: desc}, func(value.Value) bool { return false })
		run(Query{Op: Range, Hi: value.Float(math.Inf(-1)), HasHi: true, Desc: desc}, func(value.Value) bool { return false })
	}
}

// One truncated group larger than several batches, with exact texts on
// both sides of it, Bytes with 0xFF and NUL bytes, both directions,
// against a byte-wise oracle.
func TestTextGroupsAcrossBatches(t *testing.T) {
	for _, kind := range []string{"string", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			db := openDB(t)
			tx := begin(t, db)
			defer tx.Rollback()
			ix := declare(t, tx, "t", false, MinPrefixBytes)
			r := rand.New(rand.NewPCG(9, 9))
			mk := func(s string) value.Value {
				if kind == "bytes" {
					return value.Bytes([]byte(s))
				}
				return value.String(s)
			}
			bases := []string{
				strings.Repeat("a", 15) + "\x00",     // cut before the escape: 15 bytes
				strings.Repeat("a", 16),              // cut at 16
				strings.Repeat("a", 15) + "\x01",     // just after the NUL group
				strings.Repeat("a", 14) + "\x00\x00", // escape at 14-15, cut at 16
				"",
			}
			if kind == "bytes" {
				bases = append(bases, strings.Repeat("a", 15)+"\xff", "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff")
			}
			alpha := "a\x00\x01z"
			if kind == "bytes" {
				alpha += "\xff"
			}
			vals := map[uint64]string{}
			do(t, tx, func(s *engine.Stmt) error {
				for e := uint64(1); e <= 700; e++ {
					v := bases[r.IntN(len(bases))]
					v += randFrom(r, 6, alpha)
					if r.IntN(10) == 0 {
						v = v[:r.IntN(len(v)+1)]
					}
					vals[e] = v
					putBase(t, s, ix, e, mk(v))
					if err := Insert(ctx, s, ix, e, mk(v)); err != nil {
						return err
					}
				}
				return nil
			})
			pick := func() string {
				e := uint64(r.IntN(700) + 1)
				v := vals[e]
				switch r.IntN(4) {
				case 0:
					return v[:r.IntN(len(v)+1)]
				case 1:
					return v + "\x00"
				}
				return v
			}
			for range 600 {
				desc := r.IntN(2) == 0
				var q Query
				var keep func(string) bool
				switch r.IntN(3) {
				case 0:
					v := pick()
					q, keep = Query{Op: Equal, Value: mk(v)}, func(s string) bool { return s == v }
				case 1:
					lo, hi := pick(), pick()
					q = Query{Op: Range, Lo: mk(lo), Hi: mk(hi), HasLo: r.IntN(4) > 0, HasHi: r.IntN(4) > 0, LoIncl: r.IntN(2) == 0, HiIncl: r.IntN(2) == 0}
					if !q.HasLo && !q.HasHi {
						q.HasLo = true
					}
					keep = func(s string) bool {
						if q.HasLo && (s < lo || s == lo && !q.LoIncl) {
							return false
						}
						if q.HasHi && (s > hi || s == hi && !q.HiIncl) {
							return false
						}
						return true
					}
				default:
					if kind == "bytes" {
						continue
					}
					p := pick()
					q, keep = Query{Op: Prefix, Value: value.String(p)}, func(s string) bool { return strings.HasPrefix(s, p) }
				}
				q.Desc = desc
				var want []uint64
				for e, v := range vals {
					if keep(v) {
						want = append(want, e)
					}
				}
				slices.SortFunc(want, func(a, b uint64) int {
					c := bytes.Compare([]byte(vals[a]), []byte(vals[b]))
					if c == 0 {
						c = cmpUint(a, b)
					}
					if desc {
						c = -c
					}
					return c
				})
				got, err := seekAll(tx, ix, q)
				if err != nil {
					t.Fatal(err)
				}
				ids := make([]uint64, len(got))
				for i, h := range got {
					ids[i] = h.Entity
				}
				if !slices.Equal(ids, want) {
					t.Fatalf("%s %+v: %d hits, want %d", kind, q, len(ids), len(want))
				}
			}
			_ = fmt.Sprint
		})
	}
}
