package versions

import (
	"cmp"
	"slices"
	"sync/atomic"

	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Sealed is the image of a page version sealed by a commit, shared by the
// directories that reference it. Before the append it holds only the
// image; the coordinator places it in the WAL right after the append, and
// releases the image after the Sync, when reads go to the WAL. The
// content never changes. A reader that already holds the image keeps it:
// there is no buffer pool, so the garbage collector frees it last.
//
// Place and Release are called by one goroutine, the coordinator; readers
// load the state at any time. A version that a later intent of the same
// group replaced is never appended, so it is never placed: the
// coordinator releases only the versions it placed (see Placed).
type Sealed struct {
	commit uint64
	state  atomic.Pointer[sealedState]
}

type sealedState struct {
	img    *page.Buf
	loc    wal.Locator
	placed bool
}

// NewSealed returns a sealed version of img, whose header carries commit
// and its CRC.
func NewSealed(commit uint64, img *page.Buf) *Sealed {
	s := &Sealed{commit: commit}
	s.state.Store(&sealedState{img: img})
	return s
}

// Place records where the append put the version.
func (s *Sealed) Place(loc wal.Locator) {
	st := *s.state.Load()
	st.loc, st.placed = loc, true
	s.state.Store(&st)
}

// Release drops the image once the WAL holds the version durably; reads
// go to the WAL afterwards. It panics if the version was never placed: the
// image would be lost.
func (s *Sealed) Release() {
	st := *s.state.Load()
	if !st.placed {
		panic("versions: release of a sealed version never placed in the WAL")
	}
	st.img = nil
	s.state.Store(&st)
}

// Placed reports whether the version has its place in the WAL.
func (s *Sealed) Placed() bool { return s.state.Load().placed }

// Image returns the image while it is held.
func (s *Sealed) Image() *page.Buf { return s.state.Load().img }

// Loc is where the version of a page lives: a sealed version, or a PAGE
// record of the WAL.
type Loc struct {
	commit uint64
	sealed *Sealed
	wal    wal.Locator
}

// Commit returns the CommitID of the version.
func (l Loc) Commit() uint64 { return l.commit }

// SealedLoc locates a sealed version.
func SealedLoc(s *Sealed) Loc { return Loc{commit: s.commit, sealed: s} }

// WALLoc locates a version in the WAL, as recovery finds it.
func WALLoc(commit uint64, loc wal.Locator) Loc { return Loc{commit: commit, wal: loc} }

// Directory is the immutable view of the pages of one commit: the State
// and, for each page with a version newer than the main file may hold, its
// location. A page without an entry is read from the main file.
type Directory struct {
	State     wal.State
	CommitLSN uint64 // LSN of the COMMIT that published it; zero until known
	pages     Tree[Loc]
	sweep     uint64 // where the next incremental prune continues
}

// NewDirectory returns the directory of a state with every page in the
// main file, as after a checkpoint.
func NewDirectory(state wal.State) *Directory { return &Directory{State: state} }

// Lookup returns the location of the version of a page, if newer than the
// main file may hold.
func (d *Directory) Lookup(id page.ID) (Loc, bool) { return d.pages.Get(uint64(id)) }

// Len returns the number of pages with an entry.
func (d *Directory) Len() int { return d.pages.Len() }

// Newer calls yield, in PageID order, for each page whose version in d is
// newer than commit after, with the CommitID of that version, until yield
// returns false. A checkpoint from base after copies these versions.
func (d *Directory) Newer(after uint64, yield func(id page.ID, commit uint64) bool) {
	d.pages.All(func(k uint64, l Loc) bool {
		return l.commit <= after || yield(page.ID(k), l.commit)
	})
}

// Version is a page and the location of its new version.
type Version struct {
	ID  page.ID
	Loc Loc
}

// Apply returns the directory of the next commit: state, and the new
// versions. Recovery applies the groups it reads; finalization applies a
// transaction.
func (d *Directory) Apply(state wal.State, versions []Version) *Directory {
	vs := slices.Clone(versions)
	slices.SortStableFunc(vs, func(a, b Version) int { return cmp.Compare(a.ID, b.ID) })
	keys, locs := make([]uint64, 0, len(vs)), make([]Loc, 0, len(vs))
	for i, v := range vs {
		if i+1 < len(vs) && vs[i+1].ID == v.ID {
			continue // a later version of the same page wins
		}
		keys, locs = append(keys, uint64(v.ID)), append(locs, v.Loc)
	}
	return &Directory{State: state, pages: d.pages.SetAll(keys, locs), sweep: d.sweep}
}

// Prune drops up to budget nodes of entries with a CommitID at or below
// base: those versions are in the main file, the same content (design
// §4.4 of M2), so the entries only cost memory. Each call continues where
// the previous one stopped.
func (d *Directory) Prune(base uint64, budget int) *Directory {
	n := *d
	n.pages, n.sweep = d.pages.Prune(base, d.sweep, budget)
	return &n
}
