package tacdb

import (
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/graph"
)

// EntityKind says whether an index covers nodes or edges.
type EntityKind uint8

// Entity kinds.
const (
	NodeEntity EntityKind = iota
	EdgeEntity
)

// IndexKind is the kind of an index. Full-text and vector indexes arrive
// with their milestones.
type IndexKind uint8

// Index kinds.
const (
	PropertyIndex IndexKind = iota
)

// IndexDefinition declares an index (SPEC §12.2): a property of the nodes
// of a label, or of the edges of a type. Unique applies to property
// indexes; Null and absent values never collide.
type IndexDefinition struct {
	Name     string
	Entity   EntityKind
	Scope    string // a label for nodes, an edge type for edges
	Property string
	Kind     IndexKind
	Unique   bool
}

// CreateIndex declares an index and fills it in this write transaction,
// which publishes both at its commit. The same declaration under the same
// name is a no-op; another one is INDEX_EXISTS_DIFFERENT. Equal values in
// a unique index are UNIQUE_VIOLATION; an index that does not fit
// MaxTxBytes is LIMIT_EXCEEDED.
func (tx *Tx) CreateIndex(def IndexDefinition) error {
	const op = "tacdb.CreateIndex"
	if def.Kind != PropertyIndex {
		return errs.Newf(errs.CodeInvalidArgument, op, "index kind %d", def.Kind)
	}
	if def.Entity > EdgeEntity {
		return errs.Newf(errs.CodeInvalidArgument, op, "entity kind %d", def.Entity)
	}
	d := graph.IndexDef{Name: def.Name, Entity: graph.Entity(def.Entity), Scope: def.Scope, Property: def.Property, Unique: def.Unique}
	return tx.write(op, func(s *engine.Stmt) error { return tx.db.g.CreateIndex(ctx, s, d) })
}

// DropIndex removes an index and its entries; a missing one is
// INDEX_NOT_FOUND.
func (tx *Tx) DropIndex(name string) error {
	return tx.write("tacdb.DropIndex", func(s *engine.Stmt) error { return tx.db.g.DropIndex(ctx, s, name) })
}

// Indexes returns the indexes tx sees, by name.
func (tx *Tx) Indexes() ([]IndexDefinition, error) {
	var defs []graph.IndexDef
	err := tx.read("tacdb.Indexes", func(r engine.Reader) error {
		var err error
		defs, err = tx.db.g.ListIndexes(ctx, r)
		return err
	})
	out := make([]IndexDefinition, len(defs))
	for i, d := range defs {
		out[i] = IndexDefinition{Name: d.Name, Entity: EntityKind(d.Entity), Scope: d.Scope, Property: d.Property, Unique: d.Unique}
	}
	return out, err
}
