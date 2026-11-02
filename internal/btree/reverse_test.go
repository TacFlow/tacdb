package btree_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// Multi-level trees: reverse equals reverse(forward) for random bounds.
func TestReverseMultiLevel(t *testing.T) {
	e := newEnv(t)
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 6000; i++ {
		k := []byte(fmt.Sprintf("%08d", r.Intn(1<<20)))
		e.put(k, bytes.Repeat([]byte{'x'}, r.Intn(300)))
		if i%997 == 0 {
			e.commit()
		}
	}
	for i := 0; i < 2000; i++ {
		k := []byte(fmt.Sprintf("%08d", r.Intn(1<<20)))
		if _, ok := e.model[string(k)]; ok {
			e.del(k)
		}
	}
	for j := 0; j < 200; j++ {
		var lo, hi []byte
		if r.Intn(3) > 0 {
			lo = []byte(fmt.Sprintf("%08d", r.Intn(1<<20)))
		}
		if r.Intn(3) > 0 {
			hi = []byte(fmt.Sprintf("%08d", r.Intn(1<<20)))
		}
		var fwd, rev []string
		if err := e.tree.Range(ctx, e.pins, lo, hi, func(k, _ []byte) error { fwd = append(fwd, string(k)); return nil }); err != nil {
			t.Fatal(err)
		}
		if err := e.tree.RangeReverse(ctx, e.pins, lo, hi, func(k, _ []byte) error { rev = append(rev, string(k)); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(fwd) != len(rev) {
			t.Fatalf("[%s,%s): %d forward, %d reverse", lo, hi, len(fwd), len(rev))
		}
		for i := range fwd {
			if fwd[i] != rev[len(rev)-1-i] {
				t.Fatalf("[%s,%s): mismatch at %d", lo, hi, i)
			}
		}
	}
	e.noPins()
}
