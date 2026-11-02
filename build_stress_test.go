package tacdb

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/value"
)

var errReviewAbort = errors.New("abort")

// node and edge builds at once, with writers that create and
// delete nodes and edges, relabel, set properties twice, and abort
// multi-operation transactions; every build must end ONLINE and equal to
// a rebuild.
func TestBuildsWithEdgeWriters(t *testing.T) {
	for seedN := range 3 {
		t.Run(fmt.Sprint(seedN), func(t *testing.T) {
			db := openFS(t, vfs.NewMemFS(), buildOptions())
			defer db.Close()
			var nodes []NodeID
			var edges []EdgeID
			err := db.Update(bg, func(tx *Tx) error {
				for i := range 200 {
					nd, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"P"}, Properties: map[string]value.Value{"p": value.Int(int64(i % 37))}})
					if err != nil {
						return err
					}
					nodes = append(nodes, nd.ID)
				}
				for i := range 300 {
					e, err := tx.CreateEdge(nodes[i%200], nodes[(i*7)%200], []string{"R", "S"}[i%2], map[string]value.Value{"w": value.Int(int64(i % 11))})
					if err != nil {
						return err
					}
					edges = append(edges, e.ID)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for w := range 3 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewPCG(uint64(seedN), uint64(w)))
					for {
						select {
						case <-stop:
							return
						default:
						}
						db.Update(bg, func(tx *Tx) error {
							for range 1 + r.IntN(4) {
								n := nodes[r.IntN(len(nodes))]
								e := edges[r.IntN(len(edges))]
								switch r.IntN(10) {
								case 0:
									tx.SetProperty(n, "p", value.Int(r.Int64N(50)))
									tx.SetProperty(n, "p", value.Int(r.Int64N(50)))
								case 1:
									tx.RemoveLabel(n, "P")
								case 2:
									tx.AddLabel(n, "P")
								case 3:
									tx.DeleteNode(n, true)
								case 4:
									nd, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"P"}, Properties: map[string]value.Value{"p": value.Int(r.Int64N(50))}})
									if err == nil {
										tx.CreateEdge(nd.ID, n, "R", map[string]value.Value{"w": value.Int(r.Int64N(20))})
									}
								case 5:
									tx.SetEdgeProperty(e, "w", value.Int(r.Int64N(20)))
									tx.SetEdgeProperty(e, "w", value.Float(float64(r.Int64N(20))))
								case 6:
									tx.RemoveEdgeProperty(e, "w")
								case 7:
									tx.DeleteEdge(e)
								case 8:
									tx.RemoveProperty(n, "p")
								case 9:
									tx.CreateEdge(n, nodes[r.IntN(len(nodes))], "R", map[string]value.Value{"w": value.Null()})
								}
							}
							if r.IntN(4) == 0 {
								return errReviewAbort
							}
							return nil
						})
					}
				}()
			}
			var bw sync.WaitGroup
			results := make([]error, 3)
			states := make([]IndexBuildStatus, 3)
			defs := []IndexDefinition{
				{Name: "np", Scope: "P", Property: "p"},
				{Name: "ew", Entity: EdgeEntity, Scope: "R", Property: "w"},
				{Name: "ews", Entity: EdgeEntity, Scope: "S", Property: "w"},
			}
			for i, d := range defs {
				bw.Add(1)
				go func() {
					defer bw.Done()
					states[i], results[i] = db.BuildIndex(bg, d)
				}()
			}
			bw.Wait()
			close(stop)
			wg.Wait()
			for i := range defs {
				if results[i] != nil || states[i].State != IndexOnline {
					t.Fatalf("%s: %+v (failure %v), %v", defs[i].Name, states[i], states[i].Failure, results[i])
				}
			}
			verifyDB(t, db)
		})
	}
}
