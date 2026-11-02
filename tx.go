package tacdb

import (
	"context"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/value"
)

// Tx is a read or write transaction. It has one user at a time: a call
// while another call of the same Tx or of one of its cursors runs fails
// with TX_IN_USE. Graph operations are bounded by MaxWriteTxDuration or
// MaxReadTxAge, not by a context.
type Tx struct {
	db      *DB
	r       *engine.ReadTx  // a read transaction
	w       *engine.WriteTx // a write transaction
	busy    atomic.Bool
	managed bool // owned by View or Update
	closed  bool // a read transaction ended by Commit or Rollback
}

// ctx is the context of graph operations, which take none (SPEC §6).
var ctx = context.Background()

func (tx *Tx) enter(op string) error {
	if !tx.busy.CompareAndSwap(false, true) {
		return errs.New(errs.CodeTxInUse, op, "the transaction is in use by another call")
	}
	return nil
}

func (tx *Tx) leave() { tx.busy.Store(false) }

// acquire waits until no call uses tx and takes it. Calls are bounded by
// MaxWriteTxDuration or MaxReadTxAge, so the wait is too.
func (tx *Tx) acquire() {
	for !tx.busy.CompareAndSwap(false, true) {
		time.Sleep(50 * time.Microsecond)
	}
}

func (tx *Tx) reader() engine.Reader {
	if tx.w != nil {
		return tx.w
	}
	return tx.r
}

// ReadOnly reports whether tx is a read transaction.
func (tx *Tx) ReadOnly() bool { return tx.w == nil }

// Snapshot returns the commit tx reads from: its snapshot, or the base of
// a write transaction.
func (tx *Tx) Snapshot() CommitID {
	if tx.w != nil {
		return CommitID(tx.w.Base().CommitID)
	}
	return CommitID(tx.r.State().CommitID)
}

// Commit ends tx. A write transaction is sealed and waits as the
// durability mode says; before it seals, a failure leaves it active. A
// read transaction just closes, with a receipt of its snapshot and a zero
// token.
func (tx *Tx) Commit(ctx context.Context) (CommitReceipt, error) {
	const op = "tacdb.Commit"
	if err := tx.enter(op); err != nil {
		return CommitReceipt{}, err
	}
	defer tx.leave()
	if tx.managed {
		return CommitReceipt{}, errs.New(errs.CodeInvalidArgument, op, "the transaction belongs to View or Update")
	}
	if tx.w == nil {
		if tx.closed {
			return CommitReceipt{}, errs.New(errs.CodeTxClosed, op, "transaction closed")
		}
		tx.closed = true
		tx.r.Close()
		return CommitReceipt{ID: CommitID(tx.r.State().CommitID), Durable: tx.r.Durable()}, nil
	}
	rc, err := tx.w.Commit(ctx)
	return CommitReceipt{ID: CommitID(rc.CommitID), Token: rc.Token, Durable: rc.Durable}, err
}

// Rollback ends tx without effect. After a sealed commit it returns
// COMMIT_PENDING while the commit is pending and TX_CLOSED after.
func (tx *Tx) Rollback() error {
	const op = "tacdb.Rollback"
	if err := tx.enter(op); err != nil {
		return err
	}
	defer tx.leave()
	if tx.managed {
		return errs.New(errs.CodeInvalidArgument, op, "the transaction belongs to View or Update")
	}
	if tx.w == nil {
		tx.closed = true
		return tx.r.Close()
	}
	return tx.w.Rollback()
}

// read runs fn over the reader of tx.
func (tx *Tx) read(op string, fn func(r engine.Reader) error) error {
	if err := tx.enter(op); err != nil {
		return err
	}
	defer tx.leave()
	return fn(tx.reader())
}

// write runs fn as one statement of the write transaction.
func (tx *Tx) write(op string, fn func(s *engine.Stmt) error) error {
	if err := tx.enter(op); err != nil {
		return err
	}
	defer tx.leave()
	if tx.w == nil {
		return errs.New(errs.CodeReadOnly, op, "a read transaction cannot write")
	}
	return tx.w.Statement(op, fn)
}

// CreateNode creates a node.
func (tx *Tx) CreateNode(o CreateNodeOptions) (Node, error) {
	var id uint64
	err := tx.write("tacdb.CreateNode", func(s *engine.Stmt) error {
		var err error
		id, err = tx.db.g.CreateNode(ctx, s, o.Labels, o.Properties)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	var labels []string
	for _, l := range o.Labels {
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	return Node{ID: NodeID(id), Labels: labels, Properties: copyProps(o.Properties)}, nil
}

func copyProps(p map[string]value.Value) map[string]value.Value {
	if p == nil {
		return map[string]value.Value{}
	}
	return maps.Clone(p) // values are immutable
}

// GetNode reads node id, or fails with NOT_FOUND.
func (tx *Tx) GetNode(id NodeID) (Node, error) {
	var n graph.Node
	err := tx.read("tacdb.GetNode", func(r engine.Reader) error {
		var err error
		n, err = tx.db.g.GetNode(ctx, r, uint64(id))
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return Node{ID: id, Labels: n.Labels, Properties: n.Properties}, nil
}

// DeleteNode removes node id. A node with edges fails with
// INVALID_ARGUMENT, unless detach removes the edges too.
func (tx *Tx) DeleteNode(id NodeID, detach bool) error {
	return tx.write("tacdb.DeleteNode", func(s *engine.Stmt) error {
		return tx.db.g.DeleteNode(ctx, s, uint64(id), detach)
	})
}

// AddLabel adds label to node id; a label it has is a no-op.
func (tx *Tx) AddLabel(id NodeID, label string) error {
	return tx.write("tacdb.AddLabel", func(s *engine.Stmt) error {
		return tx.db.g.AddLabel(ctx, s, uint64(id), label)
	})
}

// RemoveLabel removes label from node id; a label it lacks is a no-op.
func (tx *Tx) RemoveLabel(id NodeID, label string) error {
	return tx.write("tacdb.RemoveLabel", func(s *engine.Stmt) error {
		return tx.db.g.RemoveLabel(ctx, s, uint64(id), label)
	})
}

func (tx *Tx) getProperty(op string, e graph.Entity, id uint64, key string) (value.Value, bool, error) {
	var v value.Value
	var ok bool
	err := tx.read(op, func(r engine.Reader) error {
		var err error
		v, ok, err = tx.db.g.GetProperty(ctx, r, e, id, key)
		return err
	})
	return v, ok, err
}

// GetProperty reads a property of node id. An absent property and a
// stored Null differ: the first gives false.
func (tx *Tx) GetProperty(id NodeID, key string) (value.Value, bool, error) {
	return tx.getProperty("tacdb.GetProperty", graph.NodeEntity, uint64(id), key)
}

// SetProperty writes a property of node id; Null is stored as a value.
func (tx *Tx) SetProperty(id NodeID, key string, v value.Value) error {
	return tx.write("tacdb.SetProperty", func(s *engine.Stmt) error {
		return tx.db.g.SetProperty(ctx, s, graph.NodeEntity, uint64(id), key, v)
	})
}

// RemoveProperty removes a property of node id; an absent one is a no-op.
func (tx *Tx) RemoveProperty(id NodeID, key string) error {
	return tx.write("tacdb.RemoveProperty", func(s *engine.Stmt) error {
		return tx.db.g.RemoveProperty(ctx, s, graph.NodeEntity, uint64(id), key)
	})
}

// CreateEdge creates an edge of type typ between two existing nodes.
// Parallel edges are distinct entities.
func (tx *Tx) CreateEdge(from, to NodeID, typ string, p map[string]value.Value) (Edge, error) {
	var id uint64
	err := tx.write("tacdb.CreateEdge", func(s *engine.Stmt) error {
		var err error
		id, err = tx.db.g.CreateEdge(ctx, s, uint64(from), uint64(to), typ, p)
		return err
	})
	if err != nil {
		return Edge{}, err
	}
	return Edge{ID: EdgeID(id), From: from, To: to, Type: typ, Properties: copyProps(p)}, nil
}

// GetEdge reads edge id, or fails with NOT_FOUND.
func (tx *Tx) GetEdge(id EdgeID) (Edge, error) {
	var e graph.Edge
	err := tx.read("tacdb.GetEdge", func(r engine.Reader) error {
		var err error
		e, err = tx.db.g.GetEdge(ctx, r, uint64(id))
		return err
	})
	if err != nil {
		return Edge{}, err
	}
	return Edge{ID: id, From: NodeID(e.From), To: NodeID(e.To), Type: e.Type, Properties: e.Properties}, nil
}

// DeleteEdge removes edge id.
func (tx *Tx) DeleteEdge(id EdgeID) error {
	return tx.write("tacdb.DeleteEdge", func(s *engine.Stmt) error {
		return tx.db.g.DeleteEdge(ctx, s, uint64(id))
	})
}

// GetEdgeProperty reads a property of edge id.
func (tx *Tx) GetEdgeProperty(id EdgeID, key string) (value.Value, bool, error) {
	return tx.getProperty("tacdb.GetEdgeProperty", graph.EdgeEntity, uint64(id), key)
}

// SetEdgeProperty writes a property of edge id.
func (tx *Tx) SetEdgeProperty(id EdgeID, key string, v value.Value) error {
	return tx.write("tacdb.SetEdgeProperty", func(s *engine.Stmt) error {
		return tx.db.g.SetProperty(ctx, s, graph.EdgeEntity, uint64(id), key, v)
	})
}

// RemoveEdgeProperty removes a property of edge id.
func (tx *Tx) RemoveEdgeProperty(id EdgeID, key string) error {
	return tx.write("tacdb.RemoveEdgeProperty", func(s *engine.Stmt) error {
		return tx.db.g.RemoveProperty(ctx, s, graph.EdgeEntity, uint64(id), key)
	})
}

// Direction selects edges by the side of the node they leave from.
type Direction uint8

// Directions. Both, the zero value, yields each self-loop once.
const (
	Both Direction = iota
	Outgoing
	Incoming
)

// EdgeScanOptions select the edges of a node: by direction and, when
// Types is not empty, by type. Unknown types match nothing.
type EdgeScanOptions struct {
	Direction Direction
	Types     []string
}

// EdgeRef is an edge as the adjacency of a node tells it, without its
// properties.
type EdgeRef struct {
	ID       EdgeID
	From, To NodeID
	Type     string
}

// EdgeCursor walks the edges of a node, grouped by direction and type and
// in order of peer and ID within a group. It belongs to its transaction,
// which it uses on each Next, and has one user at a time.
type EdgeCursor struct {
	tx   *Tx
	c    *graph.EdgeCursor
	cur  EdgeRef
	err  error
	busy bool // err is a TX_IN_USE collision, which the next call retries
}

// Edges returns a cursor over the edges of node id.
func (tx *Tx) Edges(id NodeID, o EdgeScanOptions) (*EdgeCursor, error) {
	dir := graph.Both
	switch o.Direction {
	case Outgoing:
		dir = graph.Out
	case Incoming:
		dir = graph.In
	case Both:
	default:
		return nil, errs.Newf(errs.CodeInvalidArgument, "tacdb.Edges", "direction %d", o.Direction)
	}
	var c *graph.EdgeCursor
	err := tx.read("tacdb.Edges", func(r engine.Reader) error {
		var err error
		c, err = tx.db.g.Edges(ctx, r, uint64(id), dir, o.Types)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &EdgeCursor{tx: tx, c: c}, nil
}

// Next moves to the next edge and reports whether there is one. At the
// end, or on an error that Err returns, it returns false.
func (c *EdgeCursor) Next() bool {
	if c.busy {
		c.err, c.busy = nil, false
	}
	if c.err != nil {
		return false
	}
	if c.err = c.tx.enter("tacdb.EdgeCursor.Next"); c.err != nil {
		c.busy = true
		return false
	}
	defer c.tx.leave()
	if !c.c.Next(ctx) {
		c.err = c.c.Err()
		return false
	}
	e := c.c.Edge()
	c.cur = EdgeRef{ID: EdgeID(e.ID), From: NodeID(e.From), To: NodeID(e.To), Type: e.Type}
	return true
}

// Edge returns the current edge.
func (c *EdgeCursor) Edge() EdgeRef { return c.cur }

// Err returns the error that ended the walk, if any.
func (c *EdgeCursor) Err() error { return c.err }

// Close ends the cursor; it is idempotent. It fails with TX_IN_USE while
// another call uses the transaction.
func (c *EdgeCursor) Close() error {
	if err := c.tx.enter("tacdb.EdgeCursor.Close"); err != nil {
		return err
	}
	defer c.tx.leave()
	c.c.Close()
	return nil
}
