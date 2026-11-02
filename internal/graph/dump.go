package graph

import (
	"bufio"
	"context"
	"encoding/hex"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/engine"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Dump writes the canonical dump of the graph that r reads: one line per
// node in ID order, with its labels in insertion order and its properties
// by name, then one line per edge in ID order. Values keep their type and
// bits: Int 1 and Float 1 differ, and so do -0 and the payloads of NaN.
// Equal graphs give equal bytes, whatever the history that built them.
func (g *Graph) Dump(ctx context.Context, r engine.Reader, w io.Writer) error {
	bw := bufio.NewWriter(w)
	for _, e := range []Entity{NodeEntity, EdgeEntity} {
		var line strings.Builder
		var props map[string]value.Value
		var cur uint64 // the entity of line; 0 before the first
		flush := func() {
			if cur != 0 {
				writeProps(&line, props)
				line.WriteByte('\n')
				bw.WriteString(line.String())
				line.Reset()
			}
		}
		err := r.Range(ctx, e.slot(), nil, nil, func(k, v []byte) error {
			kr := codec.NewKeyReader(k)
			id, err := kr.ReadUint64()
			if err != nil || id == 0 {
				return errs.Newf(errs.CodeCorrupt, "graph.Dump", "%v key %x", e, k)
			}
			tag, err := kr.ReadUint8()
			if err != nil {
				return errs.Newf(errs.CodeCorrupt, "graph.Dump", "%v key %x", e, k)
			}
			if tag == tagRecord && kr.Len() == 0 {
				flush()
				cur, props = id, map[string]value.Value{}
				return g.writeEntity(ctx, r, &line, e, id, v)
			}
			pid, err := kr.ReadUint32()
			if tag != tagProperty || err != nil || kr.Len() != 0 || pid == 0 || id != cur {
				return errs.Newf(errs.CodeCorrupt, "graph.Dump", "%v key %x without its record", e, k)
			}
			n, err := name(ctx, g, r, catalog.PropertyID(pid))
			if err != nil {
				return err
			}
			props[n], err = g.decode(v)
			return err
		})
		if err != nil {
			return err
		}
		flush()
	}
	return bw.Flush()
}

func (g *Graph) writeEntity(ctx context.Context, r engine.Reader, b *strings.Builder, e Entity, id uint64, v []byte) error {
	b.WriteString(e.String())
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(id, 10))
	b.WriteByte(' ')
	if e == EdgeEntity {
		rec, err := decodeEdge(v)
		if err != nil {
			return err
		}
		t, err := name(ctx, g, r, rec.typ)
		if err != nil {
			return err
		}
		b.WriteString(strconv.FormatUint(rec.from, 10) + " -> " + strconv.FormatUint(rec.to, 10) + " " + strconv.Quote(t))
		return nil
	}
	rec, err := decodeNode(v)
	if err != nil {
		return err
	}
	b.WriteByte('[')
	for i, l := range rec.labels {
		n, err := name(ctx, g, r, l)
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Quote(n))
	}
	b.WriteByte(']')
	return nil
}

func writeProps(b *strings.Builder, props map[string]value.Value) {
	b.WriteString(" {")
	for i, n := range slices.Sorted(maps.Keys(props)) {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(n))
		b.WriteString(": ")
		writeValue(b, props[n])
	}
	b.WriteByte('}')
}

// writeValue writes the exact, typed text of v. Floats are tagged and
// keep their bits: finite ones round-trip through 'g', NaN shows its bits.
func writeValue(b *strings.Builder, v value.Value) {
	switch v.Kind() {
	case value.KindNull:
		b.WriteString("null")
	case value.KindBool:
		x, _ := v.AsBool()
		b.WriteString(strconv.FormatBool(x))
	case value.KindInt:
		x, _ := v.AsInt()
		b.WriteString(strconv.FormatInt(x, 10))
	case value.KindFloat:
		x, _ := v.AsFloat()
		b.WriteString("float(" + formatFloat(x) + ")")
	case value.KindString:
		s, _ := v.AsString()
		b.WriteString(strconv.Quote(s))
	case value.KindBytes:
		x, _ := v.AsBytes()
		b.WriteString("0x" + hex.EncodeToString(x))
	case value.KindVector:
		b.WriteString("vector[")
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(formatFloat32(v.VectorAt(i)))
		}
		b.WriteByte(']')
	case value.KindList:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			writeValue(b, v.Index(i))
		}
		b.WriteByte(']')
	case value.KindMap:
		b.WriteByte('{')
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Quote(k) + ": ")
			writeValue(b, item)
		}
		b.WriteByte('}')
	}
}

func formatFloat(x float64) string {
	if math.IsNaN(x) {
		return "NaN:" + strconv.FormatUint(math.Float64bits(x), 16)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

func formatFloat32(x float32) string {
	if x != x {
		return "NaN:" + strconv.FormatUint(uint64(math.Float32bits(x)), 16)
	}
	return strconv.FormatFloat(float64(x), 'g', -1, 32)
}
