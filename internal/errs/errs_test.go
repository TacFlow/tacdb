package errs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
)

func TestAllCodesAreUniqueAndWellFormed(t *testing.T) {
	if got, want := len(AllCodes), 77; got != want {
		t.Fatalf("len(AllCodes) = %d, want %d (SPEC §6.1)", got, want)
	}
	shape := regexp.MustCompile(`^[A-Z][A-Z_]*[A-Z]$`)
	seen := make(map[Code]bool)
	for _, c := range AllCodes {
		if seen[c] {
			t.Errorf("duplicate code %s", c)
		}
		seen[c] = true
		if !shape.MatchString(string(c)) {
			t.Errorf("code %q is not UPPER_SNAKE_CASE", c)
		}
	}
}

func TestIsMatchesByCode(t *testing.T) {
	err := fmt.Errorf("outer: %w", New(CodeCorrupt, "btree.descend", "cycle"))
	if !errors.Is(err, Sentinel(CodeCorrupt)) {
		t.Fatal("errors.Is did not match a CORRUPT error against the CORRUPT sentinel")
	}
	if errors.Is(err, Sentinel(CodeIO)) {
		t.Fatal("errors.Is matched a CORRUPT error against the IO sentinel")
	}
	if errors.Is(err, (*Error)(nil)) {
		t.Fatal("errors.Is matched a nil *Error")
	}
}

func TestUnwrapKeepsCause(t *testing.T) {
	err := Wrap(CodeCanceled, "governor.reserve", context.Canceled, "wait canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cause is not reachable through errors.Is")
	}
	if CodeOf(fmt.Errorf("x: %w", err)) != CodeCanceled {
		t.Fatal("CodeOf did not find the wrapped code")
	}
	if CodeOf(errors.New("plain")) != "" {
		t.Fatal(`CodeOf of a plain error must be ""`)
	}
}

func TestErrorStringIncludesSortedDetails(t *testing.T) {
	err := New(CodeResourceLimit, "governor.reserve", "budget exhausted").
		With("requested", 4096).With("available", 0).With("limit", 1024)
	want := "tacdb: RESOURCE_LIMIT: governor.reserve: budget exhausted " +
		"(available=0, limit=1024, requested=4096)"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestWithNeverModifiesASentinel(t *testing.T) {
	s := Sentinel(CodeLimitExceeded)
	e := s.With("limit", 1)
	if s.Details != nil {
		t.Fatal("With modified the sentinel")
	}
	if e == s || e.Details["limit"] != 1 || !errors.Is(e, s) {
		t.Fatalf("With on a sentinel = %#v", e)
	}
}

func TestErrorStringMinimal(t *testing.T) {
	if got, want := Sentinel(CodeBusy).Error(), "tacdb: BUSY"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	err := Wrap(CodeIO, "vfs.write", errors.New("disk on fire"), "")
	if got, want := err.Error(), "tacdb: IO: vfs.write: disk on fire"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// A query error names its stage and where in the source it is.
func TestErrorStringIncludesLocation(t *testing.T) {
	e := New(CodeQueryParse, "query.Parse", "expected ')'")
	e.Stage = StageParse
	e.Location = &Location{Start: 12, End: 13, Line: 2, Column: 3}
	if got, want := e.Error(), "tacdb: QUERY_PARSE: query.Parse: expected ')' at line 2, column 3"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}
