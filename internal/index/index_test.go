package index

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

var ctx = context.Background()

func openDB(t *testing.T) *engine.DB {
	t.Helper()
	o := engine.DefaultOptions()
	o.Create = true
	o.WALSegmentBytes = 256 << 10
	o.CacheBytes = 1 << 20
	gov, err := govern.New(govern.Config{Limit: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	o.Governor = gov
	db, err := engine.Open(ctx, vfs.NewMemFS(), "i.tacdb", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func begin(t *testing.T, db *engine.DB) *engine.WriteTx {
	t.Helper()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func do(t *testing.T, tx *engine.WriteTx, fn func(s *engine.Stmt) error) {
	t.Helper()
	if err := tx.Statement("test", fn); err != nil {
		t.Fatal(err)
	}
}

// key is the sortable key of v alone.
func key(t *testing.T, v value.Value, prefix int) ([]byte, form) {
	t.Helper()
	k, f, err := appendSortable(nil, v, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return k, f
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

func randText(r *rand.Rand, n int) string { return randFrom(r, n, "a\x00bz~") }

func randBytes(r *rand.Rand, n int) []byte { return []byte(randFrom(r, n, "a\x00b\xffz")) }

func randFrom(r *rand.Rand, n int, alphabet string) string {
	b := make([]byte, r.IntN(n))
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

func randFloat(r *rand.Rand) float64 {
	switch r.IntN(6) {
	case 0:
		return math.Copysign(0, -1)
	case 1:
		return float64(r.Int64N(10) - 5)
	case 2:
		return math.Float64frombits(r.Uint64()&^(0x7ff<<52) | uint64(r.IntN(0x7fe)+1)<<52) // finite
	case 3:
		return math.SmallestNonzeroFloat64 * float64(r.IntN(5)-2)
	case 4:
		return -math.MaxFloat64
	}
	return r.NormFloat64() * 1e6
}

// Within each family the key order is the value order, truncated keys
// included when their prefixes differ (SPEC §12.2).
func TestSortableOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	gens := map[string]func() value.Value{
		"int":    func() value.Value { return value.Int(int64(r.Uint64())) },
		"small":  func() value.Value { return value.Int(r.Int64N(7) - 3) },
		"float":  func() value.Value { return value.Float(randFloat(r)) },
		"string": func() value.Value { return value.String(randText(r, 40)) },
		"bytes":  func() value.Value { return value.Bytes(randBytes(r, 40)) },
		"bool":   func() value.Value { return value.Bool(r.IntN(2) == 0) },
	}
	for name, gen := range gens {
		for range 3000 {
			a, b := gen(), gen()
			ka, fa := key(t, a, MinPrefixBytes)
			kb, fb := key(t, b, MinPrefixBytes)
			if fa == truncated && fb == truncated && bytes.Equal(truncGroup(append(slices.Clone(append(make([]byte, 8), ka...)), make([]byte, 8)...)), truncGroup(append(slices.Clone(append(make([]byte, 8), kb...)), make([]byte, 8)...))) {
				continue // one group: the seek sorts it by the full value
			}
			want := value.Compare(a, b)
			if got := bytes.Compare(ka, kb); sign(got) != sign(want) {
				t.Fatalf("%s: %v vs %v: keys %x vs %x compare %d, values %d", name, a, b, ka, kb, got, want)
			}
		}
	}
	// Families sort by tag, Null apart.
	order := []value.Value{value.Bool(true), value.Int(math.MaxInt64), value.Float(-1e300), value.String(""), value.Bytes(nil)}
	for i := 1; i < len(order); i++ {
		a, _ := key(t, order[i-1], 896)
		b, _ := key(t, order[i], 896)
		if bytes.Compare(a, b) >= 0 {
			t.Fatalf("family of %v does not precede %v", order[i-1], order[i])
		}
	}
	if a, _ := key(t, value.Float(math.Copysign(0, -1)), 896); !bytes.Equal(a, must(key(t, value.Float(0), 896))) {
		t.Fatal("-0 and +0 have different keys")
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := appendSortable(nil, value.Float(f), 896); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("%v: %v", f, err)
		}
	}
}

func must(k []byte, _ form) []byte { return k }

// The cut never splits an escape, the truncated key sorts after the text
// equal to its prefix and before any greater text, and the longest key of
// MaxPrefixBytes fits a key.
func TestTruncation(t *testing.T) {
	esc := []byte{'a', 0x00, 0xFF, 'b'}
	for limit, want := range map[int]int{1: 1, 2: 1, 3: 3, 4: 4, 9: 4} {
		if got := cutPoint(esc, limit); got != want {
			t.Fatalf("cutPoint(%x, %d) = %d, want %d", esc, limit, got, want)
		}
	}
	long := strings.Repeat("x", 20) + "\x00" + strings.Repeat("y", 30)
	k, f := key(t, value.String(long), 21) // the cut falls on the escape of \x00
	if f != truncated || k[1+20] != 0x00 || k[1+21] != 0x01 {
		t.Fatalf("truncated key %x", k)
	}
	prefix, _ := key(t, value.String(strings.Repeat("x", 20)), 21)
	greater, _ := key(t, value.String(strings.Repeat("x", 20)+"\x01"), 21)
	if bytes.Compare(prefix, k) >= 0 || bytes.Compare(k, greater) >= 0 {
		t.Fatalf("order: %x %x %x", prefix, k, greater)
	}
	if k, _ := key(t, value.String(strings.Repeat("\x00", 2000)), MaxPrefixBytes); len(k)+16 > codec.MaxKeyLen {
		t.Fatalf("the longest key holds %d bytes", len(k)+16)
	}
	if _, f := key(t, value.String(strings.Repeat("a", 896)), 896); f != exact {
		t.Fatal("a text of exactly IndexKeyPrefixBytes is truncated")
	}
	if _, f := key(t, value.String(strings.Repeat("a", 897)), 896); f != truncated {
		t.Fatal("a text above IndexKeyPrefixBytes is exact")
	}
}

// The equality of uniqueness (SPEC §12.2).
func TestEqualKey(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		a, b value.Value
		eq   bool
	}{
		{value.Int(1), value.Float(1), true},
		{value.Float(0), value.Float(math.Copysign(0, -1)), true},
		{value.Int(0), value.Float(math.Copysign(0, -1)), true},
		{value.Int(1<<53 + 1), value.Float(1 << 53), false},
		{value.Int(math.MinInt64), value.Float(-(1 << 63)), true},
		{value.Int(math.MaxInt64), value.Float(1 << 63), false},
		{value.Float(0.5), value.Float(0.25), false},
		{value.Int(-1), value.Int(1), false},
		{value.Float(-2), value.Int(2), false},
		{value.List(value.Int(1)), value.List(value.Float(1)), true},
		{value.List(value.Float(nan)), value.List(value.Float(math.Float64frombits(0x7ff8000000000001))), true},
		{value.List(value.Int(1)), value.Vector([]float32{1}), false},
		{value.Vector([]float32{0.5}), value.Vector([]float32{0.5}), true},
		{value.MapOf(map[string]value.Value{"a": value.Int(2)}), value.MapOf(map[string]value.Value{"a": value.Float(2)}), true},
		{value.MapOf(map[string]value.Value{"a": value.Int(2)}), value.MapOf(map[string]value.Value{"b": value.Int(2)}), false},
		{value.String("a"), value.Bytes([]byte("a")), false},
		{value.Null(), value.Null(), true},
		{value.List(), value.MapOf(nil), false},
		{value.Float(math.Inf(1)), value.Float(math.Inf(-1)), false},
	} {
		if got := equal(c.a, c.b); got != c.eq {
			t.Fatalf("equal(%v, %v) = %v", c.a, c.b, got)
		}
	}
	for _, v := range []value.Value{value.Int(1), value.Int(-7), value.Int(math.MinInt64), value.Float(3), value.Float(-2)} {
		s, ok := sibling(v)
		if !ok || !equal(s, v) || s.Kind() == v.Kind() {
			t.Fatalf("sibling of %v: %v %v", v, s, ok)
		}
	}
	for _, v := range []value.Value{value.Int(1<<53 + 1), value.Int(math.MaxInt64), value.Float(0.5), value.Float(1 << 63), value.Float(math.Inf(1))} {
		if s, ok := sibling(v); ok {
			t.Fatalf("%v has a sibling %v", v, s)
		}
	}
}

func putBase(t *testing.T, s *engine.Stmt, ix *Index, e uint64, v value.Value) {
	t.Helper()
	b, err := codec.AppendValue(nil, v, value.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, ix.Entity.base(), catalog.PropertyKey(e, ix.Property), b); err != nil {
		t.Fatal(err)
	}
}

func declare(t *testing.T, tx *engine.WriteTx, name string, unique bool, prefix int) *Index {
	t.Helper()
	var ix *Index
	do(t, tx, func(s *engine.Stmt) (err error) {
		ix, err = Declare(ctx, s, Definition{Name: name, Entity: Nodes, Scope: 1, Property: 1, Unique: unique}, Online, prefix)
		return err
	})
	return ix
}

// A unique index refuses a second live entity with an equal value by the
// codec of equality; Null and absence do not take part; the statement
// undoes the operation.
func TestUnique(t *testing.T) {
	db := openDB(t)
	tx := begin(t, db)
	defer tx.Rollback()
	ix := declare(t, tx, "u", true, MinPrefixBytes)
	n := uint64(0)
	add := func(v value.Value) error {
		n++
		e := n
		return tx.Statement("t", func(s *engine.Stmt) error {
			putBase(t, s, ix, e, v)
			return Insert(ctx, s, ix, e, v)
		})
	}
	long := strings.Repeat("p", 40)
	for _, c := range []struct {
		first, second value.Value
		dup           bool
	}{
		{value.Int(1), value.Float(1), true},
		{value.Float(math.Copysign(0, -1)), value.Int(0), true},
		{value.Int(1<<53 + 1), value.Float(1 << 53), false},
		{value.List(value.Int(1)), value.List(value.Float(1)), true},
		{value.List(value.Float(math.NaN())), value.List(value.Float(math.NaN())), true},
		{value.String(long + "a"), value.String(long + "a"), true},
		{value.String(long + "b"), value.String(long + "c"), false},
		{value.Null(), value.Null(), false},
		{value.Bytes([]byte("k")), value.String("k"), false},
	} {
		sp, _ := tx.Savepoint()
		if err := add(c.first); err != nil {
			t.Fatalf("%v: %v", c.first, err)
		}
		before, _ := Entries(ctx, tx, ix)
		err := add(c.second)
		if c.dup != (errs.CodeOf(err) == errs.CodeUniqueViolation) || !c.dup && err != nil {
			t.Fatalf("%v then %v: %v", c.first, c.second, err)
		}
		if after, _ := Entries(ctx, tx, ix); c.dup && after != before {
			t.Fatalf("a violation left %d entries, not %d", after, before)
		}
		if c.dup {
			if _, ok, _ := tx.Get(ctx, catalog.SlotNodes, catalog.PropertyKey(n, ix.Property)); ok {
				t.Fatal("the statement of a violation kept its write")
			}
		}
		tx.RollbackTo(sp)
		tx.Release(sp)
	}
	// The same entity may write its own value again after removing it.
	do(t, tx, func(s *engine.Stmt) error {
		putBase(t, s, ix, 99, value.Int(5))
		if err := Insert(ctx, s, ix, 99, value.Int(5)); err != nil {
			return err
		}
		if err := Remove(ctx, s, ix, 99, value.Int(5)); err != nil {
			return err
		}
		return Insert(ctx, s, ix, 99, value.Float(5))
	})
	if err := tx.Statement("t", func(s *engine.Stmt) error { return Remove(ctx, s, ix, 99, value.Int(7)) }); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("removing a missing entry: %v", err)
	}
	if err := tx.Statement("t", func(s *engine.Stmt) error { return Insert(ctx, s, ix, 99, value.Float(5)) }); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("an entry twice: %v", err)
	}
	// A hash only names candidates: an entry whose key matches but whose
	// record holds another value, as after a collision, is no duplicate.
	do(t, tx, func(s *engine.Stmt) error {
		putBase(t, s, ix, 200, value.List(value.Int(2)))
		k, _ := EntryKey(ix, 200, value.List(value.Int(1))) // forged: the key of another value
		if err := s.Put(ctx, ix.Entity.Slot(), k, nil); err != nil {
			return err
		}
		putBase(t, s, ix, 201, value.List(value.Int(1)))
		return Insert(ctx, s, ix, 201, value.List(value.Int(1)))
	})
	if err := tx.Statement("t", func(s *engine.Stmt) error { return Insert(ctx, s, ix, 100, value.Float(math.NaN())) }); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("NaN in an ordered index: %v", err)
	}
}

// Declarations: monotonic IDs never reused, names in order, the epoch
// raised once per commit, Drop removing entries and counter, and the
// cache following committed epochs only.
func TestDeclarations(t *testing.T) {
	db := openDB(t)
	var c Cache
	tx := begin(t, db)
	base := tx.Base().SchemaEpoch
	a := declare(t, tx, "b-index", false, 896)
	b := declare(t, tx, "a-index", true, 100)
	if a.ID != 1 || b.ID != 2 || tx.State().SchemaEpoch != base+1 {
		t.Fatalf("IDs %d %d, epoch %d", a.ID, b.ID, tx.State().SchemaEpoch)
	}
	set, err := c.Get(ctx, tx)
	if err != nil || len(set.All()) != 2 || set.All()[0].Name != "a-index" || c.cur.Load() != nil {
		t.Fatalf("set %v, %v; the cache took an uncommitted epoch: %v", set, err, c.cur.Load())
	}
	if got, ok := set.Named("b-index"); !ok || *got != *a {
		t.Fatalf("Named: %+v", got)
	}
	if len(set.Scoped(Nodes, 1)) != 2 || len(set.Scoped(Edges, 1)) != 0 {
		t.Fatal("Scoped")
	}
	// A FAILED index is declared but not maintained.
	sp, _ := tx.Savepoint()
	do(t, tx, func(s *engine.Stmt) error {
		_, err := Declare(ctx, s, Definition{Name: "failed", Entity: Nodes, Scope: 1, Property: 1}, Failed, 896)
		return err
	})
	if set, _ := c.Get(ctx, tx); len(set.All()) != 3 || len(set.Scoped(Nodes, 1)) != 2 {
		t.Fatalf("a FAILED index: %d declared, %d maintained", len(set.All()), len(set.Scoped(Nodes, 1)))
	}
	tx.RollbackTo(sp)
	tx.Release(sp)
	do(t, tx, func(s *engine.Stmt) error {
		for e := range uint64(10) {
			putBase(t, s, a, e+1, value.Int(int64(e)))
			if err := Insert(ctx, s, a, e+1, value.Int(int64(e))); err != nil {
				return err
			}
		}
		return nil
	})
	if n, _ := Entries(ctx, tx, a); n != 10 {
		t.Fatalf("%d entries", n)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	if set, err := c.Get(ctx, r); err != nil || len(set.All()) != 2 || c.cur.Load() == nil {
		t.Fatalf("a reader: %v, %v", set, err)
	}
	r.Close()
	tx = begin(t, db)
	if set, _ := c.Get(ctx, tx); set != c.cur.Load().set {
		t.Fatal("a writer without DDL does not use the cache")
	}
	do(t, tx, func(s *engine.Stmt) error { return Drop(ctx, s, a) })
	set, _ = c.Get(ctx, tx)
	if _, ok := set.Named("b-index"); ok || len(set.All()) != 1 {
		t.Fatal("the writer does not see its Drop")
	}
	// The entries go in batches, by a purge the drop scheduled.
	p := indexPrefix(a.ID)
	count := func() int {
		n := 0
		tx.Range(ctx, a.Entity.Slot(), p, codec.PrefixEnd(p), func(_, _ []byte) error { n++; return nil })
		return n
	}
	purges, err := Purges(ctx, tx)
	if err != nil || len(purges) != 1 || purges[0] != (Purge{a.ID, Nodes}) || count() != 10 {
		t.Fatalf("after Drop: purges %v, %v, %d entries", purges, err, count())
	}
	for i := 0; ; i++ {
		var done bool
		do(t, tx, func(s *engine.Stmt) (err error) { done, err = PurgeBatch(ctx, s, purges[0], 3); return err })
		if done {
			if i != 3 {
				t.Fatalf("10 entries took %d batches of 3", i+1)
			}
			break
		}
		if count() != 10-3*(i+1) {
			t.Fatalf("batch %d left %d entries", i, count())
		}
	}
	if left, _ := Purges(ctx, tx); len(left) != 0 || count() != 0 {
		t.Fatalf("after the purge: purges %v, %d entries", left, count())
	}
	if got, _ := Entries(ctx, tx, a); got != 0 {
		t.Fatalf("the purge left the counter at %d", got)
	}
	c2 := declare(t, tx, "b-index", false, 896)
	if c2.ID != 3 {
		t.Fatalf("a new index after a drop takes ID %d", c2.ID)
	}
	tx.Rollback()
	r, _ = db.BeginRead(ctx)
	defer r.Close()
	if set, _ := c.Get(ctx, r); len(set.All()) != 2 {
		t.Fatal("the rollback leaked into the cache")
	}
	if _, err := decodeDeclaration(declarationKey(0), nil); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("declaration 0: %v", err)
	}
	for f, v := range map[string]value.Value{"entity": value.Int(2), "scope": value.Int(0), "property": value.Int(1 << 32),
		"kind": value.Int(1), "state": value.Int(3), "prefixBytes": value.Int(MaxPrefixBytes + 1), "name": value.String("")} {
		bad := *a
		rec, _ := bad.encode()
		m, _ := declaration.Decode(rec)
		m[f] = v
		b, _ := declaration.Encode(m)
		if _, err := decodeDeclaration(declarationKey(1), b); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("declaration with %s %v: %v", f, v, err)
		}
	}
	// Two declarations under one name are corruption.
	w := begin(t, db)
	twin := *b
	twin.ID = 9
	rec, _ := twin.encode()
	do(t, w, func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotCatalog, declarationKey(9), rec) })
	if _, err := load(ctx, w); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("two indexes under one name: %v", err)
	}
	w.Rollback()
	if _, err := Declare(ctx, nil, Definition{}, Online, MinPrefixBytes-1); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("a prefix below the minimum: %v", err)
	}
}

// oracle is the predicate of a query on one value, written apart from
// the code under test.
func oracle(q Query, v value.Value) bool {
	num := func(x value.Value) bool { k := x.Kind(); return k == value.KindInt || k == value.KindFloat }
	nan := func(x value.Value) bool { f, ok := x.AsFloat(); return ok && f != f }
	switch q.Op {
	case Equal:
		var eq func(a, b value.Value) bool
		eq = func(a, b value.Value) bool {
			switch {
			case num(a) && num(b):
				return !nan(a) && !nan(b) && value.Compare(a, b) == 0
			case a.Kind() != b.Kind() || a.IsNull():
				return false
			case a.Kind() == value.KindList:
				if a.Len() != b.Len() {
					return false
				}
				for i := range a.Len() {
					if !eq(a.Index(i), b.Index(i)) {
						return false
					}
				}
				return true
			}
			return value.Compare(a, b) == 0
		}
		return eq(q.Value, v)
	case Prefix:
		s, ok := v.AsString()
		p, _ := q.Value.AsString()
		return ok && strings.HasPrefix(s, p)
	case Range:
		fam := func(x value.Value) string {
			if num(x) {
				return "n"
			}
			return x.Kind().String()
		}
		for _, b := range []struct {
			has, incl bool
			v         value.Value
			dir       int
		}{{q.HasLo, q.LoIncl, q.Lo, 1}, {q.HasHi, q.HiIncl, q.Hi, -1}} {
			if !b.has {
				continue
			}
			if b.v.IsNull() || nan(b.v) || fam(b.v) != fam(v) {
				return false
			}
			c := value.Compare(v, b.v) * b.dir
			if c < 0 || c == 0 && !b.incl {
				return false
			}
		}
		return true
	}
	return true
}

func randIndexed(r *rand.Rand, long string) value.Value {
	switch r.IntN(12) {
	case 0:
		return value.Null()
	case 1:
		return value.Bool(r.IntN(2) == 0)
	case 2, 3:
		return value.Int(r.Int64N(9) - 4)
	case 4:
		return value.Float(float64(r.IntN(9)-4) / 2)
	case 5:
		return value.Float(math.Copysign(0, -1))
	case 6, 7:
		return value.String(randText(r, 6))
	case 8, 9:
		return value.String(long + randText(r, 8))
	case 10:
		return value.Bytes(randBytes(r, 5))
	}
	switch r.IntN(3) {
	case 0:
		return value.List(value.Null())
	case 1:
		return value.List(value.Float(math.NaN()))
	}
	return value.List(value.Int(r.Int64N(2)))
}

func randQuery(r *rand.Rand, long string) Query {
	q := Query{Op: Op(r.IntN(4)), Desc: r.IntN(2) == 0}
	switch q.Op {
	case Equal:
		q.Value = randIndexed(r, long)
		if r.IntN(10) == 0 {
			q.Value = value.Float(math.NaN())
		}
	case Prefix:
		q.Value = value.String(randText(r, 3))
		if r.IntN(2) == 0 {
			q.Value = value.String(long[:r.IntN(len(long))] + randText(r, 3))
		}
	case Range:
		q.HasLo, q.HasHi = r.IntN(3) > 0, r.IntN(3) > 0
		if !q.HasLo && !q.HasHi {
			q.HasLo = true
		}
		q.LoIncl, q.HiIncl = r.IntN(2) == 0, r.IntN(2) == 0
		for {
			q.Lo, q.Hi = randIndexed(r, long), randIndexed(r, long)
			if q.Lo.Kind() != value.KindList && q.Hi.Kind() != value.KindList {
				break
			}
		}
		if r.IntN(3) > 0 { // mostly one family
			q.Hi = randIndexed(r, long)
			for (q.Hi.Kind() == value.KindInt || q.Hi.Kind() == value.KindFloat) != (q.Lo.Kind() == value.KindInt || q.Lo.Kind() == value.KindFloat) && q.Hi.Kind() != q.Lo.Kind() || q.Hi.Kind() == value.KindList {
				q.Hi = randIndexed(r, long)
			}
		}
	}
	return q
}

// Seeks against a full scan sorted by value and entity, over values of
// every family, numbers in both families, long texts that truncate at
// MinPrefixBytes with shared prefixes and NULs, in both directions, in a
// write transaction and after the commit.
func TestSeekAgainstScan(t *testing.T) {
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { seekAgainstScan(t, seed) })
	}
}

func seekAgainstScan(t *testing.T, seed uint64) {
	r := rand.New(rand.NewPCG(seed, 99))
	db := openDB(t)
	tx := begin(t, db)
	ix := declare(t, tx, "s", false, MinPrefixBytes)
	long := strings.Repeat("q", MinPrefixBytes-1) + "\x00" // the cut falls on an escape
	vals := map[uint64]value.Value{}
	do(t, tx, func(s *engine.Stmt) error {
		for e := uint64(1); e <= 300; e++ {
			v := randIndexed(r, long)
			vals[e] = v
			putBase(t, s, ix, e, v)
			if err := Insert(ctx, s, ix, e, v); err != nil {
				return err
			}
		}
		return nil
	})
	check := func(rd engine.Reader) {
		for range 300 {
			q := randQuery(r, long)
			var want []Hit
			for e, v := range vals {
				if oracle(q, v) {
					want = append(want, Hit{e, v})
				}
			}
			it := &Iterator{desc: q.Desc}
			slices.SortFunc(want, func(a, b Hit) int {
				// Null last ascending, first descending.
				if a.Value.IsNull() != b.Value.IsNull() {
					c := 1
					if a.Value.IsNull() == q.Desc {
						c = -1
					}
					return c
				}
				if it.before(a, b) {
					return -1
				}
				return 1
			})
			got, err := seekAll(rd, ix, q)
			if q.Op == All && err == ErrUnordered {
				continue
			}
			if err != nil {
				t.Fatalf("%+v: %v", q, err)
			}
			if len(got) != len(want) {
				t.Fatalf("%+v: %d hits, want %d\n%v\n%v", q, len(got), len(want), got, want)
			}
			for i := range got {
				if got[i].Entity != want[i].Entity || !value.Identical(got[i].Value, want[i].Value) {
					t.Fatalf("%+v: hit %d is %v, want %v", q, i, got[i], want[i])
				}
			}
		}
	}
	check(tx)
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rd, _ := db.BeginRead(ctx)
	defer rd.Close()
	check(rd)
	// A seek holds one batch of keys at most, whatever the index holds.
	it, err := Seek(ctx, rd, ix, Query{Op: Range, Lo: value.String(""), HasLo: true})
	if err != nil {
		t.Fatal(err)
	}
	it.Next(ctx)
	for _, s := range it.stages[0] {
		if len(s.keys) > seekBatch {
			t.Fatalf("a source holds %d keys", len(s.keys))
		}
	}
	// Without hashed values, All orders everything.
	tx = begin(t, db)
	defer tx.Rollback()
	do(t, tx, func(s *engine.Stmt) error {
		for e, v := range vals {
			if v.Kind() == value.KindList {
				if err := Remove(ctx, s, ix, e, v); err != nil {
					return err
				}
				delete(vals, e)
			}
		}
		return nil
	})
	for _, desc := range []bool{false, true} {
		got, err := seekAll(tx, ix, Query{Op: All, Desc: desc})
		if err != nil || len(got) != len(vals) {
			t.Fatalf("All: %d hits, %v; want %d", len(got), err, len(vals))
		}
		want := slices.Clone(got)
		order := &Iterator{desc: desc}
		slices.SortStableFunc(want, func(a, b Hit) int {
			if a.Value.IsNull() != b.Value.IsNull() {
				if a.Value.IsNull() != desc { // Null last ascending, first descending
					return 1
				}
				return -1
			}
			if order.before(a, b) {
				return -1
			}
			if order.before(b, a) {
				return 1
			}
			return 0
		})
		for i := range got {
			if got[i].Entity != want[i].Entity {
				t.Fatalf("All (desc %v): hit %d is %v, want %v", desc, i, got[i], want[i])
			}
		}
		end := got[len(got)-1]
		if desc {
			end = got[0]
		}
		if !end.Value.IsNull() {
			t.Fatalf("All (desc %v) does not put Null at its end", desc)
		}
	}
}

func seekAll(r engine.Reader, ix *Index, q Query) ([]Hit, error) {
	it, err := Seek(ctx, r, ix, q)
	if err != nil {
		return nil, err
	}
	var out []Hit
	for it.Next(ctx) {
		out = append(out, it.Hit())
	}
	return out, it.Err()
}

// FuzzSortableKey checks the order of keys against value.Compare inside
// each family, with truncation at the smallest prefix.
func FuzzSortableKey(f *testing.F) {
	f.Add(uint8(0), []byte("a\x00b"), []byte("a\x00"), int64(3), int64(-3))
	f.Add(uint8(1), []byte{}, []byte{0}, int64(1), int64(1<<62))
	f.Fuzz(func(t *testing.T, kind uint8, a, b []byte, x, y int64) {
		var va, vb value.Value
		switch kind % 4 {
		case 0:
			va, vb = value.String(string(a)), value.String(string(b))
		case 1:
			va, vb = value.Bytes(a), value.Bytes(b)
		case 2:
			va, vb = value.Int(x), value.Int(y)
		default:
			fa, fb := math.Float64frombits(uint64(x)), math.Float64frombits(uint64(y))
			if math.IsNaN(fa) || math.IsNaN(fb) || math.IsInf(fa, 0) || math.IsInf(fb, 0) {
				return
			}
			va, vb = value.Float(fa), value.Float(fb)
		}
		if s, ok := va.AsString(); ok && !utf8.ValidString(s) {
			return
		}
		if s, ok := vb.AsString(); ok && !utf8.ValidString(s) {
			return
		}
		ka, fa, _ := appendSortable(make([]byte, 8), va, MinPrefixBytes)
		kb, fb, _ := appendSortable(make([]byte, 8), vb, MinPrefixBytes)
		ka, kb = append(ka, make([]byte, 8)...), append(kb, make([]byte, 8)...)
		if fa == truncated && fb == truncated && bytes.Equal(truncGroup(ka), truncGroup(kb)) {
			return
		}
		if (fa == truncated) != (truncGroup(ka) != nil) || (fb == truncated) != (truncGroup(kb) != nil) {
			t.Fatalf("truncGroup misreads %x or %x", ka, kb)
		}
		if got, want := sign(bytes.Compare(ka[:len(ka)-8], kb[:len(kb)-8])), sign(value.Compare(va, vb)); got != want {
			t.Fatalf("%v vs %v: keys %d, values %d", va, vb, got, want)
		}
	})
}
