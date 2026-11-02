package tacdb

import (
	"time"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/value"
)

// Durability is the durability mode of a handle (SPEC §9.2).
type Durability = engine.Durability

// Durability modes.
const (
	// GroupSync makes each commit durable before it returns, sharing
	// Syncs among concurrent commits.
	GroupSync = engine.GroupSync
	// Sync makes each commit durable with a Sync of its own.
	Sync = engine.Sync
	// Async returns once the commit is in the log, with Durable false;
	// Flush, WaitDurable or a periodic Sync make it durable.
	Async = engine.Async
)

// PressurePolicy says what happens when old snapshots hold the log
// (SPEC §10.1).
type PressurePolicy = engine.PressurePolicy

// Pressure policies.
const (
	ExpireOldest = engine.ExpireOldest
	RejectWrites = engine.RejectWrites
)

// Unlimited is the MaxReadTxAge that never expires a snapshot by age.
const Unlimited = engine.Unlimited

// Event is a diagnostic event of maintenance.
type Event = engine.Event

// Options configure a handle (SPEC §24.1). Start from DefaultOptions: a
// zero BusyTimeout, for example, means fail at once.
type Options struct {
	Create   bool // create the database if it does not exist; DefaultOptions sets it
	ReadOnly bool

	Durability Durability

	BusyTimeout           time.Duration // wait of BeginWrite in the writer queue
	WriterQueueCapacity   int
	MaxActiveTransactions int
	MaxWriteTxDuration    time.Duration // from admission
	MaxTxBytes            int64         // pages changed by one transaction

	MaxPendingCommits     int
	MaxPendingCommitBytes int64
	MaxWALBytes           int64
	WALSegmentBytes       int64
	GroupMaxCommits       int
	GroupTargetBytes      int64
	AsyncSyncInterval     time.Duration
	AsyncSyncBytes        int64

	IDReservationInitial uint64
	IDReservationMax     uint64

	CheckpointThreshold int64 // log bytes between passive checkpoints
	CheckpointSegments  int
	ScrubFreedPages     bool
	MaintenanceInterval time.Duration

	MaxReadTxAge              time.Duration // Unlimited to never expire by age
	SnapshotPressurePolicy    PressurePolicy
	WALPinnedSoftBytes        int64
	MinSnapshotAgeForEviction time.Duration
	// OnEvent receives maintenance events, with no lock of the handle
	// held; it must not block.
	OnEvent func(Event)

	CacheBytes int64

	MaxSymbolsPerNamespace uint32
	MaxLabelsPerNode       int
	MaxPropertiesPerEntity int
	// IndexKeyPrefixBytes is the escaped length past which an index entry
	// keeps a prefix of a String or Bytes value and a hash (SPEC §12.2).
	// Each index keeps the value it was created with.
	IndexKeyPrefixBytes int
	// ValueLimits bound each property value: depth, encoded bytes,
	// elements, map key bytes and vector dimensions.
	ValueLimits value.Limits
}

// DefaultOptions returns the defaults of SPEC §24.1, with Create set.
func DefaultOptions() Options {
	e := engine.DefaultOptions()
	return Options{
		Create:                    true,
		Durability:                e.Durability,
		BusyTimeout:               e.BusyTimeout,
		WriterQueueCapacity:       e.MaxWriterQueue,
		MaxActiveTransactions:     e.MaxActiveTransactions,
		MaxWriteTxDuration:        e.MaxWriteTxDuration,
		MaxTxBytes:                e.MaxTxBytes,
		MaxPendingCommits:         e.MaxPendingCommits,
		MaxPendingCommitBytes:     e.MaxPendingCommitBytes,
		MaxWALBytes:               e.MaxWALBytes,
		WALSegmentBytes:           e.WALSegmentBytes,
		GroupMaxCommits:           e.GroupMaxCommits,
		GroupTargetBytes:          e.GroupMaxBytes,
		AsyncSyncInterval:         e.AsyncSyncInterval,
		AsyncSyncBytes:            e.AsyncSyncBytes,
		IDReservationInitial:      e.IDReservationInitial,
		IDReservationMax:          e.IDReservationMax,
		CheckpointThreshold:       e.CheckpointBytes,
		CheckpointSegments:        e.CheckpointSegments,
		ScrubFreedPages:           e.ScrubFreedPages,
		MaintenanceInterval:       e.MaintenanceInterval,
		MaxReadTxAge:              e.MaxReadTxAge,
		SnapshotPressurePolicy:    e.SnapshotPressurePolicy,
		WALPinnedSoftBytes:        e.WALPinnedSoftBytes,
		MinSnapshotAgeForEviction: e.MinSnapshotAgeForEviction,
		CacheBytes:                e.CacheBytes,
		MaxSymbolsPerNamespace:    catalog.DefaultMaxSymbols,
		MaxLabelsPerNode:          graph.DefaultMaxLabels,
		MaxPropertiesPerEntity:    graph.DefaultMaxProperties,
		IndexKeyPrefixBytes:       index.DefaultPrefixBytes,
		ValueLimits:               value.DefaultLimits(),
	}
}

func (o Options) engine() engine.Options {
	e := engine.DefaultOptions()
	e.Create, e.ReadOnly, e.Durability = o.Create, o.ReadOnly, o.Durability
	e.BusyTimeout, e.MaxWriterQueue = o.BusyTimeout, o.WriterQueueCapacity
	e.MaxActiveTransactions, e.MaxWriteTxDuration, e.MaxTxBytes = o.MaxActiveTransactions, o.MaxWriteTxDuration, o.MaxTxBytes
	e.MaxPendingCommits, e.MaxPendingCommitBytes = o.MaxPendingCommits, o.MaxPendingCommitBytes
	e.MaxWALBytes, e.WALSegmentBytes = o.MaxWALBytes, o.WALSegmentBytes
	e.GroupMaxCommits, e.GroupMaxBytes = o.GroupMaxCommits, o.GroupTargetBytes
	e.AsyncSyncInterval, e.AsyncSyncBytes = o.AsyncSyncInterval, o.AsyncSyncBytes
	e.IDReservationInitial, e.IDReservationMax = o.IDReservationInitial, o.IDReservationMax
	e.CheckpointBytes, e.CheckpointSegments, e.ScrubFreedPages = o.CheckpointThreshold, o.CheckpointSegments, o.ScrubFreedPages
	e.MaintenanceInterval = o.MaintenanceInterval
	e.MaxReadTxAge, e.SnapshotPressurePolicy = o.MaxReadTxAge, o.SnapshotPressurePolicy
	e.WALPinnedSoftBytes, e.MinSnapshotAgeForEviction = o.WALPinnedSoftBytes, o.MinSnapshotAgeForEviction
	e.OnEvent, e.CacheBytes = o.OnEvent, o.CacheBytes
	return e
}

// check validates the options of the graph; the engine checks its own.
func (o Options) check() error {
	bad := func(what string) error {
		return errs.New(errs.CodeInvalidArgument, "tacdb.Open", "invalid option: "+what)
	}
	switch {
	case o.MaxSymbolsPerNamespace == 0:
		return bad("MaxSymbolsPerNamespace")
	case o.MaxLabelsPerNode <= 0:
		return bad("MaxLabelsPerNode")
	case o.MaxPropertiesPerEntity <= 0:
		return bad("MaxPropertiesPerEntity")
	case o.IndexKeyPrefixBytes < index.MinPrefixBytes || o.IndexKeyPrefixBytes > index.MaxPrefixBytes:
		return bad("IndexKeyPrefixBytes")
	}
	return o.ValueLimits.Check()
}
