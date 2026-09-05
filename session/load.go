package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/agentfox/agentkit-go/core"
)

// RepairKind names one class of structural damage the loader tolerated.
//
// REQ-SESS-05 enumerates three. It is missing three more that are reachable
// from the same crash (P-37): a malformed line in the INTERIOR of the file, a
// DUPLICATE entry id, and a compaction entry whose first_kept_entry_id
// resolves to nothing. The third matters most — silently keeping such a
// checkpoint deletes messages from every later request, which reads to the
// user as the model forgetting the conversation rather than as file damage.
type RepairKind string

const (
	// RepairTruncatedFinalLine: the file did not end at a line boundary and
	// the trailing bytes are not a complete entry. The ordinary outcome of a
	// crash mid-write (REQ-SESS-05.1). Discarded, and the FILE IS TRUNCATED
	// to the last newline before it is reopened for append — see P-3 and
	// Open.
	RepairTruncatedFinalLine RepairKind = "truncated_final_line"

	// RepairMissingFinalNewline: the file did not end at a line boundary but
	// the trailing bytes are a complete entry, so only the terminator was
	// lost. The entry is KEPT and the newline is restored before the next
	// append. Discarding a provably complete entry would lose a turn.
	RepairMissingFinalNewline RepairKind = "missing_final_newline"

	// RepairMalformedInterior: a newline-terminated line that is not a JSON
	// object (P-37). The line is skipped in memory and left in the file: an
	// append-only log is not rewritten in place, so this repair is reported
	// on every subsequent load, by design.
	RepairMalformedInterior RepairKind = "malformed_interior_line"

	// RepairMissingHeader: line 1 is not a session header. A header is
	// synthesized and line 1 is reconsidered as an entry.
	RepairMissingHeader RepairKind = "missing_header"

	// RepairUnknownEntryType: an entry type this build does not model
	// (REQ-SESS-05.2). Retained verbatim and re-emitted on write. Nothing was
	// changed, so this is informational — reported anyway because a caller
	// deciding whether to refuse a file needs to know that part of it is
	// opaque to this build.
	RepairUnknownEntryType RepairKind = "unknown_entry_type"

	// RepairMissingEntryID: an entry with no id. One is synthesized, because
	// every later parent_id and every compaction anchor is an id.
	RepairMissingEntryID RepairKind = "missing_entry_id"

	// RepairDuplicateEntryID: an id already used earlier in the file (P-37).
	// The LATER entry is re-identified with a suffix and kept; references to
	// the shared id continue to resolve to the first. Dropping it instead
	// would delete a turn that is on disk and readable.
	RepairDuplicateEntryID RepairKind = "duplicate_entry_id"

	// RepairDanglingParent: parent_id names no entry (REQ-SESS-05.3). The
	// entry is re-parented to the last valid entry.
	RepairDanglingParent RepairKind = "dangling_parent"

	// RepairUnresolvedAnchor: a compaction entry whose first_kept_entry_id
	// resolves to no entry (P-37). Fold refuses to apply such a checkpoint;
	// applying it would drop every message before an anchor that does not
	// exist, which is the whole transcript.
	RepairUnresolvedAnchor RepairKind = "unresolved_anchor"
)

// Repair is one reported repair. REQ-SESS-05.4: repairs are reported, never
// silent, so a caller can surface or refuse them.
type Repair struct {
	Kind RepairKind
	// Line is the 1-based line number in the file, counting the header as
	// line 1. Zero when the repair is not line-scoped.
	Line int
	// EntryID is the entry the repair concerns, after any re-identification.
	EntryID core.EntryID
	Detail  string
}

func (r Repair) String() string {
	if r.Line > 0 {
		return fmt.Sprintf("%s at line %d: %s", r.Kind, r.Line, r.Detail)
	}
	return fmt.Sprintf("%s: %s", r.Kind, r.Detail)
}

// LostData reports whether this repair discarded bytes that were on disk. It
// is the predicate a caller wants when deciding whether to refuse a session:
// a retained unknown entry type is not data loss, a discarded malformed line
// is.
func (r Repair) LostData() bool {
	switch r.Kind {
	case RepairTruncatedFinalLine, RepairMalformedInterior:
		return true
	}
	return false
}

// Loaded is the result of reading a session log. It is always usable: the
// loader tolerates damage rather than rejecting the file (REQ-SESS-05), so a
// non-nil *Loaded with a non-empty Repairs list is the normal outcome of
// resuming after a crash, not an error.
type Loaded struct {
	Path      string
	Header    core.SessionHeader
	HeaderRaw json.RawMessage
	Repairs   []Repair

	t *tree

	// File-level repair state, consumed by Open.
	truncate     bool
	truncateTo   int64
	needsNewline bool
	needsHeader  bool
}

// Entries returns every accepted entry in file order.
func (l *Loaded) Entries() []core.Entry { return l.t.snapshot() }

// Head is the active leaf: the last accepted entry in file order (P-38).
func (l *Loaded) Head() core.EntryID { return l.t.head }

// Leaves returns the divergent tips (REQ-SESS-07).
func (l *Loaded) Leaves() []core.EntryID { return l.t.leaves() }

// Branch returns the root->leaf path. Branch(core.NullLeaf) returns nothing.
func (l *Loaded) Branch(leaf core.EntryID) ([]core.Entry, error) { return l.t.branch(leaf) }

// Fold folds the active branch into agent construction inputs (REQ-SESS-02).
func (l *Loaded) Fold() Resume {
	branch, err := l.t.branch(l.t.head)
	if err != nil {
		branch = nil
	}
	r := Fold(l.Header, branch)
	r.Path = l.Path
	r.LoadRepairs = l.Repairs
	return r
}

// HasLostData reports whether any repair discarded bytes.
func (l *Loaded) HasLostData() bool {
	for _, r := range l.Repairs {
		if r.LostData() {
			return true
		}
	}
	return false
}

// Load reads a session log and repairs it in memory. It returns an error only
// for I/O failure: structural damage is repaired and reported, never returned
// (REQ-SESS-05).
func Load(path string) (*Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadBytes(path, data), nil
}

// LoadBytes runs the loader over bytes that are not on disk. It exists for
// the fuzz target and for callers holding a log in memory; it performs no I/O
// and therefore cannot fail.
func LoadBytes(name string, data []byte) *Loaded { return loadBytes(name, data) }

func loadBytes(path string, data []byte) *Loaded {
	l := &Loaded{Path: path, t: newTree()}
	l.Header = core.SessionHeader{Version: core.SessionLogVersion}

	// The complete prefix ends at the last newline. Everything after it was
	// not terminated, and an entry is written as ONE line-terminated write,
	// so those bytes are by definition an incomplete write.
	complete := len(data)
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		complete = i + 1
	} else {
		complete = 0
	}
	l.truncateTo = int64(complete)

	lines := splitLines(data[:complete])
	fragment := data[complete:]

	if len(data) == 0 {
		l.needsHeader = true
		return l
	}

	next := 0
	if len(lines) > 0 {
		if h, err := DecodeHeader(lines[0]); err == nil {
			l.Header = h
			l.HeaderRaw = append(json.RawMessage(nil), lines[0]...)
			next = 1
		} else {
			l.report(Repair{Kind: RepairMissingHeader, Line: 1, Detail: err.Error()})
		}
	} else if len(fragment) > 0 {
		// The whole file is one unterminated line; it may be the header.
		if h, err := DecodeHeader(fragment); err == nil {
			l.Header = h
			l.HeaderRaw = append(json.RawMessage(nil), fragment...)
			l.needsNewline = true
			l.report(Repair{Kind: RepairMissingFinalNewline, Line: 1,
				Detail: "header line was written without its terminator"})
			return l
		}
		l.truncate = true
		l.report(Repair{Kind: RepairTruncatedFinalLine, Line: 1,
			Detail: "file contains a single incomplete line"})
		l.needsHeader = true
		return l
	}

	for i := next; i < len(lines); i++ {
		l.accept(lines[i], i+1, false)
	}

	if len(fragment) > 0 {
		// REQ-SESS-05.1 and P-3. A fragment that parses as a whole entry lost
		// only its terminator and is kept; anything else is discarded AND the
		// file is truncated, because leaving the partial bytes in place makes
		// the next append concatenate onto them and lose both entries.
		if !l.accept(fragment, len(lines)+1, true) {
			l.truncate = true
		}
	}
	l.checkAnchors()
	return l
}

// accept parses and indexes one line, applying every entry-scoped repair.
// It reports whether the line was accepted.
func (l *Loaded) accept(line []byte, num int, isFragment bool) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		if isFragment {
			return false
		}
		return true // a blank line carries nothing; nothing was lost
	}
	e, err := DecodeEntry(line)
	if err != nil {
		if isFragment {
			l.report(Repair{Kind: RepairTruncatedFinalLine, Line: num,
				Detail: "final line is not a complete JSON object: " + err.Error()})
			return false
		}
		l.report(Repair{Kind: RepairMalformedInterior, Line: num, Detail: err.Error()})
		return false
	}
	if isFragment {
		l.needsNewline = true
		l.report(Repair{Kind: RepairMissingFinalNewline, Line: num, EntryID: e.ID,
			Detail: "final entry was written without its terminator"})
	}
	if e.ID == core.NullLeaf {
		e.ID = deriveID(l.Header.ID, num)
		e.Raw = nil // the envelope changed; rebuild rather than patch a line with no id
		l.report(Repair{Kind: RepairMissingEntryID, Line: num, EntryID: e.ID,
			Detail: "entry had no id; one was synthesized"})
	}
	if l.t.has(e.ID) {
		orig := e.ID
		e.ID = l.uniqueID(orig)
		l.report(Repair{Kind: RepairDuplicateEntryID, Line: num, EntryID: e.ID,
			Detail: fmt.Sprintf("id %q was already used; this entry was re-identified", orig)})
	}
	if e.ParentID != core.NullLeaf && !l.t.has(e.ParentID) {
		orig := e.ParentID
		// REQ-SESS-05.3: re-parent to the last valid entry, which is the head
		// at this point in the file.
		e.ParentID = l.t.head
		l.report(Repair{Kind: RepairDanglingParent, Line: num, EntryID: e.ID,
			Detail: fmt.Sprintf("parent %q is missing; re-parented to %q", orig, e.ParentID)})
	}
	if !e.Known() {
		l.report(Repair{Kind: RepairUnknownEntryType, Line: num, EntryID: e.ID,
			Detail: fmt.Sprintf("entry type %q is not modelled by this build; retained verbatim", e.Type)})
	}
	l.t.add(e)
	return true
}

// checkAnchors reports every compaction entry whose anchor resolves to no
// entry in the file (P-37). Fold performs the same check against the BRANCH,
// which is stricter — an anchor on a different branch is unresolvable from
// here — but a file-scoped report is what lets a caller refuse the session
// before constructing an agent.
func (l *Loaded) checkAnchors() {
	for _, e := range l.t.entries {
		if e.Type != core.EntryCompaction || e.Compaction == nil {
			continue
		}
		anchor := e.Compaction.FirstKeptEntryID
		if anchor != core.NullLeaf && l.t.has(anchor) {
			continue
		}
		l.report(Repair{Kind: RepairUnresolvedAnchor, EntryID: e.ID,
			Detail: fmt.Sprintf("first_kept_entry_id %q names no entry; the checkpoint will not be applied", anchor)})
	}
}

func (l *Loaded) report(r Repair) { l.Repairs = append(l.Repairs, r) }

func (l *Loaded) uniqueID(base core.EntryID) core.EntryID {
	for n := 2; ; n++ {
		cand := core.EntryID(fmt.Sprintf("%s#%d", base, n))
		if !l.t.has(cand) {
			return cand
		}
	}
}

func deriveID(sessionID string, line int) core.EntryID {
	if sessionID == "" {
		sessionID = "session"
	}
	return core.EntryID(fmt.Sprintf("%s-line%d", sessionID, line))
}

// splitLines splits on '\n'. data always ends with '\n' here, so the trailing
// empty element is dropped.
func splitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{'\n'})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
}
