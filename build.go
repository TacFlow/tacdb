package tacdb

import (
	"context"
	"sync"
	"time"

	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/index"
)

// IndexState is the state of an index (SPEC §12.2).
type IndexState uint8

// Index states. Only ONLINE indexes answer queries.
const (
	IndexBuilding IndexState = iota
	IndexOnline
	IndexFailed
)

// IndexBuildStatus is what an index's build reached (SPEC §12.2).
type IndexBuildStatus struct {
	Name      string
	State     IndexState
	Scanned   uint64 // entries the batches inserted
	Estimated uint64 // entities of the kind when the build started
	StartedAt CommitID
	// Failure is the cause of a FAILED build: UNIQUE_VIOLATION with the
	// first pairs of entities in Conflicts, INDEX_INCONSISTENT, or the
	// error of a value the index refuses.
	Failure   *Error
	Conflicts [][2]uint64

	id uint64
}

// DefaultIndexBuildBatchBytes is the default of IndexBuildBatchBytes.
const DefaultIndexBuildBatchBytes = 4 << 20

// BuildIndex builds an index online (SPEC §12.2): a first commit declares
// it BUILDING, after which every writer keeps it; batches of at most
// IndexBuildBatchBytes, each a short write transaction in the FIFO queue
// that yields to other writers, fill it by ascending entity ID; a last
// commit checks its counts and publishes it ONLINE, or FAILED. A crash
// resumes from the last batch when a write handle opens. BuildIndex waits
// for the end of the build, or for ctx: then the build goes on and
// IndexStatus follows it. A close of the handle ends the wait with CLOSED,
// a drop of the index with INDEX_NOT_FOUND. Calling it again for the same
// declaration waits for the same build; DropIndex cancels it.
func (db *DB) BuildIndex(ctx context.Context, def IndexDefinition) (IndexBuildStatus, error) {
	const op = "tacdb.BuildIndex"
	if def.Kind != PropertyIndex {
		return IndexBuildStatus{}, errs.Newf(errs.CodeInvalidArgument, op, "index kind %d", def.Kind)
	}
	if def.Entity > EdgeEntity {
		return IndexBuildStatus{}, errs.Newf(errs.CodeInvalidArgument, op, "entity kind %d", def.Entity)
	}
	d := graph.IndexDef{Name: def.Name, Entity: graph.Entity(def.Entity), Scope: def.Scope, Property: def.Property, Unique: def.Unique}
	var ix *index.Index
	err := db.Update(ctx, func(tx *Tx) error {
		return tx.write(op, func(s *engine.Stmt) error {
			var err error
			ix, err = db.g.StartBuild(context.Background(), s, d)
			return err
		})
	})
	if err != nil {
		return IndexBuildStatus{}, err
	}
	for ix.State == index.Building {
		if done := db.work.build(ix.ID); done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				st, _ := db.statusOf(ix.ID, def.Name)
				return st, ctxErr(ctx, op)
			}
		}
		// The builder stopped: at the end of the build, for Close, or on an
		// error of the handle. Close holds db.mu until it closes the handle
		// or starts the workers again, and its check refuses new
		// transactions with SHUTTING_DOWN meanwhile: wait for it, then read
		// the status, which fails with CLOSED on a closed handle.
		db.mu.Lock()
		db.mu.Unlock()
		st, err := db.statusOf(ix.ID, def.Name)
		if code := errs.CodeOf(err); code == errs.CodeShuttingDown || retry(err) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil || st.State != IndexBuilding {
			return st, err
		}
		time.Sleep(time.Millisecond) // the workers start again
	}
	return db.statusOf(ix.ID, def.Name)
}

// statusOf is IndexStatus of the index called name, which must still be
// the one with ID id: a build dropped and declared again under its name is
// INDEX_NOT_FOUND for the callers that waited for the old one.
func (db *DB) statusOf(id uint64, name string) (IndexBuildStatus, error) {
	st, err := db.IndexStatus(name)
	if err != nil {
		return st, err
	}
	if st.id != id {
		return IndexBuildStatus{}, errs.Newf(errs.CodeIndexNotFound, "tacdb.BuildIndex", "the index %q was dropped", name)
	}
	return st, nil
}

func ctxErr(ctx context.Context, op string) error {
	if ctx.Err() == context.DeadlineExceeded {
		return errs.Wrap(errs.CodeDeadline, op, ctx.Err(), "context ended")
	}
	return errs.Wrap(errs.CodeCanceled, op, ctx.Err(), "context ended")
}

// IndexStatus returns the state of the index called name in the latest
// commit; a missing one is INDEX_NOT_FOUND.
func (db *DB) IndexStatus(name string) (IndexBuildStatus, error) {
	var out IndexBuildStatus
	err := db.View(context.Background(), func(tx *Tx) error {
		set, err := db.g.Indexes.Get(context.Background(), tx.r)
		if err != nil {
			return err
		}
		ix, ok := set.Named(name)
		if !ok {
			return errs.Newf(errs.CodeIndexNotFound, "tacdb.IndexStatus", "no index %q", name)
		}
		out = IndexBuildStatus{Name: name, State: IndexState(ix.State), id: ix.ID}
		st, ok, err := index.ReadBuild(context.Background(), tx.r, ix)
		if err != nil || !ok {
			return err // an index made by CreateIndex has no build
		}
		out.Scanned, out.Estimated, out.StartedAt = st.Scanned, st.Estimated, CommitID(st.Started)
		if st.Failure != "" {
			out.Failure = errs.New(errs.Code(st.Failure), "tacdb.BuildIndex", st.Message)
		}
		for i := 0; i+1 < len(st.Conflicts); i += 2 {
			out.Conflicts = append(out.Conflicts, [2]uint64{st.Conflicts[i], st.Conflicts[i+1]})
		}
		return nil
	})
	return out, err
}

// workers run the builds and purges of a write handle in the background.
// They stop for Close and start again when it fails with BUSY or a handle
// opens; their state is in the database, so they resume where they were.
type workers struct {
	db         *DB
	batchBytes int64
	purgeBatch int

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	builds  map[uint64]chan struct{} // the done channel of each running build
	purging bool
	again   bool // a purge was scheduled while the purger ran
	retries bool // a resume waits to try again

	emptyHook func() // tests: runs after the purger found nothing to purge
}

// start runs the builds and purges the database holds.
func (w *workers) start() {
	w.mu.Lock()
	if w.cancel == nil {
		w.ctx, w.cancel = context.WithCancel(context.Background())
		w.builds = map[uint64]chan struct{}{}
	}
	w.mu.Unlock()
	w.resume()
}

// stop ends the workers and waits for them.
func (w *workers) stop() {
	w.mu.Lock()
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.wg.Wait()
}

// resume starts a worker for each build and purge of the latest commit
// that has none. When the handle cannot read now (its transactions are at
// MaxActiveTransactions, for one), it tries again in the background.
func (w *workers) resume() {
	if err := w.scan(); retry(err) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.cancel == nil || w.retries {
			return
		}
		w.retries = true
		ctx := w.ctx
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for ctx.Err() == nil {
				time.Sleep(time.Millisecond)
				if err := w.scan(); !retry(err) {
					break
				}
			}
			w.mu.Lock()
			w.retries = false
			w.mu.Unlock()
		}()
	}
}

// scan starts the workers the latest commit needs.
func (w *workers) scan() error {
	var ids []uint64
	var purges bool
	err := w.db.View(context.Background(), func(tx *Tx) error {
		set, err := w.db.g.Indexes.Get(context.Background(), tx.r)
		if err != nil {
			return err
		}
		for _, ix := range set.All() {
			if ix.State == index.Building {
				ids = append(ids, ix.ID)
			}
		}
		ps, err := index.Purges(context.Background(), tx.r)
		purges = len(ps) > 0
		return err
	})
	if err != nil {
		return err // closing or failed: nothing to resume; or busy: try again
	}
	for _, id := range ids {
		w.build(id)
	}
	if purges {
		w.purge()
	}
	return nil
}

// build starts the build of index id unless it runs, and returns its done
// channel; nil when the workers are stopped.
func (w *workers) build(id uint64) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel == nil {
		return nil
	}
	if done, ok := w.builds[id]; ok {
		return done
	}
	done := make(chan struct{})
	w.builds[id] = done
	w.wg.Add(1)
	go w.runBuild(w.ctx, id, done)
	return done
}

// purge starts the purger, or asks the running one for another pass.
func (w *workers) purge() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel == nil {
		return
	}
	if w.purging {
		w.again = true
		return
	}
	w.purging = true
	w.wg.Add(1)
	go w.runPurge(w.ctx)
}

// yield waits while writers wait for the writer slot: background batches
// give way to the application between transactions.
func (w *workers) yield(ctx context.Context) {
	for w.db.e.Waiting() > 0 && ctx.Err() == nil {
		time.Sleep(100 * time.Microsecond)
	}
}

// statement runs fn in its own write transaction and commits it. retry
// reports an error worth another attempt: the queue, the timeout of a
// transaction or of the queue, the log limit.
func (w *workers) statement(ctx context.Context, op string, fn func(s *engine.Stmt) error) error {
	tx, err := w.db.e.BeginWrite(ctx)
	if err != nil {
		return err
	}
	if err := tx.Statement(op, fn); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Commit(ctx); err != nil {
		tx.Rollback()
		return err
	}
	return nil
}

func retry(err error) bool {
	switch errs.CodeOf(err) {
	case errs.CodeBusyTimeout, errs.CodeWriterQueueFull, errs.CodeWriteTxTimeout, errs.CodeCommitQueueFull,
		errs.CodeWALLimit, errs.CodeTooManyTransactions, errs.CodeResourceLimit:
		return true
	}
	return false
}

// refused reports an error of the data the build met: the index fails
// with it instead of trying again.
func refused(err error) bool {
	switch errs.CodeOf(err) {
	case errs.CodeInvalidArgument, errs.CodeLimitExceeded, errs.CodeCorrupt, errs.CodeUnsupportedFormat:
		return true
	}
	return false
}

func (w *workers) runBuild(ctx context.Context, id uint64, done chan struct{}) {
	defer w.wg.Done()
	defer func() {
		w.mu.Lock()
		delete(w.builds, id)
		w.mu.Unlock()
		close(done)
	}()
	const op = "tacdb.BuildIndex"
	for ctx.Err() == nil {
		w.yield(ctx)
		var gone, finished bool
		err := w.statement(ctx, op, func(s *engine.Stmt) error {
			ix, end, err := w.db.g.BuildBatch(ctx, s, id, w.batchBytes)
			gone, finished = ix == nil, end
			return err
		})
		switch {
		case refused(err):
			// The batch failed with a value the index refuses: the build
			// fails with it, in a transaction of its own.
			ferr := w.statement(ctx, op, func(s *engine.Stmt) error { return w.db.g.FailBuild(ctx, s, id, err) })
			if ferr == nil {
				w.purge()
				return
			}
			err = ferr
		case err != nil:
		case gone:
			return
		case finished:
			var state index.State
			err = w.statement(ctx, op, func(s *engine.Stmt) error {
				var err error
				state, err = w.db.g.FinishBuild(ctx, s, id)
				return err
			})
			if err == nil {
				if state == index.Failed {
					w.purge()
				}
				return
			}
		}
		if err != nil && !retry(err) {
			return // the handle is closing or failed: the build resumes at the next open
		}
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
}

func (w *workers) runPurge(ctx context.Context) {
	defer w.wg.Done()
	ended := false // the loop cleared purging itself
	defer func() {
		if !ended {
			w.mu.Lock()
			w.purging = false
			w.mu.Unlock()
		}
	}()
	for ctx.Err() == nil {
		var purges []index.Purge
		err := w.db.View(context.Background(), func(tx *Tx) error {
			var err error
			purges, err = index.Purges(ctx, tx.r)
			return err
		})
		if retry(err) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			return
		}
		if len(purges) == 0 {
			w.mu.Lock()
			hook := w.emptyHook
			w.mu.Unlock()
			if hook != nil {
				hook()
			}
			// The check and the end are one step under the lock: a purge
			// scheduled after it starts a new purger.
			w.mu.Lock()
			again := w.again
			w.again = false
			if !again {
				w.purging, ended = false, true
			}
			w.mu.Unlock()
			if !again {
				return
			}
			continue
		}
		for _, p := range purges {
			for done := false; !done && ctx.Err() == nil; {
				w.yield(ctx)
				err := w.statement(ctx, "tacdb.DropIndex", func(s *engine.Stmt) error {
					var err error
					done, err = index.PurgeBatch(ctx, s, p, w.purgeBatch)
					return err
				})
				if err != nil && !retry(err) {
					return
				}
			}
		}
	}
}
