package tacdb

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

// NodeID identifies a node: opaque, above zero, never reused.
type NodeID uint64

// EdgeID identifies an edge: opaque, above zero, never reused.
type EdgeID uint64

// CommitID is the number of a commit; snapshots are named by the commit
// they read.
type CommitID uint64

// CommitToken identifies a commit for CommitStatus.
type CommitToken [16]byte

// CommitReceipt is the answer of a commit (SPEC §9.2). Token is zero when
// the transaction ended without a new commit.
type CommitReceipt struct {
	ID      CommitID
	Token   CommitToken
	Durable bool
}

// CommitState is what a handle knows of a commit token.
type CommitState int

// Commit states.
const (
	// CommitUnknown: never seen by this handle, forgotten after a
	// checkpoint, or lost in a failure.
	CommitUnknown  CommitState = iota
	CommitPending              // sealed, not yet in the log
	CommitVolatile             // in the log and visible in Async, not yet durable
	CommitDurable
)

// Node is a node with its labels, in insertion order, and properties. It
// belongs to the caller.
type Node struct {
	ID         NodeID
	Labels     []string
	Properties map[string]value.Value
}

// Edge is an edge with its type and properties. It belongs to the caller.
type Edge struct {
	ID         EdgeID
	From, To   NodeID
	Type       string
	Properties map[string]value.Value
}

// CreateNodeOptions describe a new node. Repeated labels count once.
type CreateNodeOptions struct {
	Labels     []string
	Properties map[string]value.Value
}

// DB is a database handle, safe for concurrent use.
type DB struct {
	e  *engine.DB
	fs vfs.FS
	g  *graph.Graph

	mu     sync.Mutex // serializes Close
	closed bool
}

// Open opens the database at path, creating it when opts.Create is set.
// Its log lives next to it, in path + "-wal".
func Open(path string, opts Options) (*DB, error) {
	fs, err := vfs.OpenDir(filepath.Dir(path), vfs.Options{})
	if err != nil {
		return nil, err
	}
	db, err := open(fs, filepath.Base(path), opts)
	if err != nil {
		fs.Close()
	}
	return db, err
}

// OpenMemory opens a new database that lives in memory and is lost on
// Close. It has the log, durability and recovery of a file database.
func OpenMemory(opts Options) (*DB, error) {
	opts.Create, opts.ReadOnly = true, false
	return open(vfs.NewMemFS(), "memory.tacdb", opts)
}

func open(fs vfs.FS, name string, opts Options) (*DB, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	e, err := engine.Open(context.Background(), fs, name, opts.engine())
	if err != nil {
		return nil, err
	}
	g := &graph.Graph{
		MaxSymbols:    opts.MaxSymbolsPerNamespace,
		MaxLabels:     opts.MaxLabelsPerNode,
		MaxProperties: opts.MaxPropertiesPerEntity,
		Limits:        opts.ValueLimits,
		Symbols:       catalog.NewCache(symbolCacheSize),
	}
	return &DB{e: e, fs: fs, g: g}, nil
}

// symbolCacheSize bounds the symbols a handle remembers per direction.
const symbolCacheSize = 1 << 16

// BeginRead opens a read transaction over the latest visible commit.
func (db *DB) BeginRead(ctx context.Context) (*Tx, error) {
	r, err := db.e.BeginRead(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, r: r}, nil
}

// BeginReadDurable opens a read transaction over the latest durable
// commit.
func (db *DB) BeginReadDurable(ctx context.Context) (*Tx, error) {
	r, err := db.e.BeginReadDurable(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, r: r}, nil
}

// BeginWrite waits in the FIFO writer queue, up to BusyTimeout, for the
// writer slot.
func (db *DB) BeginWrite(ctx context.Context) (*Tx, error) {
	w, err := db.e.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, w: w}, nil
}

// TryBeginWrite takes the writer slot only if it is free now; otherwise
// it fails with DB_BUSY.
func (db *DB) TryBeginWrite(ctx context.Context) (*Tx, error) {
	w, err := db.e.TryBeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, w: w}, nil
}

// View runs fn in a read transaction and closes it after.
func (db *DB) View(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.BeginRead(ctx)
	if err != nil {
		return err
	}
	tx.managed = true
	defer func() {
		tx.acquire() // a call fn left running ends first
		tx.r.Close()
		tx.leave()
	}()
	return fn(tx)
}

// Update runs fn in a write transaction and commits it only if fn returns
// nil. An error or panic of fn rolls it back, and the panic goes on. A
// COMMIT_PENDING or COMMIT_UNKNOWN error keeps its token: the commit may
// still happen, and fn is never run again.
func (db *DB) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		return err
	}
	tx.managed = true
	ok := false
	held := false
	defer func() {
		if !held {
			tx.acquire()
		}
		if !ok {
			tx.w.Rollback() // after a sealed commit, a no-op
		}
		tx.leave()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	tx.acquire() // a call fn left running ends before the commit
	held = true
	if _, err := tx.w.Commit(ctx); err != nil {
		return err
	}
	ok = true
	return nil
}

// Flush waits until every commit sealed so far is durable.
func (db *DB) Flush(ctx context.Context) error { return db.e.Flush(ctx) }

// WaitDurable waits until commit id is durable.
func (db *DB) WaitDurable(ctx context.Context, id CommitID) error {
	return db.e.WaitDurable(ctx, uint64(id))
}

// Checkpoint copies durable commits to the main file and removes the log
// segments no snapshot needs.
func (db *DB) Checkpoint(ctx context.Context) error { return db.e.Checkpoint(ctx) }

// CommitStatus returns what the handle knows of token.
func (db *DB) CommitStatus(token CommitToken) (CommitState, error) {
	db.mu.Lock()
	closed := db.closed
	db.mu.Unlock()
	if closed {
		return CommitUnknown, errs.New(errs.CodeClosed, "tacdb.CommitStatus", "database closed")
	}
	return CommitState(db.e.CommitStatus(token)), nil
}

// Close makes the sealed commits durable and closes the handle. With
// transactions open it fails with BUSY and the handle stays open. It is
// idempotent once it succeeds.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	err := db.e.CloseIdle()
	if errs.CodeOf(err) == errs.CodeBusy {
		return err
	}
	db.closed = true
	if ferr := db.fs.Close(); err == nil {
		err = ferr
	}
	return err
}
