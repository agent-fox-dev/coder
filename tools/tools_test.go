package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// ------------------------------------------------------------------ edit_file

// TestEditRejectionsAreEvaluatedInGlobalPhaseOrder pins ruling P-22.
//
// REQ-TOOL-04b's "fixed order" is ambiguous between per-edit and per-phase, and
// the difference is a real behavioural fork: with a non-unique edits[0] and a
// not-found edits[1], per-edit ordering reports "not found" while phase
// ordering reports "not unique". Two conforming implementations would show the
// model different problems to fix.
func TestEditRejectionsAreEvaluatedInGlobalPhaseOrder(t *testing.T) {
	const content = "alpha\nalpha\nbravo\n"
	_, _, err := ApplyEdits(content, []Edit{
		{OldString: "alpha", NewString: "x"}, // appears twice -> phase 3
		{OldString: "zulu", NewString: "y"},  // absent        -> phase 2
	})
	if err == nil {
		t.Fatal("want a rejection")
	}
	var ee *EditError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EditError, got %T", err)
	}
	if ee.Phase != "not_found" {
		t.Fatalf("phase = %q, want %q.\nPhase 2 (not found) is evaluated for the WHOLE "+
			"batch before phase 3 (not unique), so edits[1] is reported even though "+
			"edits[0] appears earlier (ruling P-22).", ee.Phase, "not_found")
	}
}

// TestNonUniqueIsARejectionNotAReplaceAll is the single most important tool
// invariant. Silent multi-site replacement is how an agent corrupts a file it
// was asked to touch once.
func TestNonUniqueIsARejectionNotAReplaceAll(t *testing.T) {
	const content = "x := 1\ny := 1\nz := 1\n"
	out, n, err := ApplyEdits(content, []Edit{{OldString: "1", NewString: "2"}})
	if err == nil {
		t.Fatalf("a non-unique old_string must be REJECTED, not replaced everywhere.\n"+
			"got %d edits applied and content:\n%s", n, out)
	}
	want := "Found 3 occurrences of the string to replace. The text must be unique. " +
		"Please provide more context to make it unique."
	if err.Error() != want {
		t.Fatalf("error text is model-visible contract.\n got: %q\nwant: %q", err.Error(), want)
	}
}

// TestAllEditsMatchAgainstTheOriginal pins REQ-TOOL-04a. If edits were applied
// sequentially, the second would match text the first created.
func TestAllEditsMatchAgainstTheOriginal(t *testing.T) {
	const content = "one two three"
	out, n, err := ApplyEdits(content, []Edit{
		{OldString: "one", NewString: "two"}, // creates a second "two"
		{OldString: "two", NewString: "ONE"}, // must match the ORIGINAL "two"
	})
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if n != 2 {
		t.Fatalf("edits applied = %d, want 2", n)
	}
	if out != "two ONE three" {
		t.Fatalf("content = %q, want %q.\nEdits must match the ORIGINAL content, never "+
			"the result of an earlier edit in the same call (REQ-TOOL-04a).", out, "two ONE three")
	}
}

func TestOverlappingEditsAreRejected(t *testing.T) {
	const content = "abcdef"
	_, _, err := ApplyEdits(content, []Edit{
		{OldString: "abcd", NewString: "X"},
		{OldString: "cdef", NewString: "Y"}, // overlaps at "cd"
	})
	if err == nil {
		t.Fatal("overlapping edits must be rejected: applying either alone silently drops the other")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("error should name the overlap: %q", err.Error())
	}
}

func TestNoOpEditIsRejected(t *testing.T) {
	_, _, err := ApplyEdits("hello", []Edit{{OldString: "hello", NewString: "hello"}})
	if err == nil {
		t.Fatal("an edit batch that changes nothing must be rejected: reporting success " +
			"lets the model believe it edited something it did not")
	}
}

func TestBOMAndCRLFArePreserved(t *testing.T) {
	orig := "\ufeffline one\r\nline two\r\n"
	norm, bom, ending := NormalizeForEdit(orig)
	if !bom || ending != CRLF {
		t.Fatalf("bom=%v ending=%v, want true/crlf", bom, ending)
	}
	if strings.Contains(norm, "\r") {
		t.Fatal("normalized content still contains CR; matching would fail against what the model saw")
	}
	out, _, err := ApplyEdits(norm, []Edit{{OldString: "line one", NewString: "line 1"}})
	if err != nil {
		t.Fatal(err)
	}
	restored := Restore(out, bom, ending)
	if !strings.HasPrefix(restored, "\ufeff") {
		t.Error("BOM was not restored")
	}
	if !strings.Contains(restored, "\r\n") {
		t.Error("CRLF was not restored")
	}
}

func TestRepairEditArgsHandlesTheThreeObservedShapes(t *testing.T) {
	want := `[{"new_string":"b","old_string":"a"}]`
	norm := func(m map[string]any) string {
		b, _ := json.Marshal(m["edits"])
		return string(b)
	}
	t.Run("edits as a JSON string", func(t *testing.T) {
		got := repairEditArgs(map[string]any{"edits": `[{"old_string":"a","new_string":"b"}]`})
		if norm(got) != want {
			t.Fatalf("got %s, want %s", norm(got), want)
		}
	})
	t.Run("bare object instead of an array", func(t *testing.T) {
		got := repairEditArgs(map[string]any{
			"edits": map[string]any{"old_string": "a", "new_string": "b"}})
		if norm(got) != want {
			t.Fatalf("got %s, want %s", norm(got), want)
		}
	})
	t.Run("legacy top-level keys", func(t *testing.T) {
		got := repairEditArgs(map[string]any{"old_string": "a", "new_string": "b"})
		if norm(got) != want {
			t.Fatalf("got %s, want %s", norm(got), want)
		}
		if _, still := got["old_string"]; still {
			t.Error("legacy top-level keys must be removed once folded into edits[]")
		}
	})
}

// ------------------------------------------------------------------ path guard

func TestNormalizationRunsBeforeCanonicalization(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A file:// URL must be unwrapped BEFORE the path is canonicalized. A
	// guard that canonicalizes first sees the literal string "file:/..." as a
	// relative path and joins it to the root, checking a string it will never
	// open.
	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ws.Resolve("file://" + target)
	if err != nil {
		t.Fatalf("file:// URL was not unwrapped before canonicalization: %v", err)
	}
	if got != target {
		t.Fatalf("resolved to %q, want %q", got, target)
	}
}

func TestPathContainmentRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	ws, _ := NewWorkspace(dir)
	for _, p := range []string{"../outside.txt", "a/../../outside.txt", "/etc/passwd"} {
		if _, err := ws.Resolve(p); !errors.Is(err, ErrPathNotAllowed) {
			t.Errorf("Resolve(%q) err = %v, want ErrPathNotAllowed", p, err)
		}
	}
}

// TestContainmentComparesSegmentsNotStringPrefixes: a prefix test says
// /work/space is inside /work/s.
func TestContainmentComparesSegmentsNotStringPrefixes(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "s")
	sibling := filepath.Join(base, "space")
	for _, d := range []string{inside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, _ := NewWorkspace(inside)
	if _, err := ws.Resolve(filepath.Join(sibling, "f.txt")); !errors.Is(err, ErrPathNotAllowed) {
		t.Fatalf("a sibling directory sharing a string prefix was accepted: err = %v", err)
	}
}

func TestNewFileUnderTheWorkspaceIsAllowed(t *testing.T) {
	dir := t.TempDir()
	ws, _ := NewWorkspace(dir)
	// The file does not exist yet. A guard that EvalSymlinks the whole path
	// fails here, which would make every file the agent creates unreachable.
	if _, err := ws.Resolve("new/nested/file.txt"); err != nil {
		t.Fatalf("a not-yet-created file inside the workspace must resolve: %v", err)
	}
}

func TestTildeUserIsRejectedNotExpanded(t *testing.T) {
	_, err := Normalize("~root/secrets")
	if !errors.Is(err, ErrPathMalformed) {
		t.Fatalf("err = %v, want ErrPathMalformed: ~user needs a cgo-backed lookup, "+
			"which is exactly what the dependency gate exists to catch (ruling P-46)", err)
	}
}

// TestExecuteIsNotPathContained. The test name is the documentation
// (REQ-SEC-01): path containment constrains the FILE tools and says nothing
// about `execute`, and pretending otherwise would be worse than admitting it.
func TestExecuteIsNotPathContained(t *testing.T) {
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("readable"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), "cat "+outside, ExecOptions{Dir: dir, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "readable") {
		t.Skip("shell could not read the file; environment-dependent")
	}
	// This is the documented state of affairs, not a bug: the boundary for
	// execute is the BeforeToolCall interceptor (REQ-SEC-03).
	t.Log("execute read a file outside the workspace, as documented: " +
		"the boundary for execute is the interceptor, not the path guard")
}

// ------------------------------------------------------------------ execute

func TestClassifyOutcomePrecedence(t *testing.T) {
	// abort > timeout > signal > exit. Each row sets EVERY lower-precedence
	// condition too, so a wrong precedence fails rather than coincidentally
	// agreeing.
	cases := []struct {
		name              string
		aborted, timedOut bool
		exitCode          int
		signaled          bool
		want              Outcome
	}{
		{"abort wins over everything", true, true, 137, true, OutcomeAbort},
		{"timeout wins over signal and exit", false, true, 137, true, OutcomeTimeout},
		{"signal wins over exit", false, false, -1, true, OutcomeSignal},
		{"plain non-zero exit", false, false, 1, false, OutcomeExit},
		{"success", false, false, 0, false, OutcomeOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyOutcome(c.aborted, c.timedOut, c.exitCode, c.signaled); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStdoutAndStderrInterleaveInWriteOrder(t *testing.T) {
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	// Alternating writes to the two streams. With separate captures the output
	// would be "1\n3\n2\n4\n" — the error appearing before the line that
	// caused it.
	res, err := Run(context.Background(),
		`echo 1; echo 2 >&2; echo 3; echo 4 >&2`,
		ExecOptions{MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(res.Output), "")
	if got != "1234" {
		t.Fatalf("output order = %q, want 1234: stdout and stderr must share ONE pipe "+
			"so they interleave in true write order (REQ-TOOL-17.4)", got)
	}
}

// TestGrandchildDoesNotSurviveTreeKill is REQ-TOOL-17.7's regression test.
//
// The shell starts a background grandchild that writes a marker AFTER the
// parent would be gone. Killing only the direct child orphans it and the
// marker appears — which in production is a dev server still holding a port
// long after the agent believes the command ended.
func TestGrandchildDoesNotSurviveTreeKill(t *testing.T) {
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-survived")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	// The grandchild sleeps past the kill, then writes.
	cmd := "( sleep 1; touch " + marker + " ) & sleep 5"
	_, err := Run(ctx, cmd, ExecOptions{Dir: dir, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}

	// Wait past when the grandchild would have written.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the grandchild survived the kill and wrote its marker.\n" +
			"Cancellation must kill the whole PROCESS GROUP, not just the direct " +
			"child (REQ-TOOL-17.2).")
	}
}

func TestExecuteDoesNotLeakAPIKeys(t *testing.T) {
	env := ReducedEnv([]string{
		"PATH=/usr/bin:/bin",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"OPENAI_API_KEY=sk-openai-secret",
		"HOME=/home/user",
	})
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"sk-ant-secret", "sk-openai-secret"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("credential %q reached the subprocess environment", leaked)
		}
	}
	// PATH must survive verbatim: dropping it is the obvious reading of
	// "reduced environment" and it breaks every command (ruling P-47).
	if !strings.Contains(joined, "PATH=/usr/bin:/bin") {
		t.Error("PATH was stripped; every command would fail")
	}
	if !strings.Contains(joined, "HOME=/home/user") {
		t.Error("HOME was stripped")
	}
}

// ------------------------------------------------------------------ accumulator

// TestAccumulatorIsBoundedRegardlessOfVolume pins REQ-TOOL-15: peak retention
// is ~2x cap however much is written. The naive implementation — buffer
// everything, truncate at the end — is quadratic and OOMs on exactly the
// runaway command the cap exists to contain.
func TestAccumulatorIsBoundedRegardlessOfVolume(t *testing.T) {
	const cap = 1024
	a := NewAccumulator(cap, TruncateTail)
	chunk := strings.Repeat("x", 4096)
	for i := 0; i < 2000; i++ { // 8 MB written
		if _, err := a.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(a.head) + len(a.tail); got > 2*cap {
		t.Fatalf("retained %d bytes for a cap of %d after writing 8MB", got, cap)
	}
	if a.Total() != 2000*4096 {
		t.Fatalf("Total() = %d, want %d", a.Total(), 2000*4096)
	}
	if !a.Truncated() {
		t.Fatal("Truncated() should be true")
	}
}

// TestExecuteTruncatesFromTheTail pins REQ-TOOL-09a: a failing build puts its
// error at the END, and head-truncation preserves the banner and discards the
// failure.
func TestExecuteTruncatesFromTheTail(t *testing.T) {
	a := NewAccumulator(64, TruncateTail)
	_, _ = a.Write([]byte(strings.Repeat("BANNER", 100)))
	_, _ = a.Write([]byte("FINAL_ERROR_LINE"))
	out := a.String()
	if !strings.Contains(out, "FINAL_ERROR_LINE") {
		t.Fatal("tail truncation dropped the end of the output, which is where the " +
			"error is (REQ-TOOL-09a)")
	}
	if !strings.Contains(out, "elided") {
		t.Fatal("a truncated result must carry a marker naming what was elided")
	}
}

func TestReadMarkerOffsetsAre1Based(t *testing.T) {
	// The PRD's own example marker says "Use offset=2001" after showing lines
	// 1-2000, which is only correct 1-based. A 0-based reading re-reads line
	// 2000 forever (ruling P-21).
	got := ReadOffsetMarker(1, 2000, 8000)
	if !strings.Contains(got, "offset=2001") {
		t.Fatalf("marker = %q, want it to name offset=2001", got)
	}
}

// ------------------------------------------------------------------ glob

func TestGlobDialect(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", true}, // basename fallback
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "c.go", true}, // ** matches zero segments
		{"a/**/c.go", "a/b/x/c.go", true},
		{"a/**/c.go", "a/c.go", true},
		{"{a,b}/*.go", "a/x.go", true},
		{"{a,b}/*.go", "c/x.go", false},
		{"{a,{b,c}}/x.go", "c/x.go", true}, // nested braces
		{"[!x]*.go", "main.go", true},
		{"src/*.go", "src/sub/x.go", false}, // * does not cross /
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestGitignoreIsHonoured(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "node_modules/\n*.log\n!keep.log\n")
	write("node_modules/dep/index.js", "")
	write("app.log", "")
	write("keep.log", "")
	write("main.go", "")

	ig := loadIgnore(dir)
	if !ig.match("node_modules", true) {
		t.Error("node_modules/ should be ignored")
	}
	if !ig.match("app.log", false) {
		t.Error("*.log should be ignored")
	}
	if ig.match("keep.log", false) {
		t.Error("!keep.log should re-include it: later patterns win")
	}
	if ig.match("main.go", false) {
		t.Error("main.go should not be ignored")
	}
	if !ig.match(".git", true) {
		t.Error(".git is always ignored")
	}
}

// ------------------------------------------------------------------ locks

// TestConcurrentEditsToOneFileSerialize pins REQ-LOOP-12. Models routinely
// emit two edits to the same file in one batch; without per-path serialization
// they interleave read-modify-write and one silently loses.
func TestConcurrentEditsToOneFileSerialize(t *testing.T) {
	locks := newPathLocks()
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(target, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two spellings of ONE file. Keying the lock on the resolved path is what
	// makes them share it.
	spellings := []string{target, filepath.Join(dir, ".", "shared.txt")}

	var wg sync.WaitGroup
	var inCritical, maxConcurrent int
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release := locks.acquire(lockKey(spellings[i%len(spellings)]))
			defer release()
			mu.Lock()
			inCritical++
			if inCritical > maxConcurrent {
				maxConcurrent = inCritical
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inCritical--
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Fatalf("max concurrent holders = %d, want 1: two spellings of one file must "+
			"share a lock (REQ-LOOP-12)", maxConcurrent)
	}
	// The table must not grow unbounded.
	if len(locks.m) != 0 {
		t.Fatalf("lock table retained %d entries; entries must be released at "+
			"refcount zero", len(locks.m))
	}
}

func TestDistinctFilesDoNotSerialize(t *testing.T) {
	locks := newPathLocks()
	dir := t.TempDir()
	release1 := locks.acquire(lockKey(filepath.Join(dir, "a.txt")))
	defer release1()

	done := make(chan struct{})
	go func() {
		release2 := locks.acquire(lockKey(filepath.Join(dir, "b.txt")))
		release2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("edits to DISTINCT files must stay concurrent; the lock is per-path, " +
			"not global")
	}
}

// ------------------------------------------------------------------ wiring

func TestAllToolsHaveSchemasAndExactlyOneHandler(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	all, err := All(Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no tools returned")
	}
	for _, tl := range all {
		if tl.InputSchema == nil {
			t.Errorf("%s has no schema", tl.Name)
		}
		if (tl.Handler == nil) == (tl.Execute == nil) {
			t.Errorf("%s must set exactly one of Handler and Execute", tl.Name)
		}
		if !tl.Builtin {
			t.Errorf("%s should be marked Builtin so NoTools=\"builtin\" can find it", tl.Name)
		}
		// The wire projection must not carry loop-only fields.
		w := tl.Wire()
		if w.Name != tl.Name || w.InputSchema != tl.InputSchema {
			t.Errorf("%s wire projection is wrong", tl.Name)
		}
	}
}

// TestDeletedToolsAreAbsent: REQ-TOOL-04's minimality is the requirement.
// Deletion, renaming, appending and stat are one shell word each.
func TestDeletedToolsAreAbsent(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	all, _ := All(Options{Workspace: ws})
	present := map[string]bool{}
	for _, tl := range all {
		present[tl.Name] = true
	}
	for _, gone := range []string{"delete_file", "move_file", "stat_file", "append_file", "fetch_url"} {
		if present[gone] {
			t.Errorf("%s is in the default set; REQ-TOOL-04 ships it as a shell word, "+
				"and REQ-TOOL-07 keeps fetch_url out of the default set", gone)
		}
	}
}

func TestWriteAndReadRoundTripThroughTheTools(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	all, _ := All(Options{Workspace: ws})
	byName := map[string]core.Tool{}
	for _, tl := range all {
		byName[tl.Name] = tl
	}
	ctx := context.Background()

	w := byName["write_file"].Execute(ctx, json.RawMessage(`{"path":"a/b.txt","content":"hello\nworld\n"}`))
	if !w.OK {
		t.Fatalf("write failed: %+v", w)
	}
	r := byName["read_file"].Execute(ctx, json.RawMessage(`{"path":"a/b.txt"}`))
	if !r.OK {
		t.Fatalf("read failed: %+v", r)
	}
	if got := r.Data["content"].(string); !strings.Contains(got, "hello") {
		t.Fatalf("content = %q", got)
	}

	e := byName["edit_file"].Execute(ctx,
		json.RawMessage(`{"path":"a/b.txt","edits":[{"old_string":"world","new_string":"there"}]}`))
	if !e.OK {
		t.Fatalf("edit failed: %+v", e)
	}
	r2 := byName["read_file"].Execute(ctx, json.RawMessage(`{"path":"a/b.txt"}`))
	if got := r2.Data["content"].(string); !strings.Contains(got, "there") {
		t.Fatalf("edit did not apply: %q", got)
	}
}

func TestFileToolsRefusePathsOutsideTheWorkspace(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	all, _ := All(Options{Workspace: ws})
	for _, tl := range all {
		if tl.Name == "execute" {
			continue // deliberately not contained
		}
		res := tl.Execute(context.Background(), json.RawMessage(`{"path":"../../etc/passwd","content":"x","pattern":"*","edits":[]}`))
		if res.OK {
			t.Errorf("%s accepted a path outside the workspace", tl.Name)
		}
	}
}
