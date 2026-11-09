// Package query holds helpers for writing tacdb-cypher/1 text (SPEC
// §15.1, §20.7). Values always travel as parameters; names that come from
// data go through QuoteIdentifier.
package query

import (
	"strings"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
)

// IdentifierKind says what a quoted name stands for.
type IdentifierKind uint8

// Kinds of identifiers.
const (
	Label IdentifierKind = iota
	RelationshipType
	PropertyKey
	Variable
)

// MaxNameBytes bounds a label, relationship type or property key (SPEC
// §5.1).
const MaxNameBytes = 255

// QuoteIdentifier returns name between backticks, with backticks doubled,
// ready to be placed in query text as an identifier of kind. A name that
// is empty, not valid UTF-8, holds NUL or is longer than MaxNameBytes is
// refused, as the database would refuse it (SPEC §20.7).
func QuoteIdentifier(kind IdentifierKind, name string) (string, error) {
	const op = "query.QuoteIdentifier"
	switch {
	case kind > Variable:
		return "", errs.Newf(errs.CodeInvalidArgument, op, "identifier kind %d", kind)
	case name == "":
		return "", errs.New(errs.CodeInvalidArgument, op, "empty name")
	case !utf8.ValidString(name):
		return "", errs.New(errs.CodeInvalidArgument, op, "name is not valid UTF-8")
	case strings.IndexByte(name, 0) >= 0:
		return "", errs.New(errs.CodeInvalidArgument, op, "name holds a NUL byte")
	case len(name) > MaxNameBytes:
		return "", errs.Newf(errs.CodeLimitExceeded, op, "name of %d bytes", len(name)).
			With("bytes", int64(len(name))).With("limit", MaxNameBytes)
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`", nil
}
