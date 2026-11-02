package graph

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/index"
)

// IndexDef describes a property index by names.
type IndexDef struct {
	Name     string
	Entity   Entity
	Scope    string // a label for nodes, an edge type for edges
	Property string
	Unique   bool
	State    index.State // reported by Indexes; ignored by CreateIndex
}

// backfillCheck is how many entities the backfill indexes between checks
// of MaxTxBytes.
const backfillCheck = 256

// CreateIndex declares a property index, ONLINE, and fills it from the
// transaction's snapshot and overlay, in the statement s (SPEC §12.2). An
// identical declaration under the same name is a no-op; a different one
// is INDEX_EXISTS_DIFFERENT. A duplicate in a unique index is
// UNIQUE_VIOLATION; work beyond MaxTxBytes is LIMIT_EXCEEDED.
func (g *Graph) CreateIndex(ctx context.Context, s *engine.Stmt, d IndexDef) error {
	const op = "tacdb.CreateIndex"
	for _, n := range []string{d.Name, d.Scope, d.Property} {
		if err := catalog.ValidName(op, n); err != nil {
			return err
		}
	}
	if d.Entity > EdgeEntity {
		return errs.Newf(errs.CodeInvalidArgument, op, "entity kind %d", d.Entity)
	}
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return err
	}
	if ix, ok := set.Named(d.Name); ok {
		same, err := g.sameIndex(ctx, s, ix, d)
		if err == nil && !same {
			err = errs.Newf(errs.CodeIndexExistsDifferent, op, "an index %q exists with another declaration", d.Name)
		}
		return err
	}
	var scope uint32
	if d.Entity == EdgeEntity {
		t, err := catalog.Intern[catalog.EdgeTypeID](ctx, s, d.Scope, g.MaxSymbols)
		if err != nil {
			return err
		}
		scope = uint32(t)
	} else {
		l, err := catalog.Intern[catalog.LabelID](ctx, s, d.Scope, g.MaxSymbols)
		if err != nil {
			return err
		}
		scope = uint32(l)
	}
	pid, err := catalog.Intern[catalog.PropertyID](ctx, s, d.Property, g.MaxSymbols)
	if err != nil {
		return err
	}
	ix, err := index.Declare(ctx, s, index.Definition{Name: d.Name, Entity: d.Entity.index(), Scope: scope, Property: pid, Unique: d.Unique},
		index.Online, g.IndexPrefixBytes)
	if err != nil {
		return err
	}
	err = g.backfill(ctx, s, ix)
	var e *errs.Error
	if errs.CodeOf(err) == errs.CodeLimitExceeded && errors.As(err, &e) {
		w := errs.Wrap(errs.CodeLimitExceeded, op, err, "the index does not fit one transaction: build it with BuildIndex")
		for k, v := range e.Details {
			w.With(k, v)
		}
		return w
	}
	return err
}

// sameIndex reports whether ix declares d, by names, without interning.
func (g *Graph) sameIndex(ctx context.Context, r engine.Reader, ix *index.Index, d IndexDef) (bool, error) {
	var scope uint32
	var ok bool
	var err error
	if d.Entity == EdgeEntity {
		var t catalog.EdgeTypeID
		t, ok, err = catalog.Lookup[catalog.EdgeTypeID](ctx, r, d.Scope)
		scope = uint32(t)
	} else {
		var l catalog.LabelID
		l, ok, err = catalog.Lookup[catalog.LabelID](ctx, r, d.Scope)
		scope = uint32(l)
	}
	if err != nil || !ok {
		return false, err
	}
	pid, ok, err := catalog.Lookup[catalog.PropertyID](ctx, r, d.Property)
	if err != nil || !ok {
		return false, err
	}
	return ix.Same(index.Definition{Entity: d.Entity.index(), Scope: scope, Property: pid, Unique: d.Unique}), nil
}

// backfill inserts the entries of every entity in the scope of ix that
// has its property, by ascending ID.
func (g *Graph) backfill(ctx context.Context, s *engine.Stmt, ix *index.Index) error {
	e := NodeEntity
	slot, lo := catalog.SlotLabels, codec.AppendKeyUint32(nil, ix.Scope)
	if ix.Entity == index.Edges {
		e, slot, lo = EdgeEntity, catalog.SlotEdges, nil
	}
	var hi []byte
	if lo != nil {
		hi = codec.PrefixEnd(lo)
	}
	n := 0
	for {
		// Read a batch of IDs first: the walk must not write its tree.
		var ids []uint64
		next := lo
		err := s.Range(ctx, slot, lo, hi, func(k, v []byte) error {
			if len(ids) == backfillCheck {
				return errStop
			}
			next = append(append(next[:0:0], k...), 0)
			if e == NodeEntity {
				ids = append(ids, binary.BigEndian.Uint64(k[4:]))
				return nil
			}
			if len(k) != 9 || k[8] != tagRecord {
				return nil // a property of an edge
			}
			rec, err := decodeEdge(v)
			if err != nil {
				return err
			}
			if uint32(rec.typ) == ix.Scope {
				ids = append(ids, binary.BigEndian.Uint64(k))
			}
			return nil
		})
		if err != nil && err != errStop {
			return err
		}
		for _, id := range ids {
			b, ok, err := s.Get(ctx, e.slot(), propertyKey(id, ix.Property))
			if err != nil {
				return err
			}
			if ok {
				v, err := stored(b)
				if err != nil {
					return err
				}
				if err := index.Insert(ctx, s, ix, id, v); err != nil {
					return err
				}
			}
			if n++; n%backfillCheck == 0 {
				if err := s.CheckLimit(); err != nil {
					return err
				}
			}
		}
		if err != errStop {
			return s.CheckLimit() // the remainder after the last periodic check
		}
		lo = next
	}
}

// DropIndex removes the index called name with its entries (SPEC §12.2);
// a missing one is INDEX_NOT_FOUND.
func (g *Graph) DropIndex(ctx context.Context, s *engine.Stmt, name string) error {
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return err
	}
	ix, ok := set.Named(name)
	if !ok {
		return errs.Newf(errs.CodeIndexNotFound, "tacdb.DropIndex", "no index %q", name)
	}
	return index.Drop(ctx, s, ix)
}

// ListIndexes returns the declared indexes by name.
func (g *Graph) ListIndexes(ctx context.Context, r engine.Reader) ([]IndexDef, error) {
	set, err := g.Indexes.Get(ctx, r)
	if err != nil {
		return nil, err
	}
	var out []IndexDef
	for _, ix := range set.All() {
		d := IndexDef{Name: ix.Name, Unique: ix.Unique, State: ix.State}
		if ix.Entity == index.Edges {
			d.Entity = EdgeEntity
			d.Scope, err = name(ctx, g, r, catalog.EdgeTypeID(ix.Scope))
		} else {
			d.Scope, err = name(ctx, g, r, catalog.LabelID(ix.Scope))
		}
		if err != nil {
			return nil, err
		}
		if d.Property, err = name(ctx, g, r, ix.Property); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}
