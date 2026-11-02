package engine

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/versions"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Status is what CommitStatus knows of a token (SPEC §9.2).
type Status int

// Commit statuses.
const (
	Unknown  Status = iota // never seen, forgotten after a checkpoint, or lost in a failure
	Pending                // sealed, not yet appended
	Volatile               // appended and published in Async, not yet durable
	Durable
)

// token is what the handle remembers of a commit token.
type token struct {
	status Status
	commit uint64
}

// intent is a sealed transaction on its way to the log.
type intent struct {
	commit, parent uint64
	token          [16]byte
	final          *versions.Final
	res            *govern.Reservation // its sealed images
	bytes          int64               // reserved in the commit queue
	done           chan struct{}       // closed with receipt and err set
	receipt        Receipt
	err            error
}

func (in *intent) finish(durable bool, err error) {
	in.receipt = Receipt{CommitID: in.commit, Token: in.token, Durable: durable}
	in.err = err
	close(in.done)
}

// appended is a group in the log, waiting for a Sync.
type appended struct {
	lsn     uint64 // of its COMMIT
	dir     *versions.Directory
	intents []*intent
	sealed  []*versions.Sealed // the versions placed in the log
}

// coordinator appends sealed intents in groups, in the order of their
// parents, syncs and publishes (SPEC §9.2). One goroutine does it all:
// while it syncs, new intents gather in the queue and form the next group.
type coordinator struct {
	wake      chan struct{}
	quit      chan struct{}
	done      chan struct{}
	flush     atomic.Bool           // a Flush waits
	room      atomic.Bool           // a commit waits for room in the queue
	group     uint64                // last GroupID
	seq, off  atomic.Int64          // where the next record goes
	floor     atomic.Int64          // the lowest segment kept
	ckLSN     atomic.Uint64         // checkpoint_lsn in force, for new segment headers
	ext       atomic.Pointer[error] // a fatal error found outside the coordinator
	inflight  []*intent             // taken from the queue, not yet appended
	unsynced  []*appended
	reserves  []reserveLSN // appended, not yet durable
	sinceSync int64
}

type reserveLSN struct {
	wal.Reserve
	lsn uint64
}

func (c *coordinator) start(db *DB) {
	c.wake = make(chan struct{}, 1)
	c.quit = make(chan struct{})
	c.done = make(chan struct{})
	go c.run(db)
}

func (c *coordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *coordinator) stop() {
	close(c.quit)
	<-c.done
}

// setPosition records where the log ends, for MaxWALBytes and the
// checkpoint trigger, and asks for a checkpoint when the log grew enough.
func (c *coordinator) setPosition(db *DB) {
	p := db.w.Position()
	c.seq.Store(int64(p.Seq))
	c.off.Store(p.Offset)
	if db.mt.due(db) {
		db.mt.signal()
	}
}

// walBytes is the size of the log, counted in segment files: every kept
// segment but the last is SegmentBytes, the last up to its end.
func (c *coordinator) walBytes(db *DB) int64 {
	return (c.seq.Load()-c.floor.Load())*db.opts.WALSegmentBytes + c.off.Load()
}

// abort makes the coordinator fail the handle with err, from another
// goroutine: only the coordinator touches its groups.
func (c *coordinator) abort(err error) {
	c.ext.CompareAndSwap(nil, &err)
	c.signal()
}

func (c *coordinator) run(db *DB) {
	defer close(c.done)
	var tick <-chan time.Time
	if db.opts.Durability == Async {
		t := time.NewTicker(db.opts.AsyncSyncInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		due := false
		select {
		case <-c.wake:
		case <-tick:
			due = true
		case <-c.quit:
			return
		}
		err := c.cycle(db, due)
		if p := c.ext.Load(); p != nil && err == nil {
			err = *p
		}
		if err != nil {
			// A roll may have synced groups before the failing call:
			// they are durable and keep their status.
			c.publish(db)
			db.fail(err)
			return
		}
	}
}

// cycle appends what is waiting and syncs as the mode says, until there
// is nothing left.
func (c *coordinator) cycle(db *DB, due bool) error {
	for {
		if c.ext.Load() != nil {
			return nil // run fails the handle
		}
		db.w.SetCheckpointLSN(c.ckLSN.Load())
		reqs := db.ids.take()
		for _, r := range reqs { // between groups, never inside one
			lsn, err := db.w.AppendReserve(r)
			if err != nil {
				return err
			}
			db.stats.appends.Add(1)
			c.reserves = append(c.reserves, reserveLSN{r, lsn})
		}
		batch := db.takeBatch()
		if len(batch) > 0 {
			c.inflight = batch // until appended, the batch is in neither queue
			if err := c.append(db, batch); err != nil {
				return err
			}
			c.inflight = nil
		}
		c.setPosition(db)
		c.publish(db) // a roll syncs what came before
		groups := len(c.unsynced) > 0
		var sync bool
		switch {
		case db.ids.waiting() && len(c.reserves) > 0:
			sync = true // a writer waits for identifiers
		case !groups:
		case db.opts.Durability != Async:
			sync = true
		default:
			// The requests are taken here, not cleared after the Sync: one
			// made during the Sync is seen by the next cycle.
			flush, room := c.flush.Swap(false), c.room.Swap(false)
			sync = due || flush || room || c.sinceSync >= db.opts.AsyncSyncBytes
		}
		if sync {
			if err := db.w.Sync(); err != nil {
				return err
			}
			db.stats.syncs.Add(1)
			if groups {
				db.stats.groupSyncs.Add(1)
			} else {
				db.stats.idSyncs.Add(1)
			}
			c.sinceSync = 0
			c.publish(db)
		}
		due = false
		if len(reqs) == 0 && len(batch) == 0 && !sync {
			return nil
		}
	}
}

// takeBatch removes the next group from the queue: one intent in Sync and
// Async, up to GroupMaxCommits and GroupMaxBytes in GroupSync, and always
// at least one.
func (db *DB) takeBatch() []*intent {
	db.mu.Lock()
	defer db.mu.Unlock()
	n := 0
	var bytes int64
	for n < len(db.queue) {
		if n > 0 && (db.opts.Durability != GroupSync || n >= db.opts.GroupMaxCommits ||
			bytes+db.queue[n].bytes > db.opts.GroupMaxBytes) {
			break
		}
		bytes += db.queue[n].bytes
		n++
	}
	batch := slices.Clone(db.queue[:n])
	db.queue = db.queue[n:]
	db.unappended -= bytes
	return batch
}

// append writes one group: the BEGINs, one PAGE per PageID with its final
// version, in PageID order, and the COMMIT with the State of the last
// intent. Versions replaced inside the group are never appended.
func (c *coordinator) append(db *DB, batch []*intent) error {
	type ver struct {
		img *page.Buf
		s   *versions.Sealed
	}
	byID := map[page.ID]int{}
	var vers []ver
	for _, in := range batch {
		for i, img := range in.final.Pages {
			id := page.HeaderOf(img).ID()
			if j, ok := byID[id]; ok {
				vers[j] = ver{img, in.final.Sealed[i]}
				db.stats.coalesced.Add(1)
				continue
			}
			byID[id] = len(vers)
			vers = append(vers, ver{img, in.final.Sealed[i]})
		}
	}
	slices.SortFunc(vers, func(a, b ver) int {
		return int(page.HeaderOf(a.img).ID()) - int(page.HeaderOf(b.img).ID())
	})
	last := batch[len(batch)-1]
	c.group++
	g := &wal.Group{ID: c.group}
	var m wal.Manifest
	for _, in := range batch {
		b := wal.Begin{Token: in.token, Parent: in.parent, CommitID: in.commit}
		g.Begins = append(g.Begins, b)
		m.AddBegin(&b)
	}
	sealed := make([]*versions.Sealed, len(vers))
	for i, v := range vers {
		g.Pages = append(g.Pages, v.img)
		sealed[i] = v.s
		m.AddPage(page.HeaderOf(v.img).ID(), v.img)
	}
	g.Commit = wal.Commit{IntentCount: uint32(len(batch)), PageCount: uint32(len(vers)),
		State: last.final.Dir.State, Digest: m.Digest()}
	a, err := db.w.AppendGroup(g)
	if err != nil {
		return err
	}
	for i, s := range sealed {
		s.Place(a.Pages[i])
	}
	db.stats.appends.Add(1)
	db.stats.groups.Add(1)
	db.stats.intents.Add(int64(len(batch)))
	c.sinceSync += int64(len(vers)) * (page.Size + wal.RecordHeaderSize)
	// stagedHead already holds the directory; the published one is a copy
	// that also knows the LSN of its COMMIT, for the checkpoint.
	pub := *last.final.Dir
	pub.CommitLSN = a.CommitLSN
	dir := &pub
	c.unsynced = append(c.unsynced, &appended{lsn: a.CommitLSN, dir: dir, intents: batch, sealed: sealed})
	if db.opts.Durability == Async {
		// Published once appended, with Durable=false (SPEC §9.2 step 8).
		db.mu.Lock()
		db.visible.Store(dir)
		for _, in := range batch {
			db.tokens[in.token] = token{Volatile, in.commit}
			in.finish(false, nil)
		}
		db.mu.Unlock()
	}
	return nil
}

// publish makes durable the groups and reservations the log has synced:
// visibleHead, then durableHead, the receipts and the images.
func (c *coordinator) publish(db *DB) {
	d := db.w.DurableLSN()
	for len(c.reserves) > 0 && c.reserves[0].lsn <= d {
		db.ids.durable(c.reserves[0].Reserve)
		db.mt.noteReserve(c.reserves[0])
		c.reserves = c.reserves[1:]
	}
	n := 0
	for n < len(c.unsynced) && c.unsynced[n].lsn <= d {
		n++
	}
	if n == 0 {
		return
	}
	done := c.unsynced[:n]
	c.unsynced = slices.Clone(c.unsynced[n:])
	head := done[n-1].dir
	db.mu.Lock()
	if db.opts.Durability != Async {
		db.visible.Store(head) // never behind durable
	}
	if h := db.hooks.betweenHeads; h != nil {
		h()
	}
	db.durable.Store(head)
	for _, g := range done {
		for _, s := range g.sealed {
			s.Release() // reads go to the log now
		}
		for _, in := range g.intents {
			db.tokens[in.token] = token{Durable, in.commit}
			in.res.Close()
			db.pending--
			db.pendBytes -= in.bytes
			if db.opts.Durability != Async {
				in.finish(true, nil)
			}
		}
	}
	db.signalLocked()
	db.mu.Unlock()
}

// signalLocked wakes the waits for durability and for queue room.
func (db *DB) signalLocked() {
	close(db.durableCh)
	db.durableCh = make(chan struct{})
	close(db.queueFree)
	db.queueFree = make(chan struct{})
}

// fail makes the handle RECOVERY_REQUIRED after an append or Sync error
// (SPEC §9.2): the queue is drained, every pending commit gets
// COMMIT_UNKNOWN, and the Sync is never retried.
func (db *DB) fail(cause error) {
	err := errs.Wrap(errs.CodeRecoveryRequired, "engine", cause, "the log is in an unknown state")
	db.mu.Lock()
	failed := error(err)
	db.failedErr.CompareAndSwap(nil, &failed)
	var lost []*intent
	for _, g := range db.co.unsynced {
		lost = append(lost, g.intents...)
	}
	lost = append(lost, db.co.inflight...)
	lost = append(lost, db.queue...)
	db.co.unsynced, db.co.inflight, db.queue = nil, nil, nil
	for _, in := range lost {
		db.tokens[in.token] = token{Unknown, in.commit}
		in.res.Close()
		select {
		case <-in.done: // a volatile receipt of Async
		default:
			in.finish(false, errs.Wrap(errs.CodeCommitUnknown, "engine.Commit", cause, "the log failed before the commit was durable"))
		}
	}
	db.pending, db.pendBytes, db.unappended = 0, 0, 0
	db.signalLocked()
	db.mu.Unlock()
	db.ids.fail(err)
}

// reserveQueue takes room for a commit in the queue of commits not yet
// durable and checks MaxWALBytes (SPEC §9.2 step 2). It waits for room,
// unless wait is false; a commit alone always fits.
func (db *DB) reserveQueue(ctx context.Context, bytes int64, wait bool) error {
	const op = "engine.Commit"
	var ev *Event
	defer func() { // after the unlock below
		if ev != nil {
			db.emit(*ev)
		}
	}()
	db.mu.Lock()
	defer db.mu.Unlock()
	relieved := false
	for {
		if err := db.usable(op); err != nil {
			return err
		}
		if db.co.walBytes(db)+db.unappended+bytes > db.opts.MaxWALBytes {
			if !relieved && wait {
				// Apply the pressure policy and checkpoint before refusing
				// (SPEC §10.4).
				relieved = true
				db.mu.Unlock()
				err := db.mt.relieve(ctx, db)
				db.mu.Lock()
				if err != nil {
					return err
				}
				continue
			}
			db.mt.signal()
			e, err := db.walLimit(op)
			ev = &e
			return err
		}
		if db.pending == 0 || db.pending < db.opts.MaxPendingCommits && db.pendBytes+bytes <= db.opts.MaxPendingCommitBytes {
			db.pending++
			db.pendBytes += bytes
			db.unappended += bytes
			return nil
		}
		// In Async only a Sync frees room: ask the coordinator for one.
		db.co.room.Store(true)
		db.co.signal()
		if !wait {
			return errs.New(errs.CodeCommitQueueFull, op, "the commit queue is full").
				With("pending", int64(db.pending))
		}
		ch := db.queueFree
		db.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			db.mu.Lock()
			return ctxErr(ctx.Err(), op)
		}
		db.mu.Lock()
	}
}

func (db *DB) unreserveLocked(bytes int64) {
	if db.failedErr.Load() != nil {
		return // fail already emptied the accounting
	}
	db.pending--
	db.pendBytes -= bytes
	db.unappended -= bytes
	close(db.queueFree)
	db.queueFree = make(chan struct{})
}

// WaitDurable waits until commit id, issued by this handle, is durable.
func (db *DB) WaitDurable(ctx context.Context, id uint64) error {
	const op = "engine.WaitDurable"
	if id <= db.firstCommit || id > db.staged.Load().State.CommitID {
		if id <= db.durable.Load().State.CommitID {
			return nil
		}
		return errs.Newf(errs.CodeInvalidArgument, op, "commit %d was not issued by this handle", id)
	}
	return db.waitDurable(ctx, id, op)
}

func (db *DB) waitDurable(ctx context.Context, id uint64, op string) error {
	for {
		db.mu.Lock()
		ch := db.durableCh
		db.mu.Unlock()
		if db.durable.Load().State.CommitID >= id {
			return nil
		}
		if p := db.failedErr.Load(); p != nil {
			return *p
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctxErr(ctx.Err(), op)
		}
	}
}

// Flush waits until every commit sealed before the call is durable;
// later commits do not extend the wait.
func (db *DB) Flush(ctx context.Context) error {
	if db.w == nil {
		return nil
	}
	barrier := db.staged.Load().State.CommitID
	if db.durable.Load().State.CommitID >= barrier {
		return nil
	}
	db.co.flush.Store(true)
	db.co.signal()
	return db.waitDurable(ctx, barrier, "engine.Flush")
}

// Active returns the open read and write transactions.
func (db *DB) Active() int64 { return db.active.Load() }

// Waiting returns the candidates waiting for the writer slot, so that
// background work can yield to them.
func (db *DB) Waiting() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.waiters.len()
}

// CloseIdle closes the handle only when no transaction is open; otherwise
// it fails with BUSY and the handle stays open. While it checks, a
// transaction that begins fails with SHUTTING_DOWN, so none can slip in
// between the check and the close. Calls must not overlap.
func (db *DB) CloseIdle() error {
	db.closing.Store(true)
	if n := db.active.Load(); n > 0 {
		db.closing.Store(false)
		return errs.Newf(errs.CodeBusy, "engine.Close", "%d transactions open", n).With("active", n)
	}
	return db.Close()
}

// CommitStatus returns what the handle knows of a commit token.
func (db *DB) CommitStatus(token [16]byte) Status {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.tokens[token].status
}
