package index

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// prefixEntries is the prefix of the entry counters in the Stats tree.
const prefixEntries = 1

var errStop = errors.New("stop")

func indexPrefix(id uint64) []byte { return codec.AppendKeyUint64(nil, id) }

// EntryKey is the key of the entry of entity in ix for v, for checks
// that rebuild an index.
func EntryKey(ix *Index, entity uint64, v value.Value) ([]byte, error) {
	k, _, err := entryKey(ix, entity, v)
	return k, err
}

// entryKey is the key of the entry of entity in ix for v.
func entryKey(ix *Index, entity uint64, v value.Value) ([]byte, form, error) {
	k, f, err := appendSortable(indexPrefix(ix.ID), v, ix.PrefixBytes)
	if err != nil {
		return nil, f, err
	}
	return codec.AppendKeyUint64(k, entity), f, nil
}

func counterKey(id uint64) []byte {
	return codec.AppendKeyUint64(codec.AppendKeyUint8(nil, prefixEntries), id)
}

// Entries returns the entry counter of ix, which every insertion and
// removal adjusts in the same statement.
func Entries(ctx context.Context, r engine.Reader, ix *Index) (uint64, error) {
	b, ok, err := r.Get(ctx, catalog.SlotStats, counterKey(ix.ID))
	if err != nil || !ok {
		return 0, err
	}
	if len(b) != 8 || binary.LittleEndian.Uint64(b) == 0 {
		return 0, errs.Newf(errs.CodeCorrupt, "index", "entry counter %x of index %d", b, ix.ID)
	}
	return binary.LittleEndian.Uint64(b), nil
}

func addEntries(ctx context.Context, s *engine.Stmt, ix *Index, by int64) error {
	n, err := Entries(ctx, s, ix)
	if err != nil {
		return err
	}
	if by < 0 && n < uint64(-by) {
		return errs.Newf(errs.CodeCorrupt, "index", "index %d counts %d entries, below a change of %d", ix.ID, n, by)
	}
	if n = uint64(int64(n) + by); n == 0 {
		_, err = s.Delete(ctx, catalog.SlotStats, counterKey(ix.ID))
		return err
	}
	return s.Put(ctx, catalog.SlotStats, counterKey(ix.ID), binary.LittleEndian.AppendUint64(nil, n))
}

// Insert adds the entry of entity for v to ix. In a unique ONLINE index,
// another live entity with an equal value fails it with UNIQUE_VIOLATION,
// and the statement undoes the operation (SPEC §12.2).
func Insert(ctx context.Context, s *engine.Stmt, ix *Index, entity uint64, v value.Value) error {
	k, _, err := entryKey(ix, entity, v)
	if err != nil {
		return err
	}
	if err := codec.CheckKey(k); err != nil {
		return err
	}
	if ix.Unique && ix.State == Online && !v.IsNull() {
		if err := unique(ctx, s, ix, entity, v); err != nil {
			return err
		}
	}
	if _, ok, err := s.Get(ctx, ix.Entity.Slot(), k); err != nil {
		return err
	} else if ok {
		return errs.Newf(errs.CodeCorrupt, "index", "index %d already holds an entry of %d", ix.ID, entity)
	}
	if err := s.Put(ctx, ix.Entity.Slot(), k, nil); err != nil {
		return err
	}
	return addEntries(ctx, s, ix, 1)
}

// Remove deletes the entry of entity for v from ix; a missing entry is
// corruption.
func Remove(ctx context.Context, s *engine.Stmt, ix *Index, entity uint64, v value.Value) error {
	k, _, err := entryKey(ix, entity, v)
	if err != nil {
		return errs.Wrap(errs.CodeCorrupt, "index", err, "a stored value has no entry key")
	}
	found, err := s.Delete(ctx, ix.Entity.Slot(), k)
	if err != nil {
		return err
	}
	if !found {
		return errs.Newf(errs.CodeCorrupt, "index", "index %d lacks the entry of %d", ix.ID, entity)
	}
	return addEntries(ctx, s, ix, -1)
}

// unique looks for another entity with a value equal to v, by the codec
// of equality: in v's family and, for a number, in the other numeric
// family when v converts to it exactly. A truncated or hashed key only
// names candidates, which the base record confirms.
func unique(ctx context.Context, s *engine.Stmt, ix *Index, entity uint64, v value.Value) error {
	probes := []value.Value{v}
	if sib, ok := sibling(v); ok {
		probes = append(probes, sib)
	}
	for _, p := range probes {
		lo, f, err := appendSortable(indexPrefix(ix.ID), p, ix.PrefixBytes)
		if err != nil {
			return err
		}
		var dup bool
		err = s.Range(ctx, ix.Entity.Slot(), lo, codec.PrefixEnd(lo), func(k, _ []byte) error {
			if len(k) != len(lo)+8 {
				return nil // a longer key under the prefix: another value
			}
			other := binary.BigEndian.Uint64(k[len(lo):])
			if other == entity {
				return nil
			}
			if f != exact {
				w, err := baseValue(ctx, s, ix, other)
				if err != nil {
					return err
				}
				if !equal(w, v) {
					return nil
				}
			}
			dup = true
			return errStop
		})
		if err != nil && err != errStop {
			return err
		}
		if dup {
			return errs.Newf(errs.CodeUniqueViolation, "index", "the unique index %q already holds an equal value", ix.Name)
		}
	}
	return nil
}

// baseValue reads the indexed property of entity from its record.
func baseValue(ctx context.Context, r engine.Reader, ix *Index, entity uint64) (value.Value, error) {
	b, ok, err := r.Get(ctx, ix.Entity.base(), catalog.PropertyKey(entity, ix.Property))
	if err != nil {
		return value.Value{}, err
	}
	if !ok {
		return value.Value{}, errs.Newf(errs.CodeCorrupt, "index", "index %d has an entry of %d without the property", ix.ID, entity)
	}
	return codec.DecodeValue(b, codec.DecodeOptions{Limits: StoredLimits})
}

// StoredLimits decode values already stored: they were validated when
// written, maybe under larger limits than the handle's now, so the work
// that keeps indexes and structures uses the maxima of the format.
var StoredLimits = value.Limits{
	MaxDepth:            value.FormatMaxDepth,
	MaxValueBytes:       value.FormatMaxValueBytes,
	MaxValueElements:    1 << 30,
	MaxMapKeyBytes:      value.FormatMaxMapKeyBytes,
	MaxVectorDimensions: value.FormatMaxVectorDimensions,
}

// Definition describes a new property index by symbol identifiers.
type Definition struct {
	Name     string
	Entity   Entity
	Scope    uint32
	Property catalog.PropertyID
	Unique   bool
}

// Same reports whether ix declares d.
func (ix *Index) Same(d Definition) bool {
	return ix.Entity == d.Entity && ix.Scope == d.Scope && ix.Property == d.Property && ix.Unique == d.Unique
}

// Declare writes the declaration of a new index in state, with a new ID,
// and raises the schema epoch. The caller checked the name.
func Declare(ctx context.Context, s *engine.Stmt, d Definition, state State, prefixBytes int) (*Index, error) {
	if prefixBytes < MinPrefixBytes || prefixBytes > MaxPrefixBytes {
		return nil, errs.Newf(errs.CodeInvalidArgument, "index", "IndexKeyPrefixBytes %d outside [%d, %d]", prefixBytes, MinPrefixBytes, MaxPrefixBytes)
	}
	id, err := nextID(ctx, s)
	if err != nil {
		return nil, err
	}
	ix := &Index{ID: id, Name: d.Name, Entity: d.Entity, Scope: d.Scope, Property: d.Property, Unique: d.Unique,
		State: state, PrefixBytes: prefixBytes}
	b, err := ix.encode()
	if err != nil {
		return nil, err
	}
	if err := s.Put(ctx, catalog.SlotCatalog, declarationKey(id), b); err != nil {
		return nil, err
	}
	return ix, s.BumpSchemaEpoch()
}

// Drop removes the declaration of ix, its entries and its counter, and
// raises the schema epoch.
func Drop(ctx context.Context, s *engine.Stmt, ix *Index) error {
	found, err := s.Delete(ctx, catalog.SlotCatalog, declarationKey(ix.ID))
	if err != nil {
		return err
	}
	if !found {
		return errs.Newf(errs.CodeCorrupt, "index", "index %d has no declaration", ix.ID)
	}
	var keys [][]byte
	p := indexPrefix(ix.ID)
	if err := s.Range(ctx, ix.Entity.Slot(), p, codec.PrefixEnd(p), func(k, _ []byte) error {
		keys = append(keys, append([]byte(nil), k...))
		return nil
	}); err != nil {
		return err
	}
	for i, k := range keys {
		if _, err := s.Delete(ctx, ix.Entity.Slot(), k); err != nil {
			return err
		}
		if i%256 == 255 {
			if err := s.CheckLimit(); err != nil {
				return err
			}
		}
	}
	if _, err := s.Delete(ctx, catalog.SlotStats, counterKey(ix.ID)); err != nil {
		return err
	}
	return s.BumpSchemaEpoch()
}
