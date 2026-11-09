package ast

import "github.com/TacFlow/tacdb/internal/errs"

// Locate converts a span of src into the location of an error: byte
// offsets plus line and column, both 1-based; the column counts code
// points.
func Locate(src string, span Span) *errs.Location {
	start := min(span.Start, len(src))
	line, col := 1, 1
	for _, r := range src[:start] {
		if r == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return &errs.Location{Start: span.Start, End: span.End, Line: line, Column: col}
}
