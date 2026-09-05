package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/jsonx"
)

var fixedTime = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

// testOptions makes ids and timestamps deterministic so a whole file can be
// compared byte-for-byte.
func testOptions(prefix string) Options {
	var mu sync.Mutex
	n := 0
	return Options{
		NewID: func() core.EntryID {
			mu.Lock()
			defer mu.Unlock()
			n++
			return core.EntryID(fmt.Sprintf("%s%d", prefix, n))
		},
		Now: func() time.Time { return fixedTime },
	}
}

func testHeader() core.SessionHeader {
	return core.SessionHeader{
		Version:   core.SessionLogVersion,
		ID:        "sess-1",
		Timestamp: fixedTime,
		CWD:       "/work",
	}
}

func userMsg(text string) core.UserMessage {
	return core.UserMessage{Content: core.Content{core.TextBlock{Text: text}}, Timestamp: fixedTime}
}

func assistantMsg(text string) core.AssistantMessage {
	return core.AssistantMessage{
		Content:    core.Content{core.TextBlock{Text: text}},
		StopReason: core.StopReasonStop,
		Timestamp:  fixedTime,
	}
}

func mustCreate(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	s, err := Create(path, testHeader(), opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(b)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------- REQ-SESS-09

func TestCreateWritesNothingUntilTheFirstFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Create touched the filesystem; REQ-SESS-09 makes the FIRST FLUSH the create point (stat err = %v)", err)
	}
	if err := s.Append(NewMessageEntry(userMsg("hi"))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first Append did not create the log: %v", err)
	}
}

func TestHeaderAndFirstUserEntryAreOnDiskBeforeTheFirstAssistantMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.Append(NewMessageEntry(userMsg("hello"))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Deliberately no Close and no assistant message: REQ-SESS-09 says a
	// session that crashes during turn 1 must still leave a header and the
	// user entry on disk.
	lines := nonEmptyLines(readFile(t, path))
	if len(lines) != 2 {
		t.Fatalf("want header + user entry on disk, got %d lines: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], `{"type":"session"`) {
		t.Errorf("line 1 is not the header: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"hello"`) {
		t.Errorf("line 2 is not the user entry: %s", lines[1])
	}
}

func TestHeaderLineMatchesTheRequiredShapeExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := nonEmptyLines(readFile(t, path))[0]
	want := `{"type":"session","version":1,"id":"sess-1","timestamp":"2024-03-01T12:00:00Z","cwd":"/work"}`
	if got != want {
		t.Errorf("header line\n got %s\nwant %s", got, want)
	}
}

func TestEntryLineLeadsWithTheEnvelopeInRequiredOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.Append(NewMessageEntry(userMsg("hi"))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := nonEmptyLines(readFile(t, path))[1]
	prefix := `{"id":"e1","parent_id":"","type":"message","timestamp":"2024-03-01T12:00:00Z","message":{`
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("entry envelope\n got %s\nwant prefix %s", got, prefix)
	}
}

func TestFirstFlushRefusesToClobberAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("pre-existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reported []error
	opts := testOptions("e")
	opts.OnPersistError = func(err error) { reported = append(reported, err) }
	s := mustCreate(t, path, opts)

	err := s.Append(NewMessageEntry(userMsg("hi")))
	if !errors.Is(err, core.ErrSessionExists) {
		t.Fatalf("Append over an existing file: got %v, want core.ErrSessionExists", err)
	}
	if len(reported) != 1 {
		t.Fatalf("REQ-SESS-08: the collision must reach OnPersistError; got %d reports", len(reported))
	}
	if readFile(t, path) != "pre-existing\n" {
		t.Error("O_EXCL did not hold: the existing file was modified")
	}
}

func TestAppendReturnsItsErrorAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a file, not a directory: the open fails.
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var reported []error
	opts := testOptions("e")
	opts.OnPersistError = func(err error) { reported = append(reported, err) }
	s := mustCreate(t, filepath.Join(blocker, "s.jsonl"), opts)

	if err := s.Append(NewMessageEntry(userMsg("hi"))); err == nil {
		t.Fatal("REQ-SESS-08: Append must return its write error")
	}
	if len(reported) != 1 {
		t.Fatalf("OnPersistError got %d reports, want 1", len(reported))
	}
}

func TestPerEntryDurabilityIsAvailableAndBufferedIsTheDefault(t *testing.T) {
	// P-36: exactly two levels ship. This pins that the API offers no third,
	// turn-scoped level the store could not honour, and that both levels put
	// the bytes in the file on every Append.
	for _, d := range []Durability{DurabilityBuffered, DurabilityPerEntry} {
		path := filepath.Join(t.TempDir(), "s.jsonl")
		opts := testOptions("e")
		opts.Durability = d
		s := mustCreate(t, path, opts)
		if err := s.Append(NewMessageEntry(userMsg("hi"))); err != nil {
			t.Fatalf("durability %d: %v", d, err)
		}
		if n := len(nonEmptyLines(readFile(t, path))); n != 2 {
			t.Errorf("durability %d: %d lines on disk after one Append, want 2", d, n)
		}
		s.Close()
	}
	if DurabilityBuffered != 0 {
		t.Error("the zero value of Durability must be the buffered level")
	}
}

func TestSyncCreatesTheLogWithItsHeaderEvenWithNoEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n := len(nonEmptyLines(readFile(t, path))); n != 1 {
		t.Fatalf("want header only, got %d lines", n)
	}
	// And a following Append must not write the header twice.
	if err := s.Append(NewMessageEntry(userMsg("hi"))); err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(readFile(t, path))
	if len(lines) != 2 || strings.Contains(lines[1], `"type":"session"`) {
		t.Fatalf("header written twice: %q", lines)
	}
}

// ---------------------------------------------------------------- P-3

// TestAppendAfterTruncatedTailLandsOnItsOwnLine is the P-3 test, and it
// reloads TWICE on purpose.
//
// REQ-SESS-05.1 says to discard a truncated final line. Discarding it in
// memory is not enough: the O_APPEND offset still sits past the surviving
// partial bytes, so the next entry concatenates onto the partial and BOTH are
// lost — and the loader reports the same repair on every subsequent load. The
// single-reload version of this test cannot see that, because after one
// reload the damaged bytes are still ahead of the write cursor and nothing has
// been appended past them yet.
func TestAppendAfterTruncatedTailLandsOnItsOwnLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")

	s := mustCreate(t, path, testOptions("e"))
	if err := s.Append(NewMessageEntry(userMsg("one"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(NewMessageEntry(assistantMsg("two"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a kill mid-write: a partial line with no terminator.
	partial := `{"id":"e3","parent_id":"e2","type":"messa`
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(partial); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// First reload: the damage is reported and repaired.
	s2, l2, err := Open(path, testOptions("f"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !hasRepair(l2.Repairs, RepairTruncatedFinalLine) {
		t.Fatalf("first load did not report the truncated tail: %v", l2.Repairs)
	}
	if got := len(l2.Entries()); got != 2 {
		t.Fatalf("first load kept %d entries, want 2", got)
	}
	if err := s2.Append(NewMessageEntry(userMsg("three"))); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	// Second reload: the whole point. Without truncation the appended entry
	// was concatenated onto the partial bytes, so it is neither parseable nor
	// present, and the same repair is reported forever.
	l3, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(l3.Repairs) != 0 {
		t.Errorf("second load still reports repairs, so the damage was never removed from the file: %v", l3.Repairs)
	}
	entries := l3.Entries()
	if len(entries) != 3 {
		t.Fatalf("second load has %d entries, want 3 (the appended entry was swallowed by the partial line)", len(entries))
	}
	if got := entries[2].Message.Message.(core.UserMessage).Content.Text(); got != "three" {
		t.Errorf("entry 3 text = %q, want %q", got, "three")
	}
	if strings.Contains(readFile(t, path), partial) {
		t.Error("the partial bytes are still in the file")
	}
	for i, line := range nonEmptyLines(readFile(t, path)) {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not valid JSON: %s", i+1, line)
		}
	}
}

func TestCompleteFinalEntryMissingOnlyItsNewlineIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.Append(NewMessageEntry(userMsg("one"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Strip the final newline only: the entry itself is provably complete, so
	// discarding it would lose a turn that is entirely on disk.
	b := readFile(t, path)
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(b, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	s2, l2, err := Open(path, testOptions("f"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasRepair(l2.Repairs, RepairMissingFinalNewline) {
		t.Fatalf("repairs = %v, want a missing-final-newline report", l2.Repairs)
	}
	if len(l2.Entries()) != 1 {
		t.Fatalf("the complete final entry was discarded: %d entries", len(l2.Entries()))
	}
	if err := s2.Append(NewMessageEntry(userMsg("two"))); err != nil {
		t.Fatal(err)
	}
	s2.Close()

	l3, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l3.Repairs) != 0 {
		t.Errorf("second load reports %v; the missing terminator was not restored", l3.Repairs)
	}
	if len(l3.Entries()) != 2 {
		t.Fatalf("second load has %d entries, want 2", len(l3.Entries()))
	}
}

// ---------------------------------------------------------------- REQ-SESS-07

func TestForkFromKeepsBothBranchesInOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	for _, m := range []core.Message{userMsg("a"), assistantMsg("b"), userMsg("c")} {
		if err := s.Append(NewMessageEntry(m)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ForkFrom("e1"); err != nil {
		t.Fatalf("ForkFrom: %v", err)
	}
	if s.Head() != "e1" {
		t.Fatalf("Head after ForkFrom = %q, want e1", s.Head())
	}
	if err := s.Append(NewMessageEntry(assistantMsg("b-alt"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if n := len(nonEmptyLines(readFile(t, path))); n != 5 {
		t.Fatalf("file has %d lines, want header + 4 entries: nothing may be rewritten or deleted", n)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Leaves(); !equalIDs(got, []core.EntryID{"e3", "e4"}) {
		t.Errorf("Leaves = %v, want [e3 e4]", got)
	}
	if got := branchIDs(t, l, "e3"); !equalIDs(got, []core.EntryID{"e1", "e2", "e3"}) {
		t.Errorf("Branch(e3) = %v, want [e1 e2 e3]", got)
	}
	if got := branchIDs(t, l, "e4"); !equalIDs(got, []core.EntryID{"e1", "e4"}) {
		t.Errorf("Branch(e4) = %v, want [e1 e4]", got)
	}
	// P-38: the active leaf is the last accepted entry in FILE order, with no
	// format change and no head field in the write-once header.
	if l.Head() != "e4" {
		t.Errorf("Head after reload = %q, want e4 (last entry in file order)", l.Head())
	}
}

func TestNullLeafIsAnExplicitState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if s.Head() != core.NullLeaf {
		t.Fatalf("Head before the first entry = %q, want the null leaf", s.Head())
	}
	b, err := s.Branch(core.NullLeaf)
	if err != nil || b != nil {
		t.Fatalf("Branch(NullLeaf) = %v, %v; want nil, nil", b, err)
	}
	if err := s.Append(NewMessageEntry(userMsg("a"))); err != nil {
		t.Fatal(err)
	}
	if err := s.ForkFrom(core.NullLeaf); err != nil {
		t.Fatalf("ForkFrom(NullLeaf): %v", err)
	}
	if err := s.Append(NewMessageEntry(userMsg("fresh root"))); err != nil {
		t.Fatal(err)
	}
	if got := branchIDs(t, s, "e2"); !equalIDs(got, []core.EntryID{"e2"}) {
		t.Errorf("Branch(e2) = %v, want [e2]: a null-leaf fork starts a new root", got)
	}
	if got := s.Leaves(); !equalIDs(got, []core.EntryID{"e1", "e2"}) {
		t.Errorf("Leaves = %v, want [e1 e2]", got)
	}
	s.Close()
}

func TestForkFromRejectsAnUnknownEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.ForkFrom("nope"); !errors.Is(err, ErrUnknownEntry) {
		t.Fatalf("ForkFrom(unknown) = %v, want ErrUnknownEntry", err)
	}
}

// ---------------------------------------------------------------- P-57

// TestUnknownEntryTypeRoundTripsByteExact is the P-57 test.
//
// NFR-TEST-03 is only achievable through Entry.Raw passthrough, and not only
// for unknown entry types: decode-then-re-encode of a MODELLED entry also
// reorders keys and rewrites numeric literals. The fixture below therefore
// carries, in BOTH a modelled and an unmodelled entry, keys out of sorted
// order at three nesting depths and numeric literals that a float64 round-trip
// would destroy.
func TestUnknownEntryTypeRoundTripsByteExact(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")

	header := `{"type":"session","version":1,"id":"sess-1","timestamp":"2024-03-01T12:00:00Z","cwd":"/work"}`
	// An entry type this build does not model, with a payload no part of this
	// package understands.
	unknown := `{"id":"u1","parent_id":"","type":"telemetry_v9",` +
		`"timestamp":"2024-03-01T12:00:00Z",` +
		`"telemetry_v9":{"zeta":1,"alpha":{"yankee":true,` +
		`"bravo":[{"zulu":"z","charlie":0}]},` +
		`"nums":{"big":9007199254740993,"exp":1e3,"trailing":1.10}}}`
	// A MODELLED entry whose tool_use input has no key in sorted position at
	// three depths, and whose numbers must survive verbatim.
	modelled := `{"id":"u2","parent_id":"u1","type":"message","timestamp":"2024-03-01T12:00:00Z",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search",` +
		`"input":{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0}]},` +
		`"nums":{"big":9007199254740993,"exp":1e3,"trailing":1.10}}}],` +
		`"stop_reason":"tool_use","provider":"anthropic","api":"anthropic-messages","model":"m"}}`

	lines := []string{header, unknown, modelled}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRepair(l.Repairs, RepairUnknownEntryType) {
		t.Errorf("an unmodelled entry type must be reported, not silently kept: %v", l.Repairs)
	}
	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("loaded %d entries, want 2 (a loader that drops what it does not model loses data)", len(entries))
	}

	for i, e := range entries {
		if string(e.Raw) != lines[i+1] {
			t.Errorf("entry %d Raw\n got %s\nwant %s", i, e.Raw, lines[i+1])
		}
		got, err := EncodeEntry(e)
		if err != nil {
			t.Fatalf("EncodeEntry: %v", err)
		}
		if string(got) != lines[i+1] {
			t.Errorf("re-encoded entry %d\n got %s\nwant %s", i, got, lines[i+1])
		}
	}

	// And through a real store, so the passthrough survives Append's envelope
	// assignment as well.
	dst := filepath.Join(dir, "dst.jsonl")
	s, err := Create(dst, l.Header, testOptions("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := s.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, dst), readFile(t, src); got != want {
		t.Errorf("copied log differs\n got %s\nwant %s", got, want)
	}
}

func TestReEncodingAModelledEntryDoesNotLaunderNumbersThroughFloat64(t *testing.T) {
	// The negative control for the test above: this is what a value-level
	// round-trip does to the same bytes.
	in := []byte(`{"big":9007199254740993,"exp":1e3,"trailing":1.10}`)
	var m map[string]any
	if err := json.Unmarshal(in, &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(in, out) {
		t.Fatal("the fixture does not discriminate: a map round-trip reproduced it byte-for-byte")
	}
	// jsonx, which the codec uses, does not.
	ord, err := jsonx.DecodeOrderedObject(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ord.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, got) {
		t.Errorf("ordered round-trip\n got %s\nwant %s", got, in)
	}
}

// ---------------------------------------------------------------- misc

func TestAppendIsSafeUnderConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Append(NewMessageEntry(userMsg(fmt.Sprint(i)))); err != nil {
				t.Errorf("Append: %v", err)
			}
			_ = s.Entries()
			_ = s.Leaves()
		}(i)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries()) != 16 || len(l.Repairs) != 0 {
		t.Fatalf("got %d entries, repairs %v; want 16 clean entries", len(l.Entries()), l.Repairs)
	}
}

func TestAppendRefusesADuplicateEntryID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	e := NewMessageEntry(userMsg("a"))
	e.ID = "fixed"
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(e); err == nil {
		t.Fatal("Append must refuse an id already in the log")
	}
	if n := len(nonEmptyLines(readFile(t, path))); n != 2 {
		t.Fatalf("the refused entry was written anyway: %d lines", n)
	}
}

func hasRepair(rs []Repair, k RepairKind) bool {
	for _, r := range rs {
		if r.Kind == k {
			return true
		}
	}
	return false
}

func countRepairs(rs []Repair, k RepairKind) int {
	n := 0
	for _, r := range rs {
		if r.Kind == k {
			n++
		}
	}
	return n
}

type brancher interface {
	Branch(core.EntryID) ([]core.Entry, error)
}

func branchIDs(t *testing.T, b brancher, leaf core.EntryID) []core.EntryID {
	t.Helper()
	es, err := b.Branch(leaf)
	if err != nil {
		t.Fatalf("Branch(%q): %v", leaf, err)
	}
	out := make([]core.EntryID, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func equalIDs(a, b []core.EntryID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
