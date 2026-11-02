package engine

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Kinds of identifiers (RESERVE_IDS, SPEC §9.1).
const (
	KindNode = 1
	KindEdge = 2
)

// idAlloc hands out node and edge identifiers in adaptive blocks (SPEC
// §9.3). An identifier is handed out only once the RESERVE_IDS of its
// block is durable. When half of the current block is used, the next
// block is requested; it becomes durable with the next group Sync, and
// a dedicated Sync happens only when a writer runs out first.
type idAlloc struct {
	mu      sync.Mutex
	db      *DB
	kinds   [3]idKind // by kind; 0 unused
	reqs    []wal.Reserve
	changed chan struct{} // closed and replaced when a limit moves or on failure
	err     error
}

type idKind struct {
	next     uint64    // the next identifier to hand out
	limit    uint64    // identifiers up to limit are durably reserved
	reserved uint64    // the highest watermark requested
	blocks   []idBlock // requested blocks; blocks[0] is the one in use
	size     uint64
	last     time.Time // when the last block was requested
}

type idBlock struct{ lo, hi uint64 }

// init starts after the recovered watermarks: the rest of a reservation
// of the previous process is skipped.
func (a *idAlloc) init(db *DB, nodeHWM, edgeHWM uint64) {
	a.db = db
	a.changed = make(chan struct{})
	for kind, hwm := range map[int]uint64{KindNode: nodeHWM, KindEdge: edgeHWM} {
		a.kinds[kind] = idKind{next: hwm + 1, limit: hwm, reserved: hwm, size: db.opts.IDReservationInitial}
	}
}

func (a *idAlloc) next(ctx context.Context, kind int) (uint64, error) {
	const op = "engine.NextID"
	if kind != KindNode && kind != KindEdge {
		return 0, errs.Newf(errs.CodeInvalidArgument, op, "unknown identifier kind %d", kind)
	}
	a.mu.Lock()
	for {
		if a.err != nil {
			a.mu.Unlock()
			return 0, a.err
		}
		k := &a.kinds[kind]
		if k.next <= k.limit {
			id := k.next
			k.next++
			for len(k.blocks) > 0 && id > k.blocks[0].hi {
				k.blocks = k.blocks[1:]
			}
			if len(k.blocks) == 1 && k.reserved == k.limit && id >= k.blocks[0].lo+(k.blocks[0].hi-k.blocks[0].lo)/2 {
				a.request(k, kind) // half used: the next block goes with the next group
			}
			wake := len(a.reqs) > 0
			a.mu.Unlock()
			if wake {
				a.db.co.signal()
			}
			return id, nil
		}
		if k.limit >= math.MaxInt64 {
			a.mu.Unlock()
			return 0, errs.New(errs.CodeLimitExceeded, op, "identifiers exhausted")
		}
		if k.reserved < k.next {
			a.request(k, kind)
		}
		ch := a.changed
		a.mu.Unlock()
		a.db.co.signal() // a dedicated Sync: the writer waits
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctxErr(ctx.Err(), op)
		}
		a.mu.Lock()
	}
}

// request asks for the next block. Its size doubles when blocks go in
// less than a second and returns to the initial size after a minute
// without pressure. a.mu is held.
func (a *idAlloc) request(k *idKind, kind int) {
	if k.reserved >= math.MaxInt64 {
		return // nothing left to reserve; next reports the exhaustion
	}
	o := &a.db.opts
	now := o.Now()
	if !k.last.IsZero() {
		switch d := now.Sub(k.last); {
		case d < time.Second:
			k.size = min(2*k.size, o.IDReservationMax)
		case d > time.Minute:
			k.size = o.IDReservationInitial
		}
	}
	k.last = now
	lo := k.reserved + 1
	hi := uint64(math.MaxInt64)
	if math.MaxInt64-k.reserved > k.size {
		hi = k.reserved + k.size
	}
	k.blocks = append(k.blocks, idBlock{lo, hi})
	k.reserved = hi
	a.reqs = append(a.reqs, wal.Reserve{Kind: uint8(kind), HighWatermark: hi})
}

// take hands the requested reservations to the coordinator.
func (a *idAlloc) take() []wal.Reserve {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.reqs
	a.reqs = nil
	return r
}

// waiting reports whether a writer is out of identifiers.
func (a *idAlloc) waiting() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range a.kinds[1:] {
		if k.next > k.limit && k.reserved > k.limit {
			return true
		}
	}
	return false
}

// durable records a reservation the log has synced.
func (a *idAlloc) durable(r wal.Reserve) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := &a.kinds[r.Kind]
	k.limit = max(k.limit, r.HighWatermark)
	close(a.changed)
	a.changed = make(chan struct{})
}

// limit returns the durable watermark of kind, for the State of a commit.
func (a *idAlloc) limit(kind int) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kinds[kind].limit
}

// fail stops the allocator: waiting and later calls get err.
func (a *idAlloc) fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err == nil && a.changed != nil {
		a.err = err
		close(a.changed)
	}
}
