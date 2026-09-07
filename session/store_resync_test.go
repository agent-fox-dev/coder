package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// shortWriter fails the first Write part-way, leaving failAfter bytes of it
// on disk with no terminator — a disk fault, not a crash, which is the case
// Open's tail repair cannot see because the process is still alive.
type shortWriter struct {
	*os.File
	failAfter     int
	truncateFails bool
	failed        bool
}

func (w *shortWriter) Write(b []byte) (int, error) {
	if w.failed {
		return w.File.Write(b)
	}
	w.failed = true
	n, _ := w.File.Write(b[:w.failAfter])
	return n, errors.New("disk: short write")
}

func (w *shortWriter) Truncate(n int64) error {
	if w.truncateFails {
		return errors.New("disk: truncate refused")
	}
	return w.File.Truncate(n)
}

func text(t *testing.T, e core.Entry) string {
	t.Helper()
	if e.Message == nil {
		t.Fatalf("entry %s is not a message", e.ID)
	}
	return e.Message.Message.(core.UserMessage).Content.Text()
}

// TestAPartialWriteDoesNotSwallowTheNextEntry is the in-process twin of P-3.
//
// Before the fix, a failed Write left the store's state untouched: no
// pendingNewline, headerLine intact. The NEXT append then concatenated onto
// the partial bytes, the loader discarded the joined line as malformed, and
// both the failed entry and the good one after it were gone — while Append
// had returned nil for the second.
func TestAPartialWriteDoesNotSwallowTheNextEntry(t *testing.T) {
	for _, tc := range []struct {
		name          string
		truncateFails bool
		wantRepairs   bool
	}{
		{"truncation restores the file exactly", false, false},
		{"when truncation fails the partial line is terminated instead", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			s := mustCreate(t, path, testOptions("e"))
			if err := s.Append(NewMessageEntry(userMsg("one"))); err != nil {
				t.Fatal(err)
			}
			s.f = &shortWriter{File: s.f.(*os.File), failAfter: 12, truncateFails: tc.truncateFails}

			if err := s.Append(NewMessageEntry(userMsg("two"))); err == nil {
				t.Fatal("the short write must surface as Append's error (REQ-SESS-08)")
			}
			if err := s.Append(NewMessageEntry(userMsg("three"))); err != nil {
				t.Fatalf("the append after the fault must succeed: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			l := mustLoad(t, path)
			entries := l.Entries()
			if len(entries) != 2 {
				t.Fatalf("loaded %d entries, want 2 (one, three): the entry after the fault "+
					"was concatenated onto the partial bytes and lost. repairs=%v", len(entries), l.Repairs)
			}
			if got := text(t, entries[1]); got != "three" {
				t.Fatalf("entry 2 = %q, want %q", got, "three")
			}
			if entries[1].ParentID != entries[0].ID {
				t.Fatalf("entry 2 parent = %q, want %q; the failed entry never advanced Head", entries[1].ParentID, entries[0].ID)
			}
			if tc.wantRepairs != hasRepair(l.Repairs, RepairMalformedInterior) {
				t.Fatalf("repairs = %v; truncation should leave nothing to report, and the "+
					"fallback should leave exactly a reported malformed line (REQ-SESS-05.4)", l.Repairs)
			}
			if !tc.wantRepairs && len(l.Repairs) != 0 {
				t.Fatalf("repairs = %v, want none after a clean truncation", l.Repairs)
			}
		})
	}
}

// TestAPartialWriteOfTheHeaderKeepsTheHeaderPending: the first flush writes
// header and entry together (REQ-SESS-09). If that write fails part-way, the
// header must still be written on the next flush, and line 1 must still be
// the header.
func TestAPartialWriteOfTheHeaderKeepsTheHeaderPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.ensureFile(); err != nil {
		t.Fatal(err)
	}
	s.f = &shortWriter{File: s.f.(*os.File), failAfter: 20}

	if err := s.Append(NewMessageEntry(userMsg("one"))); err == nil {
		t.Fatal("the short write must surface")
	}
	if err := s.Append(NewMessageEntry(userMsg("two"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	l := mustLoad(t, path)
	if len(l.Repairs) != 0 {
		t.Fatalf("repairs = %v, want none", l.Repairs)
	}
	if l.Header.ID != "sess-1" {
		t.Fatalf("header id = %q; the header was lost with the failed first flush", l.Header.ID)
	}
	if got := len(l.Entries()); got != 1 || text(t, l.Entries()[0]) != "two" {
		t.Fatalf("entries = %d, want exactly the entry appended after the fault", got)
	}
}
