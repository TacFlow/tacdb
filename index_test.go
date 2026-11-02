package tacdb

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/TacFlow/tacdb/internal/index"
	"github.com/TacFlow/tacdb/value"
)

// Indexes through the public API: creation in a write transaction, the
// declaration visible to later snapshots only, uniqueness, errors, and
// the index surviving a reopen with its entries.
func TestIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.tacdb")
	o := options()
	db, err := Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	var ids []NodeID
	err = db.Update(bg, func(tx *Tx) error {
		for _, v := range []value.Value{value.Int(1), value.Int(2), value.Null()} {
			n, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"User"}, Properties: map[string]value.Value{"email": v}})
			if err != nil {
				return err
			}
			ids = append(ids, n.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := db.BeginRead(bg)
	def := IndexDefinition{Name: "email", Scope: "User", Property: "email", Unique: true}
	if err := db.Update(bg, func(tx *Tx) error { return tx.CreateIndex(def) }); err != nil {
		t.Fatal(err)
	}
	if defs, err := old.Indexes(); err != nil || len(defs) != 0 {
		t.Fatalf("an older snapshot sees %v, %v", defs, err)
	}
	old.Rollback()
	err = db.Update(bg, func(tx *Tx) error {
		_, err := tx.CreateNode(CreateNodeOptions{Labels: []string{"User"}, Properties: map[string]value.Value{"email": value.Float(2)}})
		return err
	})
	if code(err) != CodeUniqueViolation {
		t.Fatalf("a duplicate through the API: %v", err)
	}
	for name, fn := range map[string]func(tx *Tx) error{
		"kind": func(tx *Tx) error {
			return tx.CreateIndex(IndexDefinition{Name: "x", Scope: "A", Property: "p", Kind: 3})
		},
		"entity": func(tx *Tx) error {
			return tx.CreateIndex(IndexDefinition{Name: "x", Entity: 4, Scope: "A", Property: "p"})
		},
	} {
		if err := db.Update(bg, fn); code(err) != CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := db.Update(bg, func(tx *Tx) error { return tx.DropIndex("nope") }); code(err) != CodeIndexNotFound {
		t.Fatalf("DropIndex of a missing index: %v", err)
	}
	err = db.Update(bg, func(tx *Tx) error {
		return tx.CreateIndex(IndexDefinition{Name: "email", Scope: "User", Property: "email"})
	})
	if code(err) != CodeIndexExistsDifferent {
		t.Fatalf("another declaration under the name: %v", err)
	}
	db.View(bg, func(tx *Tx) error {
		if err := tx.CreateIndex(def); code(err) != CodeReadOnly {
			t.Fatalf("CreateIndex in a read transaction: %v", err)
		}
		return nil
	})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	o.Create = false
	o.IndexKeyPrefixBytes = 100 // only for new indexes
	db, err = Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(bg, func(tx *Tx) error {
		defs, err := tx.Indexes()
		if err != nil || !slices.Equal(defs, []IndexDefinition{def}) {
			t.Fatalf("after the reopen: %v, %v", defs, err)
		}
		set, _ := db.g.Indexes.Get(bg, tx.r)
		ix, _ := set.Named("email")
		if ix.PrefixBytes != index.DefaultPrefixBytes {
			t.Fatalf("the index took the new IndexKeyPrefixBytes: %d", ix.PrefixBytes)
		}
		it, err := index.Seek(bg, tx.r, ix, index.Query{Op: index.Equal, Value: value.Float(1)})
		if err != nil {
			t.Fatal(err)
		}
		var got []NodeID
		for it.Next(bg) {
			got = append(got, NodeID(it.Hit().Entity))
		}
		if !slices.Equal(got, ids[:1]) {
			t.Fatalf("seek after the reopen: %v", got)
		}
		return nil
	})
	if err := db.Update(bg, func(tx *Tx) error { return tx.DropIndex("email") }); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int{index.MinPrefixBytes - 1, index.MaxPrefixBytes + 1} {
		o := options()
		o.IndexKeyPrefixBytes = bad
		if _, err := OpenMemory(o); code(err) != CodeInvalidArgument {
			t.Fatalf("IndexKeyPrefixBytes %d: %v", bad, err)
		}
	}
}
