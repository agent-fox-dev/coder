package session

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestRecordCompactionRefusesAnAnchorThatIsNotInTheLog: the recorder's own
// documentation said the anchor must already be in the log, and nothing
// checked it. The entry was written with the bad anchor, the in-memory
// checkpoint got PrefixLen 0 (so the compaction dropped nothing), and every
// later Load reported RepairUnresolvedAnchor for damage the store itself had
// produced. The log is append-only, so the refusal has to happen before the
// append.
func TestRecordCompactionRefusesAnAnchorThatIsNotInTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	hist := core.NewConversationHistory()
	var hooked []error
	rec := NewRecorder(s, hist, func(err error) { hooked = append(hooked, err) })

	if _, err := rec.RecordMessage(userMsg("hi")); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []core.EntryID{"never-written", core.NullLeaf} {
		_, err := rec.RecordCompaction("summary", bad, "")
		if err == nil {
			t.Fatalf("anchor %q was accepted; it names no entry", bad)
		}
		if !errors.Is(err, ErrUnknownEntry) {
			t.Fatalf("err = %v, want ErrUnknownEntry so a caller can branch on it", err)
		}
	}
	if len(hooked) != 2 {
		t.Fatalf("hook called %d times, want 2: every recorder error is also routed to OnPersistError", len(hooked))
	}
	if got := len(s.Entries()); got != 1 {
		t.Fatalf("log holds %d entries, want 1: a refused compaction must not be written", got)
	}
	if _, has := hist.Checkpoint(); has {
		t.Fatal("a refused compaction must not install a checkpoint")
	}

	// The good path is unchanged, and the file it leaves loads clean.
	if _, err := rec.RecordCompaction("summary", "e1", ""); err != nil {
		t.Fatalf("a real anchor was refused: %v", err)
	}
	s.Close()
	if l := mustLoad(t, path); hasRepair(l.Repairs, RepairUnresolvedAnchor) {
		t.Fatalf("repairs = %v", l.Repairs)
	}
}

// A recorder with no store validates against history, so a persisted and an
// in-memory session refuse the same anchors.
func TestRecordCompactionWithoutAStoreValidatesAgainstHistory(t *testing.T) {
	hist := core.NewConversationHistory()
	rec := NewRecorder(nil, hist, nil)
	id, err := rec.RecordMessage(userMsg("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordCompaction("s", "not-recorded", ""); !errors.Is(err, ErrUnknownEntry) {
		t.Fatalf("err = %v, want ErrUnknownEntry", err)
	}
	if _, err := rec.RecordCompaction("s", id, ""); err != nil {
		t.Fatalf("the id history recorded was refused: %v", err)
	}
}
