package btree_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/alloc"
	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/btree/btreetest"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/check"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// structural runs the global checker on the published state.
func (e *env) structural() {
	e.t.Helper()
	st := e.store.State()
	rep, err := check.Structural(ctx, e.store.Reader(), st.Roots, st.PageCount, st.FreePageCount)
	if err != nil || !rep.OK() {
		e.t.Fatalf("check: %v %v", err, rep.Err())
	}
}

// valueFor returns a value whose full cell with key has exactly size bytes.
func valueFor(key []byte, size int) []byte {
	for n := size; n >= 0; n-- {
		if page.FullCellSize(len(key), uint64(n)) == uint64(size) {
			return bytes.Repeat([]byte{'v'}, n)
		}
	}
	panic("no value length gives that cell size")
}

// leafOf returns the leaf page that holds key, decoded.
func (e *env) leafOf(key []byte) page.Node {
	e.t.Helper()
	n, err := btree.LeafOf(ctx, e.tx, e.pins, e.tree.Root(), key)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// BT-06: cells at the 1344-byte limit stay inline, one more byte goes out
// of the page, and pages full of the largest inline cells always split in
// two.
func TestInlineLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 60; i++ {
		key := []byte(fmt.Sprintf("key/%03d/%s", i, strings.Repeat("k", i*7)))
		size := page.MaxInlineCell
		if i%2 == 1 {
			size++
		}
		e.put(key, valueFor(key, size))
		v := e.leafOf(key).Value(func() int { i, _ := e.leafOf(key).Search(key); return i }())
		if inline := !v.Out(); inline != (size <= page.MaxInlineCell) {
			t.Fatalf("cell of %d bytes: inline = %v", size, inline)
		}
	}
	e.check()
	e.commit()
	e.structural()
}

// BT-06: the adversarial sequence of rule 1.2, full cells of 2000, 2064 and
// 2000 bytes, which once could not split in two.
func TestAdversarial2000(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 20; round++ {
		for j, size := range []int{2000, 2064, 2000} {
			key := []byte(fmt.Sprintf("%02d/%d/%s", round, j, strings.Repeat("x", 1000)))
			e.put(key, valueFor(key, size))
		}
	}
	e.check()
	e.commit()
	e.structural()
}

// BT-06 and R13-16: a key without the prefix of a page full of short
// suffixes. Measured with the page prefix, the page would seem to have
// room; the exact measure of each side finds the split.
func TestKeyWithoutThePrefix(t *testing.T) {
	e := newEnv(t)
	prefix := strings.Repeat("p", 1000)
	for i := 0; i < 160; i++ {
		e.put([]byte(fmt.Sprintf("%s%03d", prefix, i)), nil)
	}
	if rep := e.check(); rep.Leaves != 1 {
		t.Fatalf("%d leaves before the new key", rep.Leaves)
	}
	e.put([]byte("b"), bytes.Repeat([]byte{'v'}, 1300))
	if rep := e.check(); rep.Leaves != 2 || rep.Height != 2 {
		t.Fatalf("after the new key: %+v", rep)
	}
	e.commit()
	e.structural()
}

// R13-19: the insertion fits only with the prefix of the resulting set.
// Six cells kaaa..kfff with values of 3977 bytes in total take 4097 bytes
// without a prefix and exactly 4096 with "k".
func TestFitsOnlyWithTheNewPrefix(t *testing.T) {
	e := newEnv(t)
	keys := []string{"kaaa", "kbbb", "kccc", "kddd", "keee", "kfff"}
	cells := make([]page.LeafCell, len(keys))
	for i, k := range keys {
		n := 663
		if i == 5 {
			n = 662
		}
		cells[i] = page.LeafCell{Key: []byte(k), Value: bytes.Repeat([]byte{'v'}, n)}
	}
	if got := page.LeafPageSize(cells); got != page.Size {
		t.Fatalf("the six cells take %d bytes, want %d", got, page.Size)
	}
	for _, c := range cells {
		e.put(c.Key, c.Value)
	}
	if rep := e.check(); rep.Leaves != 1 || rep.Height != 1 {
		t.Fatalf("the six keys did not stay in one page: %+v", rep)
	}
	e.commit()
	e.structural()
}

// R13-16: a redistribution whose new separator does not fit in the parent
// is not made, and the node stays below 25%; with room, it is made.
func TestRedistributionThatWouldOverflowTheParent(t *testing.T) {
	for _, fullParent := range []bool{true, false} {
		t.Run(fmt.Sprint("full parent ", fullParent), func(t *testing.T) {
			e := newEnv(t)
			long := "m" + strings.Repeat("x", 1000)
			fill := 3
			if fullParent {
				fill = 12
			}
			// Leaves: L = a1, a2; R = four keys under a long prefix; then one
			// leaf per long separator that fills the parent.
			small := []byte("1234567890")
			mk := func(keys ...string) []page.LeafCell {
				var cs []page.LeafCell
				for _, k := range keys {
					v := small
					if strings.HasPrefix(k, "m") {
						v = bytes.Repeat([]byte{'r'}, 300)
					}
					cs = append(cs, page.LeafCell{Key: []byte(k), Value: v})
				}
				return cs
			}
			leaves := [][]page.LeafCell{mk("a1", "a2"), mk(long+"1", long+"2", long+"3", long+"4")}
			var seps []string
			seps = append(seps, "m")
			for i := 0; i < fill; i++ {
				k := fmt.Sprintf("z%s%02d", strings.Repeat("s", 300), i)
				seps = append(seps, k)
				leaves = append(leaves, mk(k))
			}
			ids := make([]page.ID, len(leaves))
			for i := range ids {
				id, err := e.alloc.Alloc(ctx, 0)
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
			}
			for i, cells := range leaves {
				next := page.ID(0)
				if i+1 < len(ids) {
					next = ids[i+1]
				}
				for _, c := range cells {
					e.model[string(c.Key)] = c.Value
				}
				if err := e.tree.StoreLeaf(ctx, ids[i], cells, next); err != nil {
					t.Fatal(err)
				}
			}
			root, err := e.alloc.Alloc(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			var top []page.InternalCell
			for i, s := range seps {
				top = append(top, page.InternalCell{Key: []byte(s), Right: ids[i+1]})
			}
			if err := e.tree.StoreInternal(ctx, root, 1, ids[0], top); err != nil {
				t.Fatal(err)
			}
			e.tree.SetRoot(root)
			e.check()
			grown := page.InternalPageSize(top) + len(long) + 1 - 1 // the separator "m" becomes 1002 bytes
			if fullParent != (grown > page.Size) {
				t.Fatalf("parent of %d bytes would grow to %d", page.InternalPageSize(top), grown)
			}

			e.del([]byte("a1")) // L is now below 25%
			e.check()
			l := e.leafOf([]byte("a2"))
			if fullParent && l.Len() != 1 {
				t.Fatalf("a redistribution that overflows the parent was made: L has %d keys", l.Len())
			}
			if !fullParent && l.Len() != 2 {
				t.Fatalf("the redistribution was not made: L has %d keys", l.Len())
			}
			e.commit()
			e.structural()
		})
	}
}

// Cursors are invalidated by changes to their own tree and by savepoint
// restores only, and a restore never lets an old counter come back (ABA).
func TestCursorInvalidation(t *testing.T) {
	e := newEnv(t)
	other := btree.New(e.tx, e.alloc, 0)
	for i := 0; i < 50; i++ {
		e.put([]byte(fmt.Sprintf("k%02d", i)), []byte("v"))
	}
	c, err := e.tree.First(ctx, e.pins)
	if err != nil || !c.Valid() || string(c.Key()) != "k00" {
		t.Fatalf("First: %v", err)
	}
	if _, err := other.Put(ctx, e.pins, []byte("x"), []byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := c.Next(ctx); err != nil || string(c.Key()) != "k01" {
		t.Fatalf("a change to another tree invalidated the cursor: %v", err)
	}
	e.put([]byte("k99"), []byte("v"))
	if err := c.Next(ctx); !isCode(err, errs.CodeInvalidArgument) || errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("Next after a change of its tree: %v", err)
	}
	if _, err := c.Value(ctx); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("Value after a change of its tree: %v", err)
	}

	// A savepoint restore invalidates, even with no change to the tree.
	c, _ = e.tree.First(ctx, e.pins)
	sp := e.tx.Savepoint()
	if err := e.tx.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	if err := c.Next(ctx); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("Next after a restore: %v", err)
	}

	// ABA: a cursor taken before a change, a change, then a restore to
	// the state before the change; the counters do not come back.
	root := e.tree.Root()
	sp = e.tx.Savepoint()
	c, _ = e.tree.First(ctx, e.pins)
	e.put([]byte("k98"), []byte("v"))
	if err := e.tx.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	e.tree.Reset(root)
	delete(e.model, "k98")
	if err := c.Next(ctx); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("Next after change and restore: %v", err)
	}
	e.check()
	if c, err = e.tree.SeekGT(ctx, e.pins, []byte("k10")); err != nil || string(c.Key()) != "k11" {
		t.Fatalf("SeekGT after the restore: %v", err)
	}

	// A reverse walk whose fn changes its tree fails after that call.
	n := 0
	err = e.tree.RangeReverse(ctx, e.pins, nil, nil, func(k, _ []byte) error {
		if n++; n == 2 {
			e.put([]byte("k97"), []byte("v"))
		}
		return nil
	})
	if !isCode(err, errs.CodeInvalidArgument) || n != 2 {
		t.Fatalf("RangeReverse changing its tree: %d calls, %v", n, err)
	}
	stop := errors.New("stop")
	n = 0
	err = e.tree.RangeReverse(ctx, e.pins, []byte("k10"), nil, func(k, _ []byte) error {
		if n++; string(k) == "k47" {
			return stop
		}
		return nil
	})
	if err != stop || n != 5 { // k99, k97, k49, k48, k47
		t.Fatalf("RangeReverse stopped by fn: %d calls, %v", n, err)
	}
}

// BT-04: values from 1 KiB to 16 MiB round-trip; a value above
// LargeValueBypassBytes is read without evicting the hot set.
func TestLargeValues(t *testing.T) {
	e := newEnv(t)
	sizes := []int{1 << 10, 5000, 100 << 10, 1<<20 + 1, 3 << 20, page.MaxValue}
	for i, n := range sizes {
		v := make([]byte, n)
		for j := range v {
			v[j] = byte(j*7 + i)
		}
		e.put([]byte(fmt.Sprintf("big/%d", i)), v)
	}
	for i := 0; i < 200; i++ {
		e.put([]byte(fmt.Sprintf("hot/%03d", i)), []byte("h"))
	}
	e.check()
	e.commit()
	e.structural()

	// Warm the cache with the hot keys, then read the big values.
	r := e.store.Reader()
	root := e.store.State().Roots[0]
	readHot := func() {
		for i := 0; i < 200; i += 10 {
			if _, ok, err := btree.Get(ctx, r, e.pins, root, []byte(fmt.Sprintf("hot/%03d", i))); err != nil || !ok {
				t.Fatalf("hot key %d: %v", i, err)
			}
		}
	}
	readHot()
	before := e.store.Cache().Stats()
	for i, n := range sizes[3:] {
		v, ok, err := btree.Get(ctx, r, e.pins, root, []byte(fmt.Sprintf("big/%d", i+3)))
		if err != nil || !ok || len(v) != n || v[n-1] != byte((n-1)*7+i+3) {
			t.Fatalf("big value %d: %v", i+3, err)
		}
	}
	readHot()
	after := e.store.Cache().Stats()
	if after.Misses-before.Misses > 10 {
		t.Fatalf("reading large values evicted the hot set: %d new misses", after.Misses-before.Misses)
	}
	e.noPins()
}

// BT-02: keys at the limits.
func TestKeyLimits(t *testing.T) {
	e := newEnv(t)
	e.put([]byte{}, []byte("empty key"))
	e.put(bytes.Repeat([]byte{0xff}, page.MaxKeyLen), []byte("longest key"))
	e.put(bytes.Repeat([]byte{0x00}, page.MaxKeyLen), bytes.Repeat([]byte{1}, 2000))
	if _, err := e.tree.Put(ctx, e.pins, make([]byte, page.MaxKeyLen+1), nil); !isCode(err, errs.CodeLimitExceeded) {
		t.Fatalf("key above the limit: %v", err)
	}
	if _, err := e.tree.Put(ctx, e.pins, []byte("k"), make([]byte, page.MaxValue+1)); !isCode(err, errs.CodeLimitExceeded) {
		t.Fatalf("value above the limit: %v", err)
	}
	e.check()
	e.commit()
	e.structural()
}

// forgedStore returns a committed tree and the file system holding it.
func forgedStore(t *testing.T) (*vfs.MemFS, page.ID) {
	t.Helper()
	e := newEnv(t)
	for i := 0; i < 300; i++ {
		v := []byte("value")
		if i%50 == 0 {
			v = bytes.Repeat([]byte{byte(i)}, 9000)
		}
		e.put([]byte(fmt.Sprintf("key/%04d", i)), v)
	}
	// Single-page holes make the next value take six extents and a descriptor.
	for i := 0; i < 12; i++ {
		e.put([]byte(fmt.Sprintf("hole/%02d", i)), bytes.Repeat([]byte{'h'}, 2000))
	}
	for i := 0; i < 12; i += 2 {
		e.del([]byte(fmt.Sprintf("hole/%02d", i)))
	}
	e.put([]byte("many"), bytes.Repeat([]byte{'m'}, 6*page.Usable))
	e.commit()
	return e.fs.(*vfs.MemFS), e.store.State().Roots[0]
}

// exerciseForged reads the whole tree of a store that may be forged:
// every error is allowed, a panic or a hang is not.
func exerciseForged(t *testing.T, fs vfs.FS) {
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		return
	}
	defer s.Close()
	st := s.State()
	pins := cache.NewPins(0)
	root := st.Roots[0]
	btree.Verify(ctx, s.Reader(), pins, root)
	btree.Range(ctx, s.Reader(), pins, root, nil, nil, func(k, v []byte) error { return nil })
	btree.RangeReverse(ctx, s.Reader(), pins, root, nil, nil, func(k, v []byte) error { return nil })
	btree.RangeReverse(ctx, s.Reader(), pins, root, []byte("key/0100"), []byte("key/0200"), func(k, v []byte) error { return nil })
	for i := 0; i < 300; i += 37 {
		btree.Get(ctx, s.Reader(), pins, root, []byte(fmt.Sprintf("key/%04d", i)))
	}
	check.Structural(ctx, s.Reader(), st.Roots, st.PageCount, st.FreePageCount)
	// Writes over the forged pages: errors are fine, a panic is not.
	if tx, err := s.Begin(); err == nil {
		tree := btree.New(tx, alloc.New(tx), root)
		for i := 0; i < 300; i += 23 {
			key := []byte(fmt.Sprintf("key/%04d", i))
			if i%2 == 0 {
				tree.Put(ctx, pins, key, bytes.Repeat([]byte{'w'}, i*20))
			} else {
				tree.Delete(ctx, pins, key)
			}
		}
		tx.Rollback()
	}
	if pins.Used() != 0 {
		t.Fatalf("%d bytes still pinned", pins.Used())
	}
}

// BT-06: a descriptor page that claims 338 extents is CORRUPT.
func TestDescriptorWithTooManyExtents(t *testing.T) {
	fs, root := forgedStore(t)
	f, err := fs.Open(btreetest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	var desc page.ID
	size, _ := f.Size()
	var b page.Buf
	for id := page.ID(3); int64(id)*page.Size < size; id++ {
		if err := f.ReadAt(b[:], int64(id)*page.Size); err != nil {
			t.Fatal(err)
		}
		if page.HeaderOf(&b).Type() == page.TypeOverflow {
			if _, err := page.DescriptorExtents(&b); err == nil {
				desc = id
				break
			}
		}
	}
	if desc == 0 {
		t.Fatal("no descriptor page")
	}
	binary.LittleEndian.PutUint16(b[24:], 338*page.ExtentSize) // the count field of the header
	page.Seal(&b)
	if err := f.WriteAt(b[:], int64(desc)*page.Size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := btree.Get(ctx, s.Reader(), cache.NewPins(0), root, []byte("many")); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("value with 338 extents: %v", err)
	}
	exerciseForged(t, fs)
}

// Reopening through the OS file system gives back the same tree, which
// the checker approves.
func TestReopenOnDisk(t *testing.T) {
	dir, err := vfs.OpenDir(t.TempDir(), vfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	e := &env{t: t, fs: dir, pins: cache.NewPins(0), model: map[string][]byte{}}
	if e.store, err = btreetest.Create(ctx, dir, btreetest.Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.store.Close() })
	e.begin()
	for i := 0; i < 500; i++ {
		e.put([]byte(fmt.Sprintf("disk/%04d", i)), bytes.Repeat([]byte{byte(i)}, i*3))
	}
	e.reopen()
	e.check()
	e.structural()
}

// Verify enforces max(L) < s <= min(R) for every separator, and the leaf
// chain (SPEC §8.1).
func TestVerifyRejectsBadSeparators(t *testing.T) {
	cases := []struct {
		name        string
		left, right []string
		sep         string
		chain       bool
	}{
		{"valid", []string{"a", "c"}, []string{"d"}, "d", true},
		{"left key not below the separator", []string{"a", "c"}, []string{"d"}, "c", true},
		{"right key below the separator", []string{"a"}, []string{"d"}, "e", true},
		{"broken leaf chain", []string{"a"}, []string{"d"}, "b", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ids := make([]page.ID, 3)
			for i := range ids {
				id, err := e.alloc.Alloc(ctx, 0)
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
			}
			cells := func(keys []string) []page.LeafCell {
				var cs []page.LeafCell
				for _, k := range keys {
					cs = append(cs, page.LeafCell{Key: []byte(k), Value: []byte("v")})
				}
				return cs
			}
			next := ids[1]
			if !c.chain {
				next = 0
			}
			if err := e.tree.StoreLeaf(ctx, ids[0], cells(c.left), next); err != nil {
				t.Fatal(err)
			}
			if err := e.tree.StoreLeaf(ctx, ids[1], cells(c.right), 0); err != nil {
				t.Fatal(err)
			}
			if err := e.tree.StoreInternal(ctx, ids[2], 1, ids[0], []page.InternalCell{{Key: []byte(c.sep), Right: ids[1]}}); err != nil {
				t.Fatal(err)
			}
			_, err := btree.Verify(ctx, e.tx, e.pins, ids[2])
			if valid := c.name == "valid"; (err == nil) != valid {
				t.Fatalf("Verify: %v", err)
			}
			if err != nil && !isCode(err, errs.CodeCorrupt) {
				t.Fatalf("Verify: %v, want CORRUPT", err)
			}
		})
	}
}

// readNode returns a copy of page id read through the transaction.
func (e *env) readNode(id page.ID) page.Node {
	e.t.Helper()
	ref, err := e.tx.Page(ctx, e.pins, id)
	if err != nil {
		e.t.Fatal(err)
	}
	b := *ref.Bytes()
	ref.Release()
	return page.NodeOf(&b)
}

// alloc returns a page just allocated.
func (e *env) allocPage() page.ID {
	e.t.Helper()
	id, err := e.alloc.Alloc(ctx, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// BT-03 and REV-19: leaf separators are truncated to min(R)[:lcp+1]. With
// 604-byte keys that differ in their first four bytes, full separators
// would fit six to an internal page and need a third level.
func TestSeparatorsAreTruncated(t *testing.T) {
	e := newEnv(t)
	tail := strings.Repeat("z", 600)
	for i := 0; i < 600; i++ {
		e.put([]byte(fmt.Sprintf("%04d%s", i, tail)), []byte("v"))
	}
	rep := e.check()
	if rep.Height != 2 || rep.Leaves < 90 {
		t.Fatalf("shape %+v", rep)
	}
	root := e.readNode(e.tree.Root())
	for i := 0; i < root.Len(); i++ {
		l, r := e.readNode(root.Child(i)), e.readNode(root.Child(i+1))
		maxL, minR := l.AppendKey(nil, l.Len()-1), r.AppendKey(nil, 0)
		p := 0
		for p < len(maxL) && maxL[p] == minR[p] {
			p++
		}
		if sep := root.AppendKey(nil, i); !bytes.Equal(sep, minR[:p+1]) {
			t.Fatalf("separator %d is %q, want %q", i, sep, minR[:p+1])
		}
	}
	e.commit()
	e.structural()
}

// An empty internal node whose merge does not fit is redistributed even
// though the new separator overflows the parent, which then splits: a
// Delete that grows the tree.
func TestForcedRedistribution(t *testing.T) {
	e := newEnv(t)
	long := func(head string, n int) string { return head + strings.Repeat("m", n-len(head)) }
	type child struct {
		seps   []string
		leaves [][]string
	}
	kids := []child{
		{seps: []string{long("A", 1000)}, leaves: [][]string{{"A"}, {long("A", 1000), long("A", 1000) + "x"}}},
		{seps: []string{long("Ba", 1000), long("Bb", 1000), long("Bc", 1000), long("Bd", 984)},
			leaves: [][]string{{"B"}, {long("Ba", 1000)}, {long("Bb", 1000)}, {long("Bc", 1000)}, {long("Bd", 984)}}},
		{seps: []string{long("Cn", 1000)}, leaves: [][]string{{long("C", 1000)}, {long("Cn", 1000)}}},
		{seps: []string{long("Dn", 1000)}, leaves: [][]string{{long("D", 1000)}, {long("Dn", 1000)}}},
		{seps: []string{long("En", 1000)}, leaves: [][]string{{long("E", 1000)}, {long("En", 1000)}}},
	}
	var leafIDs []page.ID
	var leafKeys [][]string
	for _, k := range kids {
		for _, l := range k.leaves {
			leafIDs = append(leafIDs, e.allocPage())
			leafKeys = append(leafKeys, l)
		}
	}
	for i, keys := range leafKeys {
		next := page.ID(0)
		if i+1 < len(leafIDs) {
			next = leafIDs[i+1]
		}
		var cells []page.LeafCell
		for _, k := range keys {
			cells = append(cells, page.LeafCell{Key: []byte(k), Value: []byte("v")})
			e.model[k] = []byte("v")
		}
		if err := e.tree.StoreLeaf(ctx, leafIDs[i], cells, next); err != nil {
			t.Fatal(err)
		}
	}
	li := 0
	var kidIDs []page.ID
	for _, k := range kids {
		id := e.allocPage()
		kidIDs = append(kidIDs, id)
		left := leafIDs[li]
		li++
		var seps []page.InternalCell
		for _, s := range k.seps {
			seps = append(seps, page.InternalCell{Key: []byte(s), Right: leafIDs[li]})
			li++
		}
		if err := e.tree.StoreInternal(ctx, id, 1, left, seps); err != nil {
			t.Fatal(err)
		}
	}
	root := e.allocPage()
	var seps []page.InternalCell
	for i, s := range []string{"B", long("C", 1000), long("D", 1000), long("E", 1000)} {
		seps = append(seps, page.InternalCell{Key: []byte(s), Right: kidIDs[i+1]})
	}
	if err := e.tree.StoreInternal(ctx, root, 2, kidIDs[0], seps); err != nil {
		t.Fatal(err)
	}
	e.tree.SetRoot(root)
	if rep := e.check(); rep.Height != 3 {
		t.Fatalf("height %d before the delete", rep.Height)
	}
	// The leaf of the second key merges into "A"; the first child of the
	// root is left with no separator and cannot merge with its sibling.
	e.del([]byte(long("A", 1000) + "x"))
	e.del([]byte(long("A", 1000)))
	if rep := e.check(); rep.Height != 4 {
		t.Fatalf("height %d after the deletes, want 4: the forced redistribution split the root", rep.Height)
	}
	e.commit()
	e.structural()
}

// SPEC §8.1: a node rebalances below 25% of the usable bytes, not at 25%.
func TestUnderfullThreshold(t *testing.T) {
	for _, c := range []struct {
		value  int // the leaf keeps one cell of 9+value bytes with its slot
		leaves int
	}{{page.Usable/4 - 9, 2}, {page.Usable/4 - 10, 1}} {
		t.Run(fmt.Sprint(c.value), func(t *testing.T) {
			e := newEnv(t)
			l, r, root := e.allocPage(), e.allocPage(), e.allocPage()
			left := []page.LeafCell{{Key: []byte("a"), Value: bytes.Repeat([]byte{'v'}, c.value)}, {Key: []byte("b"), Value: []byte("x")}}
			right := []page.LeafCell{{Key: []byte("c"), Value: []byte("y")}}
			for _, cell := range append(slices.Clone(left), right...) {
				e.model[string(cell.Key)] = cell.Value
			}
			if err := e.tree.StoreLeaf(ctx, l, left, r); err != nil {
				t.Fatal(err)
			}
			if err := e.tree.StoreLeaf(ctx, r, right, 0); err != nil {
				t.Fatal(err)
			}
			if err := e.tree.StoreInternal(ctx, root, 1, l, []page.InternalCell{{Key: []byte("c"), Right: r}}); err != nil {
				t.Fatal(err)
			}
			e.tree.SetRoot(root)
			e.del([]byte("b"))
			if rep := e.check(); rep.Leaves != c.leaves {
				t.Fatalf("%d leaves, want %d", rep.Leaves, c.leaves)
			}
		})
	}
}

// BT-02: forged trees whose descent must stop with CORRUPT: a pointer to
// an ancestor, a child at the wrong level and a chain deeper than 40.
func TestForgedDescents(t *testing.T) {
	cases := map[string]func(e *env) page.ID{
		"pointer to an ancestor": func(e *env) page.ID {
			root, leaf := e.allocPage(), e.allocPage()
			e.tree.StoreLeaf(ctx, leaf, []page.LeafCell{{Key: []byte("z"), Value: []byte("v")}}, 0)
			e.tree.StoreInternal(ctx, root, 1, root, []page.InternalCell{{Key: []byte("m"), Right: leaf}})
			return root
		},
		"wrong level": func(e *env) page.ID {
			root, leaf := e.allocPage(), e.allocPage()
			e.tree.StoreLeaf(ctx, leaf, []page.LeafCell{{Key: []byte("a"), Value: []byte("v")}}, 0)
			e.tree.StoreInternal(ctx, root, 3, leaf, []page.InternalCell{{Key: []byte("m"), Right: leaf}})
			return root
		},
		"41 levels": func(e *env) page.ID {
			leaf := e.allocPage()
			e.tree.StoreLeaf(ctx, leaf, []page.LeafCell{{Key: []byte("a"), Value: []byte("v")}}, 0)
			child := leaf
			for level := uint16(1); level <= page.MaxTreeHeight; level++ {
				id := e.allocPage()
				e.tree.StoreInternal(ctx, id, level, child, []page.InternalCell{{Key: []byte("z"), Right: leaf}})
				child = id
			}
			return child
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			root := build(e)
			if _, _, err := btree.Get(ctx, e.tx, e.pins, root, []byte("a")); !isCode(err, errs.CodeCorrupt) {
				t.Fatalf("Get: %v", err)
			}
			if _, err := btree.Verify(ctx, e.tx, e.pins, root); !isCode(err, errs.CodeCorrupt) {
				t.Fatalf("Verify: %v", err)
			}
			e.noPins()
		})
	}
}

// BT-02: a leaf chain that loops back stops the walk with CORRUPT.
func TestLeafChainCycle(t *testing.T) {
	fs, root := forgedStore(t)
	f, err := fs.Open(btreetest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := f.Size()
	var first, last page.ID
	var b page.Buf
	for id := page.ID(3); int64(id)*page.Size < size; id++ {
		if err := f.ReadAt(b[:], int64(id)*page.Size); err != nil {
			t.Fatal(err)
		}
		h := page.HeaderOf(&b)
		if h.Type() != page.TypeLeaf {
			continue
		}
		if first == 0 {
			first = id
		}
		if h.Sibling() == 0 {
			last = id
		}
	}
	if err := f.ReadAt(b[:], int64(last)*page.Size); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(b[32:], uint64(first)) // the next-leaf field of the header
	page.Seal(&b)
	if err := f.WriteAt(b[:], int64(last)*page.Size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = btree.Range(tctx, s.Reader(), cache.NewPins(0), root, nil, nil, func(k, v []byte) error { return nil })
	if !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("Range over a cycle: %v", err)
	}
	if _, err := btree.Verify(ctx, s.Reader(), cache.NewPins(0), root); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("Verify of a cycle: %v", err)
	}
}

// BT-04: invalid extents inside a descriptor page, which page.Verify does
// not look at, are CORRUPT when the value is read.
func TestForgedDescriptorExtents(t *testing.T) {
	fs, root := forgedStore(t)
	f, err := fs.Open(btreetest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := f.Size()
	var b page.Buf
	var desc page.ID
	for id := page.ID(3); int64(id)*page.Size < size && desc == 0; id++ {
		if err := f.ReadAt(b[:], int64(id)*page.Size); err != nil {
			t.Fatal(err)
		}
		if _, err := page.DescriptorExtents(&b); page.HeaderOf(&b).Type() == page.TypeOverflow && err == nil {
			desc = id
		}
	}
	if desc == 0 {
		t.Fatal("no descriptor page")
	}
	pages := binary.LittleEndian.Uint32(b[page.HeaderSize+8:])
	binary.LittleEndian.PutUint32(b[page.HeaderSize+8:], pages+1) // the extents now hold a page too many
	page.Seal(&b)
	if err := f.WriteAt(b[:], int64(desc)*page.Size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := btree.Get(ctx, s.Reader(), cache.NewPins(0), root, []byte("many")); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("value with forged extents: %v", err)
	}
	exerciseForged(t, fs)
}
