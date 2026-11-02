package index

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/wal"
	"github.com/TacFlow/tacdb/value"
)

// Entity is the kind of entity an index covers.
type Entity uint8

// Entities.
const (
	Nodes Entity = iota
	Edges
)

// Slot is the tree of the entries of indexes over e.
func (e Entity) Slot() int {
	if e == Edges {
		return catalog.SlotEdgeProperty
	}
	return catalog.SlotNodeProperty
}

// base is the tree of the records of e.
func (e Entity) base() int {
	if e == Edges {
		return catalog.SlotEdges
	}
	return catalog.SlotNodes
}

// State is the state of an index (SPEC §12.2).
type State uint8

// States.
const (
	Building State = iota
	Online
	Failed
)

// kindProperty is the only kind of index in this milestone; full-text and
// vector indexes come with their own.
const kindProperty = 0

// Index is a declared index.
type Index struct {
	ID          uint64
	Name        string
	Entity      Entity
	Scope       uint32 // a label for nodes, an edge type for edges
	Property    catalog.PropertyID
	Unique      bool
	State       State
	PrefixBytes int // IndexKeyPrefixBytes when the index was created
}

// maintained reports whether writers keep the entries of the index.
func (ix *Index) maintained() bool { return ix.State == Building || ix.State == Online }

// declaration is the Catalog record of an index, at key (2, ID).
var declaration = catalog.Schema{Name: "index", Version: 1, Fields: []catalog.Field{
	{Name: "name", Kind: value.KindString, Required: true},
	{Name: "entity", Kind: value.KindInt, Required: true},
	{Name: "scope", Kind: value.KindInt, Required: true},
	{Name: "property", Kind: value.KindInt, Required: true},
	{Name: "kind", Kind: value.KindInt, Required: true},
	{Name: "unique", Kind: value.KindBool, Required: true},
	{Name: "state", Kind: value.KindInt, Required: true},
	{Name: "prefixBytes", Kind: value.KindInt, Required: true},
}}

func declarationKey(id uint64) []byte {
	return codec.AppendKeyUint64(codec.AppendKeyUint8(nil, catalog.PrefixIndex), id)
}

func (ix *Index) encode() ([]byte, error) {
	return declaration.Encode(map[string]value.Value{
		"name":        value.String(ix.Name),
		"entity":      value.Int(int64(ix.Entity)),
		"scope":       value.Int(int64(ix.Scope)),
		"property":    value.Int(int64(ix.Property)),
		"kind":        value.Int(kindProperty),
		"unique":      value.Bool(ix.Unique),
		"state":       value.Int(int64(ix.State)),
		"prefixBytes": value.Int(int64(ix.PrefixBytes)),
	})
}

func decodeDeclaration(k, b []byte) (*Index, error) {
	bad := func(what string) error {
		return errs.Newf(errs.CodeCorrupt, "index", "index declaration %x: %s", k, what)
	}
	kr := codec.NewKeyReader(k)
	if p, err := kr.ReadUint8(); err != nil || p != catalog.PrefixIndex {
		return nil, bad("key")
	}
	id, err := kr.ReadUint64()
	if err != nil || kr.Len() != 0 || id == 0 {
		return nil, bad("key")
	}
	rec, err := declaration.Decode(b)
	if err != nil {
		return nil, err
	}
	num := func(f string, lo, hi int64) (int64, error) {
		n, _ := rec[f].AsInt()
		if n < lo || n > hi {
			return 0, bad(f)
		}
		return n, nil
	}
	ix := &Index{ID: id}
	ix.Name, _ = rec["name"].AsString()
	ix.Unique, _ = rec["unique"].AsBool()
	var n int64
	if n, err = num("entity", 0, int64(Edges)); err != nil {
		return nil, err
	}
	ix.Entity = Entity(n)
	if n, err = num("scope", 1, 1<<32-1); err != nil {
		return nil, err
	}
	ix.Scope = uint32(n)
	if n, err = num("property", 1, 1<<32-1); err != nil {
		return nil, err
	}
	ix.Property = catalog.PropertyID(n)
	if _, err = num("kind", kindProperty, kindProperty); err != nil {
		return nil, err
	}
	if n, err = num("state", 0, int64(Failed)); err != nil {
		return nil, err
	}
	ix.State = State(n)
	if n, err = num("prefixBytes", MinPrefixBytes, MaxPrefixBytes); err != nil {
		return nil, err
	}
	ix.PrefixBytes = int(n)
	if catalog.ValidName("index", ix.Name) != nil {
		return nil, bad("name")
	}
	return ix, nil
}

// Set is the indexes declared in one state of the database.
type Set struct {
	all     []*Index // by name
	byScope map[scope][]*Index
}

type scope struct {
	entity Entity
	scope  uint32
}

// All returns the indexes, by name.
func (s *Set) All() []*Index { return s.all }

// Named returns the index called name.
func (s *Set) Named(name string) (*Index, bool) {
	i, ok := slices.BinarySearchFunc(s.all, name, func(ix *Index, n string) int { return strings.Compare(ix.Name, n) })
	if !ok {
		return nil, false
	}
	return s.all[i], true
}

// Scoped returns the indexes that writers maintain over entities of e in
// scope sc, whatever the property.
func (s *Set) Scoped(e Entity, sc uint32) []*Index { return s.byScope[scope{e, sc}] }

// Empty reports whether no index is declared.
func (s *Set) Empty() bool { return len(s.all) == 0 }

// load reads the declarations r sees.
func load(ctx context.Context, r engine.Reader) (*Set, error) {
	s := &Set{byScope: map[scope][]*Index{}}
	lo := codec.AppendKeyUint8(nil, catalog.PrefixIndex)
	err := r.Range(ctx, catalog.SlotCatalog, lo, codec.PrefixEnd(lo), func(k, v []byte) error {
		ix, err := decodeDeclaration(k, v)
		if err != nil {
			return err
		}
		s.all = append(s.all, ix)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(s.all, func(a, b *Index) int { return strings.Compare(a.Name, b.Name) })
	for i, ix := range s.all {
		if i > 0 && s.all[i-1].Name == ix.Name {
			return nil, errs.Newf(errs.CodeCorrupt, "index", "two indexes are called %q", ix.Name)
		}
		if ix.maintained() {
			k := scope{ix.Entity, ix.Scope}
			s.byScope[k] = append(s.byScope[k], ix)
		}
	}
	return s, nil
}

// Cache keeps the declarations of the last committed schema epoch a
// handle read. Every change of a declaration raises the epoch in its
// commit, so two committed states with the same epoch declare the same
// indexes. A write transaction that changed the epoch reads its own
// overlay each time: its epoch is not committed and a rollback to a
// savepoint may undo a declaration.
type Cache struct {
	cur atomic.Pointer[cached]
}

type cached struct {
	epoch uint64
	set   *Set
}

// Get returns the indexes r sees.
func (c *Cache) Get(ctx context.Context, r engine.Reader) (*Set, error) {
	st := r.State()
	// Only committed epochs are stored, and a writer that changed the
	// epoch is above every committed one, so a hit is always committed.
	if e := c.cur.Load(); e != nil && e.epoch == st.SchemaEpoch {
		return e.set, nil
	}
	s, err := load(ctx, r)
	if err != nil {
		return nil, err
	}
	if committedEpoch(r, st) {
		c.cur.Store(&cached{st.SchemaEpoch, s})
	}
	return s, nil
}

// committedEpoch reports whether the schema epoch r sees was committed: a
// read snapshot, or a write transaction that has not raised it.
func committedEpoch(r engine.Reader, st wal.State) bool {
	if w, ok := r.(interface{ Base() wal.State }); ok {
		return w.Base().SchemaEpoch == st.SchemaEpoch
	}
	_, ok := r.(*engine.ReadTx)
	return ok
}

// watermarkKey holds the largest index ID handed out (Catalog prefix 4).
var watermarkKey = codec.AppendKeyUint8(codec.AppendKeyUint8(nil, catalog.PrefixInternal), 1)

var idWatermark = catalog.Schema{Name: "index watermark", Version: 1, Fields: []catalog.Field{
	{Name: "hwm", Kind: value.KindInt, Required: true},
}}

// nextID hands out a new index ID, never reused, within s.
func nextID(ctx context.Context, s *engine.Stmt) (uint64, error) {
	b, ok, err := s.Get(ctx, catalog.SlotCatalog, watermarkKey)
	if err != nil {
		return 0, err
	}
	var hwm int64
	if ok {
		rec, err := idWatermark.Decode(b)
		if err != nil {
			return 0, err
		}
		if hwm, _ = rec["hwm"].AsInt(); hwm < 1 {
			return 0, errs.Newf(errs.CodeCorrupt, "index", "index watermark %d", hwm)
		}
	}
	rec, err := idWatermark.Encode(map[string]value.Value{"hwm": value.Int(hwm + 1)})
	if err != nil {
		return 0, err
	}
	return uint64(hwm + 1), s.Put(ctx, catalog.SlotCatalog, watermarkKey, rec)
}
