package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

// TestDetachedDescendantOutputIsDrainedOnAReArmingTimer is REQ-TOOL-17.5.
//
// It is the inverse of the test above and the two are the whole contract:
// there, a descendant must not SURVIVE a kill; here, a descendant that
// legitimately outlives its parent must still have every byte it writes
// drained. The grandchild writes for longer than any fixed post-exit deadline
// (the old code closed the pipe 2s after the child exited), so a drain that
// does not re-arm on each read loses the tail of the log — which in production
// is the half of a background job's output that says what went wrong.
func TestDetachedDescendantOutputIsDrainedOnAReArmingTimer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fractional sleep in a background job is not portable to the Windows shell")
	}
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	// The parent shell exits IMMEDIATELY — it does not wait for the job — so
	// every line after the first arrives on a pipe whose only remaining writer
	// is a process the tool never started.
	const lines = 30
	cmd := fmt.Sprintf(`( i=0; while [ $i -lt %d ]; do echo "late-$i"; sleep 0.1; i=$((i+1)); done ) &`, lines)

	type outcome struct {
		res ExecResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := Run(context.Background(), cmd, ExecOptions{
			MaxBytes: 1 << 16,
			// Shortened only to keep the test quick. The property under test
			// is that output ARRIVING inside the window re-arms it; the
			// grandchild writes every 100ms for ~3s, so a non-re-arming drain
			// of any length under 3s truncates it.
			DrainIdle: 500 * time.Millisecond,
		})
		ch <- outcome{res, err}
	}()

	var got outcome
	select {
	case got = <-ch:
	case <-time.After(30 * time.Second):
		// A re-arming timer must still terminate: DrainCeiling is the bound.
		t.Fatal("Run never returned; the post-exit drain must be bounded")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	for _, want := range []string{"late-0", fmt.Sprintf("late-%d", lines-1)} {
		if !strings.Contains(got.res.Output, want) {
			t.Fatalf("output is missing %q:\n%s\n\nAfter the child exits, output must be "+
				"drained on a RE-ARMING idle timer, not a fixed post-exit deadline "+
				"(REQ-TOOL-17.5).", want, got.res.Output)
		}
	}
}

func TestExecuteDoesNotLeakAPIKeys(t *testing.T) {
	env := ReducedEnv([]string{
		"PATH=/usr/bin:/bin",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"OPENAI_API_KEY=sk-openai-secret",
		"anthropic_api_key=sk-lower-secret", // the prefix rule is case-insensitive
		"HOME=/home/user",
	})
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"sk-ant-secret", "sk-openai-secret", "sk-lower-secret"} {
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

	ig := newIgnoreEngine(dir, NoGlobalExcludes())
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

// TestConcurrentEditsToOneFileSerialize pins REQ-LOOP-12 THROUGH THE TOOL.
//
// Models routinely emit two edits to the same file in one batch; without
// per-path serialization they interleave read-modify-write and one silently
// loses. The two spellings here are ones the tool actually receives — a
// symlink and a relative path — not two strings filepath.Join cleans to the
// same thing, which the previous version of this test used and which would
// have passed with the lock keyed on the raw string.
//
// Serialization is asserted directly: while the real file's lock is held,
// an edit through either spelling must BLOCK, and an edit to a different file
// must not.
func TestConcurrentEditsToOneFileSerialize(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("alpha\nbravo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("charlie\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs := newFileTools(Options{Workspace: ws}.withDefaults())
	edit := fs.editFile()

	// Hold the lock the way write_file/edit_file compute it.
	key, err := lockKey(real)
	if err != nil {
		t.Fatal(err)
	}
	release := fs.locks.acquire(key)

	run := func(path, old, new string) <-chan core.ToolResult {
		ch := make(chan core.ToolResult, 1)
		go func() {
			ch <- edit.Execute(context.Background(), json.RawMessage(
				`{"path":"`+path+`","edits":[{"old_string":"`+old+`","new_string":"`+new+`"}]}`))
		}()
		return ch
	}
	viaLink := run("link.txt", "alpha", "ALPHA")
	viaRel := run("./sub/../real.txt", "bravo", "BRAVO")
	viaOther := run("other.txt", "charlie", "CHARLIE")

	select {
	case res := <-viaOther:
		if !res.OK {
			t.Fatalf("edit to a distinct file failed: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an edit to a DISTINCT file must stay concurrent; the lock is per-path, " +
			"not global (REQ-LOOP-12)")
	}
	select {
	case res := <-viaLink:
		t.Fatalf("an edit through a SYMLINK spelling ran while the file's lock was held: %+v", res)
	case res := <-viaRel:
		t.Fatalf("an edit through a RELATIVE spelling ran while the file's lock was held: %+v", res)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	for name, ch := range map[string]<-chan core.ToolResult{"symlink": viaLink, "relative": viaRel} {
		select {
		case res := <-ch:
			if !res.OK {
				t.Fatalf("%s edit failed after release: %+v", name, res)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s edit never ran after the lock was released", name)
		}
	}
	got, _ := os.ReadFile(real)
	if string(got) != "ALPHA\nBRAVO\n" {
		t.Fatalf("a lost update: file = %q, want both edits applied", got)
	}
	// The table must not grow unbounded.
	if len(fs.locks.m) != 0 {
		t.Fatalf("lock table retained %d entries; entries must be released at "+
			"refcount zero", len(fs.locks.m))
	}
}

// TestLockKeyPropagatesResolutionErrors pins REQ-LOOP-12's "any other
// resolution error propagates rather than being swallowed". Only not-exist
// may fall back to the absolute path.
func TestLockKeyPropagatesResolutionErrors(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if k, err := lockKey(filepath.Join(dir, "not-yet.txt")); err != nil || k != filepath.Join(dir, "not-yet.txt") {
		t.Fatalf("a not-yet-created file must key on its absolute path: %q, %v", k, err)
	}
	// A symlink loop is a resolution error that is NOT not-exist.
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := lockKey(loop); err == nil {
		t.Fatal("a symlink loop must propagate as an error, not fall back to the raw " +
			"path: a key from an unresolvable path is one two spellings may not share")
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

// ------------------------------------------------------------------ regressions

// toolByName builds the real default set over dir and returns one tool, with
// the global ignore layer pinned empty (NFR-TEST-04).
func toolByName(t *testing.T, dir, name string, opts ...Options) core.Tool {
	t.Helper()
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Workspace: ws, Ignore: NoGlobalExcludes()}
	if len(opts) > 0 {
		o = opts[0]
		o.Workspace = ws
	}
	all, err := All(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("%s is not in the default tool set", name)
	return core.Tool{}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

// TestDanglingSymlinkCannotEscapeTheWorkspace is REQ-SEC-01 / NFR-SEC-02.
//
// EvalSymlinks fails on a dangling link exactly as it fails on a missing
// file, so a guard that resolves the parent and rejoins the base sees
// `ws/link` — inside — while os.WriteFile follows the link and creates the
// file OUTSIDE. The link's target is where containment must be decided.
func TestDanglingSymlinkCannotEscapeTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	ws, _ := NewWorkspace(dir)
	write := toolByName(t, dir, "write_file")

	t.Run("dangling link to outside is rejected", func(t *testing.T) {
		mustSymlink(t, filepath.Join(outside, "newfile"), filepath.Join(dir, "escape"))
		if _, err := ws.Resolve("escape"); !errors.Is(err, ErrPathNotAllowed) {
			t.Fatalf("Resolve(escape) err = %v, want ErrPathNotAllowed", err)
		}
		res := write.Execute(context.Background(), json.RawMessage(`{"path":"escape","content":"pwned"}`))
		if res.OK || res.Error != "path_not_allowed" {
			t.Fatalf("write through a dangling link to outside: %+v", res)
		}
		if _, err := os.Stat(filepath.Join(outside, "newfile")); err == nil {
			t.Fatal("the file was created OUTSIDE the workspace")
		}
	})
	t.Run("dangling link inside is allowed", func(t *testing.T) {
		mustSymlink(t, "inner/newfile", filepath.Join(dir, "stay"))
		if err := os.MkdirAll(filepath.Join(dir, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := ws.Resolve("stay")
		if err != nil {
			t.Fatalf("a dangling link whose target is inside must resolve: %v", err)
		}
		if got != filepath.Join(ws.Root, "inner", "newfile") {
			t.Fatalf("resolved to %q, want the link TARGET", got)
		}
		res := write.Execute(context.Background(), json.RawMessage(`{"path":"stay","content":"ok"}`))
		if !res.OK {
			t.Fatalf("write through an inside dangling link failed: %+v", res)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "inner", "newfile")); string(b) != "ok" {
			t.Fatalf("content landed elsewhere: %q", b)
		}
	})
	t.Run("chained dangling links are followed to the end", func(t *testing.T) {
		mustSymlink(t, filepath.Join(outside, "deep"), filepath.Join(dir, "hop2"))
		mustSymlink(t, "hop2", filepath.Join(dir, "hop1"))
		if _, err := ws.Resolve("hop1"); !errors.Is(err, ErrPathNotAllowed) {
			t.Fatalf("Resolve(hop1) err = %v, want ErrPathNotAllowed: hop1 -> hop2 -> outside", err)
		}
	})
	t.Run("a link loop is malformed, not accepted", func(t *testing.T) {
		mustSymlink(t, "cycle", filepath.Join(dir, "cycle"))
		if _, err := ws.Resolve("cycle"); err == nil {
			t.Fatal("a symlink cycle must be rejected")
		}
	})
}

// TestCheckWriteTargetRefusesALinkPlantedAfterResolve pins the second half
// of the fix: the final path is Lstat'ed immediately before the open.
func TestCheckWriteTargetRefusesALinkPlantedAfterResolve(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	ws, _ := NewWorkspace(dir)
	abs := filepath.Join(ws.Root, "target.txt")
	if err := ws.CheckWriteTarget(abs); err != nil {
		t.Fatalf("an absent target is fine: %v", err)
	}
	mustSymlink(t, filepath.Join(outside, "x"), abs) // planted between Resolve and open
	if err := ws.CheckWriteTarget(abs); !errors.Is(err, ErrPathNotAllowed) {
		t.Fatalf("err = %v, want ErrPathNotAllowed for a link whose target is outside", err)
	}
}

// TestCapMarkersNameACallThatWorks is REQ-TOOL-09b: a marker at the cap must
// not name limit=2×cap, which the tool clamps straight back.
func TestCapMarkersNameACallThatWorks(t *testing.T) {
	if m := FindMarker(FindResultCap); strings.Contains(m, "limit=") {
		t.Fatalf("at the cap the marker must not name a larger limit: %q", m)
	}
	if m := FindMarker(300); !strings.Contains(m, "limit=600") {
		t.Fatalf("below the cap the marker names the doubled limit: %q", m)
	}
	if m := FindMarker(700); !strings.Contains(m, "limit=1000") {
		t.Fatalf("the doubled limit is clamped to the cap: %q", m)
	}
	if m := ListMarker(ListEntryCap); strings.Contains(m, "limit=") {
		t.Fatalf("list_files at the cap: %q", m)
	}
	if m := SearchMarker(50); !strings.Contains(m, "max_matches=100") || strings.Contains(m, "limit=") {
		t.Fatalf("search_files' parameter is max_matches, capped at 100: %q", m)
	}
	if m := SearchMarker(SearchMatchCap); strings.Contains(m, "max_matches=") {
		t.Fatalf("search_files at the cap: %q", m)
	}

	// Through the tool: 501 entries. At the DEFAULT limit (200) the marker
	// names limit=400, a call that works; at the cap it says so.
	dir := t.TempDir()
	for i := 0; i < ListEntryCap+1; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := toolByName(t, dir, "list_files")
	res := list.Execute(context.Background(), json.RawMessage(`{}`))
	note, _ := res.Data["note"].(string)
	if entries := res.Data["entries"].([]string); len(entries) != ListEntryDefault {
		t.Fatalf("the default list limit is %d, got %d entries", ListEntryDefault, len(entries))
	}
	if !strings.Contains(note, "limit=400") {
		t.Fatalf("at the default the marker names the doubled limit: %q", note)
	}
	res = list.Execute(context.Background(), json.RawMessage(`{"limit":500}`))
	note, _ = res.Data["note"].(string)
	if note == "" || strings.Contains(note, "limit=") {
		t.Fatalf("list_files at the cap must say so, not name limit=1000: %+v", res.Data)
	}
	// REQ-TOOL-09b (item: list_files): the marker is NOT an entry.
	entries := res.Data["entries"].([]string)
	if len(entries) != ListEntryCap || strings.HasPrefix(entries[len(entries)-1], "[") {
		t.Fatalf("the marker must live under `note`, not be appended to entries as a "+
			"filename: last entry %q, %d entries", entries[len(entries)-1], len(entries))
	}
}

// TestReadFileByteCutIsOnWholeLinesWithARealOffset is REQ-TOOL-09b/09c.
func TestReadFileByteCutIsOnWholeLinesWithARealOffset(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("x", 1023) // 1 KB with the newline
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "%03d%s\n", i, line[3:])
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res := toolByName(t, dir, "read_file").Execute(context.Background(), json.RawMessage(`{"path":"big.txt"}`))
	if !res.OK || res.Metadata == nil || res.Metadata.TruncatedBy != "bytes" {
		t.Fatalf("100 KB must truncate by bytes: %+v", res.Metadata)
	}
	content := res.Data["content"].(string)
	lines := strings.Split(content, "\n")
	marker := lines[len(lines)-1]
	shown := lines[:len(lines)-1]
	for i, l := range shown {
		if len(l) != 1023 || l[:3] != fmt.Sprintf("%03d", i) {
			t.Fatalf("line %d was cut mid-line: %d bytes, prefix %q", i+1, len(l), l[:3])
		}
	}
	want := fmt.Sprintf("[Showing lines 1-%d of 100. Use offset=%d to continue.]", len(shown), len(shown)+1)
	if marker != want {
		t.Fatalf("marker = %q\nwant     %q: the byte cut must emit ONE marker with the real offset", marker, want)
	}
	if len(content) > DefaultByteLimit+len(marker)+1 {
		t.Fatalf("content is %d bytes, over the 50 KB budget", len(content))
	}
	if res.Metadata.TotalLines != 100 {
		t.Fatalf("total_lines = %d, want 100", res.Metadata.TotalLines)
	}
}

// TestReadFileLongLineGetsTheSedMarker is REQ-TOOL-09c: LongLineMarker had no
// callers, so a single oversized line was silently cut instead.
func TestReadFileLongLineGetsTheSedMarker(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("é", 60*1024) // 120 KB, multi-byte
	body := "first\n" + huge + "\nlast\n"
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res := toolByName(t, dir, "read_file").Execute(context.Background(), json.RawMessage(`{"path":"long.txt"}`))
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	content := res.Data["content"].(string)
	wantMarker := LongLineMarker(2, int64(len(huge)), DefaultByteLimit, "long.txt")
	if !strings.Contains(content, wantMarker) {
		t.Fatalf("content lacks the REQ-TOOL-09c marker %q:\n%.200s", wantMarker, content)
	}
	if !strings.Contains(wantMarker, "sed -n '2p' 'long.txt' | head -c 51200") {
		t.Fatalf("the marker must name the sed workaround, with the path shell-quoted: %q", wantMarker)
	}
	if strings.Contains(content, "éé") {
		t.Fatal("the oversized line's bytes must not be shown at all")
	}
	if !strings.HasPrefix(content, "first\n") || !strings.HasSuffix(content, "\nlast") {
		t.Fatalf("the read must continue past the long line: %.60s ... %.60s",
			content, content[len(content)-30:])
	}
	if res.Metadata.TruncatedBy != "bytes" || res.Metadata.TotalLines != 3 {
		t.Fatalf("metadata = %+v", res.Metadata)
	}
}

// TestReadFileCountsLinesLikeAnEditor: "a\nb\n" is two lines, not three.
// strings.Split's trailing empty element inflated every total and made
// offset=N for an N-line file a rejection.
func TestReadFileCountsLinesLikeAnEditor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "two.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, dir, "read_file")
	res := read.Execute(context.Background(), json.RawMessage(`{"path":"two.txt"}`))
	if !res.OK || res.Metadata.TotalLines != 2 || res.Metadata.TotalBytes != 4 {
		t.Fatalf("metadata = %+v, want 2 lines / 4 bytes", res.Metadata)
	}
	if got := res.Data["content"]; got != "a\nb" {
		t.Fatalf("content = %q", got)
	}
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"two.txt","offset":2}`))
	if !res.OK || res.Data["content"] != "b" || res.Metadata.Truncated {
		t.Fatalf("offset=2 of a 2-line file is the last line with no marker: %+v %+v", res.Data, res.Metadata)
	}
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"two.txt","offset":3}`))
	if res.OK || res.Error != "offset_past_end" {
		t.Fatalf("offset=3 of a 2-line file: %+v", res)
	}
	// limit landing exactly on the last line is not a truncation.
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"two.txt","limit":2}`))
	if !res.OK || res.Metadata.Truncated {
		t.Fatalf("limit=2 of a 2-line file is complete: %+v", res.Metadata)
	}
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"two.txt","limit":1}`))
	if !res.OK || res.Metadata.TruncatedBy != "lines" || res.Data["content"] != "a\n"+ReadOffsetMarker(1, 1, 2) {
		t.Fatalf("limit=1: %+v %+v", res.Data, res.Metadata)
	}
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"empty.txt"}`))
	if !res.OK || res.Data["content"] != "" {
		t.Fatalf("an empty file reads as empty content, not offset_past_end: %+v", res)
	}
}

// TestReadFileStreamsInsteadOfLoadingTheFile: a read of the first lines of a
// large file must not allocate the file.
func TestReadFileStreamsInsteadOfLoadingTheFile(t *testing.T) {
	dir := t.TempDir()
	const size = 16 << 20
	f, err := os.Create(filepath.Join(dir, "large.log"))
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("0123456789abcdef0123456789abcde\n", 32)) // 1 KB
	for written := 0; written < size; written += len(chunk) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	read := toolByName(t, dir, "read_file")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res := read.Execute(context.Background(), json.RawMessage(`{"path":"large.log","limit":5}`))
	runtime.ReadMemStats(&after)
	if !res.OK || res.Metadata.TotalLines != size/32 {
		t.Fatalf("%+v %+v", res.Error, res.Metadata)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/4 {
		t.Fatalf("reading 5 lines of a 16 MB file allocated %d bytes; the read must "+
			"stream through a bounded buffer", alloc)
	}
}

// TestCapLineCutsOnRuneBoundaries: the PRD says 500 CHARS.
func TestCapLineCutsOnRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 600)
	got := capLine(s)
	if !utf8.ValidString(got) {
		t.Fatal("capLine produced invalid UTF-8: it sliced a rune in half")
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n != SearchLineChars {
		t.Fatalf("kept %d runes, want %d", n, SearchLineChars)
	}
	if short := strings.Repeat("é", 300); capLine(short) != short {
		t.Fatal("300 runes of 2 bytes each is under the 500-char cap")
	}
}

// TestOverlappingOccurrencesAreNotUnique is REQ-TOOL-04c: strings.Count says
// "aa" occurs once in "aaa"; there are two sites the model could mean.
func TestOverlappingOccurrencesAreNotUnique(t *testing.T) {
	_, _, err := ApplyEdits("aaa", []Edit{{OldString: "aa", NewString: "b"}})
	if err == nil {
		t.Fatal("\"aa\" in \"aaa\" is two overlapping sites and must be rejected as not unique")
	}
	var ee *EditError
	if !errors.As(err, &ee) || ee.Phase != "not_unique" || !strings.Contains(ee.Text, "Found 2 occurrences") {
		t.Fatalf("want not_unique with 2 occurrences, got %v", err)
	}
}

// TestFindFilesFileType is REQ-TOOL-04's table: file_type string (default
// "file"), with "dir"/"directory" and "any".
func TestFindFilesFileType(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"src/a.go", "src/pkg/b.go", "docs/c.md"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	find := toolByName(t, dir, "find_files")
	run := func(args string) []string {
		res := find.Execute(context.Background(), json.RawMessage(args))
		if !res.OK {
			t.Fatalf("%s: %+v", args, res)
		}
		files := res.Data["files"].([]string)
		for i := range files {
			files[i] = filepath.ToSlash(files[i])
		}
		return files
	}
	if got := run(`{"pattern":"**/*"}`); strings.Join(got, " ") != "docs/c.md src/a.go src/pkg/b.go" {
		t.Fatalf("default is files only: %v", got)
	}
	if got := run(`{"pattern":"**/*","file_type":"dir"}`); strings.Join(got, " ") != "docs/ src/ src/pkg/" {
		t.Fatalf("file_type=dir: %v", got)
	}
	if got := run(`{"pattern":"src/*","file_type":"directory"}`); strings.Join(got, " ") != "src/pkg/" {
		t.Fatalf("file_type=directory is a spelling of dir: %v", got)
	}
	if got := run(`{"pattern":"src/*","file_type":"any"}`); strings.Join(got, " ") != "src/a.go src/pkg/" {
		t.Fatalf("file_type=any: %v", got)
	}
	if res := find.Execute(context.Background(), json.RawMessage(`{"pattern":"*","file_type":"symlink"}`)); res.Error != "invalid_arguments" {
		t.Fatalf("an unknown file_type is invalid_arguments: %+v", res)
	}
}

// TestExecuteSpillsByDefault is REQ-TOOL-09d: the requirement says output over
// the cap IS streamed to a temp file, so the default cannot be "off".
func TestExecuteSpillsByDefault(t *testing.T) {
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	dir := t.TempDir()
	cmd := json.RawMessage(`{"command":"head -c 70000 /dev/zero | tr '\\0' x"}`)

	res := toolByName(t, dir, "execute").Execute(context.Background(), cmd)
	if !res.OK || !res.Metadata.Truncated {
		t.Fatalf("70 KB must truncate: %+v %+v", res.Error, res.Metadata)
	}
	spill := res.Metadata.SpillPath
	if spill == "" {
		t.Fatal("no spill path: spilling must be ON by default (REQ-TOOL-09d)")
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(spill)) })
	if rel, err := filepath.Rel(os.TempDir(), spill); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("spill %q is not under os.TempDir() %q", spill, os.TempDir())
	}
	if st, err := os.Stat(spill); err != nil || st.Size() != 70000 {
		t.Fatalf("spill file must hold the COMPLETE output: %v %v", st, err)
	}
	if !strings.Contains(res.Data["output"].(string), spill) {
		t.Fatal("the marker must name the spill path")
	}

	ws, _ := NewWorkspace(dir)
	off := toolByName(t, dir, "execute", Options{Workspace: ws, DisableSpill: true}).Execute(context.Background(), cmd)
	if off.Metadata.SpillPath != "" {
		t.Fatalf("DisableSpill must leave no file: %q", off.Metadata.SpillPath)
	}
}

// TestReadFileForwardsWebP is REQ-TOOL-14: providers accept WebP; this build
// cannot decode it, which is a reason to forward it untouched, not to refuse.
func TestReadFileForwardsWebP(t *testing.T) {
	dir := t.TempDir()
	webp := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 24)...)
	if err := os.WriteFile(filepath.Join(dir, "pic.webp"), webp, 0o644); err != nil {
		t.Fatal(err)
	}
	res := toolByName(t, dir, "read_file").Execute(context.Background(), json.RawMessage(`{"path":"pic.webp"}`))
	if !res.OK {
		t.Fatalf("WebP must be forwarded, not refused: %+v", res)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].(core.ImageBlock).MimeType != "image/webp" {
		t.Fatalf("want one image/webp block: %+v", res.Blocks)
	}
	if note, _ := res.Data["note"].(string); !strings.Contains(note, "dimensions unknown") {
		t.Fatalf("the note must not invent dimensions: %q", note)
	}
	if _, has := res.Data["width"]; has {
		t.Fatal("width is unknown and must not be reported")
	}
}

// TestSignalKilledProcessReportsExitCode128PlusSignum is NFR-COMPAT-06's
// unix exit-code semantics.
func TestSignalKilledProcessReportsExitCode128PlusSignum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows wait status carries no signal")
	}
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	res, err := Run(context.Background(), "kill -TERM $$", ExecOptions{MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeSignal {
		t.Fatalf("outcome = %q, want signal", res.Outcome)
	}
	if res.ExitCode != 128+15 {
		t.Fatalf("exit code = %d, want 143 (128+SIGTERM)", res.ExitCode)
	}
}

// TestExecuteRejectsANonPositiveTimeout is REQ-TOOL-06: "when supplied it must
// be positive". A negative or zero value used to mean "no timeout".
func TestExecuteRejectsANonPositiveTimeout(t *testing.T) {
	dir := t.TempDir()
	exe := toolByName(t, dir, "execute")
	for _, in := range []string{`{"command":"true","timeout_s":0}`, `{"command":"true","timeout_s":-5}`} {
		res := exe.Execute(context.Background(), json.RawMessage(in))
		if res.Error != "invalid_arguments" {
			t.Fatalf("%s: want invalid_arguments, got %+v", in, res)
		}
	}
	if _, _, err := ResolveShell(); err == nil {
		if res := exe.Execute(context.Background(), json.RawMessage(`{"command":"true"}`)); !res.OK {
			t.Fatalf("absent timeout_s is fine: %+v", res)
		}
	}
}

// TestReducedEnvStripsGenericCredentialSuffixes is REQ-SEC-08: a provider
// prefix list only ever names the providers someone thought of.
func TestReducedEnvStripsGenericCredentialSuffixes(t *testing.T) {
	env := ReducedEnv([]string{
		"PATH=/usr/bin", "HOME=/h", "LANG=C.UTF-8", "TMPDIR=/tmp", "TERM=xterm",
		"GITHUB_TOKEN=ghp_1", "NPM_TOKEN=npm_1", "DB_PASSWORD=pw", "STRIPE_SECRET=sk",
		"SOME_API_KEY=k", "GOOGLE_APPLICATION_CREDENTIALS=/c.json", "vault_token=lower",
		// The suffixes a provider list never reaches.
		"GITHUB_PAT=pat1", "DOCKER_AUTH=auth1", "DB_PASS=pass1", "DB_PWD=pwd1",
		"SSH_PRIVATE_KEY=priv1", "SIGNING_KEY=key1",
		// Bare names with no suffix at all.
		"TOKEN=bare1", "API_KEY=bare2", "SECRET=bare3", "PASSWORD=bare4",
		// Provider prefixes in either case; the non-secret settings survive.
		"aws_secret_access_key=aws1", "AWS_ACCESS_KEY_ID=aws2", "google_api_key=g1",
		"AWS_PROFILE=dev", "AWS_REGION=eu-west-1", "AWS_DEFAULT_REGION=us-east-1",
		"GOOGLE_CLOUD_PROJECT=proj",
		"EDITOR=vim",
	})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, leaked := range []string{"ghp_1", "npm_1", "=pw\n", "=sk\n", "=k\n", "c.json", "lower",
		"pat1", "auth1", "pass1", "pwd1", "priv1", "key1",
		"bare1", "bare2", "bare3", "bare4", "aws1", "aws2", "=g1"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("%q reached the subprocess environment", leaked)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/h", "LANG=C.UTF-8", "TMPDIR=/tmp", "TERM=xterm", "EDITOR=vim",
		"AWS_PROFILE=dev", "AWS_REGION=eu-west-1", "AWS_DEFAULT_REGION=us-east-1", "GOOGLE_CLOUD_PROJECT=proj"} {
		if !strings.Contains(joined, "\n"+kept+"\n") {
			t.Errorf("%q was stripped", kept)
		}
	}
}

// TestExecuteBuiltOutsideAllReducesTheEnvironment: the nil-Env default must
// hold for a tool built by hand, not only through All().
func TestExecuteBuiltOutsideAllReducesTheEnvironment(t *testing.T) {
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leak")
	ws, _ := NewWorkspace(t.TempDir())
	for name, tl := range map[string]core.Tool{
		"execute":     executeTool(Options{Workspace: ws}),
		"run_command": runCommandTool(Options{Workspace: ws}),
	} {
		in := `{"command":"env"}`
		if name == "run_command" {
			in = `{"argv":["env"]}`
		}
		res := tl.Execute(context.Background(), json.RawMessage(in))
		if !res.OK {
			t.Fatalf("%s: %+v", name, res)
		}
		if strings.Contains(res.Data["output"].(string), "ghp_should_not_leak") {
			t.Fatalf("%s built outside All() leaked GITHUB_TOKEN to the child (REQ-SEC-08)", name)
		}
	}
}

// TestRunCommandResolvesARelativeProgramAgainstTheWorkspace: `./script.sh`
// means the directory the command runs in, not the process working directory.
func TestRunCommandResolvesARelativeProgramAgainstTheWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte("#!/bin/sh\necho from-workspace\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if wd, _ := os.Getwd(); wd == dir {
		t.Fatal("the test needs the process cwd to differ from the workspace")
	}
	res := toolByName(t, dir, "run_command").Execute(context.Background(), json.RawMessage(`{"argv":["./script.sh"]}`))
	if !res.OK || !strings.Contains(res.Data["output"].(string), "from-workspace") {
		t.Fatalf("./script.sh must resolve against the workspace, not the process cwd: %+v", res)
	}
}

// TestIgnoreOptionsAreThreadedAndGitConfigIsCached is NFR-TEST-04: the tool
// reads the injected global layer, and consults `git config` once per
// workspace rather than once per call.
func TestIgnoreOptionsAreThreadedAndGitConfigIsCached(t *testing.T) {
	dir := t.TempDir()
	excludes := filepath.Join(t.TempDir(), "global-ignore")
	if err := os.WriteFile(excludes, []byte("*.secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.go", "b.secret"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	ws, _ := NewWorkspace(dir)
	opts := Options{Workspace: ws, Ignore: IgnoreOptions{
		Getenv:    func(string) string { return "" },
		Home:      func() (string, error) { return "", os.ErrNotExist },
		GitConfig: func() string { calls.Add(1); return excludes },
	}}
	all, err := All(opts)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]core.Tool{}
	for _, tl := range all {
		byName[tl.Name] = tl
	}
	for i := 0; i < 3; i++ {
		res := byName["find_files"].Execute(context.Background(), json.RawMessage(`{"pattern":"*"}`))
		if got := strings.Join(res.Data["files"].([]string), " "); got != "a.go" {
			t.Fatalf("the injected global excludes must apply: %q", got)
		}
		res = byName["search_files"].Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
		for _, m := range res.Data["matches"].([]SearchMatch) {
			if m.File == "b.secret" {
				t.Fatal("search_files must honour the injected global layer too")
			}
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("git config was consulted %d times across six calls; want once per workspace", n)
	}
}

// ------------------------------------------------------------------ review fixes

// TestReadFileRefusesAnOversizedImageBeforeLoadingIt. The normalizer bounds
// what it DECODES; nothing bounded what read_file handed it. The file is
// sparse — a PNG signature and thirty-two megabytes of holes — so the test
// costs nothing to set up and the assertion is on allocation, which is what
// a size check placed after io.ReadAll would fail.
func TestReadFileRefusesAnOversizedImageBeforeLoadingIt(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "huge.png"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\x89PNG\r\n\x1a\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(ImageFileMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	read := toolByName(t, dir, "read_file")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res := read.Execute(context.Background(), json.RawMessage(`{"path":"huge.png"}`))
	runtime.ReadMemStats(&after)
	if res.OK || res.Error != "image_too_large" {
		t.Fatalf("want image_too_large, got %+v", res)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Fatalf("refusing a %d byte image allocated %d bytes: the size must be checked "+
			"BEFORE the file is read", ImageFileMaxBytes+1, alloc)
	}
	if res.Text != "" {
		t.Fatal("an error result keeps the JSON envelope; Text must be empty")
	}
}

// TestFileToolsRefuseWhatIsNotARegularFile. A directory is a list_files
// question. A FIFO is worse: a read blocks until something writes, which is
// never, and the whole batch waits behind it. The FIFO case runs with a
// deadline so a regression is a failure rather than a hang.
func TestFileToolsRefuseWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, dir, "read_file")
	edit := toolByName(t, dir, "edit_file")

	for name, in := range map[string]json.RawMessage{
		"read_file": json.RawMessage(`{"path":"sub"}`),
		"edit_file": json.RawMessage(`{"path":"sub","edits":[{"old_string":"a","new_string":"b"}]}`),
	} {
		tl := read
		if name == "edit_file" {
			tl = edit
		}
		res := tl.Execute(context.Background(), in)
		if res.OK || res.Error != "not_a_file" || !strings.Contains(res.Detail, "directory") {
			t.Fatalf("%s on a directory: want not_a_file naming a directory, got %+v", name, res)
		}
	}

	if runtime.GOOS == "windows" {
		return
	}
	fifo := filepath.Join(dir, "pipe")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v %s", err, out)
	}
	for name, tl := range map[string]core.Tool{"read_file": read, "edit_file": edit} {
		in := json.RawMessage(`{"path":"pipe","edits":[{"old_string":"a","new_string":"b"}]}`)
		done := make(chan core.ToolResult, 1)
		go func() { done <- tl.Execute(context.Background(), in) }()
		select {
		case res := <-done:
			if res.OK || res.Error != "not_a_file" {
				t.Fatalf("%s on a FIFO: want not_a_file, got %+v", name, res)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s blocked on a FIFO: the file must be Stat'ed and refused before it is opened", name)
		}
	}
}

// TestEditFileRefusesAFileOverItsSizeCeiling, with a message that names the
// tool that can do it. Sparse again: the size is what is checked.
func TestEditFileRefusesAFileOverItsSizeCeiling(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("needle\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(EditFileMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	res := toolByName(t, dir, "edit_file").Execute(context.Background(),
		json.RawMessage(`{"path":"big.log","edits":[{"old_string":"needle","new_string":"x"}]}`))
	if res.OK || res.Error != "file_too_large" || !strings.Contains(res.Detail, "execute") {
		t.Fatalf("want file_too_large pointing at execute, got %+v", res)
	}
}

// TestCheckWriteTargetReResolvesTheWholePath. The leaf is not where the swap
// happens: replace a PARENT directory with a link to outside and the leaf
// looks like an ordinary (or absent) file while the write lands elsewhere.
func TestCheckWriteTargetReResolvesTheWholePath(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	ws, _ := NewWorkspace(dir)
	sub := filepath.Join(ws.Root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(sub, "file.txt")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.CheckWriteTarget(abs); err != nil {
		t.Fatalf("a plain file under a plain directory is fine: %v", err)
	}
	// The swap: sub becomes a link to outside, and outside holds a plain
	// file.txt so the leaf Lstat's as a regular file.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, sub)
	if err := ws.CheckWriteTarget(abs); !errors.Is(err, ErrPathNotAllowed) {
		t.Fatalf("err = %v, want ErrPathNotAllowed: the parent is now a link to outside, "+
			"and a check that looks only at the leaf does not see it", err)
	}
}

// TestWriteFileChecksContainmentBeforeMkdirAll pins the ORDER. The swap is
// made while the tool is blocked on the path lock — after Resolve, before the
// write — which is the race the check exists for. MkdirAll before the check
// would create `deeper/` inside the outside directory even though the write
// itself is then refused.
func TestWriteFileChecksContainmentBeforeMkdirAll(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	ws, _ := NewWorkspace(dir)
	sub := filepath.Join(ws.Root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fs := newFileTools(Options{Workspace: ws}.withDefaults())
	abs := filepath.Join(sub, "deeper", "file.txt")
	key, err := lockKey(abs)
	if err != nil {
		t.Fatal(err)
	}
	release := fs.locks.acquire(key)

	done := make(chan core.ToolResult, 1)
	go func() {
		done <- fs.writeFile().Execute(context.Background(),
			json.RawMessage(`{"path":"sub/deeper/file.txt","content":"pwned"}`))
	}()
	// The tool has resolved the path and is now waiting on the lock.
	time.Sleep(200 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("the write ran while the lock was held: %+v", res)
	default:
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, sub)
	release()

	select {
	case res := <-done:
		if res.OK || res.Error != "path_not_allowed" {
			t.Fatalf("want path_not_allowed, got %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write never completed")
	}
	if _, err := os.Stat(filepath.Join(outside, "deeper")); err == nil {
		t.Fatal("MkdirAll created a directory OUTSIDE the workspace before the containment " +
			"re-check ran; CheckWriteTarget must come first")
	}
	if _, err := os.Stat(filepath.Join(outside, "deeper", "file.txt")); err == nil {
		t.Fatal("the file was written outside the workspace")
	}
}

// TestCRLFNeedlesMatchACRLFFile is B1. The file is normalised to LF before
// matching; a needle the model copied with its CRs intact was not, and never
// matched. Through the tool, so Restore is covered too.
func TestCRLFNeedlesMatchACRLFFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "win.txt")
	if err := os.WriteFile(path, []byte("alpha\r\nbravo\r\ncharlie\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := toolByName(t, dir, "edit_file").Execute(context.Background(), json.RawMessage(
		`{"path":"win.txt","edits":[{"old_string":"alpha\r\nbravo","new_string":"ALPHA\r\nBRAVO"}]}`))
	if !res.OK {
		t.Fatalf("a CRLF needle against a CRLF file must match: %+v", res)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "ALPHA\r\nBRAVO\r\ncharlie\r\n" {
		t.Fatalf("file = %q: the replacement must be written with the file's own CRLF, once", got)
	}
}

// TestEditRejectionsNameTheEditOnlyInABatch is B9: `edits[1]:` tells the
// model which one, and for a single edit the prefix is noise. The pinned
// not-unique wording is asserted unprefixed in
// TestNonUniqueIsARejectionNotAReplaceAll.
func TestEditRejectionsNameTheEditOnlyInABatch(t *testing.T) {
	const content = "one\ntwo\ntwo\n"
	_, _, err := ApplyEdits(content, []Edit{
		{OldString: "one", NewString: "1"},
		{OldString: "two", NewString: "2"},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "edits[1]: Found 2 occurrences") {
		t.Fatalf("a batch rejection names the edit: %v", err)
	}
	_, _, err = ApplyEdits(content, []Edit{{OldString: "zulu", NewString: "z"}})
	if err == nil || strings.HasPrefix(err.Error(), "edits[") {
		t.Fatalf("a single-edit rejection carries no index: %v", err)
	}
	_, _, err = ApplyEdits(content, []Edit{
		{OldString: "one", NewString: "1"},
		{OldString: "zulu", NewString: "z"},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "edits[1]: The string to replace was not found") {
		t.Fatalf("a batch not-found names the edit: %v", err)
	}
}

// TestNotFoundSaysWhereTheFirstLineIs is T5: the rejection reports whether
// the needle's first line occurs and at which lines (at most three), so the
// model can repair the needle without re-reading the file.
func TestNotFoundSaysWhereTheFirstLineIs(t *testing.T) {
	content := "func a() {\n\treturn 1\n}\n\nfunc a() {\n\treturn 2\n}\n\nfunc a() {\n\treturn 3\n}\n\nfunc a() {\n\treturn 4\n}\n"
	reject := func(old string) string {
		t.Helper()
		_, _, err := ApplyEdits(content, []Edit{{OldString: old, NewString: "x"}})
		if err == nil {
			t.Fatalf("%q must be rejected", old)
		}
		return err.Error()
	}
	got := reject("func a() {\n\treturn 9\n}")
	if !strings.Contains(got, "first line occurs at line 1, 5, 9, …") || strings.Contains(got, "13") {
		t.Fatalf("the hint names at most three line numbers of the first line: %q", got)
	}
	if !strings.Contains(got, "lines after it differ") {
		t.Fatalf("the hint says what to look at: %q", got)
	}
	got = reject("    return 4")
	if !strings.Contains(got, "line 14 with different indentation") {
		t.Fatalf("an indentation mismatch is named as such: %q", got)
	}
	got = reject("func zzz() {\n\treturn 1\n}")
	if !strings.Contains(got, "Not even its first line occurs") {
		t.Fatalf("an absent first line says so: %q", got)
	}
}

// TestShellQuoteIsSafeForTheSedMarker is B8: the path in the long-line marker
// is pasted into a shell.
func TestShellQuoteIsSafeForTheSedMarker(t *testing.T) {
	if got := ShellQuote("it's a $file.txt"); got != `'it'\''s a $file.txt'` {
		t.Fatalf("ShellQuote = %s", got)
	}
	m := LongLineMarker(3, 1<<20, DefaultByteLimit, "my file.txt")
	if !strings.Contains(m, "sed -n '3p' 'my file.txt' |") {
		t.Fatalf("the path must be quoted in the marker: %q", m)
	}
}

// TestAbortDuringTheDrainDoesNotReclassifyAFinishedCommand is B5. The
// command exits 0 at once and leaves a descendant holding the pipe. A
// cancellation that arrives during the post-exit drain must (a) end the drain
// now rather than at the idle/ceiling bound, and (b) not turn a command that
// had already completed into an aborted one.
func TestAbortDuringTheDrainDoesNotReclassifyAFinishedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a unix shell background job")
	}
	if _, _, err := ResolveShell(); err != nil {
		t.Skip("no shell available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := Run(ctx, "( sleep 3 ) & echo started; exit 0", ExecOptions{
		MaxBytes: 4096, DrainIdle: 10 * time.Second, DrainCeiling: 10 * time.Second,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run took %v after cancellation; the post-exit drain must stop on ctx.Done", elapsed)
	}
	if res.Outcome != OutcomeOK || !strings.Contains(res.Output, "started") {
		t.Fatalf("outcome = %q output = %q; the command finished before the cancellation and "+
			"must be classified from the state at exit", res.Outcome, res.Output)
	}
}

// ---- T1: the model-facing text (core.ToolResult.Text)

// TestReadFileTextIsTheContentWithoutAnEnvelope.
func TestReadFileTextIsTheContentWithoutAnEnvelope(t *testing.T) {
	dir := t.TempDir()
	body := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, dir, "read_file")
	res := read.Execute(context.Background(), json.RawMessage(`{"path":"main.go"}`))
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if res.Text != strings.TrimSuffix(body, "\n") {
		t.Fatalf("Text = %q, want the raw content", res.Text)
	}
	if res.Data["content"] != res.Text {
		t.Fatal("Data.content stays populated for programmatic consumers")
	}
	if res.LLMText() != res.Text || strings.HasPrefix(res.LLMText(), "{") {
		t.Fatalf("the model must read the text, not an envelope: %q", res.LLMText())
	}
	// The continuation marker is part of the text, in place.
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"main.go","limit":2}`))
	if !strings.HasSuffix(res.Text, "\n"+ReadOffsetMarker(1, 2, 5)) {
		t.Fatalf("the marker stays inside the text: %q", res.Text)
	}
	// Errors keep the envelope.
	res = read.Execute(context.Background(), json.RawMessage(`{"path":"missing.go"}`))
	if res.OK || res.Text != "" || !strings.HasPrefix(res.LLMText(), `{"detail"`) {
		t.Fatalf("an error keeps the envelope: %+v", res)
	}
}

// TestExecuteTextIsTheOutputPlusAStatusLineOnlyWhenInformative.
func TestExecuteTextIsTheOutputPlusAStatusLineOnlyWhenInformative(t *testing.T) {
	ok := execResultToTool(ExecResult{Output: "hello\n", Outcome: OutcomeOK}, 0)
	if ok.Text != "hello\n" {
		t.Fatalf("a clean exit adds nothing: %q", ok.Text)
	}
	if ok.Data["exit_code"] != 0 || ok.Data["outcome"] != "ok" {
		t.Fatalf("Data keeps exit_code/outcome for programmatic readers: %+v", ok.Data)
	}
	cases := []struct {
		res  ExecResult
		want string
	}{
		{ExecResult{Output: "boom", Outcome: OutcomeExit, ExitCode: 2}, "boom\n[exit 2]"},
		{ExecResult{Output: "", Outcome: OutcomeExit, ExitCode: 1}, "[exit 1]"},
		{ExecResult{Output: "partial\n", Outcome: OutcomeTimeout, ExitCode: 137}, "partial\n[timeout after 5s]"},
		{ExecResult{Output: "x\n", Outcome: OutcomeAbort}, "x\n[aborted]"},
		{ExecResult{Output: "y\n", Outcome: OutcomeSignal, ExitCode: 137}, "y\n[killed by signal 9]"},
	}
	for _, c := range cases {
		got := execResultToTool(c.res, 5*time.Second)
		if got.Text != c.want {
			t.Fatalf("%s: Text = %q, want %q", c.res.Outcome, got.Text, c.want)
		}
		if got.OK {
			t.Fatalf("%s is not OK", c.res.Outcome)
		}
	}
	if _, _, err := ResolveShell(); err != nil {
		return
	}
	res := toolByName(t, t.TempDir(), "execute").Execute(context.Background(),
		json.RawMessage(`{"command":"echo out; echo err >&2; exit 3"}`))
	if res.Text != "out\nerr\n[exit 3]" {
		t.Fatalf("through the tool: %q", res.Text)
	}
}

// TestSearchTextIsGrepStyleGroupedByFile.
func TestSearchTextIsGrepStyleGroupedByFile(t *testing.T) {
	res := SearchResult{FilesSearched: 7, Matches: []SearchMatch{
		{File: "a/b.go", Line: 12, Text: "\tx := needle()", Before: []string{"func f() {"}, After: []string{"\treturn x", "}"}},
		{File: "a/b.go", Line: 13, Text: "\treturn needle", Before: []string{"\tx := needle()"}, After: []string{"}"}},
		{File: "c.md", Line: 1, Text: "needle"},
	}}
	want := "a/b.go\n" +
		"  11- func f() {\n" +
		"  12: \tx := needle()\n" +
		"  13: \treturn needle\n" +
		"  14- }\n" +
		"\n" +
		"c.md\n" +
		"  1: needle\n" +
		SearchMarker(50)
	if got := RenderSearchText(res, SearchMarker(50)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := RenderSearchText(SearchResult{FilesSearched: 3}, ""); got != "No matches (3 files searched)." {
		t.Fatalf("empty: %q", got)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("one\nneedle\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := toolByName(t, dir, "search_files").Execute(context.Background(),
		json.RawMessage(`{"pattern":"needle","context_lines":1}`))
	if !out.OK || out.Text != "x.txt\n  1- one\n  2: needle\n  3- three" {
		t.Fatalf("through the tool: %+v %q", out.Error, out.Text)
	}
	if _, ok := out.Data["matches"].([]SearchMatch); !ok {
		t.Fatal("Data keeps the structured matches")
	}
}

// TestListAndFindTextIsOneEntryPerLine, plus T3's defaults and B11's
// slash-separated find paths and symlink-to-directory suffix.
func TestListAndFindTextIsOneEntryPerLine(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"src/a.go", "src/b.go", "README"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink(t, "src", filepath.Join(dir, "srclink"))
	list := toolByName(t, dir, "list_files")
	res := list.Execute(context.Background(), json.RawMessage(`{}`))
	if res.Text != "README\nsrc/\nsrclink/" {
		t.Fatalf("list_files Text = %q: one entry per line, directories and links to directories with /", res.Text)
	}
	find := toolByName(t, dir, "find_files")
	res = find.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if res.Text != "src/a.go\nsrc/b.go" {
		t.Fatalf("find_files Text = %q", res.Text)
	}
	for _, f := range res.Data["files"].([]string) {
		if strings.Contains(f, `\`) {
			t.Fatalf("find_files returns slash-separated paths: %q", f)
		}
	}

	// Defaults: 201 files at the default limit of 200 truncates with a marker
	// naming limit=400.
	many := t.TempDir()
	for i := 0; i < FindResultDefault+1; i++ {
		if err := os.WriteFile(filepath.Join(many, fmt.Sprintf("f%04d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res = toolByName(t, many, "find_files").Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt"}`))
	files := res.Data["files"].([]string)
	if len(files) != FindResultDefault || res.Data["truncated"] != true {
		t.Fatalf("default find limit is %d: got %d, truncated=%v", FindResultDefault, len(files), res.Data["truncated"])
	}
	marker, _ := res.Data["marker"].(string)
	if !strings.Contains(marker, "limit=400") || !strings.HasSuffix(res.Text, "\n"+marker) {
		t.Fatalf("the marker names limit=400 and ends the text: %q / %q", marker, res.Text)
	}
	if strings.Count(res.Text, "\n") != FindResultDefault {
		t.Fatalf("Text is %d entries then the marker", FindResultDefault)
	}
	res = toolByName(t, many, "find_files").Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt","limit":5000}`))
	if len(res.Data["files"].([]string)) != FindResultDefault+1 {
		t.Fatal("an explicit limit above the cap is clamped to the cap, which still fits 201 files")
	}
}

// TestToolDescriptionsCarryTheOperativeFacts is T5/T6: these strings are sent
// on every request, so each fact is one sentence, and the facts a model most
// often gets wrong are present.
func TestToolDescriptionsCarryTheOperativeFacts(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	all, _ := All(Options{Workspace: ws})
	byName := map[string]core.Tool{}
	for _, tl := range all {
		byName[tl.Name] = tl
	}
	for _, want := range []string{"last 50 KB", "No default timeout", "> log 2>&1 &", "workspace root"} {
		if !strings.Contains(byName["execute"].Description, want) {
			t.Errorf("execute's description lacks %q: %q", want, byName["execute"].Description)
		}
	}
	for _, want := range []string{"hidden", "max_matches (<= 100)", "context_lines (<= 20)"} {
		if !strings.Contains(byName["search_files"].Description, want) {
			t.Errorf("search_files' description lacks %q", want)
		}
	}
	if !strings.Contains(byName["list_files"].Description, "Does not apply .gitignore") {
		t.Error("list_files' description must say it ignores .gitignore")
	}
	if g := byName["write_file"].PromptGuidelines; len(g) != 1 ||
		g[0] != "Prefer edit_file for an existing file; write_file replaces the whole file." {
		t.Errorf("write_file guideline: %q", g)
	}
	if ExecuteFallbackGuideline != "Use execute for file operations like ls, rg, find." {
		t.Error("REQ-TOOL-04e's guideline is pinned")
	}
}
