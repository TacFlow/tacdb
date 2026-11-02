package catalog

import (
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// SymbolKind is a namespace of symbols (SPEC §7.4).
type SymbolKind uint8

// Namespaces of symbols.
const (
	KindLabel    SymbolKind = 1
	KindProperty SymbolKind = 2
	KindEdgeType SymbolKind = 3
)

// Symbol identifiers: u32 with zero reserved, one Go type per namespace,
// so that an ID of one namespace is never used for another.
type (
	LabelID    uint32
	PropertyID uint32
	EdgeTypeID uint32
)

func (LabelID) kind() SymbolKind    { return KindLabel }
func (PropertyID) kind() SymbolKind { return KindProperty }
func (EdgeTypeID) kind() SymbolKind { return KindEdgeType }

// ID is an identifier of one of the namespaces.
type ID interface {
	~uint32
	kind() SymbolKind
}

const (
	// MaxNameBytes limits label, property and edge type names (SPEC §5.1).
	MaxNameBytes = 255
	// DefaultMaxSymbols is the default of MaxSymbolsPerNamespace.
	DefaultMaxSymbols = 1_000_000
)

// ValidName checks a structural name: valid UTF-8, not empty, no NUL, at
// most MaxNameBytes (SPEC §5.1).
func ValidName(op, name string) error {
	switch {
	case name == "":
		return errs.New(errs.CodeInvalidArgument, op, "empty name")
	case !utf8.ValidString(name):
		return errs.New(errs.CodeInvalidArgument, op, "name is not valid UTF-8")
	case strings.IndexByte(name, 0) >= 0:
		return errs.New(errs.CodeInvalidArgument, op, "name holds a NUL byte")
	case len(name) > MaxNameBytes:
		return errs.Newf(errs.CodeLimitExceeded, op, "name of %d bytes", len(name)).
			With("bytes", int64(len(name))).With("limit", MaxNameBytes)
	}
	return nil
}

func forwardKey(k SymbolKind, name string) []byte {
	return codec.AppendKeyString(codec.AppendKeyUint8(nil, uint8(k)), name)
}

func reverseKey(k SymbolKind, id uint32) []byte {
	return codec.AppendKeyUint32(codec.AppendKeyUint8(nil, uint8(k)), id)
}

func watermarkKey(k SymbolKind) []byte {
	return codec.AppendKeyUint8(codec.AppendKeyUint8(nil, PrefixSymbols), uint8(k))
}

// symbolWatermark is the record of a namespace's watermark: the largest
// identifier handed out.
var symbolWatermark = Schema{Name: "symbol watermark", Version: 1, Fields: []Field{
	{Name: "hwm", Kind: value.KindInt, Required: true},
}}

func lookup(ctx context.Context, r engine.Reader, k SymbolKind, name string) (uint32, bool, error) {
	v, ok, err := r.Get(ctx, SlotSymbolsForward, forwardKey(k, name))
	if err != nil || !ok {
		return 0, false, err
	}
	if len(v) != 4 || binary.BigEndian.Uint32(v) == 0 {
		return 0, false, errs.Newf(errs.CodeCorrupt, "catalog.Lookup", "symbol %q holds %x", name, v)
	}
	return binary.BigEndian.Uint32(v), true, nil
}

// Lookup returns the identifier of name, without interning it: an
// unknown or invalid name is not found (SPEC §12.1).
func Lookup[T ID](ctx context.Context, r engine.Reader, name string) (T, bool, error) {
	id, ok, err := lookup(ctx, r, T.kind(0), name)
	return T(id), ok, err
}

// Name returns the name of id.
func Name[T ID](ctx context.Context, r engine.Reader, id T) (string, bool, error) {
	v, ok, err := r.Get(ctx, SlotSymbolsReverse, reverseKey(id.kind(), uint32(id)))
	if err != nil || !ok {
		return "", false, err
	}
	return string(v), true, nil
}

func watermark(ctx context.Context, r engine.Reader, k SymbolKind) (uint32, error) {
	b, ok, err := r.Get(ctx, SlotCatalog, watermarkKey(k))
	if err != nil || !ok {
		return 0, err
	}
	rec, err := symbolWatermark.Decode(b)
	if err != nil {
		return 0, err
	}
	hwm, _ := rec["hwm"].AsInt()
	if hwm < 0 || hwm > 1<<32-1 {
		return 0, errs.Newf(errs.CodeCorrupt, "catalog.watermark", "symbol watermark %d", hwm)
	}
	return uint32(hwm), nil
}

// Intern returns the identifier of name, creating it when it does not
// exist, within the statement s: a rollback undoes the symbol and the
// watermark together. limit is MaxSymbolsPerNamespace.
func Intern[T ID](ctx context.Context, s *engine.Stmt, name string, limit uint32) (T, error) {
	const op = "catalog.Intern"
	k := T.kind(0)
	if err := ValidName(op, name); err != nil {
		return 0, err
	}
	if id, ok, err := lookup(ctx, s, k, name); err != nil || ok {
		return T(id), err
	}
	if limit == 0 {
		return 0, errs.New(errs.CodeInvalidArgument, op, "MaxSymbolsPerNamespace is zero")
	}
	hwm, err := watermark(ctx, s, k)
	if err != nil {
		return 0, err
	}
	if uint64(hwm) > s.State().SymbolHWM {
		return 0, errs.Newf(errs.CodeCorrupt, op, "namespace %d watermark %d above the aggregate %d", k, hwm, s.State().SymbolHWM)
	}
	if hwm >= limit {
		return 0, errs.Newf(errs.CodeLimitExceeded, op, "namespace %d holds %d symbols", k, hwm).
			With("limit", int64(limit))
	}
	id := hwm + 1
	if _, taken, err := s.Get(ctx, SlotSymbolsReverse, reverseKey(k, id)); err != nil {
		return 0, err
	} else if taken {
		return 0, errs.Newf(errs.CodeCorrupt, op, "namespace %d watermark %d below symbol %d", k, hwm, id)
	}
	rec, err := symbolWatermark.Encode(map[string]value.Value{"hwm": value.Int(int64(id))})
	if err != nil {
		return 0, err
	}
	if err := s.Put(ctx, SlotSymbolsForward, forwardKey(k, name), binary.BigEndian.AppendUint32(nil, id)); err != nil {
		return 0, err
	}
	if err := s.Put(ctx, SlotSymbolsReverse, reverseKey(k, id), []byte(name)); err != nil {
		return 0, err
	}
	if err := s.Put(ctx, SlotCatalog, watermarkKey(k), rec); err != nil {
		return 0, err
	}
	if err := s.RaiseSymbolHWM(uint64(id)); err != nil {
		return 0, err
	}
	return T(id), nil
}

// Cache remembers committed symbols for a handle, and dies with it. It
// is filled only from read transactions, whose snapshots are published.
// Each entry keeps the commit of the snapshot that found it: symbols are
// never removed, so the entry holds for that snapshot and every later
// one, and a reader of an older snapshot reads its own trees.
type Cache struct {
	mu      sync.RWMutex
	names   map[cacheKey]cached
	ids     map[cacheID]cachedName
	maxSize int
}

type cacheKey struct {
	kind SymbolKind
	name string
}

type cacheID struct {
	kind SymbolKind
	id   uint32
}

type cached struct {
	id     uint32
	commit uint64
}

type cachedName struct {
	name   string
	commit uint64
}

// NewCache returns a cache of at most maxSize entries per direction.
func NewCache(maxSize int) *Cache {
	c := &Cache{maxSize: maxSize}
	c.reset()
	return c
}

func (c *Cache) reset() {
	c.names, c.ids = map[cacheKey]cached{}, map[cacheID]cachedName{}
}

func (c *Cache) put(k SymbolKind, name string, id uint32, commit uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.names) >= c.maxSize {
		c.reset() // the symbols in use come back on the next lookups
	}
	c.names[cacheKey{k, name}] = cached{id, commit}
	c.ids[cacheID{k, id}] = cachedName{name, commit}
}

// CachedLookup is Lookup through c.
func CachedLookup[T ID](ctx context.Context, c *Cache, r engine.Reader, name string) (T, bool, error) {
	k, at := T.kind(0), r.State().CommitID
	c.mu.RLock()
	e, ok := c.names[cacheKey{k, name}]
	c.mu.RUnlock()
	if ok && e.commit <= at {
		return T(e.id), true, nil
	}
	got, ok, err := Lookup[T](ctx, r, name)
	if err == nil && ok && committed(r) {
		c.put(k, name, uint32(got), at)
	}
	return got, ok, err
}

// CachedName is Name through c.
func CachedName[T ID](ctx context.Context, c *Cache, r engine.Reader, id T) (string, bool, error) {
	k, at := id.kind(), r.State().CommitID
	c.mu.RLock()
	e, ok := c.ids[cacheID{k, uint32(id)}]
	c.mu.RUnlock()
	if ok && e.commit <= at {
		return e.name, true, nil
	}
	name, ok, err := Name(ctx, r, id)
	if err == nil && ok && committed(r) {
		c.put(k, name, uint32(id), at)
	}
	return name, ok, err
}

func committed(r engine.Reader) bool {
	_, ok := r.(*engine.ReadTx)
	return ok
}
