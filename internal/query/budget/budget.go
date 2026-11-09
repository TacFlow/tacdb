// Package budget bounds the resources of one query execution (SPEC
// §15.4): memory, charged to the execution category of the governor
// before it is allocated; work units; and the shape of intermediate
// values. Going over a limit is RESOURCE_LIMIT, never a truncation.
package budget

import (
	"context"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/value"
)

// Limits of one execution.
type Limits struct {
	MemoryBytes int64  // QueryMemoryBytes
	WorkUnits   uint64 // QueryWorkUnits
	Values      value.Limits
}

// Defaults of SPEC §15.4.
const (
	DefaultMemoryBytes = 64 << 20
	DefaultWorkUnits   = 100_000_000
)

// DefaultLimits returns the defaults of SPEC §15.4 and §24.1.
func DefaultLimits() Limits {
	return Limits{MemoryBytes: DefaultMemoryBytes, WorkUnits: DefaultWorkUnits, Values: value.DefaultLimits()}
}

// chunk is the granularity of reservations from the governor, so that
// small allocations do not take its lock.
const chunk = 256 << 10

// Budget is the budget of one execution. It is not safe for concurrent
// use: an execution runs on one goroutine.
type Budget struct {
	ctx  context.Context
	res  *govern.Reservation // nil without a governor
	lim  Limits
	used int64
	peak int64
	work uint64
}

// New returns a budget whose memory comes from the execution category of
// g; g may be nil, and then only the limits apply.
func New(ctx context.Context, g *govern.Governor, lim Limits) (*Budget, error) {
	b := &Budget{ctx: ctx, lim: lim}
	if g != nil {
		res, err := g.Reserve(ctx, govern.Execution, 0)
		if err != nil {
			return nil, err
		}
		b.res = res
	}
	return b, nil
}

// Context returns the context of the execution.
func (b *Budget) Context() context.Context { return b.ctx }

// Limits returns the limits of the execution.
func (b *Budget) Limits() Limits { return b.lim }

// Grow charges bytes before they are allocated. Over QueryMemoryBytes, or
// when the governor refuses, it is RESOURCE_LIMIT.
func (b *Budget) Grow(bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	if bytes > b.lim.MemoryBytes-b.used {
		return errs.New(errs.CodeResourceLimit, "query", "query memory exhausted").
			With("requested", bytes).With("available", max(0, b.lim.MemoryBytes-b.used)).
			With("limit", b.lim.MemoryBytes)
	}
	if b.res != nil {
		if need := b.used + bytes - b.res.Bytes(); need > 0 {
			// A whole chunk when it is free now, else exactly what is
			// needed, with the waiting rules of the governor.
			if b.res.TryGrow((need+chunk-1)/chunk*chunk) != nil {
				if err := b.res.Grow(b.ctx, need); err != nil {
					return err // RESOURCE_LIMIT, CANCELED or DEADLINE
				}
			}
		}
	}
	b.used += bytes
	b.peak = max(b.peak, b.used)
	return nil
}

// Shrink returns bytes charged by Grow; reserved memory beyond two chunks
// of slack goes back to the governor.
func (b *Budget) Shrink(bytes int64) {
	b.used -= min(max(bytes, 0), b.used)
	if b.res != nil {
		if extra := b.res.Bytes() - b.used - 2*chunk; extra > 0 {
			b.res.Shrink(extra / chunk * chunk)
		}
	}
}

// Used returns the bytes charged now.
func (b *Budget) Used() int64 { return b.used }

// Peak returns the most bytes charged at once.
func (b *Budget) Peak() int64 { return b.peak }

// Work charges work units: pages read, rows produced by an operator,
// expansions and distances (SPEC §15.4).
func (b *Budget) Work(units uint64) error {
	if units > b.lim.WorkUnits-min(b.work, b.lim.WorkUnits) {
		return errs.New(errs.CodeResourceLimit, "query", "query work units exhausted").
			With("requested", int64(min(units, 1<<63-1))).With("limit", int64(min(b.lim.WorkUnits, 1<<63-1)))
	}
	b.work += units
	return nil
}

// WorkDone returns the work units charged.
func (b *Budget) WorkDone() uint64 { return b.work }

// CheckShape refuses an intermediate value with more elements or a deeper
// nesting than the value limits allow, before it is built.
func (b *Budget) CheckShape(elements int64, depth int) error {
	if elements > int64(b.lim.Values.MaxValueElements) {
		return errs.New(errs.CodeResourceLimit, "query", "intermediate value has too many elements").
			With("requested", elements).With("limit", int64(b.lim.Values.MaxValueElements))
	}
	if depth > b.lim.Values.MaxDepth {
		return errs.New(errs.CodeResourceLimit, "query", "intermediate value nested too deeply").
			With("requested", int64(depth)).With("limit", int64(b.lim.Values.MaxDepth))
	}
	return nil
}

// Close returns the memory to the governor; it is idempotent.
func (b *Budget) Close() {
	if b.res != nil {
		b.res.Close()
	}
	b.used = 0
}
