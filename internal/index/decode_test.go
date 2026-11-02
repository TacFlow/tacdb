package index

import (
	"math/rand/v2"
	"testing"

	"github.com/TacFlow/tacdb/internal/catalog"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Random damage to a valid declaration never panics, and whatever decodes
// stays within the documented bounds.
func TestDeclarationDamage(t *testing.T) {
	ix := &Index{ID: 7, Name: "idx", Entity: Edges, Scope: 3, Property: 9, Unique: true, State: Online, PrefixBytes: 100}
	good, err := ix.encode()
	if err != nil {
		t.Fatal(err)
	}
	k := declarationKey(7)
	r := rand.New(rand.NewPCG(1, 1))
	for range 200000 {
		b := append([]byte(nil), good...)
		for range 1 + r.IntN(4) {
			switch r.IntN(3) {
			case 0:
				b[r.IntN(len(b))] = byte(r.Uint32())
			case 1:
				b = b[:r.IntN(len(b)+1)]
			default:
				b = append(b, byte(r.Uint32()))
			}
			if len(b) == 0 {
				break
			}
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("decodeDeclaration(%x) panics: %v", b, p)
				}
			}()
			got, err := decodeDeclaration(k, b)
			if err != nil {
				return
			}
			if got.PrefixBytes < MinPrefixBytes || got.PrefixBytes > MaxPrefixBytes || got.Entity > Edges || got.State > Failed || got.Scope == 0 || got.Property == 0 {
				t.Fatalf("decodeDeclaration(%x) accepts %+v", b, got)
			}
		}()
	}
	// Wrong value kinds for required fields must not decode as zero.
	bad, err := declaration.Encode(map[string]value.Value{
		"name": value.String("x"), "entity": value.Int(0), "scope": value.Int(1), "property": value.Int(1),
		"kind": value.Int(0), "unique": value.Bool(false), "state": value.Int(1), "prefixBytes": value.Int(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeDeclaration(k, bad); err != nil {
		t.Fatalf("a valid declaration: %v", err)
	}
	for _, key := range [][]byte{nil, {catalog.PrefixIndex}, declarationKey(0), append(declarationKey(1), 0)} {
		if _, err := decodeDeclaration(key, bad); errs.CodeOf(err) != errs.CodeCorrupt {
			t.Fatalf("key %x: %v", key, err)
		}
	}
}
