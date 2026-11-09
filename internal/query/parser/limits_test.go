package parser

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

// A chain of unary plus counts toward MaxDepth, as one of '-' does.
func TestUnaryPlusEscapesMaxDepth(t *testing.T) {
	lim := Limits{MaxQueryBytes: 1 << 20, MaxDepth: 10}
	for _, src := range []string{
		"RETURN " + strings.Repeat("- ", 11) + "a",
		"RETURN " + strings.Repeat("+ ", 11) + "a",
		"RETURN " + strings.Repeat("+", 200) + "a",
	} {
		_, err := Parse(src, lim)
		var e *errs.Error
		if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded {
			t.Errorf("%.40s...: %v, want LIMIT_EXCEEDED", src, err)
		}
	}
}

// With the default limits, a 1 MiB query of '+' signs is refused by the
// depth limit without growing the stack.
func TestUnaryPlusStack(t *testing.T) {
	src := "RETURN " + strings.Repeat("+", 1<<20-8) + "a"
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Parse(src, DefaultLimits())
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Errorf("a 1 MiB chain of '+' parsed (depth limit %d); stack in use %d MiB",
			DefaultLimits().MaxDepth, after.StackSys>>20)
	}
}

// With a larger source limit, the same input is still refused instead of
// overflowing the goroutine stack, a fatal error that recover cannot
// catch. It runs in a subprocess.
func TestUnaryPlusStackOverflow(t *testing.T) {
	if os.Getenv("TACDB_PLUS_CHAIN_CHILD") == "1" {
		src := "RETURN " + strings.Repeat("+", 6<<20) + "a"
		_, _ = Parse(src, Limits{MaxQueryBytes: 8 << 20, MaxDepth: 128})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnaryPlusStackOverflow$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TACDB_PLUS_CHAIN_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.SplitN(string(out), "\n", 3)
		t.Errorf("a 6 MiB chain of '+' with MaxDepth 128 crashed the process: %v: %s", err, strings.Join(lines[:min(2, len(lines))], " | "))
	}
}

// The lexer runs as the parser needs tokens: a 1 MiB query that fails at
// its first tokens allocates little.
func TestTokenAllocation(t *testing.T) {
	src := "RETURN " + strings.Repeat(",", 1<<20-7)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := Parse(src, DefaultLimits())
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("parsed")
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 32<<20 {
		t.Errorf("a 1 MiB query rejected at byte 8 allocated %d MiB (%v)", got>>20, err)
	}
}
