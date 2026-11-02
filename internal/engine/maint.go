package engine

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/versions"
	"github.com/TacFlow/tacdb/internal/wal"
)

// EventKind says what a diagnostic event reports.
type EventKind int

// Event kinds (SPEC §10.1).
const (
	EventSnapshotExpired EventKind = iota + 1 // past MaxReadTxAge
	EventSnapshotEvicted                      // revoked by ExpireOldest
	EventWritesRejected                       // a commit refused with WAL_LIMIT
)

// Event is a diagnostic event of maintenance: no snapshot is revoked and
// no write refused in silence (SPEC §10.1).
type Event struct {
	Kind        EventKind
	Count       int           // snapshots affected or blocking
	OldestAge   time.Duration // age of the oldest of them
	Origin      string        // "read" or "write": the origin of the oldest
	PinnedBytes int64         // log kept by snapshots at the last pass
}

// maintainer runs the passive checkpoint, the scrub, the removal of
// segments and the snapshot policies on one goroutine (design §4.4). The
// coordinator wakes it when the log grows; a ticker wakes it for ages.
type maintainer struct {
	wake   chan struct{}
	quit   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc

	run       sync.Mutex  // one pass at a time: background, Checkpoint, relieve and Close
	suspended atomic.Bool // tests: the checkpoint writes nothing (the REC-11 gate)

	lastSeq, lastOff atomic.Int64 // where the log ended when the last checkpoint began

	resMu    sync.Mutex
	reserves []reserveLSN // durable reservations, not yet folded into resMax
	resMax   [3]uint64    // highest watermark reserved at or below checkpoint_lsn, by kind

	pinned   atomic.Int64              // log bytes kept by snapshots, at the last pass
	blockers atomic.Pointer[blockInfo] // the snapshots that kept them
}

type blockInfo struct {
	count  int
	oldest time.Duration
	origin string
}

func (m *maintainer) start(db *DB) {
	m.wake = make(chan struct{}, 1)
	m.quit = make(chan struct{})
	m.done = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.loop(ctx, db)
}

func (m *maintainer) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// stop cancels a pass in progress and waits for the goroutine.
func (m *maintainer) stop() {
	if m.quit == nil {
		return
	}
	m.cancel()
	close(m.quit)
	<-m.done
	m.quit = nil
}

func (m *maintainer) loop(ctx context.Context, db *DB) {
	defer close(m.done)
	t := time.NewTicker(db.opts.MaintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-m.wake:
		case <-t.C:
		case <-m.quit:
			return
		}
		db.maintain(ctx) // a fatal error fails the handle inside
	}
}

// due reports whether the log grew enough for a pass: CheckpointBytes since
// the last one, or more than CheckpointSegments segments above the floor
// and a segment since the last one.
func (m *maintainer) due(db *DB) bool {
	c := &db.co
	seq, off := c.seq.Load(), c.off.Load()
	since := (seq-m.lastSeq.Load())*db.opts.WALSegmentBytes + off - m.lastOff.Load()
	return since >= db.opts.CheckpointBytes ||
		seq-c.floor.Load() > int64(db.opts.CheckpointSegments) && since >= db.opts.WALSegmentBytes
}

// noteReserve records a durable reservation: once checkpoint_lsn passes
// its LSN, the scan of recovery skips it, so the superblock must carry
// its watermark (design §4.4 step 3).
func (m *maintainer) noteReserve(r reserveLSN) {
	m.resMu.Lock()
	m.reserves = append(m.reserves, r)
	m.resMu.Unlock()
}

// reservedUpTo folds the reservations at or below lsn into resMax and
// returns it.
func (m *maintainer) reservedUpTo(lsn uint64) [3]uint64 {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	keep := m.reserves[:0]
	for _, r := range m.reserves {
		if r.lsn <= lsn {
			m.resMax[r.Kind] = max(m.resMax[r.Kind], r.HighWatermark)
		} else {
			keep = append(keep, r)
		}
	}
	m.reserves = keep
	return m.resMax
}

// relieve runs a pass before a commit is refused for MaxWALBytes: the
// pressure policy, then a checkpoint (SPEC §10.4).
func (m *maintainer) relieve(ctx context.Context, db *DB) error {
	err := db.maintain(ctx)
	if isCode(err, errs.CodeCanceled) || isCode(err, errs.CodeDeadline) {
		return err
	}
	return nil // a fatal error already failed the handle; the caller sees it
}

// Checkpoint runs a maintenance pass now: snapshot ages, a checkpoint, the
// pressure policy and the removal of segments.
func (db *DB) Checkpoint(ctx context.Context) error {
	const op = "engine.Checkpoint"
	if db.opts.ReadOnly {
		return errs.New(errs.CodeReadOnly, op, "database opened read-only")
	}
	if err := db.usable(op); err != nil {
		return err
	}
	return db.maintain(ctx)
}

// maintain is one pass. A fatal checkpoint error fails the handle: the
// main file is in an unknown state, and its Sync is never retried.
func (db *DB) maintain(ctx context.Context) error {
	m := &db.mt
	var events []Event
	defer func() { // after the unlock: a callback may call the handle
		for _, ev := range events {
			db.emit(ev)
		}
	}()
	m.run.Lock()
	defer m.run.Unlock()
	if p := db.failedErr.Load(); p != nil {
		return *p
	}
	events = db.expireAged(events)
	err := db.checkpoint(ctx)
	if err == nil {
		var revoked bool
		if events, revoked = db.pressure(events); revoked {
			err = db.checkpoint(ctx) // the revoked snapshots no longer hold T
		}
	}
	if err != nil {
		if !isCode(err, errs.CodeCanceled) && !isCode(err, errs.CodeDeadline) {
			// Failed at once, before any other pass can retry the Sync;
			// the coordinator then drains its groups.
			failed := error(errs.Wrap(errs.CodeRecoveryRequired, "engine.Checkpoint", err, "the main file is in an unknown state"))
			db.failedErr.CompareAndSwap(nil, &failed)
			db.co.abort(err)
			return *db.failedErr.Load()
		}
		return err
	}
	db.removeSegments()
	return nil
}

// pageWrite is a page the checkpoint writes to the main file: the version
// of the target, or a zeroed free page of the scrub.
type pageWrite struct {
	id   page.ID
	free bool
}

// checkpoint is SPEC §11.1 with the design §4.4. db.mt.run is held.
func (db *DB) checkpoint(ctx context.Context) error {
	const op = "engine.Checkpoint"
	if db.mt.suspended.Load() {
		return nil
	}
	prev := db.sb
	// Measured from every pass: one that snapshots keep from moving T
	// waits for more log, not for the next commit.
	db.mt.lastSeq.Store(db.co.seq.Load())
	db.mt.lastOff.Store(db.co.off.Load())
	_, d := db.target()
	if d == nil || d.State.CommitID <= prev.CommitID {
		return nil // T did not move
	}
	db.hook("target")
	writes, err := db.checkpointPages(prev, d)
	if err != nil {
		return err
	}
	// Step 2: the versions of T and the scrub, in PageID order.
	var buf page.Buf
	scrubbed := 0
	for i, w := range writes {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return ctxErr(err, op)
			}
		}
		if w.free {
			page.EncodeFree(&buf, w.id)
			page.Seal(&buf)
			scrubbed++
		} else if err := db.b.ReadVersion(&buf, d, w.id); err != nil {
			return err
		}
		if err := db.main.WriteAt(buf[:], int64(w.id)*page.Size); err != nil {
			return err
		}
	}
	if err := db.main.Sync(); err != nil {
		return err
	}
	db.hook("synced")
	if err := ctx.Err(); err != nil {
		return ctxErr(err, op)
	}
	// Step 3: the alternate superblock.
	floor, err := db.floorFor(d.CommitLSN)
	if err != nil {
		return err
	}
	// The floor may be a segment the writer just renamed, before its own
	// SyncDir: make the name durable before the superblock refers to it.
	if err := db.fs.SyncDir(db.walDir); err != nil {
		return err
	}
	res := db.mt.reservedUpTo(d.CommitLSN)
	st := &d.State
	sb := page.Superblock{UUID: db.uuid, Generation: prev.Generation + 1, CheckpointLSN: d.CommitLSN,
		CommitID: st.CommitID, SchemaEpoch: st.SchemaEpoch, NodeCount: st.NodeCount, EdgeCount: st.EdgeCount,
		NodeHWM:   max(prev.NodeHWM, st.NodeHWM, res[KindNode]),
		EdgeHWM:   max(prev.EdgeHWM, st.EdgeHWM, res[KindEdge]),
		SymbolHWM: max(prev.SymbolHWM, st.SymbolHWM), PageCount: st.PageCount, WALFloorSegment: floor,
		Flags: prev.Flags, Roots: st.Roots, FreePageCount: st.FreePageCount,
		AppliedRaftIndex: st.AppliedRaftIndex, AppliedRaftTerm: st.AppliedRaftTerm}
	alt := 1 - db.sbSlot
	sb.Encode(&buf)
	if err := db.main.WriteAt(buf[:], int64(alt)*page.Size); err != nil {
		return err
	}
	if err := db.main.Sync(); err != nil {
		return err
	}
	db.hook("superblock")
	// Step 4: publish the base T. The superblock is durable: from here on
	// nothing is canceled.
	for _, w := range writes {
		db.b.InvalidateBase(w.id)
	}
	db.b.SetBase(st.CommitID)
	db.walFloor.Store(floor)
	db.sb, db.sbSlot = sb, alt
	db.co.ckLSN.Store(d.CommitLSN)
	db.mu.Lock()
	for tok, t := range db.tokens {
		if t.status == Durable && t.commit <= st.CommitID {
			delete(db.tokens, tok)
		}
	}
	db.mu.Unlock()
	db.stats.checkpoints.Add(1)
	db.stats.checkpointPages.Add(int64(len(writes) - scrubbed))
	db.stats.scrubbed.Add(int64(scrubbed))
	db.hook("published")
	return nil
}

// checkpointPages returns, in PageID order, what the checkpoint from prev
// to d writes, computed before any write:
//   - the pages with a version newer than prev, except those free in T;
//   - with ScrubFreedPages, the scrub: pages free in T that are allocated
//     in the bitmap of the base or have a version newer than prev.
//
// Only groups whose bitmap changed after prev can hold either kind of free
// page. The bitmaps of the base are read from the main file without the
// cache. One that is torn, or newer than the superblock, was overwritten
// by an interrupted checkpoint: every page of its group free in T is
// zeroed, which is safe since they are free in the durable state.
func (db *DB) checkpointPages(prev page.Superblock, d *versions.Directory) ([]pageWrite, error) {
	var newer []page.ID
	d.Newer(prev.CommitID, func(id page.ID, _ uint64) bool {
		newer = append(newer, id)
		return true
	})
	isNewer := make(map[page.ID]bool, len(newer))
	for _, id := range newer {
		isNewer[id] = true
	}
	pc := d.State.PageCount
	free := map[uint64]*page.Buf{} // the bitmaps of T of the changed groups
	var writes []pageWrite
	for _, id := range newer {
		if !page.IsBitmapPage(id) {
			continue
		}
		g := (uint64(id) - page.FirstBitmap) / page.GroupStride
		tb := new(page.Buf)
		if err := db.b.ReadVersion(tb, d, id); err != nil {
			return nil, err
		}
		free[g] = tb
		if !db.opts.ScrubFreedPages {
			continue
		}
		var base *page.Buf
		torn := false
		if uint64(id) < prev.PageCount {
			bb := new(page.Buf)
			if err := db.main.ReadAt(bb[:], int64(id)*page.Size); err != nil {
				return nil, err
			}
			h := page.HeaderOf(bb)
			if page.Verify(bb, id, prev.PageCount) != nil || h.Type() != page.TypeBitmap || h.CommitID() > prev.CommitID {
				torn = true
			} else {
				base = bb
			}
		}
		first := uint64(id) + 1
		n := int(min(page.GroupPages, pc-min(pc, first)))
		for i := range n {
			p := page.ID(first + uint64(i))
			if page.BitmapBit(tb, i) {
				continue
			}
			inBase := base != nil && uint64(p) < prev.PageCount && page.BitmapBit(base, i)
			if torn || inBase || isNewer[p] {
				writes = append(writes, pageWrite{id: p, free: true})
			}
		}
	}
	for _, id := range newer {
		if !page.IsBitmapPage(id) {
			g := (uint64(id) - page.FirstBitmap) / page.GroupStride
			if tb, ok := free[g]; ok && !page.BitmapBit(tb, int(uint64(id)-uint64(page.BitmapPage(g))-1)) {
				continue // free in T: scrubbed or left alone, never copied
			}
		}
		writes = append(writes, pageWrite{id: id})
	}
	slices.SortFunc(writes, func(a, b pageWrite) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	return writes, nil
}

// segments returns the first LSN of each segment with a final name, in
// seq order. A renamed segment has a synced header (design §4.1).
func (db *DB) segments() ([]segInfo, error) {
	names, err := db.fs.List(db.walDir)
	if err != nil {
		return nil, err
	}
	var out []segInfo
	for _, n := range names {
		seq, ok := wal.ParseSegmentName(n)
		if !ok {
			continue
		}
		f, err := db.segment(seq)
		if err != nil {
			return nil, err
		}
		var b [wal.SegmentHeaderSize]byte
		if err := f.ReadAt(b[:], 0); err != nil {
			return nil, err
		}
		h, err := wal.DecodeSegmentHeader(b[:])
		if err != nil {
			return nil, err
		}
		out = append(out, segInfo{seq, h.FirstLSN})
	}
	slices.SortFunc(out, func(a, b segInfo) int {
		if a.seq < b.seq {
			return -1
		}
		return 1
	})
	return out, nil
}

type segInfo struct{ seq, first uint64 }

// floorOf is the wal_floor_segment of checkpoint_lsn lsn: the segment that
// holds lsn+1, or the last one if none holds it yet (design §4.1).
func floorOf(segs []segInfo, lsn uint64) uint64 {
	f := segs[0].seq
	for _, s := range segs {
		if s.first <= lsn+1 {
			f = s.seq
		}
	}
	return f
}

func (db *DB) floorFor(lsn uint64) (uint64, error) {
	segs, err := db.segments()
	if err != nil {
		return 0, err
	}
	return floorOf(segs, lsn), nil
}

// removeSegments removes, in one batch, the segments below the floor of
// the superblock and below the floor every registered snapshot pins
// (SPEC §11.1 step 6). A failure leaves them for the next pass.
func (db *DB) removeSegments() {
	limit := db.walFloor.Load()
	for _, s := range db.reg.all() {
		limit = min(limit, s.seg.Load())
	}
	names, err := db.fs.List(db.walDir)
	if err != nil {
		return
	}
	removed := false
	lowest := limit
	for _, n := range names {
		seq, ok := wal.ParseSegmentName(n)
		if !ok || seq >= limit {
			continue
		}
		db.segMu.Lock()
		if f, ok := db.segs[seq]; ok {
			f.Close()
			delete(db.segs, seq)
		}
		db.segMu.Unlock()
		if err := db.fs.Remove(db.walDir + "/" + n); err != nil {
			lowest = min(lowest, seq)
			continue
		}
		removed = true
		db.stats.segmentsRemoved.Add(1)
	}
	if removed {
		db.fs.SyncDir(db.walDir) // a lost removal only leaves a stale segment
	}
	db.co.floor.Store(int64(lowest))
	db.hook("removed")
}

// expireAged revokes the read snapshots older than MaxReadTxAge and adds
// the event to events.
func (db *DB) expireAged(events []Event) []Event {
	if db.opts.MaxReadTxAge == Unlimited {
		return events
	}
	now := db.opts.Now()
	var old []*snapshot
	for _, s := range db.reg.all() {
		if !s.writer && now.Sub(s.began) > db.opts.MaxReadTxAge {
			old = append(old, s)
		}
	}
	return db.revoke(old, EventSnapshotExpired, events)
}

// revoke revokes snaps and adds one event for them to events; the caller
// emits it once it holds no lock.
func (db *DB) revoke(snaps []*snapshot, kind EventKind, events []Event) []Event {
	now := db.opts.Now()
	ev := Event{Kind: kind, Origin: "read", PinnedBytes: db.mt.pinned.Load()}
	for _, s := range snaps {
		if s.revoke(&db.reg) {
			ev.Count++
			ev.OldestAge = max(ev.OldestAge, now.Sub(s.began))
		}
	}
	if ev.Count == 0 {
		return events
	}
	if kind == EventSnapshotExpired {
		db.stats.expired.Add(int64(ev.Count))
	} else {
		db.stats.evicted.Add(int64(ev.Count))
	}
	return append(events, ev)
}

func (db *DB) emit(ev Event) {
	if f := db.opts.OnEvent; f != nil {
		f(ev)
	}
}

// pressure measures the log kept by snapshots and applies the policy
// (SPEC §10.1). It reports whether it revoked any snapshot.
func (db *DB) pressure(events []Event) ([]Event, bool) {
	segs, err := db.segments()
	if err != nil || len(segs) == 0 {
		return events, false
	}
	free := floorOf(segs, db.durable.Load().CommitLSN) // the floor without snapshots
	now := db.opts.Now()
	type held struct {
		s    *snapshot
		need uint64
	}
	var hs []held // the pins of operations in progress are brief: only floors count
	for _, s := range db.reg.all() {
		if d := s.dir.Load(); d != nil {
			if need := floorOf(segs, d.CommitLSN); need < free {
				hs = append(hs, held{s, need})
			}
		}
	}
	slices.SortFunc(hs, func(a, b held) int {
		if a.need != b.need {
			if a.need < b.need {
				return -1
			}
			return 1
		}
		return a.s.began.Compare(b.s.began)
	})
	pinned := func(hs []held) int64 {
		if len(hs) == 0 {
			return 0
		}
		return int64(free-hs[0].need) * db.opts.WALSegmentBytes
	}
	info := func(hs []held) *blockInfo {
		b := &blockInfo{count: len(hs)}
		for _, h := range hs {
			if age := now.Sub(h.s.began); age >= b.oldest {
				b.oldest, b.origin = age, "read"
				if h.s.writer {
					b.origin = "write"
				}
			}
		}
		return b
	}
	db.mt.pinned.Store(pinned(hs))
	db.mt.blockers.Store(info(hs))
	if db.opts.SnapshotPressurePolicy != ExpireOldest {
		return events, false
	}
	// From the oldest on: the snapshots that pin the lowest segment go
	// together, since revoking only some of them frees nothing.
	var victims []*snapshot
	rest := hs
	for len(rest) > 0 && pinned(rest) >= db.opts.WALPinnedSoftBytes {
		n := 0
		for n < len(rest) && rest[n].need == rest[0].need {
			if h := rest[n]; h.s.writer || now.Sub(h.s.began) < db.opts.MinSnapshotAgeForEviction {
				n = -1
				break
			}
			n++
		}
		if n < 0 {
			break // one of them cannot be revoked
		}
		for _, h := range rest[:n] {
			victims = append(victims, h.s)
		}
		rest = rest[n:]
	}
	if len(victims) == 0 {
		return events, false
	}
	events = db.revoke(victims, EventSnapshotEvicted, events)
	db.mt.pinned.Store(pinned(rest))
	db.mt.blockers.Store(info(rest))
	return events, true
}

// walLimit is the WAL_LIMIT of a commit, with the snapshots that keep the
// log, as the last pass found them (SPEC §10.4). db.mu is held; the event
// goes out after it is released.
func (db *DB) walLimit(op string) (Event, error) {
	err := errs.New(errs.CodeWALLimit, op, "the commit would pass MaxWALBytes").With("limit", db.opts.MaxWALBytes)
	ev := Event{Kind: EventWritesRejected, PinnedBytes: db.mt.pinned.Load()}
	if b := db.mt.blockers.Load(); b != nil && b.count > 0 {
		ev.Count, ev.OldestAge, ev.Origin = b.count, b.oldest, b.origin
		err = errs.Newf(errs.CodeWALLimit, op, "the commit would pass MaxWALBytes; %d snapshots keep the log, the oldest a %s transaction %s old",
			b.count, b.origin, b.oldest.Round(time.Millisecond)).
			With("limit", db.opts.MaxWALBytes).With("snapshots", int64(b.count)).With("oldest_age_ms", b.oldest.Milliseconds())
	}
	db.stats.walLimits.Add(1)
	return ev, err
}

func (db *DB) hook(stage string) {
	if h := db.hooks.checkpoint; h != nil {
		h(stage)
	}
}
