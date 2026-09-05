package session

import (
	"errors"
	"fmt"

	"github.com/agentfox/agentkit-go/core"
)

// ErrUnknownEntry names an entry id that is not in the log.
var ErrUnknownEntry = errors.New("session: unknown entry id")

// ErrCycle reports a parent chain that does not terminate. A log this package
// writes cannot contain one — a parent is always an entry already in the file
// — but a foreign or hand-edited log can, and walking it would hang.
var ErrCycle = errors.New("session: parent chain contains a cycle")

// tree is the in-memory index over the log. An append-only log cannot delete,
// so rewind, edit-and-retry and branch navigation are re-parenting
// (REQ-SESS-07): every entry keeps its parent forever and a "branch" is just a
// root->leaf path through the forest that the parent edges induce.
type tree struct {
	entries []core.Entry
	byID    map[core.EntryID]int
	kids    map[core.EntryID][]core.EntryID

	// head is the active leaf. REQ-SESS-01 never says where it lives (P-38):
	// it is the LAST ACCEPTED ENTRY IN FILE ORDER, recomputed on every load,
	// and moved only by ForkFrom. It deliberately is not a field in the
	// header — the header is written once, before any entry exists, and a
	// mutable pointer in a write-once line could never be kept correct.
	head core.EntryID
}

func newTree() *tree {
	return &tree{byID: map[core.EntryID]int{}, kids: map[core.EntryID][]core.EntryID{}}
}

func (t *tree) has(id core.EntryID) bool {
	_, ok := t.byID[id]
	return ok
}

// add indexes one entry. The caller has already guaranteed a unique, non-empty
// id and a resolvable (or empty) parent.
func (t *tree) add(e core.Entry) {
	t.byID[e.ID] = len(t.entries)
	t.entries = append(t.entries, e)
	t.kids[e.ParentID] = append(t.kids[e.ParentID], e.ID)
	t.head = e.ID
}

func (t *tree) snapshot() []core.Entry {
	out := make([]core.Entry, len(t.entries))
	copy(out, t.entries)
	return out
}

// branch returns the root->leaf path, which is the active conversation
// (REQ-SESS-07). branch(NullLeaf) is the explicit "before the first entry"
// state and returns no entries and no error.
func (t *tree) branch(leaf core.EntryID) ([]core.Entry, error) {
	if leaf == core.NullLeaf {
		return nil, nil
	}
	idx, ok := t.byID[leaf]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownEntry, leaf)
	}
	var rev []core.Entry
	seen := make(map[core.EntryID]struct{}, 8)
	for {
		e := t.entries[idx]
		if _, dup := seen[e.ID]; dup {
			return nil, fmt.Errorf("%w at %q", ErrCycle, e.ID)
		}
		seen[e.ID] = struct{}{}
		rev = append(rev, e)
		if e.ParentID == core.NullLeaf {
			break
		}
		idx, ok = t.byID[e.ParentID]
		if !ok {
			// Unreachable after Load, which re-parents every dangling entry
			// (REQ-SESS-05.3). Treated as the root of this branch rather than
			// as an error, so a store mutated in memory still yields a path.
			break
		}
	}
	out := make([]core.Entry, len(rev))
	for i, e := range rev {
		out[len(rev)-1-i] = e
	}
	return out, nil
}

// leaves returns every divergent tip in file order. An entry with no children
// is a leaf; the active branch's leaf is one of them, and so is the tip of
// every branch the caller navigated away from.
func (t *tree) leaves() []core.EntryID {
	var out []core.EntryID
	for _, e := range t.entries {
		if len(t.kids[e.ID]) == 0 {
			out = append(out, e.ID)
		}
	}
	return out
}
