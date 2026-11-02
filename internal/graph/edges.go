package graph

import (
	"context"
	"encoding/binary"
	"slices"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
)

// Direction selects edges by the side of a node they leave from.
type Direction uint8

// Directions. Both yields each self-loop once.
const (
	Out Direction = iota
	In
	Both
)

func encodeCount(n uint64) []byte { return binary.LittleEndian.AppendUint64(nil, n) }

func decodeCount(b []byte) (uint64, error) {
	if len(b) != 8 || binary.LittleEndian.Uint64(b) == 0 {
		return 0, errs.Newf(errs.CodeCorrupt, "graph", "degree counter %x", b)
	}
	return binary.LittleEndian.Uint64(b), nil
}

func readCount(ctx context.Context, r engine.Reader, k []byte) (uint64, error) {
	b, ok, err := r.Get(ctx, catalog.SlotStats, k)
	if err != nil || !ok {
		return 0, err
	}
	return decodeCount(b)
}

// Degree returns the number of edges of node in dir with type typ, or of
// every type when typ is 0, from the counters of the Stats tree. Both
// counts a self-loop once (SPEC §12.1).
func Degree(ctx context.Context, r engine.Reader, node uint64, dir Direction, typ catalog.EdgeTypeID) (uint64, error) {
	if dir != Both {
		return readCount(ctx, r, degreeKey(node, uint8(dir), typ))
	}
	var n [3]uint64
	for d := range n {
		var err error
		if n[d], err = readCount(ctx, r, degreeKey(node, uint8(d), typ)); err != nil {
			return 0, err
		}
	}
	if n[dirSelf] > n[dirOut] || n[dirSelf] > n[dirIn] {
		return 0, errs.Newf(errs.CodeCorrupt, "graph", "node %d has more self-loops than edges", node)
	}
	return n[dirOut] + n[dirIn] - n[dirSelf], nil
}

// EdgeRef is an edge as its adjacency entry tells it.
type EdgeRef struct {
	ID       uint64
	From, To uint64
	Type     string
}

// cursorBatch is how many adjacency entries a cursor reads per operation
// of its transaction.
const cursorBatch = 64

// EdgeCursor walks the adjacency of one node. It reads in batches, each
// one operation of the transaction that resumes after the last key it
// saw, so that it holds no page between calls; before each edge it checks
// that the transaction is still usable, so an ended transaction or a
// revoked snapshot stops it at once. Inside a write transaction it yields
// the edges that existed when it was opened and still exist when it
// reaches them: edges created after it opened never show, whatever the
// batch they fall in. A cursor has one user at a time.
type EdgeCursor struct {
	g      *Graph
	r      engine.Reader
	node   uint64
	dir    Direction
	ranges [][2][]byte // prefixes still to scan, in order
	next   []byte      // where the current range resumes
	buf    []EdgeRef
	bound  uint64 // in a write transaction, the first edge ID handed out after the open; 0 otherwise
	cur    EdgeRef
	err    error
	done   bool
}

// Edges returns a cursor over the edges of node in dir with one of types,
// or of any type when types is empty. Unknown types match nothing and are
// not interned.
func (g *Graph) Edges(ctx context.Context, r engine.Reader, node uint64, dir Direction, types []string) (*EdgeCursor, error) {
	if dir > Both {
		return nil, errs.Newf(errs.CodeInvalidArgument, "tacdb.Edges", "direction %d", dir)
	}
	if _, err := g.node(ctx, r, "tacdb.Edges", node); err != nil {
		return nil, err
	}
	var ids []catalog.EdgeTypeID
	for _, t := range types {
		id, ok, err := catalog.CachedLookup[catalog.EdgeTypeID](ctx, g.Symbols, r, t)
		if err != nil {
			return nil, err
		}
		if ok && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	c := &EdgeCursor{g: g, r: r, node: node, dir: dir}
	if w, ok := r.(interface{ IDBound(kind int) uint64 }); ok {
		c.bound = w.IDBound(engine.KindEdge)
	}
	for _, d := range []uint8{dirOut, dirIn} {
		if (dir == Out && d != dirOut) || (dir == In && d != dirIn) {
			continue
		}
		if len(types) == 0 {
			p := adjacencyPrefix(node, d)
			c.ranges = append(c.ranges, [2][]byte{p, codec.PrefixEnd(p)})
		}
		for _, t := range ids {
			p := adjacencyTypePrefix(node, d, t)
			c.ranges = append(c.ranges, [2][]byte{p, codec.PrefixEnd(p)})
		}
	}
	return c, nil
}

// Next moves to the next edge and reports whether there is one; at the
// end or on error it returns false, and Err tells which.
func (c *EdgeCursor) Next(ctx context.Context) bool {
	for {
		if c.done || c.err != nil {
			return false
		}
		if c.err = c.r.Check(); c.err != nil {
			return false
		}
		if len(c.buf) == 0 {
			if c.err = c.fill(ctx); c.err != nil {
				return false
			}
			continue
		}
		c.cur, c.buf = c.buf[0], c.buf[1:]
		if c.bound == 0 {
			return true
		}
		// A write transaction may have deleted it since its batch.
		_, ok, err := c.r.Get(ctx, catalog.SlotEdges, recordKey(c.cur.ID))
		if c.err = err; err != nil {
			return false
		}
		if ok {
			return true
		}
	}
}

// Edge returns the current edge.
func (c *EdgeCursor) Edge() EdgeRef { return c.cur }

// Err returns the error that ended the walk, if any.
func (c *EdgeCursor) Err() error { return c.err }

// Close ends the cursor.
func (c *EdgeCursor) Close() {
	c.done, c.buf, c.ranges = true, nil, nil
}

// fill reads the next batch of the current range, moving to the next
// range when it ends.
func (c *EdgeCursor) fill(ctx context.Context) error {
	if len(c.ranges) == 0 {
		c.done = true
		return nil
	}
	rg := c.ranges[0]
	lo := rg[0]
	if c.next != nil {
		lo = c.next
	}
	n := 0
	var last []byte
	var refs []adjacency
	err := c.r.Range(ctx, catalog.SlotAdjacency, lo, rg[1], func(k, _ []byte) error {
		if n == cursorBatch {
			return errStop
		}
		n++
		last = append(last[:0], k...)
		a, err := decodeAdjacency(k)
		if err != nil {
			return err
		}
		if a.node != c.node {
			return errs.Newf(errs.CodeCorrupt, "graph", "adjacency entry %x under node %d", k, c.node)
		}
		refs = append(refs, a)
		return nil
	})
	if err != nil && err != errStop {
		return err
	}
	if err == errStop {
		c.next = append(last, 0) // the least key after last
	} else {
		c.ranges, c.next = c.ranges[1:], nil
	}
	for _, a := range refs {
		if a.dir == dirIn && a.peer == c.node && c.dir == Both {
			continue // a self-loop, already given from its outgoing entry
		}
		if c.bound != 0 && a.edge >= c.bound {
			continue // created after the cursor opened
		}
		t, err := name(ctx, c.g, c.r, a.typ)
		if err != nil {
			return err
		}
		ref := EdgeRef{ID: a.edge, From: a.node, To: a.peer, Type: t}
		if a.dir == dirIn {
			ref.From, ref.To = a.peer, a.node
		}
		c.buf = append(c.buf, ref)
	}
	return nil
}
