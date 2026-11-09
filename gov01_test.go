package tacdb

import (
	"context"
	"errors"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/govern"
)

func readMetric(name string) uint64 {
	s := []metrics.Sample{{Name: name}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// GOV-01 with queries (SPEC §8.5, §15.4): 1024 read queries at once,
// spread over four handles that share one governor, each sorting every
// node of its handle, ask for far more execution memory than the
// governor has. Queries that do not get their memory in time fail with
// RESOURCE_LIMIT and its details; none waits past its deadline; and the
// live heap after a GC, with every query still open, stays within the
// limit of the governor plus 10%.
func TestGOV01Queries(t *testing.T) {
	if !slices.Contains(strings.Split(os.Getenv("TACDB_GATES"), ","), "gov01") {
		t.Skip("gate gov01: set TACDB_GATES=gov01 to run it")
	}
	const limit = 64 << 20
	gov, err := govern.New(govern.Config{Limit: limit, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const handles, nodes = 4, 3000
	dbs := make([]*DB, handles)
	for h := range dbs {
		o := options()
		o.governor = gov
		db, err := OpenMemory(o)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		setup(t, db, func(tx *Tx) {
			for i := range nodes {
				mustNode(t, tx, []string{"N"}, props("s", strings.Repeat(string(rune('a'+i%26)), 200)))
			}
		})
		dbs[h] = db
	}
	runtime.GC()
	live0 := readMetric("/gc/heap/live:bytes")

	cctx, cancel := context.WithTimeout(bg, 10*time.Minute) // nothing waits without a deadline
	defer cancel()
	const queries = 1024
	var started, done sync.WaitGroup
	started.Add(queries)
	done.Add(queries)
	finish := make(chan struct{})
	errc := make(chan error, queries)
	var mu sync.Mutex
	var peak int64
	ok := 0
	for i := range queries {
		go func() {
			defer done.Done()
			rows, err := dbs[i%handles].Query(cctx, "MATCH (n:N) RETURN n.s ORDER BY n.s DESC", nil)
			if err == nil && !rows.Next() {
				err = rows.Err()
			}
			mu.Lock()
			var used int64
			for _, u := range gov.Stats().Used {
				used += u
			}
			peak = max(peak, used)
			if err == nil {
				ok++
			}
			mu.Unlock()
			started.Done()
			if err != nil {
				errc <- err
				return
			}
			<-finish // still open when measured
			rows.Close()
		}()
	}
	started.Wait()
	runtime.GC()
	liveHeld := readMetric("/gc/heap/live:bytes")
	close(finish)
	done.Wait()
	close(errc)
	refused := 0
	for err := range errc {
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeResourceLimit || e.Details["requested"] == 0 || e.Details["limit"] == 0 {
			t.Fatalf("a query failed with %v", err)
		}
		refused++
	}
	grown := int64(liveHeld) - int64(live0)
	t.Logf("%d queries held their rows, %d refused; governor peak %d MiB of %d; live heap %+d MiB after GC",
		ok, refused, peak>>20, limit>>20, grown>>20)
	if refused == 0 || ok == 0 {
		t.Fatalf("%d held and %d refused: the load does not exercise the governor", ok, refused)
	}
	if grown > limit*11/10 {
		t.Fatalf("the live heap grew by %d bytes, more than the limit plus 10%%", grown)
	}
}
