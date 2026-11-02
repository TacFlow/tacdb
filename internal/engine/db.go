package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/versions"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// DB is an open database. It is safe for concurrent use.
type DB struct {
	fs     vfs.FS
	name   string // the main file, "name.tacdb"
	walDir string
	opts   Options
	uuid   [16]byte
	sb     page.Superblock // the superblock in force; the checkpoint changes it
	sbSlot page.ID         // the page that holds it, 0 or 1

	lock  *vfs.DatabaseLock
	main  vfs.File
	cache *cache.Cache
	b     *versions.Backing
	w     *wal.Writer // nil when read-only; used by the coordinator only

	segMu sync.Mutex
	segs  map[uint64]vfs.File

	// The three fronts of SPEC §9.2. visibleHead is stored before
	// durableHead, so visible is never behind durable.
	staged, visible, durable atomic.Pointer[versions.Directory]
	reg                      registry
	active                   atomic.Int64 // open transactions
	closed                   atomic.Bool
	failedErr                atomic.Pointer[error]
	firstCommit              uint64        // CommitIDs above it were issued by this handle
	walFloor                 atomic.Uint64 // wal_floor_segment of the base T in force

	// mu is the coordination mutex: admission, sealing, failure and the
	// token history. It is never held during I/O or a wait.
	mu         sync.Mutex
	writer     *WriteTx
	waiters    waitQueue
	queue      []*intent // sealed, not appended
	pending    int       // intents from queue admission until durable
	pendBytes  int64
	unappended int64         // reserved bytes not yet in the log, for MaxWALBytes
	queueFree  chan struct{} // closed and replaced when pending shrinks
	tokens     map[[16]byte]token
	durableCh  chan struct{} // closed and replaced when durable advances or the handle fails

	co    coordinator
	mt    maintainer
	ids   idAlloc
	stats counters
	hooks hooks
}

// hooks let tests stop the engine at exact points.
type hooks struct {
	afterRegister func() // a snapshot is registered, the head not loaded yet
	betweenHeads  func() // visibleHead is stored, durableHead not yet
	checkpoint    func(stage string)
}

// Stats are counters of a handle.
type Stats struct {
	Appends            int64 // groups and reservations appended
	Syncs              int64
	CommitGroupSyncs   int64 // Syncs that made commit groups durable
	IDReservationSyncs int64 // dedicated Syncs for identifier reservations
	Groups             int64
	Intents            int64
	CoalescedPages     int64 // page versions replaced inside a group

	Checkpoints      int64
	CheckpointPages  int64 // versions copied to the main file
	ScrubbedPages    int64 // zeroed free pages written
	SegmentsRemoved  int64
	SnapshotsExpired int64 // revoked for MaxReadTxAge
	SnapshotsEvicted int64 // revoked by ExpireOldest
	WALLimits        int64 // commits refused with WAL_LIMIT
}

type counters struct {
	appends, syncs, groupSyncs, idSyncs, groups, intents, coalesced atomic.Int64

	checkpoints, checkpointPages, scrubbed, segmentsRemoved, expired, evicted, walLimits atomic.Int64
}

// Stats returns the counters.
func (db *DB) Stats() Stats {
	s := &db.stats
	return Stats{Appends: s.appends.Load(), Syncs: s.syncs.Load(), CommitGroupSyncs: s.groupSyncs.Load(),
		IDReservationSyncs: s.idSyncs.Load(), Groups: s.groups.Load(), Intents: s.intents.Load(),
		CoalescedPages: s.coalesced.Load(), Checkpoints: s.checkpoints.Load(), CheckpointPages: s.checkpointPages.Load(),
		ScrubbedPages: s.scrubbed.Load(), SegmentsRemoved: s.segmentsRemoved.Load(), SnapshotsExpired: s.expired.Load(),
		SnapshotsEvicted: s.evicted.Load(), WALLimits: s.walLimits.Load()}
}

// Open opens the database name ("x.tacdb") in fs, creating it with
// opts.Create (SPEC §11.1), and recovers its state from the log (§11.2).
func Open(ctx context.Context, fs vfs.FS, name string, opts Options) (*DB, error) {
	const op = "engine.Open"
	if err := opts.check(); err != nil {
		return nil, err
	}
	if err := vfs.ValidName(name); err != nil {
		return nil, err
	}
	gov := opts.Governor
	if gov == nil {
		gov = govern.Default()
	}
	db := &DB{fs: fs, name: name, walDir: name + "-wal", opts: opts, segs: map[uint64]vfs.File{},
		tokens: map[[16]byte]token{}, queueFree: make(chan struct{}), durableCh: make(chan struct{})}
	mode := vfs.LockExclusive
	if opts.ReadOnly {
		mode = vfs.LockShared
	}
	lock, err := vfs.LockDatabase(fs, name, mode)
	if err != nil {
		return nil, err
	}
	db.lock = lock
	ok := false
	defer func() {
		if !ok {
			db.release()
		}
	}()
	exists, err := fs.Exists(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		if !opts.Create {
			return nil, errs.Newf(errs.CodeNotFound, op, "database %s does not exist", name)
		}
		if err := db.create(ctx); err != nil {
			return nil, err
		}
		if err := lock.AttachMainFile(fs, name); err != nil {
			return nil, err
		}
	}
	open := fs.Open
	if opts.ReadOnly {
		open = fs.OpenReadOnly
	}
	if db.main, err = open(name); err != nil {
		return nil, err
	}
	if db.cache, err = cache.New(ctx, cache.Options{Budget: opts.CacheBytes, Governor: gov}); err != nil {
		return nil, err
	}
	if err := db.recover(ctx); err != nil {
		return nil, err
	}
	db.ids.init(db, db.durable.Load().State.NodeHWM, db.durable.Load().State.EdgeHWM)
	if !opts.ReadOnly {
		db.co.start(db)
		db.removeSegments() // leftovers below the floor, now that the state is established
		db.mt.start(db)
	}
	ok = true
	return db, nil
}

// create makes a new database (SPEC §11.1): the log with segment 1 synced,
// then the main file, written as a temporary, synced and published by a
// rename. A log left alone by an interrupted creation is replaced.
func (db *DB) create(ctx context.Context) error {
	if err := db.dropOrphanLog(ctx); err != nil {
		return err
	}
	var uuid [16]byte
	rand.Read(uuid[:])
	w, err := wal.Create(db.fs, db.walDir, uuid, db.walOptions())
	if err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	var suffix [8]byte
	rand.Read(suffix[:])
	tmp := db.name + "." + hex.EncodeToString(suffix[:]) + ".tmp"
	f, err := db.fs.Create(tmp)
	if err != nil {
		return err
	}
	sb := page.Superblock{UUID: uuid, Generation: 1, PageCount: 2, WALFloorSegment: 1}
	var img [2 * page.Size]byte
	sb.Encode((*page.Buf)(img[:page.Size]))
	err = f.WriteAt(img[:], 0)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := db.fs.Rename(tmp, db.name); err != nil {
		return err
	}
	return db.fs.SyncDir(".")
}

// dropOrphanLog removes the log of a creation that crashed before the
// main file was published: only segment 1 (and the spare), with a valid
// header and no record. Any other log is never replaced (REC-12).
func (db *DB) dropOrphanLog(ctx context.Context) error {
	const op = "engine.Open"
	if ok, err := db.fs.Exists(db.walDir); err != nil || !ok {
		return err
	}
	names, err := db.fs.List(db.walDir)
	if err != nil {
		return err
	}
	refuse := errs.Newf(errs.CodeDestinationExists, op, "%s holds a log without a main file", db.walDir)
	for _, n := range names {
		if n != wal.SegmentName(1) && n != wal.SpareName {
			return refuse
		}
	}
	f, err := db.fs.OpenReadOnly(db.walDir + "/" + wal.SegmentName(1))
	if err != nil {
		return refuse
	}
	var hb [wal.SegmentHeaderSize]byte
	err = f.ReadAt(hb[:], 0)
	f.Close()
	if err != nil {
		return refuse
	}
	h, err := wal.DecodeSegmentHeader(hb[:])
	if err != nil {
		return refuse
	}
	res, err := wal.Scan(ctx, wal.ScanInput{FS: db.fs, Dir: db.walDir, UUID: h.UUID, FloorSeq: 1})
	if err != nil || len(res.Groups) > 0 || len(res.Reserves) > 0 || res.TailDiscarded ||
		res.End.Seq != 1 || res.End.Offset != wal.SegmentHeaderSize {
		return refuse
	}
	for _, n := range names {
		if err := db.fs.Remove(db.walDir + "/" + n); err != nil {
			return err
		}
	}
	if err := db.fs.Remove(db.walDir); err != nil {
		return err
	}
	return db.fs.SyncDir(".")
}

func (db *DB) walOptions() wal.Options {
	return wal.Options{SegmentBytes: db.opts.WALSegmentBytes, Now: db.opts.Now}
}

// recover chooses the superblock, replays the log on top of it and
// publishes the recovered state (SPEC §11.2).
func (db *DB) recover(ctx context.Context) error {
	const op = "engine.Open"
	var p0, p1 page.Buf
	if err := db.main.ReadAt(p0[:], 0); err != nil {
		return err
	}
	if err := db.main.ReadAt(p1[:], page.Size); err != nil {
		return err
	}
	sb, slot, err := page.ChooseSuperblock(&p0, &p1)
	if err != nil {
		return err
	}
	db.sb, db.sbSlot, db.uuid = sb, slot, sb.UUID
	db.walFloor.Store(sb.WALFloorSegment)
	if ok, err := db.fs.Exists(db.walDir); err != nil {
		return err
	} else if !ok {
		return errs.Newf(errs.CodeRecoveryRequired, op, "the log %s of an existing database is missing", db.walDir)
	}
	res, err := wal.Scan(ctx, wal.ScanInput{FS: db.fs, Dir: db.walDir, UUID: sb.UUID,
		FloorSeq: sb.WALFloorSegment, CheckpointLSN: sb.CheckpointLSN, BaseCommitID: sb.CommitID})
	if err != nil {
		return err
	}
	db.b = &versions.Backing{UUID: sb.UUID, Cache: db.cache, Main: db.main, Segment: db.segment}
	db.b.SetBase(sb.CommitID)
	base := wal.State{CommitID: sb.CommitID, SchemaEpoch: sb.SchemaEpoch, NodeCount: sb.NodeCount,
		EdgeCount: sb.EdgeCount, NodeHWM: sb.NodeHWM, EdgeHWM: sb.EdgeHWM, SymbolHWM: sb.SymbolHWM,
		PageCount: sb.PageCount, FreePageCount: sb.FreePageCount, Roots: sb.Roots,
		AppliedRaftIndex: sb.AppliedRaftIndex, AppliedRaftTerm: sb.AppliedRaftTerm}
	checkpointed := versions.NewDirectory(base)
	checkpointed.CommitLSN = sb.CheckpointLSN
	dir := checkpointed
	var begins []wal.Begin
	var buf page.Buf
	// The directories between groups are never used: the versions of every
	// group go into the directory of the last one in one pass, later pages
	// over earlier ones.
	var vs []versions.Version
	for _, g := range res.Groups {
		if err := ctx.Err(); err != nil {
			return ctxErr(err, op)
		}
		for _, p := range g.Pages {
			// Every recovered page is verified: the log may hold pages
			// with a valid record CRC but a broken structure.
			f, err := db.segment(p.Loc.Seq)
			if err != nil {
				return err
			}
			if err := f.ReadAt(buf[:], p.Loc.Offset); err != nil {
				return err
			}
			if err := page.Verify(&buf, p.ID, g.Commit.State.PageCount); err != nil {
				return errs.Wrap(errs.CodeCorrupt, op, err, "a page of the log")
			}
			vs = append(vs, versions.Version{ID: p.ID, Loc: versions.WALLoc(p.CommitID, p.Loc)})
		}
		begins = append(begins, g.Begins...)
	}
	if n := len(res.Groups); n > 0 {
		dir = dir.Apply(res.Groups[n-1].Commit.State, vs)
		dir.CommitLSN = res.Groups[n-1].CommitLSN
	}
	st := &dir.State
	for _, r := range res.Reserves { // also those of a discarded tail: they only skip IDs
		switch r.Kind {
		case kindNode:
			st.NodeHWM = max(st.NodeHWM, r.HighWatermark)
		case kindEdge:
			st.EdgeHWM = max(st.EdgeHWM, r.HighWatermark)
		}
	}
	st.NodeHWM, st.EdgeHWM = max(st.NodeHWM, sb.NodeHWM), max(st.EdgeHWM, sb.EdgeHWM)
	db.firstCommit = dir.State.CommitID
	durable := dir
	if db.opts.ReadOnly {
		// Nothing may change, so the prefix read from the operating system
		// cache can only be made durable by a Sync of a read-only handle.
		if db.syncReadOnly(res.End.Seq) != nil {
			durable = checkpointed
		}
	} else {
		db.co.group = res.LastGroupID
		w, err := wal.Open(db.fs, db.walDir, sb.UUID, wal.Resume{End: res.End, Discarded: res.TailDiscarded,
			CheckpointLSN: sb.CheckpointLSN, LastGroupID: res.LastGroupID, LastCommitID: res.LastCommitID}, db.walOptions())
		if err != nil {
			return err
		}
		db.w = w
		db.co.floor.Store(int64(sb.WALFloorSegment))
		db.co.ckLSN.Store(sb.CheckpointLSN)
		p := w.Position()
		db.co.seq.Store(int64(p.Seq))
		db.co.off.Store(p.Offset)
		db.mt.lastSeq.Store(int64(p.Seq))
		db.mt.lastOff.Store(p.Offset)
	}
	if durable == dir {
		for _, b := range begins {
			db.tokens[b.Token] = token{Durable, b.CommitID}
		}
	}
	db.staged.Store(dir)
	db.visible.Store(dir)
	db.durable.Store(durable)
	return nil
}

// syncReadOnly tries to make the recovered log durable without writing.
func (db *DB) syncReadOnly(seq uint64) error {
	f, err := db.segment(seq)
	if err != nil {
		return err
	}
	return f.Sync()
}

// segment returns a read-only handle of a segment, for reads of versions.
func (db *DB) segment(seq uint64) (vfs.File, error) {
	db.segMu.Lock()
	defer db.segMu.Unlock()
	if f, ok := db.segs[seq]; ok {
		return f, nil
	}
	f, err := db.fs.OpenReadOnly(db.walDir + "/" + wal.SegmentName(seq))
	if err != nil {
		return nil, err
	}
	db.segs[seq] = f
	return f, nil
}

// usable fails when new transactions cannot begin.
func (db *DB) usable(op string) error {
	if db.closed.Load() {
		return errs.New(errs.CodeClosed, op, "database closed")
	}
	if p := db.failedErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Close waits until the sealed commits are durable, stops the coordinator
// and the maintenance, runs a last checkpoint that removes the segments
// below the new floor, and releases the files. A write transaction still open is rolled back
// and candidates waiting for the writer get SHUTTING_DOWN. It is safe to
// call more than once.
func (db *DB) Close() error {
	if !db.closed.CompareAndSwap(false, true) {
		return nil
	}
	db.mu.Lock()
	if tx := db.writer; tx != nil {
		tx.endLocked(txAborted)
	}
	db.waiters.failAll(errs.New(errs.CodeShuttingDown, "engine.BeginWrite", "database closing"))
	db.mu.Unlock()
	var first error
	if db.w != nil {
		if err := db.Flush(context.Background()); err != nil && !errors.Is(err, errs.Sentinel(errs.CodeRecoveryRequired)) {
			first = err
		}
		db.mt.stop()
		db.co.stop()
		if db.failedErr.Load() == nil {
			if err := db.maintain(context.Background()); err != nil && first == nil {
				first = err
			}
		}
		if err := db.w.Close(); err != nil && first == nil && db.failedErr.Load() == nil {
			first = err
		}
	}
	db.ids.fail(errs.New(errs.CodeClosed, "engine.NextID", "database closed"))
	if err := db.release(); err != nil && first == nil {
		first = err
	}
	return first
}

// release closes the files and the lock.
func (db *DB) release() error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	db.segMu.Lock()
	for _, f := range db.segs {
		keep(f.Close())
	}
	db.segs = map[uint64]vfs.File{}
	db.segMu.Unlock()
	if db.cache != nil {
		db.cache.Close()
	}
	if db.main != nil {
		keep(db.main.Close())
	}
	keep(db.lock.Release())
	return first
}

// ctxErr maps a context error to CANCELED or DEADLINE.
func ctxErr(err error, op string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.CodeDeadline, op, err, "deadline exceeded")
	}
	return errs.Wrap(errs.CodeCanceled, op, err, "canceled")
}
