package tacdb

import (
	"context"
	"maps"
	"slices"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/budget"
	"github.com/TacFlow/tacdb/internal/query/exec"
	"github.com/TacFlow/tacdb/internal/query/parser"
	"github.com/TacFlow/tacdb/internal/query/plan"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/internal/query/sema"
	"github.com/TacFlow/tacdb/value"
)

// Params are the parameters of a query, by name without the $. Query
// copies them: the caller may change the map afterwards.
type Params map[string]value.Value

// QueryOptions change how a query runs. It has no field yet; the search
// mode of SPEC §31.1 arrives with vector search.
type QueryOptions struct{}

// QueryType is the type of a query value or column.
type QueryType uint8

// Query types. Any is a column whose type the analysis does not know.
const (
	TypeAny QueryType = iota
	TypeNull
	TypeBool
	TypeInt
	TypeFloat
	TypeString
	TypeBytes
	TypeVector
	TypeList
	TypeMap
	TypeNode
	TypeEdge
	TypePath
)

var typeNames = [...]string{"Any", "Null", "Bool", "Int", "Float", "String", "Bytes", "Vector", "List", "Map", "Node", "Edge", "Path"}

func (t QueryType) String() string {
	if int(t) < len(typeNames) {
		return typeNames[t]
	}
	return "QueryType(" + string(rune('0'+t)) + ")"
}

// Column is a column of a query result. Type is Any when the analysis does
// not know a single type; Nullable reports whether the column may hold
// Null.
type Column struct {
	Name     string
	Type     QueryType
	Nullable bool
}

func columnOf(c sema.Column) Column {
	col := Column{Name: c.Name, Nullable: c.Type&qv.TNull != 0 || c.Type&^qv.TNull == 0}
	if k, ok := c.Type.Known(); ok {
		col.Type = typeOf(k)
	}
	return col
}

func typeOf(k qv.Kind) QueryType {
	switch k {
	case qv.KindNull:
		return TypeNull
	case qv.KindBool:
		return TypeBool
	case qv.KindInt:
		return TypeInt
	case qv.KindFloat:
		return TypeFloat
	case qv.KindString:
		return TypeString
	case qv.KindBytes:
		return TypeBytes
	case qv.KindVector:
		return TypeVector
	case qv.KindList:
		return TypeList
	case qv.KindMap:
		return TypeMap
	case qv.KindNode:
		return TypeNode
	case qv.KindEdge:
		return TypeEdge
	case qv.KindPath:
		return TypePath
	}
	return TypeAny
}

// Path is a path of a query result: its nodes, and the edges between
// them, one fewer.
type Path struct {
	Nodes []Node
	Edges []Edge
}

// QueryValue is a value of a query result: a property value, a node, an
// edge or a path, materialized with their labels and properties, or a
// List or Map of them (SPEC §31.1). A property value never holds a node,
// an edge or a path; ToValue converts and refuses them. The zero
// QueryValue is Null.
type QueryValue struct {
	t    QueryType
	v    value.Value // Bool to Vector
	node *Node
	edge *Edge
	path *Path
	list []QueryValue
	m    map[string]QueryValue
}

// Type returns the type of q; Null for the zero QueryValue.
func (q QueryValue) Type() QueryType {
	if q.t == TypeAny {
		return TypeNull
	}
	return q.t
}

// IsNull reports whether q is Null.
func (q QueryValue) IsNull() bool { return q.Type() == TypeNull }

// Value returns q as a property value, and false for a node, an edge, a
// path, or a List or Map that holds one.
func (q QueryValue) Value() (value.Value, bool) {
	v, err := q.ToValue()
	return v, err == nil
}

// ToValue converts q to a property value. A node, an edge or a path, at
// any depth, is TYPE_MISMATCH (SPEC §31.1).
func (q QueryValue) ToValue() (value.Value, error) {
	switch q.Type() {
	case TypeNull:
		return value.Null(), nil
	case TypeNode, TypeEdge, TypePath:
		return value.Value{}, errs.Newf(errs.CodeTypeMismatch, "tacdb.QueryValue", "a %v is not a property value", q.Type())
	case TypeList:
		items := make([]value.Value, len(q.list))
		for i, it := range q.list {
			v, err := it.ToValue()
			if err != nil {
				return value.Value{}, err
			}
			items[i] = v
		}
		return value.List(items...), nil
	case TypeMap:
		m := make(map[string]value.Value, len(q.m))
		for k, it := range q.m {
			v, err := it.ToValue()
			if err != nil {
				return value.Value{}, err
			}
			m[k] = v
		}
		return value.MapOf(m), nil
	}
	return q.v, nil
}

// Node returns the node q holds.
func (q QueryValue) Node() (Node, bool) {
	if q.node == nil {
		return Node{}, false
	}
	return *q.node, true
}

// Edge returns the edge q holds.
func (q QueryValue) Edge() (Edge, bool) {
	if q.edge == nil {
		return Edge{}, false
	}
	return *q.edge, true
}

// Path returns the path q holds.
func (q QueryValue) Path() (Path, bool) {
	if q.path == nil {
		return Path{}, false
	}
	return *q.path, true
}

// List returns the items of a List.
func (q QueryValue) List() ([]QueryValue, bool) { return q.list, q.t == TypeList }

// Map returns the entries of a Map.
func (q QueryValue) Map() (map[string]QueryValue, bool) { return q.m, q.t == TypeMap }

// clone copies q deeply, so that it owns its slices and maps.
func (q QueryValue) clone() QueryValue {
	c := q
	switch q.t {
	case TypeNode:
		n := cloneNode(*q.node)
		c.node = &n
	case TypeEdge:
		e := cloneEdge(*q.edge)
		c.edge = &e
	case TypePath:
		p := Path{Nodes: make([]Node, len(q.path.Nodes)), Edges: make([]Edge, len(q.path.Edges))}
		for i, n := range q.path.Nodes {
			p.Nodes[i] = cloneNode(n)
		}
		for i, e := range q.path.Edges {
			p.Edges[i] = cloneEdge(e)
		}
		c.path = &p
	case TypeList:
		c.list = make([]QueryValue, len(q.list))
		for i, it := range q.list {
			c.list[i] = it.clone()
		}
	case TypeMap:
		c.m = make(map[string]QueryValue, len(q.m))
		for k, it := range q.m {
			c.m[k] = it.clone()
		}
	}
	return c
}

func cloneNode(n Node) Node {
	n.Labels = slices.Clone(n.Labels)
	n.Properties = maps.Clone(n.Properties)
	return n
}

func cloneEdge(e Edge) Edge {
	e.Properties = maps.Clone(e.Properties)
	return e
}

// QueryStats are the counters of a query (SPEC §31.1).
type QueryStats struct {
	Rows      uint64   // rows returned
	Examined  uint64   // entities scanned and edges read
	Snapshot  CommitID // the commit the query reads
	Durable   bool     // the snapshot is durable
	Complete  bool     // every row was returned, without error
	PlanMode  string   // "syntactic" until the planner by cost
	WorkUnits uint64
}

// queryLimits are the query options of a handle.
type queryLimits struct {
	parse   parser.Limits
	sema    sema.Config
	plan    plan.Config
	exec    exec.Config
	budget  budget.Limits
	maxPars int
}

func (o Options) queries() queryLimits {
	return queryLimits{
		parse:   parser.Limits{MaxQueryBytes: o.MaxQueryBytes, MaxDepth: o.MaxQueryDepth},
		sema:    sema.Config{MaxBindings: o.MaxBindings, MaxParams: o.MaxParams},
		plan:    plan.Config{MaxHops: o.MaxHops},
		exec:    exec.Config{MaxExpansions: o.MaxExpansions},
		budget:  budget.Limits{MemoryBytes: o.QueryMemoryBytes, WorkUnits: o.QueryWorkUnits, Values: o.ValueLimits},
		maxPars: o.MaxParams,
	}
}

// Rows is the stream of rows of a query. It belongs to the transaction of
// the query, which it uses on each call, and has one user at a time. Next
// returning false requires a look at Err; the end and an error release
// the cursor, and Close, which is idempotent, does too. A Rows of
// DB.Query owns its read transaction and closes it; a Rows of Tx.Query
// never closes the transaction. While a Rows is open, Commit of its
// transaction and the writes in it fail with BUSY; Rollback closes it,
// and it then reports TX_CLOSED.
type Rows struct {
	tx    *Tx
	own   bool
	x     *exec.Exec
	b     *budget.Budget
	cols  []Column
	n     int // columns
	cur   []QueryValue
	bytes int64 // the materialized current row, charged to b
	err   error
	busy  bool // err is a TX_IN_USE collision, which the next call retries
	open  bool
	stats QueryStats
}

// Query runs a read query in a new read transaction, which the returned
// Rows owns. A query that writes is QUERY_WRITE_REQUIRES_EXEC.
func (db *DB) Query(ctx context.Context, text string, p Params) (*Rows, error) {
	return db.QueryWithOptions(ctx, text, p, QueryOptions{})
}

// QueryWithOptions is Query with options.
func (db *DB) QueryWithOptions(ctx context.Context, text string, p Params, o QueryOptions) (*Rows, error) {
	tx, err := db.BeginRead(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.query(ctx, text, p, o, true)
	if err != nil {
		tx.r.Close()
		return nil, err
	}
	return rows, nil
}

// Query runs a read query in tx. The query reads the snapshot of tx, and,
// in a write transaction, its writes so far.
func (tx *Tx) Query(ctx context.Context, text string, p Params) (*Rows, error) {
	return tx.query(ctx, text, p, QueryOptions{}, false)
}

// QueryWithOptions is Query with options.
func (tx *Tx) QueryWithOptions(ctx context.Context, text string, p Params, o QueryOptions) (*Rows, error) {
	return tx.query(ctx, text, p, o, false)
}

func (tx *Tx) query(ctx context.Context, text string, p Params, _ QueryOptions, own bool) (*Rows, error) {
	const op = "tacdb.Query"
	if err := tx.enter(op); err != nil {
		return nil, err
	}
	defer tx.leave()
	if tx.closed {
		return nil, errs.New(errs.CodeTxClosed, op, "transaction closed")
	}
	r := tx.reader()
	if err := r.Check(); err != nil {
		return nil, err
	}
	lim := tx.db.q
	q, err := parser.Parse(text, lim.parse)
	if err != nil {
		return nil, err
	}
	info, err := sema.Analyze(q, text, lim.sema)
	if err != nil {
		return nil, err
	}
	pl, err := plan.Build(q, info, text, lim.plan)
	if err != nil {
		return nil, err
	}
	if err := info.CheckParams(p, lim.maxPars); err != nil {
		return nil, err
	}
	b, err := budget.New(ctx, tx.db.e.Governor(), lim.budget)
	if err != nil {
		return nil, err
	}
	rows := &Rows{tx: tx, own: own, b: b, n: len(pl.Columns), open: true}
	for _, c := range pl.Columns {
		rows.cols = append(rows.cols, columnOf(c))
	}
	rows.stats.Snapshot, rows.stats.PlanMode = tx.Snapshot(), "syntactic"
	if tx.r != nil {
		rows.stats.Durable = tx.r.Durable()
	}
	rows.x = exec.Start(ctx, pl, exec.Source{G: tx.db.g, R: r}, maps.Clone(map[string]value.Value(p)), b, lim.exec)
	if tx.cursors == nil {
		tx.cursors = map[*Rows]struct{}{}
	}
	tx.cursors[rows] = struct{}{}
	return rows, nil
}

// Columns returns the columns of the result; the slice belongs to the
// caller.
func (r *Rows) Columns() []Column { return slices.Clone(r.cols) }

// Next moves to the next row and reports whether there is one.
func (r *Rows) Next() bool {
	if r.busy {
		r.err, r.busy = nil, false
	}
	if r.err != nil || !r.open {
		return false
	}
	if r.err = r.tx.enter("tacdb.Rows.Next"); r.err != nil {
		r.busy = true
		return false
	}
	defer r.tx.leave()
	row, ok := r.x.Next()
	if !ok {
		r.err = r.x.Err()
		r.stats.Complete = r.err == nil
		r.finish()
		return false
	}
	vals := make([]QueryValue, r.n)
	var bytes int64
	reader := r.tx.reader()
	for i := range vals {
		v, n, err := r.tx.db.materialize(reader, row[i])
		if err != nil {
			r.err = err
			r.finish()
			return false
		}
		vals[i], bytes = v, bytes+n
	}
	r.b.Shrink(r.bytes)
	r.bytes = 0
	if err := r.b.Grow(bytes); err != nil {
		r.err = err
		r.finish()
		return false
	}
	r.cur, r.bytes = vals, bytes
	return true
}

// Values returns the current row; it is valid until the next call of
// Next.
func (r *Rows) Values() []QueryValue { return r.cur }

// CloneValues returns a copy of the current row that the caller owns.
func (r *Rows) CloneValues() []QueryValue {
	out := make([]QueryValue, len(r.cur))
	for i, v := range r.cur {
		out[i] = v.clone()
	}
	return out
}

// Err returns the error that ended the rows, if any.
func (r *Rows) Err() error { return r.err }

// Stats returns the counters of the query so far.
func (r *Rows) Stats() QueryStats {
	s := r.stats
	if r.x != nil {
		st := r.x.Stats()
		s.Rows, s.Examined = st.Rows, st.Examined
	}
	if r.b != nil {
		s.WorkUnits = r.b.WorkDone()
	}
	return s
}

// Close releases the rows; it is idempotent. It fails with TX_IN_USE
// while another call uses the transaction.
func (r *Rows) Close() error {
	if !r.open {
		return nil
	}
	if err := r.tx.enter("tacdb.Rows.Close"); err != nil {
		return err
	}
	defer r.tx.leave()
	r.finish()
	return nil
}

// finish releases the execution, its memory and, when the rows own it,
// the read transaction. The caller holds the transaction.
func (r *Rows) finish() {
	if !r.open {
		return
	}
	r.open = false
	r.x.Close()
	r.stats.Rows, r.stats.Examined = r.x.Stats().Rows, r.x.Stats().Examined
	r.stats.WorkUnits = r.b.WorkDone()
	r.b.Close()
	r.cur = nil
	delete(r.tx.cursors, r)
	if r.own {
		r.tx.closed = true
		r.tx.r.Close()
	}
}

// closeCursors ends the open rows of tx with err; the caller holds tx.
func (tx *Tx) closeCursors(code errs.Code) {
	for r := range tx.cursors {
		r.finish()
		r.err = errs.New(code, "tacdb.Rows", "transaction closed")
	}
}

// openCursors fails with BUSY while tx has an open Rows.
func (tx *Tx) openCursors(op string) error {
	if len(tx.cursors) > 0 {
		return errs.Newf(errs.CodeBusy, op, "%d query results are still open in the transaction", len(tx.cursors))
	}
	return nil
}

// materialize converts a runtime value to a query value, reading nodes
// and edges, and returns what it costs.
func (db *DB) materialize(r engine.Reader, v qv.Value) (QueryValue, int64, error) {
	switch v.Kind() {
	case qv.KindNull:
		return QueryValue{}, qv.ElementBytes, nil
	case qv.KindNode:
		n, err := db.g.GetNode(ctx, r, v.ID())
		if err != nil {
			return QueryValue{}, 0, err
		}
		node := Node{ID: NodeID(n.ID), Labels: n.Labels, Properties: n.Properties}
		return QueryValue{t: TypeNode, node: &node}, entityBytes(node.Labels, node.Properties), nil
	case qv.KindEdge:
		e, err := db.g.GetEdge(ctx, r, v.ID())
		if err != nil {
			return QueryValue{}, 0, err
		}
		edge := Edge{ID: EdgeID(e.ID), From: NodeID(e.From), To: NodeID(e.To), Type: e.Type, Properties: e.Properties}
		return QueryValue{t: TypeEdge, edge: &edge}, entityBytes([]string{edge.Type}, edge.Properties), nil
	case qv.KindPath:
		p := v.Path()
		path := &Path{}
		var total int64
		for _, id := range p.Nodes {
			q, n, err := db.materialize(r, qv.Node(id))
			if err != nil {
				return QueryValue{}, 0, err
			}
			path.Nodes, total = append(path.Nodes, *q.node), total+n
		}
		for _, id := range p.Edges {
			q, n, err := db.materialize(r, qv.Edge(id))
			if err != nil {
				return QueryValue{}, 0, err
			}
			path.Edges, total = append(path.Edges, *q.edge), total+n
		}
		return QueryValue{t: TypePath, path: path}, total, nil
	case qv.KindList:
		items := v.Items()
		out := QueryValue{t: TypeList, list: make([]QueryValue, len(items))}
		total := int64(qv.ElementBytes)
		for i, it := range items {
			q, n, err := db.materialize(r, it)
			if err != nil {
				return QueryValue{}, 0, err
			}
			out.list[i], total = q, total+n
		}
		return out, total, nil
	case qv.KindMap:
		out := QueryValue{t: TypeMap, m: make(map[string]QueryValue, v.Len())}
		total := int64(qv.ElementBytes)
		for i := 0; i < v.Len(); i++ {
			k, it := v.EntryAt(i)
			q, n, err := db.materialize(r, it)
			if err != nil {
				return QueryValue{}, 0, err
			}
			out.m[k], total = q, total+n+int64(len(k))
		}
		return out, total, nil
	}
	return QueryValue{t: typeOf(v.Kind()), v: v.Scalar()}, v.Size(), nil
}

// entityBytes is what a materialized node or edge costs.
func entityBytes(labels []string, props map[string]value.Value) int64 {
	n := int64(qv.ElementBytes)
	for _, l := range labels {
		n += int64(len(l)) + 16
	}
	for k, v := range props {
		n += int64(len(k)) + qv.FromValue(v).Size()
	}
	return n
}
