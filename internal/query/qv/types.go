package qv

import (
	"math/bits"
	"strings"
)

// Type is a static type: the set of kinds an expression may have at run
// time. The semantic analysis reports an impossible operation only when
// the types of its operands are known (SPEC §31.3).
type Type uint16

// Static types.
const (
	TNull   = Type(1) << KindNull
	TBool   = Type(1) << KindBool
	TInt    = Type(1) << KindInt
	TFloat  = Type(1) << KindFloat
	TString = Type(1) << KindString
	TBytes  = Type(1) << KindBytes
	TVector = Type(1) << KindVector
	TList   = Type(1) << KindList
	TMap    = Type(1) << KindMap
	TNode   = Type(1) << KindNode
	TEdge   = Type(1) << KindEdge
	TPath   = Type(1) << KindPath

	TNumber = TInt | TFloat
	TAny    = TNull | TBool | TNumber | TString | TBytes | TVector | TList | TMap | TNode | TEdge | TPath
)

// TypeOf returns the static type of a kind.
func TypeOf(k Kind) Type { return Type(1) << k }

// Known returns the kind of t without Null, and whether t holds exactly
// one kind besides Null.
func (t Type) Known() (Kind, bool) {
	t &^= TNull
	if t == 0 || t&(t-1) != 0 {
		return 0, false
	}
	return Kind(bits.TrailingZeros16(uint16(t))), true
}

func (t Type) String() string {
	if t == TAny {
		return "Any"
	}
	var parts []string
	for k := KindNull; k <= KindPath; k++ {
		if t&TypeOf(k) != 0 {
			parts = append(parts, k.String())
		}
	}
	return strings.Join(parts, "|")
}
