// Package errs defines the Error type and the stable error codes shared by
// every tacdb package. The public package tacdb re-exports them.
package errs

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Location points into a query source (SPEC §6.1).
type Location struct {
	Start, End   int // byte offsets
	Line, Column int // 1-based; Column counts code points
}

// Stage is the stage of query processing an error comes from (SPEC §6.1).
type Stage string

// Stages of query processing.
const (
	StageLex      Stage = "lex"
	StageParse    Stage = "parse"
	StageSemantic Stage = "semantic"
	StagePlan     Stage = "plan"
	StageExecute  Stage = "execute"
)

// Error is the error type returned by tacdb.
//
// errors.Is matches two *Error values by Code, so a sentinel such as
// tacdb.ErrCorrupt matches every CORRUPT error. errors.As gives access to
// the details.
type Error struct {
	Code        Code
	Op          string
	Message     string
	Cause       error
	Retryable   bool
	CommitToken *[16]byte
	Stage       Stage // for query errors
	Location    *Location
	// Details carries named numeric facts whose keys are stable per code
	// (SPEC §6.1). Durations are in nanoseconds. Keys in use:
	//
	//	LIMIT_EXCEEDED  limit, plus one of depth, dimensions, bytes, elements
	//	RESOURCE_LIMIT  requested, available, limit
	//	NO_SPACE        capacity (in-memory file system only)
	Details map[string]int64

	sentinel bool
}

// New returns an error with the given code, operation and message.
func New(code Code, op, msg string) *Error {
	return &Error{Code: code, Op: op, Message: msg}
}

// Newf is New with a formatted message.
func Newf(code Code, op, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Message: fmt.Sprintf(format, args...)}
}

// Wrap returns an error with the given code that wraps cause.
func Wrap(code Code, op string, cause error, msg string) *Error {
	return &Error{Code: code, Op: op, Message: msg, Cause: cause}
}

// Sentinel returns a bare error carrying only a code, for use with
// errors.Is. With never modifies a sentinel.
func Sentinel(code Code) *Error {
	return &Error{Code: code, sentinel: true}
}

// With sets a detail and returns e, for chaining at construction time. On
// a sentinel it returns a modified copy instead, so that shared sentinels
// never change.
func (e *Error) With(key string, v int64) *Error {
	if e.sentinel {
		c := *e
		c.sentinel = false
		c.Details = nil
		e = &c
	}
	if e.Details == nil {
		e.Details = make(map[string]int64)
	}
	e.Details[key] = v
	return e
}

// Error formats the error as "tacdb: CODE: op: message (k=v, ...): cause".
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("tacdb: ")
	b.WriteString(string(e.Code))
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Location != nil {
		fmt.Fprintf(&b, " at line %d, column %d", e.Location.Line, e.Location.Column)
	}
	if len(e.Details) > 0 {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", k, e.Details[k])
		}
		b.WriteString(")")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t != nil && t.Code == e.Code
}

// CodeOf returns the code of the first *Error in err's chain, or "" if
// there is none.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
