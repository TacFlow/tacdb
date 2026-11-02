package versions

import (
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

type val uint64

func (v val) Commit() uint64 { return uint64(v) }

// check compares a Tree with a reference map: lookups, order, size and
// summary.
func checkTree(t *testing.T, tr Tree[val], model map[uint64]val) {
	t.Helper()
	if tr.Len() != len(model) {
		t.Fatalf("Len %d, model %d", tr.Len(), len(model))
	}
	var keys []uint64
	var maxC uint64
	tr.All(func(k uint64, v val) bool {
		if model[k] != v {
			t.Fatalf("key %d holds %d, model %d", k, v, model[k])
		}
		keys = append(keys, k)
		return true
	})
	for _, v := range model {
		maxC = max(maxC, uint64(v))
	}
	if !slices.IsSorted(keys) || len(keys) != len(model) || tr.MaxCommit() != maxC {
		t.Fatalf("%d keys sorted %v, max %d want %d", len(keys), slices.IsSorted(keys), tr.MaxCommit(), maxC)
	}
	for k, v := range model {
		if got, ok := tr.Get(k); !ok || got != v {
			t.Fatalf("Get(%d) = %d, %v", k, got, ok)
		}
	}
}

func randomKey(r *rand.Rand) uint64 {
	switch r.IntN(4) {
	case 0:
		return r.Uint64N(64)
	case 1:
		return r.Uint64N(1 << 20)
	case 2:
		return r.Uint64()
	default:
		return 1<<40 + r.Uint64N(100)
	}
}

func TestTreeAgainstMap(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	var tr Tree[val]
	model := map[uint64]val{}
	snapshots := []struct {
		tr    Tree[val]
		model map[uint64]val
	}{}
	for i := 0; i < 20000; i++ {
		k := randomKey(r)
		if len(model) > 0 && r.IntN(3) == 0 {
			for mk := range model {
				k = mk
				break
			}
			tr = tr.Delete(k)
			delete(model, k)
		} else {
			v := val(r.Uint64N(1000))
			tr = tr.Set(k, v)
			model[k] = v
		}
		if i%2000 == 0 {
			snapshots = append(snapshots, struct {
				tr    Tree[val]
				model map[uint64]val
			}{tr, maps.Clone(model)})
		}
	}
	checkTree(t, tr, model)
	for _, s := range snapshots { // older trees never change
		checkTree(t, s.tr, s.model)
	}
	tr = tr.Delete(1 << 63) // absent
	for k := range model {
		tr = tr.Delete(k)
	}
	if tr.Len() != 0 || tr.MaxCommit() != 0 {
		t.Fatalf("emptied tree: Len %d, max %d", tr.Len(), tr.MaxCommit())
	}
}

// SetAll of sorted batches, over new and existing keys of every height,
// matches the model, and older trees never change.
func TestSetAllAgainstMap(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	var tr Tree[val]
	model := map[uint64]val{}
	var olds []Tree[val]
	var oldModels []map[uint64]val
	for i := 0; i < 3000; i++ {
		batch := map[uint64]val{}
		for range r.IntN(70) {
			k := randomKey(r)
			if len(model) > 0 && r.IntN(3) == 0 {
				for mk := range model {
					k = mk
					break
				}
			}
			batch[k] = val(r.Uint64N(1000))
		}
		keys := slices.Sorted(maps.Keys(batch))
		vals := make([]val, len(keys))
		for j, k := range keys {
			vals[j] = batch[k]
			model[k] = batch[k]
		}
		tr = tr.SetAll(keys, vals)
		if i%300 == 0 {
			olds, oldModels = append(olds, tr), append(oldModels, maps.Clone(model))
		}
	}
	checkTree(t, tr, model)
	for i := range olds {
		checkTree(t, olds[i], oldModels[i])
	}
}

// Pruning with a small budget, call after call, ends with exactly the
// values above the limit, and each call keeps everything above it.
func TestPrune(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	var tr Tree[val]
	model := map[uint64]val{}
	for i := 0; i < 5000; i++ {
		k, v := randomKey(r), val(r.Uint64N(1000))
		tr = tr.Set(k, v)
		model[k] = v
	}
	before := tr
	const limit = 600
	want := map[uint64]val{}
	for k, v := range model {
		if v > limit {
			want[k] = v
		}
	}
	next, calls := uint64(0), 0
	for {
		tr, next = tr.Prune(limit, next, 50)
		calls++
		tr.All(func(k uint64, v val) bool {
			if model[k] != v {
				t.Fatalf("pruning changed key %d", k)
			}
			return true
		})
		if next == 0 {
			break
		}
		if calls > 100000 {
			t.Fatal("the sweep does not end")
		}
	}
	checkTree(t, tr, want)
	checkTree(t, before, model)
	if calls < 10 {
		t.Fatalf("a budget of 50 nodes swept %d values in %d calls", len(model), calls)
	}
	// A summary at or below the limit drops a whole subtree at once.
	small, _ := before.Prune(1000, 0, 1)
	if small.Len() != 0 {
		t.Fatalf("every value is at or below 1000; %d left", small.Len())
	}
}
