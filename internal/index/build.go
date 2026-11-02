package index

import (
	"context"
	"errors"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Prefixes of the Maintenance tree.
const (
	prefixBuild = 1 // (1, index): the state of an online build
	prefixPurge = 4 // (4, index): entries left to remove
)

// MaxConflicts bounds the conflicting pairs a build keeps for its status.
const MaxConflicts = 8

// Build is the state of an online build (SPEC §12.2): the last entity
// the batches reached, the entries they inserted, the net entries writers
// added meanwhile, the conflicts of uniqueness found, and why it failed.
type Build struct {
	Cursor      uint64
	Scanned     uint64
	WriterDelta int64
	Estimated   uint64
	Started     uint64   // the commit that declared the index BUILDING
	Conflicts   []uint64 // pairs of entities with equal values, at most MaxConflicts
	Failure     string   // the code of the failure, for a FAILED index
	Message     string
}

var buildRecord = catalog.Schema{Name: "index build", Version: 1, Fields: []catalog.Field{
	{Name: "cursor", Kind: value.KindInt, Required: true},
	{Name: "scanned", Kind: value.KindInt, Required: true},
	{Name: "writerDelta", Kind: value.KindInt, Required: true},
	{Name: "estimated", Kind: value.KindInt, Required: true},
	{Name: "started", Kind: value.KindInt, Required: true},
	{Name: "conflicts", Kind: value.KindList, Required: true},
	{Name: "failure", Kind: value.KindString},
	{Name: "message", Kind: value.KindString},
}}

var purgeRecord = catalog.Schema{Name: "index purge", Version: 1, Fields: []catalog.Field{
	{Name: "entity", Kind: value.KindInt, Required: true},
}}

func maintenanceKey(prefix uint8, id uint64) []byte {
	return codec.AppendKeyUint64(codec.AppendKeyUint8(nil, prefix), id)
}

// ReadBuild returns the build state of ix, if it has one.
func ReadBuild(ctx context.Context, r engine.Reader, ix *Index) (Build, bool, error) {
	b, ok, err := r.Get(ctx, catalog.SlotMaintenance, maintenanceKey(prefixBuild, ix.ID))
	if err != nil || !ok {
		return Build{}, false, err
	}
	rec, err := buildRecord.Decode(b)
	if err != nil {
		return Build{}, false, err
	}
	bad := func(f string) error {
		return errs.Newf(errs.CodeCorrupt, "index", "build record of index %d: %s", ix.ID, f)
	}
	var st Build
	for f, dst := range map[string]*uint64{"cursor": &st.Cursor, "scanned": &st.Scanned, "estimated": &st.Estimated, "started": &st.Started} {
		n, _ := rec[f].AsInt()
		if n < 0 {
			return Build{}, false, bad(f)
		}
		*dst = uint64(n)
	}
	st.WriterDelta, _ = rec["writerDelta"].AsInt()
	c := rec["conflicts"]
	if c.Len() > 2*MaxConflicts || c.Len()%2 != 0 {
		return Build{}, false, bad("conflicts")
	}
	for i := 0; i < c.Len(); i++ {
		n, ok := c.Index(i).AsInt()
		if !ok || n <= 0 {
			return Build{}, false, bad("conflicts")
		}
		st.Conflicts = append(st.Conflicts, uint64(n))
	}
	st.Failure, _ = rec["failure"].AsString()
	st.Message, _ = rec["message"].AsString()
	return st, true, nil
}

func writeBuild(ctx context.Context, s *engine.Stmt, ix *Index, st Build) error {
	conflicts := make([]value.Value, len(st.Conflicts))
	for i, c := range st.Conflicts {
		conflicts[i] = value.Int(int64(c))
	}
	fields := map[string]value.Value{
		"cursor":      value.Int(int64(st.Cursor)),
		"scanned":     value.Int(int64(st.Scanned)),
		"writerDelta": value.Int(st.WriterDelta),
		"estimated":   value.Int(int64(st.Estimated)),
		"started":     value.Int(int64(st.Started)),
		"conflicts":   value.List(conflicts...),
	}
	if st.Failure != "" {
		fields["failure"], fields["message"] = value.String(st.Failure), value.String(st.Message)
	}
	b, err := buildRecord.Encode(fields)
	if err != nil {
		return err
	}
	return s.Put(ctx, catalog.SlotMaintenance, maintenanceKey(prefixBuild, ix.ID), b)
}

// build reads the build state of a BUILDING index, which must have one.
func build(ctx context.Context, s *engine.Stmt, ix *Index) (Build, error) {
	st, ok, err := ReadBuild(ctx, s, ix)
	if err == nil && !ok {
		err = errs.Newf(errs.CodeCorrupt, "index", "the building index %d has no build record", ix.ID)
	}
	return st, err
}

// StartBuild declares an index BUILDING with its build record, in the
// statement that will be the first phase of an online build: from its
// commit on, writers keep the index.
func StartBuild(ctx context.Context, s *engine.Stmt, d Definition, prefixBytes int, estimated uint64) (*Index, error) {
	ix, err := Declare(ctx, s, d, Building, prefixBytes)
	if err != nil {
		return nil, err
	}
	return ix, writeBuild(ctx, s, ix, Build{Estimated: estimated, Started: s.Base().CommitID + 1})
}

// conflict records a pair of entities with equal values in a BUILDING
// unique index; the build fails at its end.
func conflict(st *Build, a, b uint64) {
	if len(st.Conflicts) < 2*MaxConflicts {
		st.Conflicts = append(st.Conflicts, a, b)
	}
}

// Fill inserts the entry of entity for v in a BUILDING index from a batch
// of the build, unless a writer already did; uniqueness conflicts are
// recorded, not raised. It returns whether it inserted.
func Fill(ctx context.Context, s *engine.Stmt, ix *Index, st *Build, entity uint64, v value.Value) (bool, error) {
	k, _, err := entryKey(ix, entity, v)
	if err != nil {
		return false, err
	}
	if err := codec.CheckKey(k); err != nil {
		return false, err
	}
	if _, ok, err := s.Get(ctx, ix.Entity.Slot(), k); err != nil || ok {
		return false, err
	}
	if ix.Unique && !v.IsNull() {
		other, dup, err := duplicate(ctx, s, ix, entity, v)
		if err != nil {
			return false, err
		}
		if dup {
			conflict(st, other, entity)
		}
	}
	if err := s.Put(ctx, ix.Entity.Slot(), k, nil); err != nil {
		return false, err
	}
	st.Scanned++
	return true, addEntries(ctx, s, ix, 1)
}

// Advance records where the batches of a build reached.
func Advance(ctx context.Context, s *engine.Stmt, ix *Index, st Build, cursor uint64) error {
	st.Cursor = cursor
	return writeBuild(ctx, s, ix, st)
}

// SetState rewrites the declaration of ix with state and raises the
// schema epoch.
func SetState(ctx context.Context, s *engine.Stmt, ix *Index, state State) error {
	c := *ix
	c.State = state
	b, err := c.encode()
	if err != nil {
		return err
	}
	if err := s.Put(ctx, catalog.SlotCatalog, declarationKey(ix.ID), b); err != nil {
		return err
	}
	return s.BumpSchemaEpoch()
}

// Finish is the last phase of a build, in O(1): with no conflict and an
// entry counter equal to the entries of the batches plus those of the
// writers, the index goes ONLINE; otherwise FAILED, with the conflicts
// (UNIQUE_VIOLATION) or the mismatch (INDEX_INCONSISTENT), and its entries
// are purged.
func Finish(ctx context.Context, s *engine.Stmt, ix *Index) (State, error) {
	st, err := build(ctx, s, ix)
	if err != nil {
		return 0, err
	}
	n, err := Entries(ctx, s, ix)
	if err != nil {
		return 0, err
	}
	switch {
	case len(st.Conflicts) > 0:
		return Failed, Fail(ctx, s, ix, errs.Newf(errs.CodeUniqueViolation, "tacdb.BuildIndex",
			"%d entities share a value of the unique index %q", len(st.Conflicts), ix.Name))
	case int64(n) != int64(st.Scanned)+st.WriterDelta:
		return Failed, Fail(ctx, s, ix, errs.Newf(errs.CodeIndexInconsistent, "tacdb.BuildIndex",
			"index %q holds %d entries, its build counted %d", ix.Name, n, int64(st.Scanned)+st.WriterDelta))
	}
	return Online, SetState(ctx, s, ix, Online)
}

// Fail makes a BUILDING index FAILED with cause, keeps the cause in its
// build record and schedules the purge of its entries.
func Fail(ctx context.Context, s *engine.Stmt, ix *Index, cause error) error {
	st, err := build(ctx, s, ix)
	if err != nil {
		return err
	}
	st.Failure, st.Message = string(errs.CodeOf(cause)), cause.Error()
	var e *errs.Error
	if errors.As(cause, &e) {
		st.Message = e.Message
	}
	if st.Failure == "" {
		st.Failure = string(errs.CodeIndexBuildFailed)
	}
	if err := writeBuild(ctx, s, ix, st); err != nil {
		return err
	}
	if err := schedulePurge(ctx, s, ix); err != nil {
		return err
	}
	return SetState(ctx, s, ix, Failed)
}

func schedulePurge(ctx context.Context, s *engine.Stmt, ix *Index) error {
	b, err := purgeRecord.Encode(map[string]value.Value{"entity": value.Int(int64(ix.Entity))})
	if err != nil {
		return err
	}
	return s.Put(ctx, catalog.SlotMaintenance, maintenanceKey(prefixPurge, ix.ID), b)
}

// Purge is an index whose entries are being removed in batches.
type Purge struct {
	ID     uint64
	Entity Entity
}

// Purges returns the pending purges.
func Purges(ctx context.Context, r engine.Reader) ([]Purge, error) {
	var out []Purge
	lo := codec.AppendKeyUint8(nil, prefixPurge)
	err := r.Range(ctx, catalog.SlotMaintenance, lo, codec.PrefixEnd(lo), func(k, v []byte) error {
		kr := codec.NewKeyReader(k[1:])
		id, err := kr.ReadUint64()
		if err != nil || kr.Len() != 0 || id == 0 {
			return errs.Newf(errs.CodeCorrupt, "index", "purge key %x", k)
		}
		rec, err := purgeRecord.Decode(v)
		if err != nil {
			return err
		}
		e, _ := rec["entity"].AsInt()
		if e < 0 || e > int64(Edges) {
			return errs.Newf(errs.CodeCorrupt, "index", "purge of index %d over entity %d", id, e)
		}
		out = append(out, Purge{id, Entity(e)})
		return nil
	})
	return out, err
}

// PurgeBatch removes up to limit entries of a purge; once none is left it
// removes the counter and the purge itself, and reports done.
func PurgeBatch(ctx context.Context, s *engine.Stmt, p Purge, limit int) (bool, error) {
	var keys [][]byte
	lo := indexPrefix(p.ID)
	err := s.Range(ctx, p.Entity.Slot(), lo, codec.PrefixEnd(lo), func(k, _ []byte) error {
		if len(keys) == limit {
			return errStop
		}
		keys = append(keys, append([]byte(nil), k...))
		return nil
	})
	if err != nil && err != errStop {
		return false, err
	}
	for _, k := range keys {
		if _, err := s.Delete(ctx, p.Entity.Slot(), k); err != nil {
			return false, err
		}
	}
	if err == errStop {
		return false, nil
	}
	if _, err := s.Delete(ctx, catalog.SlotStats, counterKey(p.ID)); err != nil {
		return false, err
	}
	_, err = s.Delete(ctx, catalog.SlotMaintenance, maintenanceKey(prefixPurge, p.ID))
	return true, err
}
