package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

var ctx = context.Background()

var errFail = errors.New("the operation failed")

var testSchema = Schema{Name: "test", Version: 2, Fields: []Field{
	{Name: "name", Kind: value.KindString, Required: true},
	{Name: "count", Kind: value.KindInt},
	{Name: "state", Kind: value.KindInt, Required: true, Since: 2},
}}

func TestRecordRoundTrip(t *testing.T) {
	in := map[string]value.Value{"name": value.String("a"), "count": value.Int(3), "state": value.Int(1)}
	b, err := testSchema.Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(b) != 2 {
		t.Fatalf("version %d", binary.LittleEndian.Uint16(b))
	}
	out, err := testSchema.Decode(b)
	if err != nil || len(out) != 3 || !value.Identical(out["count"], value.Int(3)) {
		t.Fatalf("%v, %v", out, err)
	}
}

func TestRecordEncodeRejects(t *testing.T) {
	for _, in := range []map[string]value.Value{
		{"name": value.String("a"), "state": value.Int(1), "extra": value.Int(1)},
		{"name": value.Int(1), "state": value.Int(1)},
		{"state": value.Int(1)},
	} {
		if _, err := testSchema.Encode(in); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("%v: %v", in, err)
		}
	}
}

// raw builds a record by hand.
func raw(ver uint16, m map[string]value.Value) []byte {
	b, _ := codec.AppendValue(binary.LittleEndian.AppendUint16(nil, ver), value.MapOf(m), value.DefaultLimits())
	return b
}

func TestRecordDecodeRejects(t *testing.T) {
	cases := []struct {
		b    []byte
		code errs.Code
	}{
		{raw(3, map[string]value.Value{"name": value.String("a")}), errs.CodeUnsupportedFormat},
		{raw(0, map[string]value.Value{"name": value.String("a")}), errs.CodeCorrupt},
		{raw(2, map[string]value.Value{"name": value.String("a"), "state": value.Int(1), "x": value.Int(1)}), errs.CodeCorrupt},
		{raw(2, map[string]value.Value{"name": value.String("a")}), errs.CodeCorrupt},                        // state is required in 2
		{raw(1, map[string]value.Value{"name": value.String("a"), "state": value.Int(1)}), errs.CodeCorrupt}, // state is unknown in 1
		{raw(2, map[string]value.Value{"name": value.Bool(true), "state": value.Int(1)}), errs.CodeCorrupt},
		{[]byte{1}, errs.CodeCorrupt},
		{append(binary.LittleEndian.AppendUint16(nil, 1), 0xFF), errs.CodeCorrupt},
	}
	for i, c := range cases {
		if _, err := testSchema.Decode(c.b); errs.CodeOf(err) != c.code {
			t.Fatalf("case %d: %v, want %v", i, err, c.code)
		}
	}
	// Version 1 without the field added in 2 is valid.
	if _, err := testSchema.Decode(raw(1, map[string]value.Value{"name": value.String("a")})); err != nil {
		t.Fatal(err)
	}
}

func openDB(t *testing.T, fs vfs.FS) *engine.DB {
	t.Helper()
	o := engine.DefaultOptions()
	o.Create = true
	db, err := engine.Open(ctx, fs, "db", o)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// intern interns name in a statement of its own.
func intern[T ID](t *testing.T, tx *engine.WriteTx, name string, limit uint32) (T, error) {
	t.Helper()
	var id T
	err := tx.Statement("t", func(s *engine.Stmt) error {
		var err error
		id, err = Intern[T](ctx, s, name, limit)
		return err
	})
	return id, err
}

// Namespaces are independent: the same name gets its own identifier in
// each, counted from 1; names and identifiers map both ways; interning
// again returns the same identifier.
func TestInternNamespaces(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	l, _ := intern[LabelID](t, tx, "Person", DefaultMaxSymbols)
	p, _ := intern[PropertyID](t, tx, "Person", DefaultMaxSymbols)
	e, _ := intern[EdgeTypeID](t, tx, "KNOWS", DefaultMaxSymbols)
	l2, _ := intern[LabelID](t, tx, "City", DefaultMaxSymbols)
	again, _ := intern[LabelID](t, tx, "Person", DefaultMaxSymbols)
	if l != 1 || p != 1 || e != 1 || l2 != 2 || again != 1 {
		t.Fatalf("ids %d %d %d %d %d", l, p, e, l2, again)
	}
	if n, ok, _ := Name(ctx, tx, l2); !ok || n != "City" {
		t.Fatalf("Name(2) = %q, %v", n, ok)
	}
	if _, ok, _ := Lookup[EdgeTypeID](ctx, tx, "Person"); ok {
		t.Fatal("a label is found as an edge type")
	}
	if tx.State().SymbolHWM != 2 {
		t.Fatalf("aggregate watermark %d", tx.State().SymbolHWM)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// A symbol interned by a statement that fails is undone with its
// watermark: the next intern takes the same identifier, and the first
// name is not found.
func TestInternUndoneWithStatement(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	err := tx.Statement("t", func(s *engine.Stmt) error {
		if _, err := Intern[LabelID](ctx, s, "X", DefaultMaxSymbols); err != nil {
			return err
		}
		return errFail
	})
	if err != errFail {
		t.Fatal(err)
	}
	y, _ := intern[LabelID](t, tx, "Y", DefaultMaxSymbols)
	if _, ok, _ := Lookup[LabelID](ctx, tx, "X"); ok || y != 1 {
		t.Fatalf("X found %v, Y = %d", ok, y)
	}
	if n, _, _ := Name(ctx, tx, LabelID(1)); n != "Y" {
		t.Fatalf("identifier 1 names %q", n)
	}
	if tx.State().SymbolHWM != 1 {
		t.Fatalf("aggregate watermark %d", tx.State().SymbolHWM)
	}
}

func TestNames(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	if _, err := intern[PropertyID](t, tx, strings.Repeat("é", 127)+"a", DefaultMaxSymbols); err != nil {
		t.Fatalf("255 bytes: %v", err)
	}
	long := strings.Repeat("a", 256)
	_, err := intern[PropertyID](t, tx, long, DefaultMaxSymbols)
	var e *errs.Error
	if errs.CodeOf(err) != errs.CodeLimitExceeded || !errors.As(err, &e) || e.Details["bytes"] != 256 || e.Details["limit"] != 255 {
		t.Fatalf("256 bytes: %v", err)
	}
	for _, name := range []string{"", "a\x00b", "\xff"} {
		if _, err := intern[PropertyID](t, tx, name, DefaultMaxSymbols); errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Fatalf("%q: %v", name, err)
		}
		if _, ok, err := Lookup[PropertyID](ctx, tx, name); ok || err != nil {
			t.Fatalf("lookup of %q: %v %v", name, ok, err)
		}
	}
	for _, name := range []string{long, strings.Repeat("a", 2000)} {
		if _, ok, err := Lookup[PropertyID](ctx, tx, name); ok || err != nil {
			t.Fatalf("a name of %d bytes cannot exist and is simply not found: %v", len(name), err)
		}
	}
}

// Past MaxSymbolsPerNamespace, Intern fails with LIMIT_EXCEEDED; other
// namespaces and existing names are not affected.
func TestNamespaceLimit(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	for _, n := range []string{"a", "b", "c"} {
		if _, err := intern[LabelID](t, tx, n, 3); err != nil {
			t.Fatal(err)
		}
	}
	_, err := intern[LabelID](t, tx, "d", 3)
	var e *errs.Error
	if errs.CodeOf(err) != errs.CodeLimitExceeded || !errors.As(err, &e) || e.Details["limit"] != 3 {
		t.Fatalf("the fourth label: %v", err)
	}
	if id, err := intern[LabelID](t, tx, "b", 3); err != nil || id != 2 {
		t.Fatalf("an existing name at the limit: %d, %v", id, err)
	}
	if _, err := intern[PropertyID](t, tx, "d", 3); err != nil {
		t.Fatal(err)
	}
	if tx.State().SymbolHWM != 3 {
		t.Fatalf("a smaller identifier of another namespace lowered the aggregate watermark to %d", tx.State().SymbolHWM)
	}
}

// Symbols survive commit, crash and recovery; a lookup never writes.
func TestSymbolsRecover(t *testing.T) {
	fs := vfs.NewMemFS()
	db := openDB(t, fs)
	tx, _ := db.BeginWrite(ctx)
	intern[LabelID](t, tx, "A", DefaultMaxSymbols)
	intern[EdgeTypeID](t, tx, "R", DefaultMaxSymbols)
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, _ = db.BeginWrite(ctx)
	if _, ok, _ := Lookup[LabelID](ctx, tx, "Unknown"); ok {
		t.Fatal("unknown found")
	}
	r, err := tx.Commit(ctx)
	if err != nil || r.CommitID != 1 {
		t.Fatalf("a transaction of lookups committed %d: %v", r.CommitID, err)
	}
	fs.Crash(vfs.CrashOptions{})
	db.Close()
	db = openDB(t, fs)
	defer db.Close()
	rt, _ := db.BeginRead(ctx)
	defer rt.Close()
	if id, ok, _ := Lookup[EdgeTypeID](ctx, rt, "R"); !ok || id != 1 {
		t.Fatalf("R after recovery: %d %v", id, ok)
	}
	if rt.State().SymbolHWM != 1 {
		t.Fatalf("aggregate watermark %d", rt.State().SymbolHWM)
	}
	tx, _ = db.BeginWrite(ctx)
	defer tx.Rollback()
	if id, _ := intern[LabelID](t, tx, "B", DefaultMaxSymbols); id != 2 {
		t.Fatalf("the watermark survives recovery: B = %d", id)
	}
}

// The cache keeps only what a read transaction saw: a symbol interned by
// a write transaction that rolls back never enters it.
func TestCacheOnlyCommitted(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	c := NewCache(100)
	tx, _ := db.BeginWrite(ctx)
	intern[LabelID](t, tx, "Ghost", DefaultMaxSymbols)
	if id, ok, _ := CachedLookup[LabelID](ctx, c, tx, "Ghost"); !ok || id != 1 {
		t.Fatal("the writer sees its own symbol")
	}
	tx.Rollback()
	tx, _ = db.BeginWrite(ctx)
	if _, ok, _ := CachedLookup[LabelID](ctx, c, tx, "Ghost"); ok {
		t.Fatal("a rolled back symbol came from the cache")
	}
	intern[LabelID](t, tx, "Real", DefaultMaxSymbols)
	tx.Commit(ctx)
	rt, _ := db.BeginRead(ctx)
	if _, ok, _ := CachedLookup[LabelID](ctx, c, rt, "Real"); !ok {
		t.Fatal("Real not found")
	}
	defer rt.Close()
	if n, ok, _ := CachedName(ctx, c, rt, LabelID(1)); !ok || n != "Real" {
		t.Fatalf("Name through the cache: %q %v", n, ok)
	}
}

// A reader of a snapshot older than the one that filled an entry does not
// get it from the cache: it reads its own trees.
func TestCacheRespectsSnapshot(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	c := NewCache(100)
	old, _ := db.BeginRead(ctx)
	defer old.Close()
	tx, _ := db.BeginWrite(ctx)
	intern[LabelID](t, tx, "Later", DefaultMaxSymbols)
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now, _ := db.BeginRead(ctx)
	defer now.Close()
	if _, ok, _ := CachedLookup[LabelID](ctx, c, now, "Later"); !ok {
		t.Fatal("the new reader does not find Later")
	}
	if _, ok, _ := CachedLookup[LabelID](ctx, c, old, "Later"); ok {
		t.Fatal("an older snapshot got a later symbol from the cache")
	}
	if _, ok, _ := CachedName(ctx, c, old, LabelID(1)); ok {
		t.Fatal("an older snapshot got a later name from the cache")
	}
	tx, _ = db.BeginWrite(ctx)
	defer tx.Rollback()
	if id, ok, _ := CachedLookup[LabelID](ctx, c, tx, "Later"); !ok || id != 1 {
		t.Fatal("the writer, newer than the entry, misses it")
	}
	intern[LabelID](t, tx, "Second", DefaultMaxSymbols)
	tx.Commit(ctx)
	last, _ := db.BeginRead(ctx)
	defer last.Close()
	// A full cache starts over instead of refusing new entries.
	small := NewCache(1)
	for _, name := range []string{"Later", "Second"} {
		CachedLookup[LabelID](ctx, small, last, name)
	}
	if _, ok := small.names[cacheKey{KindLabel, "Second"}]; !ok || len(small.names) != 1 {
		t.Fatalf("a full cache kept %v and refused Second", small.names)
	}
}

// Intern refuses to hand out an identifier when the watermark of its
// namespace is behind the symbols or ahead of the aggregate, and a limit
// of zero.
func TestInternChecksWatermark(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	defer tx.Rollback()
	intern[LabelID](t, tx, "A", DefaultMaxSymbols)
	tx.Statement("t", func(s *engine.Stmt) error {
		_, err := s.Delete(ctx, SlotCatalog, watermarkKey(KindLabel))
		return err
	})
	if _, err := intern[LabelID](t, tx, "B", DefaultMaxSymbols); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a lost watermark: %v", err)
	}
	rec, _ := symbolWatermark.Encode(map[string]value.Value{"hwm": value.Int(9)})
	tx.Put(ctx, SlotCatalog, watermarkKey(KindProperty), rec)
	if _, err := intern[PropertyID](t, tx, "p", DefaultMaxSymbols); errs.CodeOf(err) != errs.CodeCorrupt {
		t.Fatalf("a watermark above the aggregate: %v", err)
	}
	if _, err := intern[EdgeTypeID](t, tx, "R", 0); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("a limit of zero: %v", err)
	}
}

// Random interns over the three namespaces, in statements that fail at
// random and transactions that commit or roll back, match a model: every
// committed name keeps its identifier, identifiers are dense from 1, and
// reverse lookups agree.
func TestInternAgainstModel(t *testing.T) {
	db := openDB(t, vfs.NewMemFS())
	defer db.Close()
	type key struct {
		k    SymbolKind
		name string
	}
	committed := map[key]uint32{}
	r := rand.New(rand.NewPCG(3, 3))
	for round := 0; round < 40; round++ {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Fatal(err)
		}
		model := maps.Clone(committed)
		next := map[SymbolKind]uint32{}
		for k, id := range model {
			next[k.k] = max(next[k.k], id)
		}
		for range 20 {
			k := SymbolKind(1 + r.IntN(3))
			name := fmt.Sprintf("n%d", r.IntN(30))
			fail := r.IntN(4) == 0
			var id uint32
			err := tx.Statement("t", func(s *engine.Stmt) error {
				var err error
				switch k {
				case KindLabel:
					var v LabelID
					v, err = Intern[LabelID](ctx, s, name, DefaultMaxSymbols)
					id = uint32(v)
				case KindProperty:
					var v PropertyID
					v, err = Intern[PropertyID](ctx, s, name, DefaultMaxSymbols)
					id = uint32(v)
				default:
					var v EdgeTypeID
					v, err = Intern[EdgeTypeID](ctx, s, name, DefaultMaxSymbols)
					id = uint32(v)
				}
				if err == nil && fail {
					err = errFail
				}
				return err
			})
			if fail {
				if err != errFail {
					t.Fatal(err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			want, ok := model[key{k, name}]
			if !ok {
				next[k]++
				want = next[k]
				model[key{k, name}] = want
			}
			if id != want {
				t.Fatalf("round %d: %d %q got %d, want %d", round, k, name, id, want)
			}
		}
		if r.IntN(3) == 0 {
			tx.Rollback()
		} else {
			if _, err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			committed = model
		}
	}
	rt, _ := db.BeginRead(ctx)
	defer rt.Close()
	for k, id := range committed {
		got, ok, err := lookup(ctx, rt, k.k, k.name)
		if err != nil || !ok || got != id {
			t.Fatalf("%v: %d %v %v, want %d", k, got, ok, err, id)
		}
		v, ok, _ := rt.Get(ctx, SlotSymbolsReverse, reverseKey(k.k, id))
		if !ok || string(v) != k.name {
			t.Fatalf("reverse of %v: %q", k, v)
		}
	}
	n := 0
	rt.Range(ctx, SlotSymbolsForward, nil, nil, func(k, v []byte) error { n++; return nil })
	if n != len(committed) {
		t.Fatalf("%d forward entries, %d committed symbols", n, len(committed))
	}
}
