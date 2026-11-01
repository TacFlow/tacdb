package engine

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// gate skips a gate that TACDB_GATES does not list: gates run as their own
// CI job, outside the short test run.
func gate(t *testing.T, name string) {
	t.Helper()
	if !slices.Contains(strings.Split(os.Getenv("TACDB_GATES"), ","), name) {
		t.Skipf("gate %s: set TACDB_GATES=%s to run it", name, name)
	}
}

func envInt(name string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil {
		return v
	}
	return def
}

// gateOptions open a database on disk with the checkpoint suspended.
func gateOptions() Options {
	o := DefaultOptions()
	o.MaintenanceInterval = time.Hour
	o.CheckpointBytes = 1 << 50
	o.CheckpointSegments = 1 << 30
	o.MaxWALBytes = 1 << 50
	return o
}

// recoveryDB builds, in a new directory, a database of pages pages whose
// log holds walBytes of groups that change pages scattered over the whole
// file: the main file is sparse, with a bitmap in every group, and the
// log was never checkpointed.
func recoveryDB(t *testing.T, pages uint64, walBytes int64) vfs.FS {
	t.Helper()
	fs, err := vfs.OpenDir(t.TempDir(), vfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	o := gateOptions()
	o.Create = true
	db, err := Open(ctx, fs, name, o)
	if err != nil {
		t.Fatal(err)
	}
	uuid := db.uuid
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := fs.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	var b page.Buf
	for g := uint64(0); uint64(page.BitmapPage(g)) < pages; g++ {
		id := page.BitmapPage(g)
		page.EncodeBitmap(&b, id)
		for i := 0; i < page.GroupPages && uint64(id)+1+uint64(i) < pages; i++ {
			page.SetBitmapBit(&b, i, true)
		}
		page.Seal(&b)
		if err := f.WriteAt(b[:], int64(id)*page.Size); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Truncate(int64(pages) * page.Size); err != nil {
		t.Fatal(err)
	}
	sb := page.Superblock{UUID: uuid, Generation: 2, PageCount: pages, WALFloorSegment: 1}
	sb.Encode(&b)
	if err := f.WriteAt(b[:], page.Size); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	w, err := wal.Open(fs, name+"-wal", uuid, wal.Resume{End: wal.Position{Seq: 1, Offset: wal.SegmentHeaderSize, NextLSN: 1}}, wal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The groups write pages of a pool of the same size over any database,
	// spread over all of it: the log replays as many distinct pages over
	// 1 GiB as over 100 GiB, so only the size of the database differs.
	rng := rand.New(rand.NewPCG(1, 1))
	const perGroup, pool = 64, 1 << 16
	stride := (pages - 4) / pool
	for c, written := uint64(1), int64(0); written < walBytes; c++ {
		ids := map[page.ID]bool{}
		for len(ids) < perGroup {
			id := page.ID(3 + rng.Uint64N(pool)*stride)
			if page.IsBitmapPage(id) {
				id++
			}
			ids[id] = true
		}
		sorted := slices.Sorted(func(yield func(page.ID) bool) {
			for id := range ids {
				if !yield(id) {
					return
				}
			}
		})
		g := &wal.Group{ID: c, Begins: []wal.Begin{{Parent: c - 1, CommitID: c}}}
		crand.Read(g.Begins[0].Token[:])
		var m wal.Manifest
		m.AddBegin(&g.Begins[0])
		for _, id := range sorted {
			img := new(page.Buf)
			page.EncodeOverflow(img, id, []byte(fmt.Sprintf("commit %d page %d", c, id)))
			page.HeaderOf(img).SetCommitID(c)
			page.Seal(img)
			g.Pages = append(g.Pages, img)
			m.AddPage(id, img)
		}
		g.Commit = wal.Commit{IntentCount: 1, PageCount: perGroup, Digest: m.Digest(),
			State: wal.State{CommitID: c, Parent: c - 1, PageCount: pages}}
		if _, err := w.AppendGroup(g); err != nil {
			t.Fatal(err)
		}
		written += perGroup * (page.Size + wal.RecordHeaderSize)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return fs
}

// readsFS counts the bytes read from the main file.
type readsFS struct {
	vfs.FS
	main atomic.Int64
}

type readsFile struct {
	vfs.File
	n *atomic.Int64
}

func (f readsFile) ReadAt(p []byte, off int64) error {
	f.n.Add(int64(len(p)))
	return f.File.ReadAt(p, off)
}

func (r *readsFS) wrap(n string, f vfs.File, err error) (vfs.File, error) {
	if err != nil || n != name {
		return f, err
	}
	return readsFile{f, &r.main}, nil
}

func (r *readsFS) Open(n string) (vfs.File, error) {
	f, err := r.FS.Open(n)
	return r.wrap(n, f, err)
}

func (r *readsFS) OpenReadOnly(n string) (vfs.File, error) {
	f, err := r.FS.OpenReadOnly(n)
	return r.wrap(n, f, err)
}

// openTime opens a database built by recoveryDB and closes it without a
// checkpoint, and returns the time of the open and the bytes it read from
// the main file.
func openTime(t *testing.T, fs vfs.FS) (time.Duration, int64) {
	t.Helper()
	r := &readsFS{FS: fs}
	start := time.Now()
	db, err := Open(ctx, r, name, gateOptions())
	d := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	read := r.main.Load()
	db.mt.suspended.Store(true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return d, read
}

// REC-11 (gate 13): the open after a crash costs in proportion to the log,
// not to the database. Over databases of 1 GiB and of 100 GiB, a log of
// 1 GiB opens reading the same bytes of the main file (its superblocks),
// and in the same time within 10%, the best of five after a warm-up. The
// bytes are the exact proof; the time is the gate of the specification.
func TestREC11(t *testing.T) {
	gate(t, "rec11")
	if runtime.GOOS == "windows" {
		t.Skip("REC-11 runs on Linux and macOS, where sparse files need no setup (COMPATIBILITY.md)")
	}
	walBytes := envInt("TACDB_REC11_WAL_BYTES", 1<<30)
	small := recoveryDB(t, (1<<30)/page.Size, walBytes)
	large := recoveryDB(t, (100<<30)/page.Size, walBytes)
	openTime(t, small) // warm the cache of the operating system for both
	openTime(t, large)
	var ts, tl []time.Duration
	for range 5 { // interleaved, the best of five of each
		d, rs := openTime(t, small)
		ts = append(ts, d)
		d, rl := openTime(t, large)
		tl = append(tl, d)
		if rs != rl || rs > 2*page.Size {
			t.Fatalf("the open read %d bytes of the main file over 1 GiB and %d over 100 GiB", rs, rl)
		}
	}
	s, l := slices.Min(ts), slices.Min(tl)
	ratio := float64(l) / float64(s)
	t.Logf("log of %d MiB: open in %v over 1 GiB, %v over 100 GiB (ratio %.3f)", walBytes>>20, s, l, ratio)
	if ratio > 1.10 {
		t.Fatalf("the open depends on the size of the database: ratio %.3f", ratio)
	}
}

func readMetric(name string) uint64 {
	s := []metrics.Sample{{Name: name}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// GOV-01, the part M2 can measure (the queries, which reserve execution
// memory and can be refused, arrive in M4): 1024 read transactions open
// at once, spread over four handles that share one governor, scan trees
// two and a half times its limit. The caches fill the governor, every
// transaction ends without waiting past its deadline, any error is
// RESOURCE_LIMIT with its details, and the live heap after a GC, with the
// transactions still open, stays within the limit plus 10%. The resident
// set is reported, not gated: without GOMEMLIMIT it follows GOGC
// (OPERATIONS.md §1.2).
func TestGOV01(t *testing.T) {
	gate(t, "gov01")
	const limit = 64 << 20 // well below the 160 MB the transactions scan
	gov, err := govern.New(govern.Config{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	const handles, keys, valueBytes = 4, 10000, 4000 // 40 MB per handle
	dbs := make([]*DB, handles)
	for h := range dbs {
		fs, err := vfs.OpenDir(t.TempDir(), vfs.Options{})
		if err != nil {
			t.Fatal(err)
		}
		o := gateOptions()
		o.Create, o.Governor = true, gov
		db, err := Open(ctx, fs, name, o)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for lo := 0; lo < keys; lo += 1000 {
			tx, err := db.BeginWrite(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for k := lo; k < lo+1000; k++ {
				v := strings.Repeat(string(rune('a'+k%26)), valueBytes)
				if err := tx.Put(ctx, 0, []byte(fmt.Sprintf("k%06d", k)), []byte(v)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		dbs[h] = db
	}
	runtime.GC()
	live0 := readMetric("/gc/heap/live:bytes")
	total0 := readMetric("/memory/classes/total:bytes") - readMetric("/memory/classes/heap/released:bytes")

	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // nothing waits without a deadline
	defer cancel()
	const txs = 1024
	var open, scanned, done sync.WaitGroup
	open.Add(txs)
	scanned.Add(txs)
	done.Add(txs)
	release, finish := make(chan struct{}), make(chan struct{})
	errc := make(chan error, txs)
	var peak int64
	var mu sync.Mutex
	used := func() int64 {
		var n int64
		for _, u := range gov.Stats().Used {
			n += u
		}
		return n
	}
	for i := range txs {
		go func() {
			defer done.Done()
			db := dbs[i%handles]
			rt, err := db.BeginRead(cctx)
			open.Done()
			if err != nil {
				scanned.Done()
				errc <- err
				return
			}
			defer rt.Close()
			defer func() { scanned.Done(); <-finish }() // still open when measured
			<-release                                   // all 1024 are open at once
			lo := i / handles % 32 * (keys / 32)        // every slice of every handle
			err = rt.Range(cctx, 0, []byte(fmt.Sprintf("k%06d", lo)), []byte(fmt.Sprintf("k%06d", lo+keys/32)),
				func(k, v []byte) error {
					mu.Lock()
					peak = max(peak, used())
					mu.Unlock()
					return nil
				})
			if err != nil {
				errc <- err
			}
		}()
	}
	open.Wait()
	close(release)
	scanned.Wait()
	runtime.GC()
	liveHeld := readMetric("/gc/heap/live:bytes")
	total := readMetric("/memory/classes/total:bytes") - readMetric("/memory/classes/heap/released:bytes")
	close(finish)
	done.Wait()
	close(errc)
	for err := range errc {
		var e *errs.Error
		if !isCode(err, errs.CodeResourceLimit) || !errors.As(err, &e) || e.Details["requested"] == 0 {
			t.Fatalf("a transaction failed with %v", err)
		}
	}
	t.Logf("governor peak %d MiB of %d; live heap %+d MiB after GC; mapped memory %+d MiB",
		peak>>20, limit>>20, (int64(liveHeld)-int64(live0))>>20, (int64(total)-int64(total0))>>20)
	if peak < limit*9/10 {
		t.Fatalf("the caches reached only %d of the governor's %d: the load is too light", peak, limit)
	}
	if int64(liveHeld)-int64(live0) > limit*11/10 {
		t.Fatalf("the live heap grew by %d bytes, more than the limit plus 10%%", int64(liveHeld)-int64(live0))
	}
}
