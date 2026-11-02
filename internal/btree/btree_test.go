package btree_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/alloc"
	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/btree/btreetest"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

var ctx = context.Background()

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

// env is a store with one open transaction and one tree in root slot 0.
type env struct {
	t     *testing.T
	fs    vfs.FS
	store *btreetest.Store
	tx    *btreetest.Txn
	alloc *alloc.Allocator
	tree  *btree.Tree
	pins  *cache.Pins
	model map[string][]byte
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, fs: vfs.NewMemFS(), pins: cache.NewPins(0), model: map[string][]byte{}}
	var err error
	if e.store, err = btreetest.Create(ctx, e.fs, btreetest.Options{CacheBytes: 256 * cache.EntryCost}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.store.Close() })
	e.begin()
	return e
}

func (e *env) begin() {
	e.t.Helper()
	var err error
	if e.tx, err = e.store.Begin(); err != nil {
		e.t.Fatal(err)
	}
	e.alloc = alloc.New(e.tx)
	e.tree = btree.New(e.tx, e.alloc, e.tx.Root(0))
}

// commit publishes the tree and opens the next transaction.
func (e *env) commit() {
	e.t.Helper()
	e.tx.SetRoot(0, e.tree.Root())
	if err := e.tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.begin()
}

// reopen commits, closes and reopens the store.
func (e *env) reopen() {
	e.t.Helper()
	e.commit()
	e.tx.Rollback()
	if err := e.store.Close(); err != nil {
		e.t.Fatal(err)
	}
	var err error
	if e.store, err = btreetest.Open(ctx, e.fs, btreetest.Options{CacheBytes: 256 * cache.EntryCost}); err != nil {
		e.t.Fatal(err)
	}
	e.begin()
}

func (e *env) put(key, value []byte) {
	e.t.Helper()
	if _, err := e.tree.Put(ctx, e.pins, key, value); err != nil {
		e.t.Fatalf("Put(%q): %v", short(key), err)
	}
	e.model[string(key)] = bytes.Clone(value)
	e.noPins()
}

func (e *env) del(key []byte) {
	e.t.Helper()
	_, want := e.model[string(key)]
	found, _, err := e.tree.Delete(ctx, e.pins, key)
	if err != nil || found != want {
		e.t.Fatalf("Delete(%q) = %v, %v; want %v", short(key), found, err, want)
	}
	delete(e.model, string(key))
	e.noPins()
}

func (e *env) noPins() {
	e.t.Helper()
	if e.pins.Used() != 0 {
		e.t.Fatalf("%d bytes still pinned", e.pins.Used())
	}
}

func short(k []byte) string {
	if len(k) > 24 {
		return fmt.Sprintf("%s…(%d bytes)", k[:24], len(k))
	}
	return string(k)
}

// check compares the tree with the model, through the transaction and,
// after a commit, through the published state, and verifies it.
func (e *env) check() btree.Report {
	e.t.Helper()
	rep, err := btree.Verify(ctx, e.tx, e.pins, e.tree.Root())
	if err != nil {
		e.t.Fatalf("Verify: %v", err)
	}
	if rep.Keys != len(e.model) {
		e.t.Fatalf("the tree holds %d keys, the model %d", rep.Keys, len(e.model))
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range e.model {
			if !yield(k) {
				return
			}
		}
	})
	i := 0
	err = e.tree.Range(ctx, e.pins, nil, nil, func(k, v []byte) error {
		if i >= len(keys) || string(k) != keys[i] {
			return fmt.Errorf("key %d is %q", i, short(k))
		}
		if !bytes.Equal(v, e.model[keys[i]]) {
			return fmt.Errorf("value of %q differs", short(k))
		}
		i++
		return nil
	})
	if err != nil || i != len(keys) {
		e.t.Fatalf("Range: %v after %d of %d keys", err, i, len(keys))
	}
	e.noPins()
	return rep
}

// seeks compares SeekGE, SeekGT and a bounded Range (a prefix scan) with
// the model, around key.
func (e *env) seeks(key []byte) {
	e.t.Helper()
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range e.model {
			if !yield(k) {
				return
			}
		}
	})
	ge, _ := slices.BinarySearch(keys, string(key))
	gt := ge
	if gt < len(keys) && keys[gt] == string(key) {
		gt++
	}
	for _, c := range []struct {
		after bool
		want  int
	}{{false, ge}, {true, gt}} {
		seek := e.tree.SeekGE
		if c.after {
			seek = e.tree.SeekGT
		}
		cur, err := seek(ctx, e.pins, key)
		if err != nil {
			e.t.Fatal(err)
		}
		if valid := c.want < len(keys); cur.Valid() != valid || valid && string(cur.Key()) != keys[c.want] {
			e.t.Fatalf("seek (after=%v) %q: valid %v at %q, want index %d of %d", c.after, short(key), cur.Valid(), short(cur.Key()), c.want, len(keys))
		}
	}
	hi := append(bytes.Clone(key), 0xff) // keys with key as a prefix, roughly
	want := 0
	for _, k := range keys[ge:] {
		if k >= string(hi) {
			break
		}
		want++
	}
	got := 0
	if err := e.tree.Range(ctx, e.pins, key, hi, func(k, v []byte) error { got++; return nil }); err != nil || got != want {
		e.t.Fatalf("Range [%q, +0xff): %d keys, %v; want %d", short(key), got, err, want)
	}
	// Reverse walks: [key, hi) and everything below key, against the model.
	for _, w := range []struct{ lo, hi []byte }{{key, hi}, {nil, key}, {nil, nil}} {
		var got []string
		err := e.tree.RangeReverse(ctx, e.pins, w.lo, w.hi, func(k, v []byte) error {
			if !bytes.Equal(v, []byte(e.model[string(k)])) {
				e.t.Fatalf("RangeReverse: value of %q differs", short(k))
			}
			got = append(got, string(k))
			return nil
		})
		var want []string
		for _, k := range slices.Backward(keys) {
			if (w.lo == nil || k >= string(w.lo)) && (w.hi == nil || k < string(w.hi)) {
				want = append(want, k)
			}
		}
		if err != nil || !slices.Equal(got, want) {
			e.t.Fatalf("RangeReverse [%q, %q): %d keys, %v; want %d", short(w.lo), short(w.hi), len(got), err, len(want))
		}
	}
	e.noPins()
}

func (e *env) get(key []byte) {
	e.t.Helper()
	v, found, err := e.tree.Get(ctx, e.pins, key)
	want, ok := e.model[string(key)]
	if err != nil || found != ok || !bytes.Equal(v, want) {
		e.t.Fatalf("Get(%q) = %d bytes, %v, %v; want %d bytes, %v", short(key), len(v), found, err, len(want), ok)
	}
	e.noPins()
}

// randomKey draws keys with long common prefixes, 0 to 1024 bytes.
func randomKey(r *rand.Rand) []byte {
	prefixes := []string{"", "a", "user/", strings.Repeat("p", 200), strings.Repeat("q", 1000)}
	k := []byte(prefixes[r.IntN(len(prefixes))])
	k = fmt.Appendf(k, "%x", r.IntN(3000))
	if r.IntN(10) == 0 {
		k = append(k, bytes.Repeat([]byte{byte('a' + r.IntN(26))}, r.IntN(300))...)
	}
	return k[:min(len(k), page.MaxKeyLen)]
}

func randomValue(r *rand.Rand) []byte {
	var n int
	switch x := r.IntN(100); {
	case x < 60:
		n = r.IntN(50)
	case x < 90:
		n = r.IntN(1400)
	case x < 99:
		n = r.IntN(20000)
	default:
		n = r.IntN(300000)
	}
	v := make([]byte, n)
	for i := range v {
		v[i] = byte(r.Uint32())
	}
	return v
}

// BT-01 and BT-03: seeded sequences against an ordered map, with splits,
// merges, overflow values, long common prefixes, commits and reopening.
func TestAgainstModel(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			e := newEnv(t)
			r := rand.New(rand.NewPCG(seed, seed))
			for step := 0; step < 3000; step++ {
				k := randomKey(r)
				switch x := r.IntN(20); {
				case x < 12:
					e.put(k, randomValue(r))
				case x < 17:
					e.del(k)
				case x < 19:
					e.get(k)
				default:
					e.seeks(k)
				}
				if step%500 == 499 {
					rep := e.check()
					if rep.Internals > 1 && rep.MinFanout < 2 {
						t.Fatalf("internal fanout %d", rep.MinFanout)
					}
					if step%1000 == 999 {
						e.reopen()
					} else {
						e.commit()
					}
					e.check()
					e.structural()
				}
			}
			for k := range e.model { // empty the tree again
				e.del([]byte(k))
			}
			e.check()
			if e.tree.Root() != 0 {
				t.Fatalf("an empty tree has root %d", e.tree.Root())
			}
			if pc, free := e.tx.PageCount(), e.tx.FreePageCount(); free != pc-3 {
				t.Fatalf("empty tree: %d pages, %d free", pc, free)
			}
		})
	}
}
