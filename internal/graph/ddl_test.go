package graph

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

func props(kv ...any) map[string]value.Value {
	m := map[string]value.Value{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1].(value.Value)
	}
	return m
}

func lookupIndex(t *testing.T, g *Graph, r engine.Reader, name string) *index.Index {
	t.Helper()
	set, err := g.Indexes.Get(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	ix, ok := set.Named(name)
	if !ok {
		t.Fatalf("no index %q", name)
	}
	return ix
}

func seekIDs(t *testing.T, r engine.Reader, ix *index.Index, q index.Query) []uint64 {
	t.Helper()
	it, err := index.Seek(ctx, r, ix, q)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for it.Next(ctx) {
		ids = append(ids, it.Hit().Entity)
	}
	if it.Err() != nil {
		t.Fatal(it.Err())
	}
	return ids
}

// CreateIndex fills the index from what exists, is idempotent for the
// same declaration, refuses another one under the name and raises the
// schema epoch once per commit; DropIndex removes it; writers keep it.
func TestCreateIndex(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	a := node(t, g, tx, []string{"P"}, props("age", value.Int(30)))
	node(t, g, tx, []string{"P"}, nil) // no property: no entry
	node(t, g, tx, []string{"Q"}, props("age", value.Int(30)))
	e := edge(t, g, tx, a, a, "T")
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, EdgeEntity, e, "w", value.Float(2)) })
	epoch := tx.State().SchemaEpoch
	if epoch != tx.Base().SchemaEpoch {
		t.Fatal("symbols, records and degrees raised the schema epoch")
	}
	def := IndexDef{Name: "age", Scope: "P", Property: "age"}
	do(t, tx, func(s *engine.Stmt) error { return g.CreateIndex(ctx, s, def) })
	do(t, tx, func(s *engine.Stmt) error { return g.CreateIndex(ctx, s, def) })
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "w", Entity: EdgeEntity, Scope: "T", Property: "w", Unique: true})
	})
	if tx.State().SchemaEpoch != epoch+1 {
		t.Fatalf("epoch %d after two creates, base %d", tx.State().SchemaEpoch, epoch)
	}
	for _, other := range []IndexDef{
		{Name: "age", Scope: "P", Property: "age", Unique: true},
		{Name: "age", Scope: "Q", Property: "age"},
		{Name: "age", Scope: "P", Property: "unknown"},
		{Name: "age", Entity: EdgeEntity, Scope: "P", Property: "age"},
	} {
		if err := tx.Statement("t", func(s *engine.Stmt) error { return g.CreateIndex(ctx, s, other) }); errs.CodeOf(err) != errs.CodeIndexExistsDifferent {
			t.Fatalf("%+v: %v", other, err)
		}
	}
	if _, ok, _ := catalog.Lookup[catalog.PropertyID](ctx, tx, "unknown"); ok {
		t.Fatal("a refused declaration interned a name")
	}
	ix := lookupIndex(t, g, tx, "age")
	if ids := seekIDs(t, tx, ix, index.Query{Op: index.Equal, Value: value.Float(30)}); !slices.Equal(ids, []uint64{a}) {
		t.Fatalf("seek after backfill: %v", ids)
	}
	if ids := seekIDs(t, tx, lookupIndex(t, g, tx, "w"), index.Query{Op: index.Equal, Value: value.Int(2)}); !slices.Equal(ids, []uint64{e}) {
		t.Fatalf("edge index: %v", ids)
	}
	b := node(t, g, tx, []string{"P", "Q"}, props("age", value.Int(41)))
	if tx.State().SchemaEpoch != epoch+1 {
		t.Fatal("index entries and counters raised the schema epoch")
	}
	do(t, tx, func(s *engine.Stmt) error { return g.AddLabel(ctx, s, a+2, "P") })
	if ids := seekIDs(t, tx, ix, index.Query{Op: index.Range, Lo: value.Int(0), HasLo: true}); !slices.Equal(ids, []uint64{a, a + 2, b}) {
		t.Fatalf("after writes: %v", ids)
	}
	verify(t, g, tx)
	defs, err := g.ListIndexes(ctx, tx)
	if err != nil || len(defs) != 2 || defs[0] != (IndexDef{Name: "age", Scope: "P", Property: "age", State: index.Online}) ||
		defs[1] != (IndexDef{Name: "w", Entity: EdgeEntity, Scope: "T", Property: "w", Unique: true, State: index.Online}) {
		t.Fatalf("ListIndexes: %+v, %v", defs, err)
	}
	do(t, tx, func(s *engine.Stmt) error { return g.DropIndex(ctx, s, "age") })
	if err := tx.Statement("t", func(s *engine.Stmt) error { return g.DropIndex(ctx, s, "age") }); errs.CodeOf(err) != errs.CodeIndexNotFound {
		t.Fatalf("a second drop: %v", err)
	}
	verify(t, g, tx)
	for _, bad := range []IndexDef{{Name: "", Scope: "P", Property: "p"}, {Name: "x", Scope: "P", Property: "a\x00"}, {Name: "x", Entity: 2, Scope: "P", Property: "p"}} {
		if err := tx.Statement("t", func(s *engine.Stmt) error { return g.CreateIndex(ctx, s, bad) }); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// IDX-04: Int 1 and Float 1.0 collide in a unique index, Null and absence
// never do; a violation undoes only its own operation; a backfill over
// duplicates fails the creation.
func TestUniqueIndex(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "k", Scope: "U", Property: "k", Unique: true})
	})
	a := node(t, g, tx, []string{"U"}, props("k", value.Int(1)))
	node(t, g, tx, []string{"U"}, props("k", value.Null()))
	node(t, g, tx, []string{"U"}, props("k", value.Null()))
	node(t, g, tx, []string{"U"}, nil)
	node(t, g, tx, []string{"V"}, props("k", value.Int(1))) // out of scope
	before := dump(t, g, tx)
	for name, fn := range map[string]func(s *engine.Stmt) error{
		"create": func(s *engine.Stmt) error {
			_, err := g.CreateNode(ctx, s, []string{"U"}, props("k", value.Float(1)))
			return err
		},
		"set": func(s *engine.Stmt) error {
			return g.SetProperty(ctx, s, NodeEntity, a+1, "k", value.Float(1))
		},
		"label": func(s *engine.Stmt) error { return g.AddLabel(ctx, s, a+4, "U") },
	} {
		if err := tx.Statement("t", fn); errs.CodeOf(err) != errs.CodeUniqueViolation {
			t.Fatalf("%s: %v", name, err)
		}
		if after := dump(t, g, tx); after != before {
			t.Fatalf("%s left\n%s", name, after)
		}
	}
	// The violating write in a bigger statement undoes the whole statement
	// and nothing before it.
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, a, "k", value.Int(2)) })
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, a+1, "k", value.Float(1)) })
	verify(t, g, tx)
	// A backfill over duplicates fails the creation.
	node(t, g, tx, []string{"D"}, props("d", value.List(value.Int(1))))
	node(t, g, tx, []string{"D"}, props("d", value.List(value.Float(1))))
	epoch := tx.State().SchemaEpoch
	err := tx.Statement("t", func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "d", Scope: "D", Property: "d", Unique: true})
	})
	if errs.CodeOf(err) != errs.CodeUniqueViolation || tx.State().SchemaEpoch != epoch {
		t.Fatalf("a unique backfill over duplicates: %v", err)
	}
	if defs, _ := g.ListIndexes(ctx, tx); len(defs) != 1 {
		t.Fatalf("the failed creation left %d indexes", len(defs))
	}
	verify(t, g, tx)
}

// Ordered indexes refuse NaN and infinities on write and on backfill;
// inside a list, NaN is a value like any other.
func TestNonFinite(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	n := node(t, g, tx, []string{"F"}, props("x", value.Float(math.Inf(1))))
	err := tx.Statement("t", func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "x", Scope: "F", Property: "x"})
	})
	if errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("a backfill over +Inf: %v", err)
	}
	do(t, tx, func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, n, "x", value.Float(1)) })
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "x", Scope: "F", Property: "x"})
	})
	for _, f := range []float64{math.NaN(), math.Inf(-1)} {
		if err := tx.Statement("t", func(s *engine.Stmt) error { return g.SetProperty(ctx, s, NodeEntity, n, "x", value.Float(f)) }); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("%v on an indexed property: %v", f, err)
		}
	}
	do(t, tx, func(s *engine.Stmt) error {
		return g.SetProperty(ctx, s, NodeEntity, n, "x", value.List(value.Float(math.NaN())))
	})
	verify(t, g, tx)
}

// A creation beyond MaxTxBytes fails with LIMIT_EXCEEDED, its limit and
// a message that points to BuildIndex, and leaves nothing.
func TestCreateIndexTooLarge(t *testing.T) {
	o := options()
	o.MaxTxBytes = 1 << 20
	db := openDB(t, vfs.NewMemFS(), o)
	defer db.Close()
	g := newGraph()
	r := rand.New(rand.NewPCG(1, 1))
	for range 8 { // 2400 entries of 800 random bytes, which prefix compression cannot shrink: about twice MaxTxBytes
		tx := begin(t, db)
		for range 300 {
			b := make([]byte, 800)
			for i := range b {
				b[i] = byte('a' + r.IntN(26))
			}
			node(t, g, tx, []string{"L"}, props("s", value.String(string(b))))
		}
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tx := begin(t, db)
	defer tx.Rollback()
	err := tx.Statement("t", func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "s", Scope: "L", Property: "s"})
	})
	var e *errs.Error
	if errs.CodeOf(err) != errs.CodeLimitExceeded || !errors.As(err, &e) || e.Details["limit"] != o.MaxTxBytes || !strings.Contains(e.Message, "BuildIndex") {
		t.Fatalf("a creation beyond MaxTxBytes: %v", err)
	}
	if defs, _ := g.ListIndexes(ctx, tx); len(defs) != 0 {
		t.Fatal("the failed creation left its declaration")
	}
}

// IDX-05: texts longer than IndexKeyPrefixBytes, sharing their first
// 896 bytes, answer equality, ranges, prefixes and ordered walks exactly
// as a full scan does.
func TestLongTexts(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	defer tx.Rollback()
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "t", Scope: "T", Property: "t"})
	})
	r := rand.New(rand.NewPCG(5, 5))
	shared := strings.Repeat("s", 890) + "\x00\x00\x00" // the cut meets escapes
	vals := map[uint64]string{}
	for range 200 {
		var v string
		switch r.IntN(4) {
		case 0:
			v = shared[:r.IntN(len(shared))]
		case 1:
			v = shared + strings.Repeat("x", r.IntN(3))
		default:
			v = shared + string(rune('a'+r.IntN(3))) + strings.Repeat("y", r.IntN(200))
		}
		vals[node(t, g, tx, []string{"T"}, props("t", value.String(v)))] = v
	}
	ix := lookupIndex(t, g, tx, "t")
	pick := func() string {
		for _, v := range vals {
			return v
		}
		return ""
	}
	for range 200 {
		var q index.Query
		var keep func(string) bool
		switch r.IntN(3) {
		case 0:
			v := pick()
			if r.IntN(3) == 0 {
				v += "z"
			}
			q, keep = index.Query{Op: index.Equal, Value: value.String(v)}, func(s string) bool { return s == v }
		case 1:
			lo, hi := pick(), pick()
			q = index.Query{Op: index.Range, Lo: value.String(lo), Hi: value.String(hi), HasLo: true, HasHi: true, LoIncl: r.IntN(2) == 0, HiIncl: r.IntN(2) == 0}
			keep = func(s string) bool {
				return (s > lo || q.LoIncl && s == lo) && (s < hi || q.HiIncl && s == hi)
			}
		default:
			p := pick()
			p = p[:r.IntN(len(p)+1)]
			q, keep = index.Query{Op: index.Prefix, Value: value.String(p)}, func(s string) bool { return strings.HasPrefix(s, p) }
		}
		q.Desc = r.IntN(2) == 0
		var want []uint64
		for id, v := range vals {
			if keep(v) {
				want = append(want, id)
			}
		}
		slices.SortFunc(want, func(a, b uint64) int {
			c := strings.Compare(vals[a], vals[b])
			if c == 0 {
				c = int(a) - int(b)
			}
			if q.Desc {
				c = -c
			}
			return c
		})
		if got := seekIDs(t, tx, ix, q); !slices.Equal(got, want) {
			t.Fatalf("%+v: %d hits, want %d", q, len(got), len(want))
		}
	}
	verify(t, g, tx)
}

// Verify finds each kind of damage to an index.
func TestVerifyFindsIndexDamage(t *testing.T) {
	db := openDB(t, vfs.NewMemFS(), options())
	defer db.Close()
	g := newGraph()
	tx := begin(t, db)
	do(t, tx, func(s *engine.Stmt) error {
		return g.CreateIndex(ctx, s, IndexDef{Name: "i", Scope: "A", Property: "p"})
	})
	a := node(t, g, tx, []string{"A"}, props("p", value.Int(1)))
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := db.BeginRead(ctx)
	ix := lookupIndex(t, g, r, "i")
	r.Close()
	good, _ := index.EntryKey(ix, a, value.Int(1))
	other, _ := index.EntryKey(ix, a+1, value.Int(1))
	stray := append(append([]byte{}, good[:7]...), append([]byte{77}, good[8:]...)...)
	for name, damage := range map[string]func(s *engine.Stmt) error{
		"extra entry":      func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotNodeProperty, other, nil) },
		"missing entry":    func(s *engine.Stmt) error { _, err := s.Delete(ctx, catalog.SlotNodeProperty, good); return err },
		"entry in edges":   func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotEdgeProperty, good, nil) },
		"undeclared index": func(s *engine.Stmt) error { return s.Put(ctx, catalog.SlotNodeProperty, stray, nil) },
		"wrong value": func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotNodes, catalog.PropertyKey(a, ix.Property), []byte{0x02, 9, 0, 0, 0, 0, 0, 0, 0})
		},
		"counter": func(s *engine.Stmt) error {
			return s.Put(ctx, catalog.SlotStats, codec.AppendKeyUint64(codec.AppendKeyUint8(nil, 1), ix.ID), []byte{5, 0, 0, 0, 0, 0, 0, 0})
		},
	} {
		tx := begin(t, db)
		do(t, tx, damage)
		if err := g.Verify(ctx, tx); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("%s: Verify says %v", name, err)
		}
		tx.Rollback()
	}
}
