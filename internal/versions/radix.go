// Package versions holds the page versions of the M2 engine (design §4.2
// of M2): an immutable directory that maps each page to its latest version
// for a snapshot — a sealed image waiting for the WAL, a PAGE record of
// the WAL, or the main file — and the overlay of private copies of a write
// transaction, with O(1) savepoints. Both are persistent radix trees that
// copy only the paths they change.
package versions

import "math/bits"

// Value is what a Tree holds. Commit summarizes the subtrees, so that
// Prune can drop every value at or below a CommitID in O(pruned nodes);
// values that are never pruned return 0.
type Value interface{ Commit() uint64 }

const (
	radixBits = 6 // 64 children per node
	maxHeight = (64 + radixBits - 1) / radixBits
)

// node is an immutable node: a sparse array of 64 slots, children at the
// upper levels and values at level 0, with the summary of its subtree.
type node[V Value] struct {
	bitmap uint64
	kids   []*node[V]
	vals   []V
	max    uint64 // the largest Commit in the subtree
	size   int    // the values in the subtree
}

// Tree is an immutable map from page IDs to V. Every change returns a new
// Tree that shares the unchanged nodes with the old one, so a Tree can be
// published and read without locks while newer Trees are built.
type Tree[V Value] struct {
	root   *node[V]
	height int // keys below 1<<(radixBits*height) fit
}

// fits reports whether key k fits a tree of height h.
func fits(k uint64, h int) bool {
	return h >= maxHeight || k>>(radixBits*h) == 0
}

func slot(k uint64, level int) uint { return uint(k>>(radixBits*level)) & (1<<radixBits - 1) }

// index returns the position of slot s in the compact arrays of n.
func (n *node[V]) index(s uint) (int, bool) {
	bit := uint64(1) << s
	return bits.OnesCount64(n.bitmap & (bit - 1)), n.bitmap&bit != 0
}

// Len returns the number of values.
func (t Tree[V]) Len() int {
	if t.root == nil {
		return 0
	}
	return t.root.size
}

// MaxCommit returns the largest Commit of the values.
func (t Tree[V]) MaxCommit() uint64 {
	if t.root == nil {
		return 0
	}
	return t.root.max
}

// Get returns the value of key k.
func (t Tree[V]) Get(k uint64) (V, bool) {
	var zero V
	n := t.root
	if n == nil || !fits(k, t.height) {
		return zero, false
	}
	for level := t.height - 1; ; level-- {
		i, ok := n.index(slot(k, level))
		if !ok {
			return zero, false
		}
		if level == 0 {
			return n.vals[i], true
		}
		n = n.kids[i]
	}
}

// Set returns a Tree where k maps to v.
func (t Tree[V]) Set(k uint64, v V) Tree[V] {
	if t.root == nil {
		t.height = 1
	}
	for !fits(k, t.height) { // grow: the old root becomes child 0
		if t.root != nil && t.root.size > 0 {
			t.root = &node[V]{bitmap: 1, kids: []*node[V]{t.root}, max: t.root.max, size: t.root.size}
		}
		t.height++
	}
	t.root = set(t.root, t.height-1, k, v)
	return t
}

func set[V Value](n *node[V], level int, k uint64, v V) *node[V] {
	c := &node[V]{}
	if n != nil {
		*c = *n
	}
	s := slot(k, level)
	i, ok := c.index(s)
	if level == 0 {
		c.vals = append([]V(nil), c.vals...)
		if ok {
			c.vals[i] = v
		} else {
			c.vals = append(c.vals[:i], append([]V{v}, c.vals[i:]...)...)
			c.bitmap |= 1 << s
		}
	} else {
		c.kids = append([]*node[V](nil), c.kids...)
		var child *node[V]
		if ok {
			child = c.kids[i]
		}
		child = set(child, level-1, k, v)
		if ok {
			c.kids[i] = child
		} else {
			c.kids = append(c.kids[:i], append([]*node[V]{child}, c.kids[i:]...)...)
			c.bitmap |= 1 << s
		}
	}
	c.summarize()
	return c
}

// SetAll returns a Tree where each keys[i] maps to vals[i]. The keys are
// strictly increasing; each node on their paths is copied once, not once
// per key.
func (t Tree[V]) SetAll(keys []uint64, vals []V) Tree[V] {
	if len(keys) == 0 {
		return t
	}
	if t.root == nil {
		t.height = 1
	}
	for !fits(keys[len(keys)-1], t.height) { // grow: the old root becomes child 0
		if t.root != nil && t.root.size > 0 {
			t.root = &node[V]{bitmap: 1, kids: []*node[V]{t.root}, max: t.root.max, size: t.root.size}
		}
		t.height++
	}
	t.root = setAll(t.root, t.height-1, keys, vals)
	return t
}

func setAll[V Value](n *node[V], level int, keys []uint64, vals []V) *node[V] {
	c := &node[V]{}
	if n != nil {
		*c = *n
	}
	bm := c.bitmap
	for _, k := range keys {
		bm |= 1 << slot(k, level)
	}
	count := bits.OnesCount64(bm)
	old, next := 0, 0 // positions in the slots of n and in keys
	if level == 0 {
		vs := make([]V, 0, count)
		for b := bm; b != 0; b &= b - 1 {
			s := uint(bits.TrailingZeros64(b))
			if next < len(keys) && slot(keys[next], 0) == s {
				vs = append(vs, vals[next])
				next++
			} else {
				vs = append(vs, c.vals[old])
			}
			if c.bitmap&(1<<s) != 0 {
				old++
			}
		}
		c.vals = vs
	} else {
		kids := make([]*node[V], 0, count)
		for b := bm; b != 0; b &= b - 1 {
			s := uint(bits.TrailingZeros64(b))
			var child *node[V]
			if c.bitmap&(1<<s) != 0 {
				child = c.kids[old]
				old++
			}
			end := next
			for end < len(keys) && slot(keys[end], level) == s {
				end++
			}
			if end > next {
				child = setAll(child, level-1, keys[next:end], vals[next:end])
				next = end
			}
			kids = append(kids, child)
		}
		c.kids = kids
	}
	c.bitmap = bm
	c.summarize()
	return c
}

// summarize recomputes the summary of a node from its slots.
func (n *node[V]) summarize() {
	n.max, n.size = 0, 0
	for _, v := range n.vals {
		n.max = max(n.max, v.Commit())
		n.size++
	}
	for _, k := range n.kids {
		n.max = max(n.max, k.max)
		n.size += k.size
	}
}

// Delete returns a Tree without key k.
func (t Tree[V]) Delete(k uint64) Tree[V] {
	if t.root == nil || !fits(k, t.height) {
		return t
	}
	if _, ok := t.Get(k); !ok {
		return t
	}
	t.root = del(t.root, t.height-1, k)
	return t
}

func del[V Value](n *node[V], level int, k uint64) *node[V] {
	s := slot(k, level)
	i, _ := n.index(s)
	c := *n
	if level == 0 {
		c.vals = append(append([]V(nil), n.vals[:i]...), n.vals[i+1:]...)
		c.bitmap &^= 1 << s
	} else {
		child := del(n.kids[i], level-1, k)
		c.kids = append([]*node[V](nil), n.kids...)
		if child == nil {
			c.kids = append(c.kids[:i], c.kids[i+1:]...)
			c.bitmap &^= 1 << s
		} else {
			c.kids[i] = child
		}
	}
	if c.bitmap == 0 {
		return nil
	}
	c.summarize()
	return &c
}

// All yields the keys and values in increasing key order.
func (t Tree[V]) All(yield func(k uint64, v V) bool) {
	if t.root != nil {
		walk(t.root, t.height-1, 0, yield)
	}
}

func walk[V Value](n *node[V], level int, base uint64, yield func(uint64, V) bool) bool {
	pos := 0
	for b := n.bitmap; b != 0; b &= b - 1 {
		s := uint64(bits.TrailingZeros64(b))
		k := base | s<<(radixBits*level)
		if level == 0 {
			if !yield(k, n.vals[pos]) {
				return false
			}
		} else if !walk(n.kids[pos], level-1, k, yield) {
			return false
		}
		pos++
	}
	return true
}

// Prune drops every value whose Commit is at or below limit, visiting at
// most budget nodes from key from on, in key order. It returns the new
// Tree and the key where the next call should continue (0 after the end:
// the sweep wraps around). Whole subtrees whose summary is at or below
// limit go in one step. The budget is raised to the height plus one, the
// cost of reaching the first value, so every call makes progress.
func (t Tree[V]) Prune(limit, from uint64, budget int) (Tree[V], uint64) {
	if t.root == nil || budget <= 0 {
		return t, from
	}
	budget = max(budget, t.height+1)
	if !fits(from, t.height) {
		from = 0
	}
	p := pruner[V]{limit: limit, budget: budget}
	root, done := p.prune(t.root, t.height-1, 0, from)
	t.root = root
	if done {
		return t, 0
	}
	return t, p.next
}

type pruner[V Value] struct {
	limit  uint64
	budget int
	next   uint64 // where the sweep stopped
}

// prune returns the pruned subtree of n, whose keys start at base, and
// whether it covered the whole subtree from key from on.
func (p *pruner[V]) prune(n *node[V], level int, base, from uint64) (*node[V], bool) {
	if n.max <= p.limit {
		p.budget--
		return nil, true
	}
	p.budget--
	c := *n
	c.vals, c.kids, c.bitmap = nil, nil, 0
	done := true
	pos := 0
	for b := n.bitmap; b != 0; b &= b - 1 {
		s := uint(bits.TrailingZeros64(b))
		k := base | uint64(s)<<(radixBits*level)
		span := uint64(1) << (radixBits * level) // keys under slot s
		keep := func() {
			c.bitmap |= 1 << s
			if level == 0 {
				c.vals = append(c.vals, n.vals[pos])
			} else {
				c.kids = append(c.kids, n.kids[pos])
			}
		}
		switch {
		case !done || k+span-1 < from: // before the start, or after the stop
			keep()
		case p.budget <= 0:
			done, p.next = false, k
			keep()
		case level == 0:
			p.budget--
			if n.vals[pos].Commit() > p.limit {
				keep()
			}
		default:
			child, all := p.prune(n.kids[pos], level-1, k, from)
			if child != nil {
				c.bitmap |= 1 << s
				c.kids = append(c.kids, child)
			}
			if !all {
				done = false
			}
		}
		pos++
	}
	if c.bitmap == 0 {
		return nil, done
	}
	c.summarize()
	return &c, done
}
