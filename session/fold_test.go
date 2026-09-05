package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestFoldRecoversAPINotJustProviderAndModel is the P-4 test.
//
// REQ-SESS-02 folds "provider and model ID". REQ-PROV-11 rule 1 computes
// same_model over the (provider, api, model) TRIPLE. Recovering two of three
// makes same_model false on the first post-resume request, which silently
// downgrades every signed ThinkingBlock to text and strips every
// thought_signature — a quality regression that appears only after a resume,
// which is why it is tested through the FILE and not through Fold alone.
func TestFoldRecoversAPINotJustProviderAndModel(t *testing.T) {
	t.Run("from the last model_change entry", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.jsonl")
		s := mustCreate(t, path, testOptions("e"))
		if err := s.Append(NewMessageEntry(userMsg("hi"))); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(NewModelChangeEntry("anthropic", core.APIAnthropicMessages, "claude-opus-4-5")); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		// Through the file, so a codec that drops "api" fails here too.
		line := nonEmptyLines(readFile(t, path))[2]
		if !strings.Contains(line, `"api":"anthropic-messages"`) {
			t.Fatalf("model_change line does not carry api: %s", line)
		}

		r := mustLoad(t, path).Fold()
		if r.Provider != "anthropic" || r.ModelID != "claude-opus-4-5" {
			t.Fatalf("provider=%q model=%q", r.Provider, r.ModelID)
		}
		if r.API != core.APIAnthropicMessages {
			t.Fatalf("API = %q, want %q: two of three is not the triple REQ-PROV-11 rule 1 needs",
				r.API, core.APIAnthropicMessages)
		}
	})

	t.Run("from the provenance of the last assistant message when no entry exists", func(t *testing.T) {
		am := assistantMsg("sure")
		am.Provider = "openai"
		am.API = core.APIOpenAICompletions
		am.Model = "gpt-x"
		am.ThinkingLevel = core.ThinkingHigh

		path := filepath.Join(t.TempDir(), "s.jsonl")
		s := mustCreate(t, path, testOptions("e"))
		if err := s.Append(NewMessageEntry(userMsg("hi"))); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(NewMessageEntry(am)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		r := mustLoad(t, path).Fold()
		if r.Provider != "openai" || r.ModelID != "gpt-x" {
			t.Fatalf("provider=%q model=%q", r.Provider, r.ModelID)
		}
		if r.API != core.APIOpenAICompletions {
			t.Fatalf("API = %q, want %q: the provenance fallback must recover all three fields",
				r.API, core.APIOpenAICompletions)
		}
	})
}

func TestFoldPrefersAnExplicitModelChangeOverMessageProvenance(t *testing.T) {
	am := assistantMsg("sure")
	am.Provider, am.API, am.Model = "openai", core.APIOpenAICompletions, "gpt-x"

	branch := []core.Entry{
		{ID: "e1", Type: core.EntryMessage, Message: &core.MessageEntry{Message: userMsg("hi")}},
		{ID: "e2", ParentID: "e1", Type: core.EntryModelChange,
			ModelChange: &core.ModelChangeEntry{Provider: "anthropic", API: core.APIAnthropicMessages, ModelID: "claude"}},
		{ID: "e3", ParentID: "e2", Type: core.EntryMessage, Message: &core.MessageEntry{Message: am}},
	}
	r := Fold(testHeader(), branch)
	if r.Provider != "anthropic" || r.API != core.APIAnthropicMessages || r.ModelID != "claude" {
		t.Fatalf("got (%q,%q,%q); REQ-SESS-02 orders the model_change entry ahead of message provenance",
			r.Provider, r.API, r.ModelID)
	}
}

func TestFoldRecoversTheReasoningLevel(t *testing.T) {
	branch := []core.Entry{
		{ID: "e1", Type: core.EntryThinkingLevelChange,
			ThinkingChange: &core.ThinkingLevelChangeEntry{Level: core.ThinkingHigh}},
		{ID: "e2", ParentID: "e1", Type: core.EntryThinkingLevelChange,
			ThinkingChange: &core.ThinkingLevelChangeEntry{Level: core.ThinkingLow}},
	}
	if got := Fold(testHeader(), branch).ThinkingLevel; got != core.ThinkingLow {
		t.Fatalf("thinking level = %q, want the LAST change (%q)", got, core.ThinkingLow)
	}
}

// TestFoldKeepsSummarizedMessagesAndReportsTheCheckpoint pins REQ-SESS-04: a
// compaction is an entry, not a rewrite. The summarized entries stay in the
// file and in the folded message list; only the per-request view drops them.
func TestFoldKeepsSummarizedMessagesAndReportsTheCheckpoint(t *testing.T) {
	branch := []core.Entry{
		{ID: "m1", Type: core.EntryMessage, Message: &core.MessageEntry{Message: userMsg("one")}},
		{ID: "m2", ParentID: "m1", Type: core.EntryMessage, Message: &core.MessageEntry{Message: assistantMsg("two")}},
		{ID: "m3", ParentID: "m2", Type: core.EntryMessage, Message: &core.MessageEntry{Message: userMsg("three")}},
		{ID: "c1", ParentID: "m3", Type: core.EntryCompaction,
			Compaction: &core.CompactionEntry{Summary: "so far…", FirstKeptEntryID: "m3"}},
		{ID: "m4", ParentID: "c1", Type: core.EntryMessage, Message: &core.MessageEntry{Message: assistantMsg("four")}},
	}
	r := Fold(testHeader(), branch)
	if len(r.Messages) != 4 {
		t.Fatalf("folded %d messages, want all 4: summarized entries are not deleted", len(r.Messages))
	}
	if !r.HasCheckpoint {
		t.Fatal("no checkpoint recovered")
	}
	if r.Checkpoint.PrefixLen != 2 {
		t.Errorf("PrefixLen = %d, want 2 (the index of the first kept message)", r.Checkpoint.PrefixLen)
	}
	if r.Checkpoint.Summary != "so far…" || r.Checkpoint.EntryID != "c1" {
		t.Errorf("checkpoint = %+v", r.Checkpoint)
	}
	// P-2: without CreatedAtLen, REQ-GO-15's skip rule (c) can never fire,
	// because nothing on an AssistantMessage records which prefix it was sent
	// under. Three messages existed when the checkpoint was written.
	if r.Checkpoint.CreatedAtLen != 3 {
		t.Errorf("CreatedAtLen = %d, want 3", r.Checkpoint.CreatedAtLen)
	}
	if got := r.Checkpoint.MinAnchorIndex(); got != 2 {
		t.Errorf("MinAnchorIndex = %d, want 2", got)
	}
	if cp, ok := r.History.Checkpoint(); !ok || cp != r.Checkpoint {
		t.Errorf("history checkpoint = %+v (ok=%v), want %+v", cp, ok, r.Checkpoint)
	}
}

func TestCompactionAnchorNamingAnUnmodelledEntryTypeStillResolves(t *testing.T) {
	// NFR-TEST-03(c): a compaction entry whose first_kept_entry_id names an
	// entry type the reader does not model — the kept tail must survive.
	unknown := `{"id":"x1","parent_id":"m1","type":"from_the_future","timestamp":"2024-03-01T12:00:00Z"}`
	compaction := `{"id":"c1","parent_id":"m2","type":"compaction","timestamp":"2024-03-01T12:00:00Z",` +
		`"compaction":{"summary":"s","first_kept_entry_id":"x1"}}`
	path := writeLog(t, hdrLine,
		msgLine("m1", "", "one"),
		unknown,
		msgLine("m2", "x1", "two"),
		compaction)

	r := mustLoad(t, path).Fold()
	if len(r.FoldRepairs) != 0 {
		t.Fatalf("anchor did not resolve: %v", r.FoldRepairs)
	}
	if !r.HasCheckpoint {
		t.Fatal("no checkpoint")
	}
	if r.Checkpoint.PrefixLen != 1 {
		t.Errorf("PrefixLen = %d, want 1: the unmodelled entry holds the position of the next message",
			r.Checkpoint.PrefixLen)
	}
	if len(r.Messages) != 2 {
		t.Fatalf("folded %d messages, want 2", len(r.Messages))
	}
}

func TestFoldFollowsOnlyTheActiveBranch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	for _, m := range []core.Message{userMsg("a"), assistantMsg("b")} {
		if err := s.Append(NewMessageEntry(m)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ForkFrom("e1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(NewMessageEntry(assistantMsg("b-alt"))); err != nil {
		t.Fatal(err)
	}
	s.Close()

	l := mustLoad(t, path)
	r := l.Fold()
	if len(r.Messages) != 2 {
		t.Fatalf("folded %d messages, want 2 (only the active branch)", len(r.Messages))
	}
	if got := r.Messages[1].(core.AssistantMessage).Content.Text(); got != "b-alt" {
		t.Errorf("folded the abandoned branch: %q", got)
	}
	if r.LeafID != "e3" {
		t.Errorf("LeafID = %q, want e3", r.LeafID)
	}
	// The other branch is still readable.
	other := Fold(l.Header, mustBranch(t, l, "e2"))
	if got := other.Messages[1].(core.AssistantMessage).Content.Text(); got != "b" {
		t.Errorf("the abandoned branch is gone: %q", got)
	}
}

// TestBranchSummaryWrapperIsPinned is the golden fixture REQ-SESS-07 asks for:
// the wrapper is model-visible format contract, so changing it changes what
// every future turn of a branched session reads.
func TestBranchSummaryWrapperIsPinned(t *testing.T) {
	const want = "[The conversation was rewound. A summary of the abandoned branch follows.]\n\nwe tried X and it failed"
	if got := RenderBranchSummary("we tried X and it failed"); got != want {
		t.Fatalf("wrapper\n got %q\nwant %q", got, want)
	}

	branch := []core.Entry{
		{ID: "b1", Type: core.EntryBranchSummary,
			BranchSummary: &core.BranchSummaryEntry{Summary: "we tried X and it failed",
				FromLeafID: "e9", ForkPointID: "e1"}},
	}
	r := Fold(testHeader(), branch)
	if len(r.Messages) != 1 {
		t.Fatalf("folded %d messages, want the branch summary rendered as one", len(r.Messages))
	}
	um, ok := r.Messages[0].(core.UserMessage)
	if !ok {
		t.Fatalf("rendered as %T, want a UserMessage", r.Messages[0])
	}
	if um.Content.Text() != want {
		t.Errorf("rendered\n got %q\nwant %q", um.Content.Text(), want)
	}
}

func TestFoldRendersACustomMessageAsAUserMessage(t *testing.T) {
	branch := []core.Entry{
		{ID: "c1", Type: core.EntryCustomMessage,
			Custom: &core.CustomMessageEntry{Kind: "note",
				Content: core.Content{core.TextBlock{Text: "host note"}}}},
	}
	r := Fold(testHeader(), branch)
	if len(r.Messages) != 1 {
		t.Fatalf("folded %d messages, want 1", len(r.Messages))
	}
	if _, ok := r.Messages[0].(core.UserMessage); !ok {
		t.Fatalf("rendered as %T, want a UserMessage", r.Messages[0])
	}
}

func TestFoldOfAnEmptyBranchIsUsable(t *testing.T) {
	r := Fold(testHeader(), nil)
	if r.History == nil {
		t.Fatal("Fold must always return a usable history")
	}
	if len(r.Messages) != 0 || r.HasCheckpoint || r.LeafID != core.NullLeaf {
		t.Fatalf("%+v", r)
	}
}

// ---------------------------------------------------------------- recorder

func TestRecorderWritesConfigChangesIntoTheSameOrderedLog(t *testing.T) {
	// REQ-SESS-03: a configuration change not written into the same ordered
	// log at the moment it happens is not recoverable by REQ-SESS-02.
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	hist := core.NewConversationHistory()
	rec := NewRecorder(s, hist, func(error) { t.Error("unexpected persist error") })

	if _, err := rec.RecordMessage(userMsg("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordModelChange("anthropic", core.APIAnthropicMessages, "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordThinkingLevel(core.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordMessage(assistantMsg("yes")); err != nil {
		t.Fatal(err)
	}
	cp, err := rec.RecordCompaction("summary", "e1", "")
	if err != nil {
		t.Fatal(err)
	}
	if cp.CreatedAtLen != 2 {
		t.Errorf("CreatedAtLen = %d, want 2", cp.CreatedAtLen)
	}
	if err := rec.Sync(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	r := mustLoad(t, path).Fold()
	if r.Provider != "anthropic" || r.API != core.APIAnthropicMessages || r.ModelID != "claude" {
		t.Errorf("model not recovered: %q %q %q", r.Provider, r.API, r.ModelID)
	}
	if r.ThinkingLevel != core.ThinkingHigh {
		t.Errorf("thinking level = %q", r.ThinkingLevel)
	}
	if !r.HasCheckpoint || r.Checkpoint.Summary != "summary" {
		t.Errorf("checkpoint = %+v", r.Checkpoint)
	}
	if hist.Len() != 2 {
		t.Errorf("history holds %d messages, want 2", hist.Len())
	}
}

func TestRecorderRoutesPersistErrorsToTheHook(t *testing.T) {
	// REQ-SESS-08: when the SDK drives the store from loop events, the caller
	// of SetModel never sees Append's return value, so the hook is the only
	// path out.
	var got []error
	rec := NewRecorder(failingStore{}, nil, func(err error) { got = append(got, err) })
	if _, err := rec.RecordMessage(userMsg("hi")); err == nil {
		t.Fatal("RecordMessage must return its error")
	}
	if len(got) != 1 {
		t.Fatalf("hook called %d times, want 1", len(got))
	}
}

type failingStore struct{ core.SessionStore }

func (failingStore) Append(core.Entry) error { return errFailingStore }
func (failingStore) Head() core.EntryID      { return core.NullLeaf }

var errFailingStore = errStr("disk on fire")

type errStr string

func (e errStr) Error() string { return string(e) }

func mustBranch(t *testing.T, l *Loaded, id core.EntryID) []core.Entry {
	t.Helper()
	es, err := l.Branch(id)
	if err != nil {
		t.Fatalf("Branch(%q): %v", id, err)
	}
	return es
}
