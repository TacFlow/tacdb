package graph

import (
	"context"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/value"
)

// StartBuild is the first phase of an online build (SPEC §12.2): it
// declares the index BUILDING with its build record, so that from its
// commit on every writer keeps the index. A declaration under the same
// name is returned as it is when it is the same, whatever its state, and
// is INDEX_EXISTS_DIFFERENT otherwise.
func (g *Graph) StartBuild(ctx context.Context, s *engine.Stmt, d IndexDef) (*index.Index, error) {
	const op = "tacdb.BuildIndex"
	for _, n := range []string{d.Name, d.Scope, d.Property} {
		if err := catalog.ValidName(op, n); err != nil {
			return nil, err
		}
	}
	if d.Entity > EdgeEntity {
		return nil, errs.Newf(errs.CodeInvalidArgument, op, "entity kind %d", d.Entity)
	}
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return nil, err
	}
	if ix, ok := set.Named(d.Name); ok {
		same, err := g.sameIndex(ctx, s, ix, d)
		if err == nil && !same {
			err = errs.Newf(errs.CodeIndexExistsDifferent, op, "an index %q exists with another declaration", d.Name)
		}
		return ix, err
	}
	def, err := g.intern(ctx, s, d)
	if err != nil {
		return nil, err
	}
	st := s.State()
	estimated := st.NodeCount
	if d.Entity == EdgeEntity {
		estimated = st.EdgeCount
	}
	return index.StartBuild(ctx, s, def, g.IndexPrefixBytes, estimated)
}

// intern resolves the names of d, interning them.
func (g *Graph) intern(ctx context.Context, s *engine.Stmt, d IndexDef) (index.Definition, error) {
	def := index.Definition{Name: d.Name, Entity: d.Entity.index(), Unique: d.Unique}
	if d.Entity == EdgeEntity {
		t, err := catalog.Intern[catalog.EdgeTypeID](ctx, s, d.Scope, g.MaxSymbols)
		if err != nil {
			return def, err
		}
		def.Scope = uint32(t)
	} else {
		l, err := catalog.Intern[catalog.LabelID](ctx, s, d.Scope, g.MaxSymbols)
		if err != nil {
			return def, err
		}
		def.Scope = uint32(l)
	}
	var err error
	def.Property, err = catalog.Intern[catalog.PropertyID](ctx, s, d.Property, g.MaxSymbols)
	return def, err
}

// building returns the index id when it is still declared and BUILDING:
// a batch of a dropped or finished build ends without effect.
func (g *Graph) building(ctx context.Context, s *engine.Stmt, id uint64) (*index.Index, error) {
	set, err := g.Indexes.Get(ctx, s)
	if err != nil {
		return nil, err
	}
	for _, ix := range set.All() {
		if ix.ID == id && ix.State == index.Building {
			return ix, nil
		}
	}
	return nil, nil
}

// BuildBatch is one batch of the second phase: it indexes the entities of
// the scope after the build's cursor, by ascending ID, until the
// transaction holds batchBytes, and moves the cursor. It reports whether
// the scope is done; a build that is gone (dropped, failed, finished)
// reports done with no effect. Values an ordered index refuses fail the
// build, as the caller decides.
func (g *Graph) BuildBatch(ctx context.Context, s *engine.Stmt, id uint64, batchBytes int64) (*index.Index, bool, error) {
	ix, err := g.building(ctx, s, id)
	if err != nil || ix == nil {
		return nil, true, err
	}
	st, ok, err := index.ReadBuild(ctx, s, ix)
	if err == nil && !ok {
		err = errs.Newf(errs.CodeCorrupt, "tacdb.BuildIndex", "the building index %d has no build record", ix.ID)
	}
	if err != nil {
		return ix, false, err
	}
	cursor, done := st.Cursor, true
	err = g.scopeWalk(ctx, s, ix, st.Cursor, func(id uint64, v value.Value, has bool) (bool, error) {
		if cursor != st.Cursor && s.Bytes() >= batchBytes { // at least one entity per batch
			done = false
			return true, nil
		}
		if has {
			if _, err := index.Fill(ctx, s, ix, &st, id, v); err != nil {
				return false, err
			}
		}
		cursor = id
		return false, nil
	})
	if err != nil {
		return ix, false, err
	}
	return ix, done, index.Advance(ctx, s, ix, st, cursor)
}

// FinishBuild is the third phase: ONLINE, or FAILED with the cause.
func (g *Graph) FinishBuild(ctx context.Context, s *engine.Stmt, id uint64) (index.State, error) {
	ix, err := g.building(ctx, s, id)
	if err != nil || ix == nil {
		return 0, err
	}
	return index.Finish(ctx, s, ix)
}

// FailBuild makes a building index FAILED with cause, if it is still
// building.
func (g *Graph) FailBuild(ctx context.Context, s *engine.Stmt, id uint64, cause error) error {
	ix, err := g.building(ctx, s, id)
	if err != nil || ix == nil {
		return err
	}
	return index.Fail(ctx, s, ix, cause)
}
