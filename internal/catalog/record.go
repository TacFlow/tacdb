// Package catalog holds the logical trees of the G2 format (SPEC §7.4):
// their root slots, the versioned records of Catalog, Stats, Sessions and
// Maintenance, and the symbols of the three namespaces.
package catalog

import (
	"encoding/binary"
	"slices"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Root slots of the logical trees (SPEC §7.4).
const (
	SlotNodes          = 0
	SlotAdjacency      = 1
	SlotLabels         = 2
	SlotSymbolsForward = 3
	SlotSymbolsReverse = 4
	SlotEdges          = 9
	SlotCatalog        = 13
	SlotNodeProperty   = 14
	SlotEdgeProperty   = 15
	SlotStats          = 16
	SlotSessions       = 17
	SlotMaintenance    = 18
)

// Prefixes of the Catalog tree (SPEC §7.4). Only changes under 2, 3 and 6
// raise the schema epoch.
const (
	PrefixConfig   = 1
	PrefixIndex    = 2
	PrefixAnalyzer = 3
	PrefixInternal = 4 // durable operational state and internal ID watermarks
	PrefixSymbols  = 5 // symbol watermarks
	PrefixPolicy   = 6
)

// Field is a named field of a record.
type Field struct {
	Name     string
	Kind     value.Kind
	Required bool
	Since    uint16 // the first version with the field; 0 means 1
}

// Schema is the format of a kind of record: version u16, then a Value Map
// whose keys are the fields of the schema (SPEC §7.4). Field names are
// part of the format.
type Schema struct {
	Name    string
	Version uint16 // the version this reader writes and the newest it reads
	Fields  []Field
}

func (s *Schema) field(name string) (Field, bool) {
	i := slices.IndexFunc(s.Fields, func(f Field) bool { return f.Name == name })
	if i < 0 {
		return Field{}, false
	}
	return s.Fields[i], true
}

// Encode writes a record of the current version with fields, which must
// belong to the schema with their kinds, and hold every required one.
func (s *Schema) Encode(fields map[string]value.Value) ([]byte, error) {
	op := "catalog.Encode " + s.Name
	for name, v := range fields {
		f, ok := s.field(name)
		if !ok {
			return nil, errs.Newf(errs.CodeInvalidArgument, op, "no field %q", name)
		}
		if v.Kind() != f.Kind {
			return nil, errs.Newf(errs.CodeInvalidArgument, op, "field %q is %v, not %v", name, f.Kind, v.Kind())
		}
	}
	for _, f := range s.Fields {
		if _, ok := fields[f.Name]; f.Required && !ok {
			return nil, errs.Newf(errs.CodeInvalidArgument, op, "required field %q missing", f.Name)
		}
	}
	b := binary.LittleEndian.AppendUint16(nil, s.Version)
	return codec.AppendValue(b, value.MapOf(fields), value.DefaultLimits())
}

// Decode reads a record. A version newer than the schema is
// UNSUPPORTED_FORMAT: it needs a newer reader. An unknown field, a field
// of the wrong kind or a missing required one, in a version the schema
// knows, is CORRUPT.
func (s *Schema) Decode(b []byte) (map[string]value.Value, error) {
	op := "catalog.Decode " + s.Name
	if len(b) < 2 {
		return nil, errs.Newf(errs.CodeCorrupt, op, "record of %d bytes", len(b))
	}
	ver := binary.LittleEndian.Uint16(b)
	switch {
	case ver == 0:
		return nil, errs.New(errs.CodeCorrupt, op, "record version 0")
	case ver > s.Version:
		return nil, errs.Newf(errs.CodeUnsupportedFormat, op, "record version %d, this reader knows up to %d", ver, s.Version).
			With("version", int64(ver)).With("supported", int64(s.Version))
	}
	m, err := codec.DecodeValue(b[2:], codec.DecodeOptions{Limits: value.DefaultLimits()})
	if err != nil {
		return nil, errs.Wrap(errs.CodeCorrupt, op, err, "record payload")
	}
	if m.Kind() != value.KindMap {
		return nil, errs.Newf(errs.CodeCorrupt, op, "record payload is %v, not Map", m.Kind())
	}
	out := make(map[string]value.Value, m.Len())
	for i := range m.Len() {
		name, v := m.EntryAt(i)
		f, ok := s.field(name)
		if !ok || max(f.Since, 1) > ver {
			return nil, errs.Newf(errs.CodeCorrupt, op, "unknown field %q in version %d", name, ver)
		}
		if v.Kind() != f.Kind {
			return nil, errs.Newf(errs.CodeCorrupt, op, "field %q is %v, not %v", name, v.Kind(), f.Kind)
		}
		out[name] = v
	}
	for _, f := range s.Fields {
		if _, ok := out[f.Name]; f.Required && max(f.Since, 1) <= ver && !ok {
			return nil, errs.Newf(errs.CodeCorrupt, op, "required field %q missing in version %d", f.Name, ver)
		}
	}
	return out, nil
}

// PropertyKey is the key of property prop of entity id in the Nodes or
// Edges tree: (id, 1, prop) (FORMAT.md §13.3).
func PropertyKey(id uint64, prop PropertyID) []byte {
	return codec.AppendKeyUint32(codec.AppendKeyUint8(codec.AppendKeyUint64(nil, id), 1), uint32(prop))
}
