package btree

import (
	"bytes"
	"context"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
)

// Cursor walks the keys of a tree in order. It holds no page between
// calls: it remembers its leaf, its slot and a copy of its key. A cursor
// of a write tree fails with INVALID_ARGUMENT (Op "cursor") once that tree
// changed or the transaction restored a savepoint (SPEC §8.1); changes to
// other trees do not affect it.
type Cursor struct {
	r        page.Reader
	pins     page.Pins
	t        *Tree // nil for a read tree
	muts     uint64
	restores uint64
	leaf     page.ID
	slot     int
	key      []byte
	valid    bool
}

// SeekGE positions a cursor of the read tree at the first key >= key.
func SeekGE(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, key []byte) (*Cursor, error) {
	c := &Cursor{r: r, pins: pins}
	return c, c.seek(ctx, root, key, false)
}

// SeekGT positions a cursor of the read tree at the first key > key.
func SeekGT(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, key []byte) (*Cursor, error) {
	c := &Cursor{r: r, pins: pins}
	return c, c.seek(ctx, root, key, true)
}

// First positions a cursor of the read tree at its first key.
func First(ctx context.Context, r page.Reader, pins page.Pins, root page.ID) (*Cursor, error) {
	return SeekGE(ctx, r, pins, root, nil)
}

func (t *Tree) cursor(pins page.Pins) *Cursor {
	return &Cursor{r: t.tx, pins: pins, t: t, muts: t.muts, restores: t.tx.Restores()}
}

// SeekGE positions a cursor at the first key >= key.
func (t *Tree) SeekGE(ctx context.Context, pins page.Pins, key []byte) (*Cursor, error) {
	c := t.cursor(pins)
	return c, c.seek(ctx, t.root, key, false)
}

// SeekGT positions a cursor at the first key > key.
func (t *Tree) SeekGT(ctx context.Context, pins page.Pins, key []byte) (*Cursor, error) {
	c := t.cursor(pins)
	return c, c.seek(ctx, t.root, key, true)
}

// First positions a cursor at the first key.
func (t *Tree) First(ctx context.Context, pins page.Pins) (*Cursor, error) {
	return t.SeekGE(ctx, pins, nil)
}

func (c *Cursor) check() error {
	if c.t != nil && (c.t.muts != c.muts || c.t.tx.Restores() != c.restores) {
		return errs.New(errs.CodeInvalidArgument, "cursor", "the tree changed or a savepoint was restored")
	}
	return nil
}

func (c *Cursor) seek(ctx context.Context, root page.ID, key []byte, after bool) error {
	if root == 0 {
		return nil
	}
	ref, err := descend(ctx, c.r, c.pins, root, key, nil)
	if err != nil {
		return err
	}
	n := page.NodeOf(ref.Bytes())
	i, found := n.Search(key)
	if found && after {
		i++
	}
	c.leaf = page.HeaderOf(ref.Bytes()).ID()
	return c.settle(ctx, ref, i, nil)
}

// settle moves the cursor to slot i of the leaf in ref, or to the first
// key of the following leaves when i is past the end, and releases ref.
// prev, when not nil, is the key the cursor leaves: keys must grow, which
// also stops a cycle in forged leaf links.
func (c *Cursor) settle(ctx context.Context, ref page.Ref, i int, prev []byte) error {
	for {
		n := page.NodeOf(ref.Bytes())
		if i < n.Len() {
			key := n.AppendKey(nil, i)
			ref.Release()
			if prev != nil && bytes.Compare(key, prev) <= 0 {
				return corrupt(c.leaf, "the leaf chain goes back to key %x", key)
			}
			c.slot, c.key, c.valid = i, key, true
			return nil
		}
		next := page.HeaderOf(ref.Bytes()).Sibling()
		ref.Release()
		c.valid = false
		if next == 0 {
			return nil
		}
		if err := ctxErr(ctx, "btree.Cursor"); err != nil {
			return err
		}
		var err error
		if ref, err = c.leafPage(ctx, next); err != nil {
			return err
		}
		c.leaf, i = next, 0
	}
}

// leafPage reads a leaf, checking that it is one.
func (c *Cursor) leafPage(ctx context.Context, id page.ID) (page.Ref, error) {
	if err := page.CheckPointer(id, c.r.PageCount()); err != nil {
		return nil, err
	}
	ref, err := c.r.Page(ctx, c.pins, id)
	if err != nil {
		return nil, err
	}
	if h := page.HeaderOf(ref.Bytes()); h.Type() != page.TypeLeaf || h.ID() != id {
		ref.Release()
		return nil, corrupt(id, "the leaf chain reaches a page of type %d", h.Type())
	}
	return ref, nil
}

// Valid reports whether the cursor is on a key.
func (c *Cursor) Valid() bool { return c.valid }

// Key returns the current key; the caller must not change it.
func (c *Cursor) Key() []byte { return c.key }

// Next moves to the following key.
func (c *Cursor) Next(ctx context.Context) error {
	if err := c.check(); err != nil {
		return err
	}
	if !c.valid {
		return nil
	}
	ref, err := c.leafPage(ctx, c.leaf)
	if err != nil {
		return err
	}
	return c.settle(ctx, ref, c.slot+1, c.key)
}

// Value returns a copy of the current value.
func (c *Cursor) Value(ctx context.Context) ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if !c.valid {
		return nil, errs.New(errs.CodeInvalidArgument, "cursor", "the cursor is not on a key")
	}
	ref, err := c.leafPage(ctx, c.leaf)
	if err != nil {
		return nil, err
	}
	n := page.NodeOf(ref.Bytes())
	if c.slot >= n.Len() || n.CompareKey(c.slot, c.key) != 0 {
		ref.Release()
		return nil, corrupt(c.leaf, "the cursor's key moved")
	}
	v := n.Value(c.slot)
	if !v.Out() {
		out := append([]byte{}, v.Inline...)
		ref.Release()
		return out, nil
	}
	ref.Release()
	return readValue(ctx, c.r, c.pins, &v)
}

// Range calls fn for each key in [lo, hi) of the read tree, in order; a
// nil hi has no upper bound. fn must not change the tree.
func Range(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, lo, hi []byte, fn func(key, value []byte) error) error {
	c, err := SeekGE(ctx, r, pins, root, lo)
	if err != nil {
		return err
	}
	return c.each(ctx, hi, fn)
}

// Range calls fn for each key in [lo, hi), in order; a nil hi has no
// upper bound. Changing the tree inside fn invalidates the walk.
func (t *Tree) Range(ctx context.Context, pins page.Pins, lo, hi []byte, fn func(key, value []byte) error) error {
	c, err := t.SeekGE(ctx, pins, lo)
	if err != nil {
		return err
	}
	return c.each(ctx, hi, fn)
}

func (c *Cursor) each(ctx context.Context, hi []byte, fn func(key, value []byte) error) error {
	for c.Valid() && (hi == nil || bytes.Compare(c.Key(), hi) < 0) {
		v, err := c.Value(ctx)
		if err != nil {
			return err
		}
		if err := fn(c.Key(), v); err != nil {
			return err
		}
		if err := c.Next(ctx); err != nil {
			return err
		}
	}
	return nil
}

// RangeReverse calls fn for each key in [lo, hi) of the read tree, in
// descending order; a nil hi has no upper bound. Leaves link only to the
// next one, so each leaf is reached by a descent from the root: O(height)
// per leaf. fn must not change the tree.
func RangeReverse(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, lo, hi []byte, fn func(key, value []byte) error) error {
	return rangeReverse(ctx, r, pins, root, lo, hi, fn, nil)
}

// RangeReverse calls fn for each key in [lo, hi), in descending order; a
// nil hi has no upper bound. Changing the tree inside fn fails the walk
// with INVALID_ARGUMENT, as for a cursor.
func (t *Tree) RangeReverse(ctx context.Context, pins page.Pins, lo, hi []byte, fn func(key, value []byte) error) error {
	c := t.cursor(pins)
	return rangeReverse(ctx, t.tx, pins, t.root, lo, hi, fn, c.check)
}

type entry struct {
	key    []byte
	inline []byte
	out    *page.LeafValue
}

func rangeReverse(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, lo, hi []byte, fn func(key, value []byte) error, check func() error) error {
	if root == 0 {
		return nil
	}
	below := hi // the walk goes on with the keys below this one; nil is +inf
	for {
		if err := ctxErr(ctx, "btree.RangeReverse"); err != nil {
			return err
		}
		ref, i, err := lastBelow(ctx, r, pins, root, below)
		if err != nil || ref == nil {
			return err
		}
		n := page.NodeOf(ref.Bytes())
		id := page.HeaderOf(ref.Bytes()).ID()
		var batch []entry
		done := false
		for s := i - 1; s >= 0; s-- {
			k := n.AppendKey(nil, s)
			if lo != nil && bytes.Compare(k, lo) < 0 {
				done = true
				break
			}
			v := n.Value(s)
			e := entry{key: k}
			if v.Out() {
				e.out = &v
			} else {
				e.inline = append([]byte{}, v.Inline...)
			}
			batch = append(batch, e)
		}
		first := n.AppendKey(nil, 0)
		ref.Release()
		if below != nil && len(batch) > 0 && bytes.Compare(batch[0].key, below) >= 0 {
			return corrupt(id, "the descent for keys below %x reaches key %x", below, batch[0].key)
		}
		for _, e := range batch {
			v := e.inline
			if e.out != nil {
				if v, err = readValue(ctx, r, pins, e.out); err != nil {
					return err
				}
			}
			if err := fn(e.key, v); err != nil {
				return err
			}
			if check != nil {
				if err := check(); err != nil {
					return err
				}
			}
		}
		if done {
			return nil
		}
		below = first
	}
}

// lastBelow returns the leaf holding the largest key below key (+inf when
// key is nil) and the number of its slots below key, or a nil ref when no
// key lies below.
func lastBelow(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, key []byte) (page.Ref, int, error) {
	if key == nil {
		ref, err := edgeLeaf(ctx, r, pins, root)
		if err != nil {
			return nil, 0, err
		}
		if n := page.NodeOf(ref.Bytes()).Len(); n > 0 {
			return ref, n, nil
		}
		ref.Release()
		return nil, 0, nil
	}
	var path []frame
	ref, err := descend(ctx, r, pins, root, key, &path)
	if err != nil {
		return nil, 0, err
	}
	if i, _ := page.NodeOf(ref.Bytes()).Search(key); i > 0 {
		return ref, i, nil
	}
	ref.Release()
	// Every key of this leaf is >= key: the answer is the last key of the
	// nearest subtree to the left.
	for j := len(path) - 1; j >= 0; j-- {
		if path[j].child == 0 {
			continue
		}
		p, err := r.Page(ctx, pins, path[j].id)
		if err != nil {
			return nil, 0, err
		}
		left := page.NodeOf(p.Bytes()).Child(path[j].child - 1)
		p.Release()
		ref, err := edgeLeaf(ctx, r, pins, left)
		if err != nil {
			return nil, 0, err
		}
		n := page.NodeOf(ref.Bytes()).Len()
		if n == 0 {
			ref.Release()
			return nil, 0, corrupt(left, "an empty leaf below an internal page")
		}
		return ref, n, nil
	}
	return nil, 0, nil
}

// edgeLeaf descends from id to its rightmost leaf, with the checks of
// descend.
func edgeLeaf(ctx context.Context, r page.Reader, pins page.Pins, id page.ID) (page.Ref, error) {
	pc := r.PageCount()
	want := -1
	for depth := 0; ; depth++ {
		if depth >= page.MaxTreeHeight {
			return nil, corrupt(id, "the tree is deeper than %d levels", page.MaxTreeHeight)
		}
		if err := page.CheckPointer(id, pc); err != nil {
			return nil, err
		}
		ref, err := r.Page(ctx, pins, id)
		if err != nil {
			return nil, err
		}
		h := page.HeaderOf(ref.Bytes())
		level := int(h.Level())
		switch {
		case h.ID() != id:
			err = corrupt(id, "the page claims to be page %d", h.ID())
		case h.Type() != page.TypeLeaf && h.Type() != page.TypeInternal:
			err = corrupt(id, "a tree page of type %d", h.Type())
		case want >= 0 && level != want:
			err = corrupt(id, "level %d below a page of level %d", level, want+1)
		}
		if err != nil {
			ref.Release()
			return nil, err
		}
		if h.Type() == page.TypeLeaf {
			return ref, nil
		}
		n := page.NodeOf(ref.Bytes())
		id, want = n.Child(n.Len()), level-1
		ref.Release()
	}
}
