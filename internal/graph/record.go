// Package graph stores nodes, edges, labels, adjacency and degrees in the
// logical trees of the G2 format (SPEC §12.1), over the transactions of
// the engine. Every change runs inside an engine statement, so a failed
// operation leaves no partial write in any tree.
package graph

import (
	"encoding/binary"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/errs"
)

// recordVersion is the version of node and edge records (SPEC §12.1).
const recordVersion = 2

// Key tags after the entity ID in the Nodes and Edges trees.
const (
	tagRecord   = 0
	tagProperty = 1
)

// Directions of adjacency entries and degree counters. Degrees also count
// self-loops apart, so that an undirected degree counts them once.
const (
	dirOut  = 0
	dirIn   = 1
	dirSelf = 2 // degree counters only
)

// prefixDegree is the prefix of degree counters in the Stats tree.
const prefixDegree = 2

func entityKey(id uint64) []byte { return codec.AppendKeyUint64(nil, id) }

func recordKey(id uint64) []byte {
	return codec.AppendKeyUint8(entityKey(id), tagRecord)
}

func propertyKey(id uint64, prop catalog.PropertyID) []byte { return catalog.PropertyKey(id, prop) }

// propertyRange bounds the properties of one entity.
func propertyRange(id uint64) (lo, hi []byte) {
	lo = codec.AppendKeyUint8(entityKey(id), tagProperty)
	return lo, codec.PrefixEnd(lo)
}

func adjacencyPrefix(node uint64, dir uint8) []byte {
	return codec.AppendKeyUint8(codec.AppendKeyUint64(nil, node), dir)
}

func adjacencyTypePrefix(node uint64, dir uint8, typ catalog.EdgeTypeID) []byte {
	return codec.AppendKeyUint32(adjacencyPrefix(node, dir), uint32(typ))
}

func adjacencyKey(node uint64, dir uint8, typ catalog.EdgeTypeID, peer, edge uint64) []byte {
	return codec.AppendKeyUint64(codec.AppendKeyUint64(adjacencyTypePrefix(node, dir, typ), peer), edge)
}

// adjacency is a decoded adjacency entry.
type adjacency struct {
	node uint64
	dir  uint8
	typ  catalog.EdgeTypeID
	peer uint64
	edge uint64
}

func decodeAdjacency(k []byte) (adjacency, error) {
	var a adjacency
	r := codec.NewKeyReader(k)
	var err error
	var typ uint32
	if a.node, err = r.ReadUint64(); err == nil {
		if a.dir, err = r.ReadUint8(); err == nil {
			if typ, err = r.ReadUint32(); err == nil {
				if a.peer, err = r.ReadUint64(); err == nil {
					a.edge, err = r.ReadUint64()
				}
			}
		}
	}
	a.typ = catalog.EdgeTypeID(typ)
	if err != nil || r.Len() != 0 || a.dir > dirIn || a.typ == 0 || a.peer == 0 || a.edge == 0 {
		return adjacency{}, errs.Newf(errs.CodeCorrupt, "graph", "adjacency entry %x", k)
	}
	return a, nil
}

func labelKey(label catalog.LabelID, node uint64) []byte {
	return codec.AppendKeyUint64(codec.AppendKeyUint32(nil, uint32(label)), node)
}

// degreeKey is the counter of node's entries in dir with type typ; type 0
// counts every type.
func degreeKey(node uint64, dir uint8, typ catalog.EdgeTypeID) []byte {
	k := codec.AppendKeyUint64(codec.AppendKeyUint8(nil, prefixDegree), node)
	return codec.AppendKeyUint32(codec.AppendKeyUint8(k, dir), uint32(typ))
}

func degreeNodePrefix(node uint64) []byte {
	return codec.AppendKeyUint64(codec.AppendKeyUint8(nil, prefixDegree), node)
}

// nodeRecord is the record of a node: its labels in insertion order.
type nodeRecord struct {
	labels []catalog.LabelID
}

func (n nodeRecord) encode() []byte {
	b := make([]byte, 8, 8+4*len(n.labels))
	binary.LittleEndian.PutUint16(b, recordVersion)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(n.labels)))
	for _, l := range n.labels {
		b = binary.LittleEndian.AppendUint32(b, uint32(l))
	}
	return b
}

func checkVersion(what string, b []byte) error {
	if len(b) < 4 {
		return errs.Newf(errs.CodeCorrupt, "graph", "%s record of %d bytes", what, len(b))
	}
	switch v := binary.LittleEndian.Uint16(b); {
	case v > recordVersion:
		return errs.Newf(errs.CodeUnsupportedFormat, "graph", "%s record version %d", what, v).
			With("version", int64(v)).With("supported", recordVersion)
	case v < recordVersion:
		return errs.Newf(errs.CodeCorrupt, "graph", "%s record version %d", what, v)
	}
	if binary.LittleEndian.Uint16(b[2:]) != 0 {
		return errs.Newf(errs.CodeCorrupt, "graph", "%s record with reserved bits", what)
	}
	return nil
}

func decodeNode(b []byte) (nodeRecord, error) {
	if err := checkVersion("node", b); err != nil {
		return nodeRecord{}, err
	}
	if len(b) < 8 || uint64(len(b)-8) != 4*uint64(binary.LittleEndian.Uint32(b[4:])) {
		return nodeRecord{}, errs.Newf(errs.CodeCorrupt, "graph", "node record of %d bytes", len(b))
	}
	n := nodeRecord{labels: make([]catalog.LabelID, 0, (len(b)-8)/4)}
	for off := 8; off < len(b); off += 4 {
		l := catalog.LabelID(binary.LittleEndian.Uint32(b[off:]))
		if l == 0 || n.has(l) {
			return nodeRecord{}, errs.Newf(errs.CodeCorrupt, "graph", "node record with label %d", l)
		}
		n.labels = append(n.labels, l)
	}
	return n, nil
}

func (n nodeRecord) has(l catalog.LabelID) bool {
	for _, x := range n.labels {
		if x == l {
			return true
		}
	}
	return false
}

// edgeRecord is the record of an edge.
type edgeRecord struct {
	from, to uint64
	typ      catalog.EdgeTypeID
}

const edgeRecordBytes = 28

func (e edgeRecord) encode() []byte {
	b := make([]byte, edgeRecordBytes)
	binary.LittleEndian.PutUint16(b, recordVersion)
	binary.LittleEndian.PutUint64(b[4:], e.from)
	binary.LittleEndian.PutUint64(b[12:], e.to)
	binary.LittleEndian.PutUint32(b[20:], uint32(e.typ))
	return b
}

func decodeEdge(b []byte) (edgeRecord, error) {
	if err := checkVersion("edge", b); err != nil {
		return edgeRecord{}, err
	}
	if len(b) != edgeRecordBytes || binary.LittleEndian.Uint32(b[24:]) != 0 {
		return edgeRecord{}, errs.Newf(errs.CodeCorrupt, "graph", "edge record of %d bytes", len(b))
	}
	e := edgeRecord{
		from: binary.LittleEndian.Uint64(b[4:]),
		to:   binary.LittleEndian.Uint64(b[12:]),
		typ:  catalog.EdgeTypeID(binary.LittleEndian.Uint32(b[20:])),
	}
	if e.from == 0 || e.to == 0 || e.typ == 0 {
		return edgeRecord{}, errs.New(errs.CodeCorrupt, "graph", "edge record with a zero endpoint or type")
	}
	return e, nil
}
