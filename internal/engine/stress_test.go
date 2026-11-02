package engine

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// Admission, expiry racing Commit, cancellations, identifiers and readers
// at once, in each mode. Identifiers are never handed out twice, and at the
// end nothing is counted, queued or reserved, every token is durable and
// the reopened database passes the structural check.
func TestStress(t *testing.T) {
	for _, mode := range []Durability{GroupSync, Sync, Async} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			o := testOptions()
			o.Durability = mode
			o.MaxWriteTxDuration = 3 * time.Millisecond
			o.BusyTimeout = 20 * time.Millisecond
			o.MaxPendingCommits = 4
			o.AsyncSyncInterval = 2 * time.Millisecond
			o.IDReservationInitial = 4
			o.IDReservationMax = 64
			gov := o.Governor
			db := open(t, vfs.NewMemFS(), o)
			var wg sync.WaitGroup
			seen := sync.Map{}
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					rng := rand.New(rand.NewSource(int64(w)))
					for i := 0; i < 150; i++ {
						c, cancel := context.WithTimeout(ctx, time.Duration(rng.Intn(5000))*time.Microsecond)
						tx, err := db.BeginWrite(c)
						if err != nil {
							cancel()
							continue
						}
						for j := 0; j < rng.Intn(4); j++ {
							if id, err := tx.NextID(c, KindNode); err == nil {
								if _, dup := seen.LoadOrStore(id, true); dup {
									t.Errorf("identifier %d handed out twice", id)
								}
							}
							tx.Put(c, 0, []byte(fmt.Sprintf("k%d", rng.Intn(50))), []byte(fmt.Sprint(i)))
						}
						if rng.Intn(5) == 0 {
							time.Sleep(time.Duration(rng.Intn(4000)) * time.Microsecond)
						}
						switch rng.Intn(3) {
						case 0:
							tx.Rollback()
						case 1:
							tx.TryCommit(c)
							tx.Rollback()
						default:
							tx.Commit(c)
							tx.Rollback()
						}
						cancel()
					}
				}(w)
			}
			for r := 0; r < 4; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 300; i++ {
						rt, err := db.BeginRead(ctx)
						if err != nil {
							continue
						}
						rt.Get(ctx, 0, []byte("k1"))
						rt.Close()
					}
				}()
			}
			wg.Wait()
			if err := db.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			db.mu.Lock()
			p, pb, ua, q, wq := db.pending, db.pendBytes, db.unappended, len(db.queue), db.waiters.len()
			db.mu.Unlock()
			if n := db.active.Load(); n != 0 || p != 0 || pb != 0 || ua != 0 || q != 0 || wq != 0 {
				t.Fatalf("leftovers: active %d pending %d bytes %d unappended %d queue %d waiters %d", n, p, pb, ua, q, wq)
			}
			st := gov.Stats()
			if st.Used[govern.Overlay] != 0 || st.Used[govern.Sealed] != 0 {
				t.Fatalf("governor leftovers: overlay %d sealed %d", st.Used[govern.Overlay], st.Used[govern.Sealed])
			}
			db.mu.Lock() // a checkpoint of the maintainer forgets tokens
			for s, v := range db.tokens {
				if v.status != Durable {
					db.mu.Unlock()
					t.Fatalf("token %x status %v after Flush", s, v)
				}
			}
			db.mu.Unlock()
			db.Close()
			db = open(t, db.fs, o)
			structural(t, db)
			db.Close()
		})
	}
}
