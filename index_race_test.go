package tacdb

import (
	"fmt"
	"sync"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

// Readers list indexes and read while a writer creates and drops them and
// writes indexed values; run with -race.
func TestIndexesConcurrent(t *testing.T) {
	db, err := OpenMemory(options())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				db.View(bg, func(tx *Tx) error {
					defs, err := tx.Indexes()
					if err != nil {
						t.Error(err)
					}
					_ = defs
					return nil
				})
			}
		}()
	}
	for i := range 200 {
		err := db.Update(bg, func(tx *Tx) error {
			name := fmt.Sprintf("i%d", i%3)
			if i%2 == 0 {
				if err := tx.CreateIndex(IndexDefinition{Name: name, Scope: "A", Property: "p"}); err != nil && code(err) != CodeIndexExistsDifferent {
					return err
				}
			} else if err := tx.DropIndex(name); err != nil && code(err) != CodeIndexNotFound {
				return err
			}
			_, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"A"}, Properties: map[string]value.Value{"p": value.Int(int64(i))}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if err := db.View(bg, func(tx *Tx) error { return db.g.Verify(bg, tx.r) }); err != nil {
		t.Fatal(err)
	}
}
