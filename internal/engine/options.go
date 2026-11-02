// Package engine is the storage engine: a database handle with snapshots,
// one admitted writer at a time, commits sealed in order of their parents
// and made durable by a coordinator that writes the log in groups (SPEC
// §9 and §10). The public API is package tacdb, over this one.
package engine

import (
	"math"
	"strconv"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Durability is the durability mode of a handle (SPEC §9.2).
type Durability int

// Durability modes.
const (
	// GroupSync makes each commit durable before it returns; commits that
	// seal while a Sync runs share the next one.
	GroupSync Durability = iota
	// Sync appends and syncs each commit as its own group.
	Sync
	// Async publishes each commit once it is appended, with Durable=false;
	// a periodic Sync, Flush or a byte limit makes it durable.
	Async
)

// PressurePolicy says what happens when snapshots keep log segments from
// being removed (SPEC §10.1).
type PressurePolicy int

// Pressure policies.
const (
	// ExpireOldest revokes the oldest snapshots, from the oldest on, once
	// they are at least MinSnapshotAgeForEviction old.
	ExpireOldest PressurePolicy = iota
	// RejectWrites keeps the readers and refuses commits with WAL_LIMIT
	// once the log reaches MaxWALBytes.
	RejectWrites
)

// Unlimited is the MaxReadTxAge that never revokes by age; it must be set
// explicitly.
const Unlimited time.Duration = math.MaxInt64

func (d Durability) String() string {
	switch d {
	case GroupSync:
		return "GroupSync"
	case Sync:
		return "Sync"
	case Async:
		return "Async"
	}
	return "Durability(" + strconv.Itoa(int(d)) + ")"
}

// Options configure a handle. Start from DefaultOptions: a zero
// BusyTimeout, for example, means fail at once, as SPEC §10.2 says.
type Options struct {
	Create   bool // create the database if it does not exist
	ReadOnly bool

	Durability Durability

	BusyTimeout           time.Duration // wait of BeginWrite in the queue
	MaxWriterQueue        int           // candidates waiting for the writer slot
	MaxActiveTransactions int           // open read and write transactions
	MaxWriteTxDuration    time.Duration // from admission
	MaxTxBytes            int64         // private pages of one transaction

	MaxPendingCommits     int   // sealed intents not yet durable
	MaxPendingCommitBytes int64 // their bytes
	MaxWALBytes           int64
	WALSegmentBytes       int64
	GroupMaxCommits       int
	GroupMaxBytes         int64
	AsyncSyncInterval     time.Duration
	AsyncSyncBytes        int64 // appended bytes that trigger a Sync in Async

	IDReservationInitial uint64
	IDReservationMax     uint64

	// The passive checkpoint runs every CheckpointBytes of log, or when
	// more than CheckpointSegments segments lie above the floor (SPEC
	// §10.4, §11.1). ScrubFreedPages writes zeroed free pages (§8.4).
	CheckpointBytes    int64
	CheckpointSegments int
	ScrubFreedPages    bool
	// MaintenanceInterval is how often the maintenance goroutine checks
	// snapshot ages and log pressure without a trigger.
	MaintenanceInterval time.Duration

	MaxReadTxAge              time.Duration // Unlimited to never revoke by age
	SnapshotPressurePolicy    PressurePolicy
	WALPinnedSoftBytes        int64 // log kept by snapshots before the policy acts
	MinSnapshotAgeForEviction time.Duration
	// OnEvent receives the diagnostic events of maintenance, on the
	// goroutine that found them (maintenance, a commit or a reader), with
	// no lock of the handle held; it must not block.
	OnEvent func(Event)

	CacheBytes  int64
	Governor    *govern.Governor // nil means govern.Default()
	PruneBudget int              // radix nodes pruned per commit
	Now         func() time.Time
}

// MaxGroupBytes bounds the bytes of a group, so that any group fits a
// segment the log scanner accepts (wal.DefaultMaxSegmentBytes) with room
// for the record headers.
const MaxGroupBytes = wal.DefaultMaxSegmentBytes / 2

// DefaultOptions returns the defaults of SPEC §9 and §10.
func DefaultOptions() Options {
	return Options{
		Durability:                GroupSync,
		BusyTimeout:               5 * time.Second,
		MaxWriterQueue:            128,
		MaxActiveTransactions:     1024,
		MaxWriteTxDuration:        30 * time.Second,
		MaxTxBytes:                64 << 20,
		MaxPendingCommits:         256,
		MaxPendingCommitBytes:     128 << 20,
		MaxWALBytes:               1 << 30,
		WALSegmentBytes:           64 << 20,
		GroupMaxCommits:           64,
		GroupMaxBytes:             8 << 20,
		AsyncSyncInterval:         100 * time.Millisecond,
		AsyncSyncBytes:            16 << 20,
		IDReservationInitial:      1024,
		IDReservationMax:          1 << 20,
		CheckpointBytes:           64 << 20,
		CheckpointSegments:        4,
		ScrubFreedPages:           true,
		MaintenanceInterval:       time.Second,
		MaxReadTxAge:              10 * time.Minute,
		SnapshotPressurePolicy:    ExpireOldest,
		WALPinnedSoftBytes:        512 << 20,
		MinSnapshotAgeForEviction: 30 * time.Second,
		CacheBytes:                64 << 20,
		PruneBudget:               64,
		Now:                       time.Now,
	}
}

func (o *Options) check() error {
	bad := func(what string) error {
		return errs.New(errs.CodeInvalidArgument, "engine.Open", "invalid option: "+what)
	}
	switch {
	case o.Durability < GroupSync || o.Durability > Async:
		return bad("durability mode")
	case o.Create && o.ReadOnly:
		return bad("Create with ReadOnly")
	case o.BusyTimeout < 0 || o.MaxWriteTxDuration <= 0 || o.AsyncSyncInterval <= 0 ||
		o.MaintenanceInterval <= 0 || o.MaxReadTxAge <= 0 || o.MinSnapshotAgeForEviction < 0:
		return bad("durations")
	case o.MaxWriterQueue <= 0 || o.MaxActiveTransactions <= 0 || o.MaxPendingCommits <= 0 || o.GroupMaxCommits <= 0:
		return bad("counts")
	case o.MaxTxBytes <= 0 || o.MaxPendingCommitBytes <= 0 || o.MaxWALBytes <= 0 || o.WALSegmentBytes <= 0 ||
		o.GroupMaxBytes <= 0 || o.AsyncSyncBytes <= 0 || o.CacheBytes <= 0 ||
		o.CheckpointBytes <= 0 || o.CheckpointSegments <= 0 || o.WALPinnedSoftBytes <= 0:
		return bad("sizes")
	case o.SnapshotPressurePolicy < ExpireOldest || o.SnapshotPressurePolicy > RejectWrites:
		return bad("snapshot pressure policy")
	case o.IDReservationInitial == 0 || o.IDReservationMax < o.IDReservationInitial:
		return bad("identifier reservation blocks")
	case o.PruneBudget <= 0 || o.Now == nil:
		return bad("prune budget or clock")
	case o.AsyncSyncBytes > o.MaxPendingCommitBytes:
		return bad("AsyncSyncBytes above MaxPendingCommitBytes")
	case o.GroupMaxBytes > MaxGroupBytes || o.MaxTxBytes > MaxGroupBytes/2:
		return bad("GroupMaxBytes or MaxTxBytes too large for a log segment")
	}
	return nil
}
