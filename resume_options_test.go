package agentkit

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/session"
)

// TestOpenSessionHonoursNewIDAndNowOnFirstCreate: session.Options.NewID and
// .Now exist so a golden can pin a whole file byte-for-byte (NFR-TEST-08).
// OpenSession is the front door an embedder is told to use, and on first
// create it used to overwrite both with a random id and the wall clock, so a
// golden through the front door was impossible while one through
// session.Create was fine.
func TestOpenSessionHonoursNewIDAndNowOnFirstCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fixed := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	opts := session.Options{
		NewID: func() core.EntryID { return "pinned-id" },
		Now:   func() time.Time { return fixed },
	}
	store, r, err := OpenSession(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	h := store.Header()
	if h.ID != "pinned-id" {
		t.Fatalf("header id = %q; opts.NewID was ignored on first create", h.ID)
	}
	if !h.Timestamp.Equal(fixed) {
		t.Fatalf("header timestamp = %v; opts.Now was ignored on first create", h.Timestamp)
	}
	if r.Header.ID != "pinned-id" {
		t.Fatalf("resume header id = %q, want the pinned one", r.Header.ID)
	}
	if err := store.Sync(); err != nil {
		t.Fatal(err)
	}
	line, err := session.EncodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"id":"pinned-id"`) || !strings.Contains(string(line), `"timestamp":"2024-03-01T12:00:00Z"`) {
		t.Fatalf("header line = %s; not pinnable", line)
	}
}

// And the caller that supplies neither still gets the "sess_" id and a real
// timestamp: the fix must not turn the default into the session package's
// bare hex id.
func TestOpenSessionDefaultsKeepTheSessPrefix(t *testing.T) {
	store, _, err := OpenSession(filepath.Join(t.TempDir(), "s.jsonl"), session.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := store.Header()
	if !strings.HasPrefix(h.ID, "sess_") {
		t.Fatalf("header id = %q, want the sess_ prefix by default", h.ID)
	}
	if h.Timestamp.IsZero() {
		t.Fatal("default timestamp must be set")
	}
}
