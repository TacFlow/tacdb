package budget

import (
	"context"
	"errors"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/value"
)

func details(t *testing.T, err error) map[string]int64 {
	t.Helper()
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeResourceLimit {
		t.Fatalf("want RESOURCE_LIMIT, got %v", err)
	}
	return e.Details
}

// Memory over QueryMemoryBytes is refused before it is allocated.
func TestMemoryLimit(t *testing.T) {
	b, err := New(context.Background(), nil, Limits{MemoryBytes: 1000, WorkUnits: 10, Values: value.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Grow(600); err != nil {
		t.Fatal(err)
	}
	d := details(t, b.Grow(401))
	if d["requested"] != 401 || d["available"] != 400 || d["limit"] != 1000 {
		t.Fatalf("details %v", d)
	}
	if err := b.Grow(400); err != nil {
		t.Fatal(err)
	}
	b.Shrink(500)
	if b.Used() != 500 || b.Peak() != 1000 {
		t.Fatalf("used %d peak %d", b.Used(), b.Peak())
	}
	b.Shrink(1 << 40)
	if b.Used() != 0 {
		t.Fatalf("used %d after shrinking everything", b.Used())
	}
}

// Memory comes from the execution category of the governor, by chunks,
// and goes back on Close.
func TestGovernor(t *testing.T) {
	cfg := govern.DefaultConfig()
	g, err := govern.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(context.Background(), g, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Grow(10); err != nil {
		t.Fatal(err)
	}
	used := g.Stats().Used[govern.Execution]
	if used < 10 || used > chunk {
		t.Fatalf("governor holds %d for 10 bytes", used)
	}
	for i := 0; i < 100; i++ { // small charges do not take the governor each time
		if err := b.Grow(10); err != nil {
			t.Fatal(err)
		}
	}
	if g.Stats().Used[govern.Execution] != used {
		t.Fatal("a small charge took more memory from the governor")
	}
	if err := b.Grow(4 * chunk); err != nil {
		t.Fatal(err)
	}
	b.Shrink(b.Used())
	if got := g.Stats().Used[govern.Execution]; got > 2*chunk {
		t.Fatalf("governor still holds %d after a shrink", got)
	}
	b.Close()
	b.Close()
	if got := g.Stats().Used[govern.Execution]; got != 0 {
		t.Fatalf("governor holds %d after Close", got)
	}
}

// The governor refuses beyond its limit even below QueryMemoryBytes.
func TestGovernorRefuses(t *testing.T) {
	cfg := govern.DefaultConfig()
	cfg.Limit = 1 << 20
	cfg.Floor = [govern.NumCategories]int64{}
	cfg.Wait = 0
	g, err := govern.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(context.Background(), g, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Grow(1 << 20); err != nil {
		t.Fatal(err)
	}
	details(t, b.Grow(1))
	if b.Used() != 1<<20 {
		t.Fatalf("a refused charge counted: %d", b.Used())
	}
}

func TestWork(t *testing.T) {
	b, _ := New(context.Background(), nil, Limits{MemoryBytes: 1, WorkUnits: 100, Values: value.DefaultLimits()})
	if err := b.Work(60); err != nil {
		t.Fatal(err)
	}
	if err := b.Work(40); err != nil {
		t.Fatal(err)
	}
	d := details(t, b.Work(1))
	if d["limit"] != 100 || d["requested"] != 1 || b.WorkDone() != 100 {
		t.Fatalf("details %v done %d", d, b.WorkDone())
	}
}

func TestShape(t *testing.T) {
	lim := value.DefaultLimits()
	lim.MaxValueElements, lim.MaxDepth = 10, 3
	b, _ := New(context.Background(), nil, Limits{MemoryBytes: 1, WorkUnits: 1, Values: lim})
	if err := b.CheckShape(10, 3); err != nil {
		t.Fatal(err)
	}
	if d := details(t, b.CheckShape(11, 1)); d["limit"] != 10 || d["requested"] != 11 {
		t.Fatalf("details %v", d)
	}
	if d := details(t, b.CheckShape(1, 4)); d["limit"] != 3 || d["requested"] != 4 {
		t.Fatalf("details %v", d)
	}
}
