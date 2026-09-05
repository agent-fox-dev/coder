package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

const hdrLine = `{"type":"session","version":1,"id":"sess-1","timestamp":"2024-03-01T12:00:00Z","cwd":"/work"}`

func msgLine(id, parent, text string) string {
	return `{"id":"` + id + `","parent_id":"` + parent + `","type":"message",` +
		`"timestamp":"2024-03-01T12:00:00Z","message":{"role":"user",` +
		`"content":[{"type":"text","text":"` + text + `"}]}}`
}

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, path string) *Loaded {
	t.Helper()
	l, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return l
}

// TestLoaderRepairsEachDamageClassAndReportsIt is the damage matrix, one
// subtest per row.
//
// REQ-SESS-05 enumerates three classes. Three more are reachable from the same
// crash and are missing from the requirement (P-37): a malformed line in the
// interior of the file, a duplicate entry id, and a compaction anchor that
// resolves to nothing. Every row asserts BOTH the repair (the file still
// loads) and the report (REQ-SESS-05.4 forbids a silent repair).
func TestLoaderRepairsEachDamageClassAndReportsIt(t *testing.T) {
	t.Run("a truncated final line is discarded and the rest of the session loads", func(t *testing.T) {
		path := writeLog(t, hdrLine, msgLine("e1", "", "one"), msgLine("e2", "e1", "two"))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(`{"id":"e3","parent_id":"e2","ty`)
		f.Close()

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairTruncatedFinalLine) {
			t.Fatalf("repairs = %v", l.Repairs)
		}
		if got := len(l.Entries()); got != 2 {
			t.Fatalf("kept %d entries, want 2", got)
		}
		if !l.HasLostData() {
			t.Error("a discarded partial line is data loss and must say so")
		}
	})

	t.Run("an unknown entry type is retained verbatim rather than dropped", func(t *testing.T) {
		unknown := `{"id":"e2","parent_id":"e1","type":"from_the_future","timestamp":"2024-03-01T12:00:00Z","payload":{"k":1}}`
		path := writeLog(t, hdrLine, msgLine("e1", "", "one"), unknown, msgLine("e3", "e2", "three"))

		l := mustLoad(t, path)
		if got := len(l.Entries()); got != 3 {
			t.Fatalf("kept %d entries, want 3: a loader that drops what it does not model destroys data", got)
		}
		e := l.Entries()[1]
		if string(e.Raw) != unknown {
			t.Errorf("Raw\n got %s\nwant %s", e.Raw, unknown)
		}
		if e.Known() {
			t.Error("the entry should not be reported as modelled")
		}
		if !hasRepair(l.Repairs, RepairUnknownEntryType) {
			t.Errorf("repairs = %v, want an unknown-entry-type report", l.Repairs)
		}
		if l.HasLostData() {
			t.Error("retaining an unknown type is not data loss")
		}
		// The branch still runs through it.
		if got := branchIDs(t, l, "e3"); !equalIDs(got, []core.EntryID{"e1", "e2", "e3"}) {
			t.Errorf("Branch(e3) = %v, want [e1 e2 e3]", got)
		}
	})

	t.Run("a dangling parent is re-parented to the last valid entry", func(t *testing.T) {
		path := writeLog(t, hdrLine,
			msgLine("e1", "", "one"),
			msgLine("e2", "e1", "two"),
			msgLine("e3", "vanished", "three"))

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairDanglingParent) {
			t.Fatalf("repairs = %v", l.Repairs)
		}
		if got := l.Entries()[2].ParentID; got != "e2" {
			t.Errorf("re-parented to %q, want e2 (the last valid entry)", got)
		}
		if got := branchIDs(t, l, "e3"); !equalIDs(got, []core.EntryID{"e1", "e2", "e3"}) {
			t.Errorf("Branch(e3) = %v, want [e1 e2 e3]", got)
		}
	})

	t.Run("a malformed interior line is skipped and the file keeps loading", func(t *testing.T) {
		path := writeLog(t, hdrLine,
			msgLine("e1", "", "one"),
			`{"id":"e2","parent_id":"e1",,,}`,
			msgLine("e3", "e2", "three"))

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairMalformedInterior) {
			t.Fatalf("repairs = %v", l.Repairs)
		}
		if got := len(l.Entries()); got != 2 {
			t.Fatalf("kept %d entries, want 2", got)
		}
		// e3's parent went with the bad line, so it is re-parented too.
		if !hasRepair(l.Repairs, RepairDanglingParent) {
			t.Errorf("repairs = %v, want the orphan of the skipped line re-parented", l.Repairs)
		}
		if got := branchIDs(t, l, "e3"); !equalIDs(got, []core.EntryID{"e1", "e3"}) {
			t.Errorf("Branch(e3) = %v, want [e1 e3]", got)
		}
	})

	t.Run("a duplicate entry id is re-identified rather than dropped", func(t *testing.T) {
		path := writeLog(t, hdrLine,
			msgLine("e1", "", "one"),
			msgLine("e1", "e1", "one again"))

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairDuplicateEntryID) {
			t.Fatalf("repairs = %v", l.Repairs)
		}
		es := l.Entries()
		if len(es) != 2 {
			t.Fatalf("kept %d entries, want 2: the second is on disk and readable", len(es))
		}
		if es[0].ID == es[1].ID {
			t.Fatalf("both entries still carry id %q", es[0].ID)
		}
		if es[1].ParentID != "e1" {
			t.Errorf("parent %q; a reference to the shared id resolves to the first entry", es[1].ParentID)
		}
	})

	t.Run("an entry with no id gets one so later references stay resolvable", func(t *testing.T) {
		path := writeLog(t, hdrLine,
			`{"parent_id":"","type":"message","timestamp":"2024-03-01T12:00:00Z","message":{"role":"user","content":[]}}`)

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairMissingEntryID) {
			t.Fatalf("repairs = %v", l.Repairs)
		}
		if l.Entries()[0].ID == core.NullLeaf {
			t.Error("the entry still has no id")
		}
		if l.Head() == core.NullLeaf {
			t.Error("Head must name the synthesized id")
		}
	})

	t.Run("a missing header is synthesized and line 1 is reconsidered as an entry", func(t *testing.T) {
		path := writeLog(t, msgLine("e1", "", "one"), msgLine("e2", "e1", "two"))

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairMissingHeader) {
			t.Fatalf("repairs = %v", l.Repairs)
		}
		if got := len(l.Entries()); got != 2 {
			t.Fatalf("kept %d entries, want 2: line 1 is a real entry", got)
		}
		if l.Header.Version != core.SessionLogVersion {
			t.Errorf("synthesized header version = %d", l.Header.Version)
		}
	})

	t.Run("a compaction anchor that resolves to nothing is reported and never applied", func(t *testing.T) {
		compaction := `{"id":"c1","parent_id":"e1","type":"compaction","timestamp":"2024-03-01T12:00:00Z",` +
			`"compaction":{"summary":"…","first_kept_entry_id":"gone"}}`
		path := writeLog(t, hdrLine, msgLine("e1", "", "one"), compaction, msgLine("e2", "c1", "two"))

		l := mustLoad(t, path)
		if !hasRepair(l.Repairs, RepairUnresolvedAnchor) {
			t.Fatalf("repairs = %v: silently keeping this checkpoint deletes messages from every later request", l.Repairs)
		}
		r := l.Fold()
		if r.HasCheckpoint {
			t.Fatal("the unresolved checkpoint was applied")
		}
		if len(r.Messages) != 2 {
			t.Fatalf("folded %d messages, want both", len(r.Messages))
		}
		if !hasRepair(r.FoldRepairs, RepairUnresolvedAnchor) {
			t.Errorf("the fold must report it too: %v", r.FoldRepairs)
		}
	})

	t.Run("an empty file loads as an empty session with no entries", func(t *testing.T) {
		path := writeLog(t)
		l := mustLoad(t, path)
		if len(l.Entries()) != 0 || l.Head() != core.NullLeaf {
			t.Fatalf("entries=%d head=%q", len(l.Entries()), l.Head())
		}
	})
}

func TestLoadDoesNotErrorOnStructuralDamage(t *testing.T) {
	// REQ-SESS-05's premise: the loader tolerates damage rather than
	// rejecting the file. Only I/O failure is an error.
	path := writeLog(t, "not json at all", "{}", `{"id":`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load returned %v; structural damage is repaired and reported, never returned", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

// TestLoaderDoesNotRepairTranscript pins the REQ-SESS-06 boundary.
//
// Structural repair of the LOG is not semantic repair of the TRANSCRIPT. A
// loaded session commonly ends mid-turn with an unanswered tool_use; that is a
// valid log and an invalid request, and it is the provider's send-time
// transform (REQ-PROV-11) that reconciles it. Repairing it here would corrupt
// the durable record.
func TestLoaderDoesNotRepairTranscript(t *testing.T) {
	toolUse := `{"id":"e2","parent_id":"e1","type":"message","timestamp":"2024-03-01T12:00:00Z",` +
		`"message":{"role":"assistant","content":[` +
		`{"type":"text","text":"looking"},` +
		`{"type":"tool_use","id":"tu_1","name":"read_file","input":{"path":"a.txt"}}],` +
		`"stop_reason":"tool_use","provider":"anthropic","api":"anthropic-messages","model":"m"}}`
	path := writeLog(t, hdrLine, msgLine("e1", "", "read a.txt"), toolUse)

	l := mustLoad(t, path)
	if len(l.Repairs) != 0 {
		t.Fatalf("an unanswered tool_use is a VALID log; repairs = %v", l.Repairs)
	}
	r := l.Fold()
	if n := len(r.Messages); n != 2 {
		t.Fatalf("folded %d messages, want 2", n)
	}
	last, ok := r.Messages[1].(core.AssistantMessage)
	if !ok {
		t.Fatalf("last message is %T, want AssistantMessage", r.Messages[1])
	}
	uses := core.ExtractToolUse(&last)
	if len(uses) != 1 || uses[0].ID != "tu_1" {
		t.Fatalf("tool uses = %+v, want the single unanswered tu_1", uses)
	}
	for _, m := range r.Messages {
		if m.Role() == core.RoleToolResult {
			t.Fatal("the loader inserted a synthetic tool result; that is the provider's job at send time (REQ-SESS-06)")
		}
	}
	// And the same holds through a reopened store: nothing is appended to the
	// file to close the turn.
	s, _, err := Open(path, testOptions("f"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := len(s.Entries()); n != 2 {
		t.Fatalf("store has %d entries; opening a session must not write a repair entry", n)
	}
}

func TestRepairsCarryTheLineTheyDescribe(t *testing.T) {
	path := writeLog(t, hdrLine,
		msgLine("e1", "", "one"),
		`{"id":"e2",,}`,
		msgLine("e3", "e1", "three"))
	l := mustLoad(t, path)
	if countRepairs(l.Repairs, RepairMalformedInterior) != 1 {
		t.Fatalf("repairs = %v", l.Repairs)
	}
	for _, r := range l.Repairs {
		if r.Kind == RepairMalformedInterior && r.Line != 3 {
			t.Errorf("malformed line reported at line %d, want 3", r.Line)
		}
	}
	if s := l.Repairs[0].String(); !strings.Contains(s, "line 3") {
		t.Errorf("Repair.String() = %q, want it to name the line", s)
	}
}
