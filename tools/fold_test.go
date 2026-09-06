package tools_test

import (
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/tools"
)

// REQ-TOOL-04d's whitespace-tolerant fallback.

// TestAnExactMatchIsNeverOverriddenByAFoldedOne.
//
// The fallback runs only after exact matching fails for the batch. If a folded
// match could beat an exact one, an edit naming a real ASCII line would
// suddenly apply to a different line that merely folds the same — the tool
// would get less predictable the more Unicode a file contains.
func TestAnExactMatchIsNeverOverriddenByAFoldedOne(t *testing.T) {
	// Two lines that fold identically: one ASCII, one curly.
	content := "a = 'x'\nb\na = ‘x’\n"
	out, n, err := tools.ApplyEdits(content, []tools.Edit{
		{OldString: "a = 'x'", NewString: "a = 'y'"},
	})
	if err != nil {
		t.Fatalf("the exact match must apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied %d edits", n)
	}
	if !strings.HasPrefix(out, "a = 'y'\n") {
		t.Fatalf("the ASCII line should have changed; got %q", out)
	}
	// The curly line is untouched, bytes intact.
	if !strings.Contains(out, "a = ‘x’") {
		t.Fatalf("the curly line must survive verbatim; got %q", out)
	}
}

// TestSmartQuotesMatchTheirASCIIForm is the case the requirement is about: a
// model saw the file through a lossy render and typed straight quotes.
func TestSmartQuotesMatchTheirASCIIForm(t *testing.T) {
	content := "func f() {\n\tname := “hello”\n}\n"
	out, n, err := tools.ApplyEdits(content, []tools.Edit{
		{OldString: "\tname := \"hello\"", NewString: "\tname := \"goodbye\""},
	})
	if err != nil {
		t.Fatalf("the folded match should have applied: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied %d edits", n)
	}
	if !strings.Contains(out, `name := "goodbye"`) {
		t.Fatalf("got %q", out)
	}
}

// TestTrailingWhitespaceIsToleratedButLeadingIsNot.
//
// Leading whitespace is indentation, and it is semantic in Go, Python, YAML
// and Makefiles alike. Trimming it would let an edit match a line at the wrong
// nesting level — silently applying to the wrong block, which is exactly what
// REQ-TOOL-04c's uniqueness rule exists to prevent.
func TestTrailingWhitespaceIsToleratedButLeadingIsNot(t *testing.T) {
	// Trailing spaces in the file, none in the edit: must match.
	trailing := "if x {\n\tdoIt()   \n}\n"
	if _, _, err := tools.ApplyEdits(trailing, []tools.Edit{
		{OldString: "\tdoIt()", NewString: "\tdoThat()"},
	}); err != nil {
		t.Fatalf("trailing whitespace must be tolerated: %v", err)
	}

	// Different LEADING whitespace must not match. The curly quote is what
	// forces this through the fold path at all: without it the exact,
	// substring-based matcher would find "\tdoIt()" inside "\t\tdoIt()" and
	// the fold would never run.
	indented := "if x {\n\t\tval := \u2018a\u2019\n}\n"
	if _, _, err := tools.ApplyEdits(indented, []tools.Edit{
		{OldString: "    val := 'a'", NewString: "    val := 'b'"},
	}); err == nil {
		t.Fatal("leading whitespace must not be folded away: an edit that matched at " +
			"the wrong nesting level would apply silently to the wrong block")
	}
}

// TestUntouchedLinesKeepTheirExactBytes is the rule that makes the fold safe.
//
// The fold is a MATCH KEY, never an output. A pass that rewrote the file to
// its folded form would silently ASCII-ify every quote and dash in it —
// turning a one-line edit into a whole-file reformat nobody asked for.
func TestUntouchedLinesKeepTheirExactBytes(t *testing.T) {
	const curly = "// “quoted” — dash … ellipsis"
	content := curly + "\n" + "target := ‘v’\n" + curly + "\n"

	out, _, err := tools.ApplyEdits(content, []tools.Edit{
		{OldString: "target := 'v'", NewString: "target := 'w'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, curly) != 2 {
		t.Fatalf("the untouched lines were rewritten to their fold.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "target := 'w'") {
		t.Fatalf("the edit did not apply: %q", out)
	}
}

// TestANonUniqueFoldedMatchIsRejected. Uniqueness is REQ-TOOL-04c and the
// fallback does not get to relax it: multiplicity means the model has not
// identified a site.
func TestANonUniqueFoldedMatchIsRejected(t *testing.T) {
	content := "x := ‘a’\ny\nx := ‘a’\n"
	if _, _, err := tools.ApplyEdits(content, []tools.Edit{
		{OldString: "x := 'a'", NewString: "x := 'b'"},
	}); err == nil {
		t.Fatal("two folded matches must be a rejection, not a replace-all")
	}
}

// TestTheFallbackReportsTheOriginalRejection.
//
// When the fold cannot help either, the caller must see the phase-ordered
// "not found" it would have seen anyway. A second, differently-worded error
// from a fallback the model never asked for tells it to fix the wrong thing.
func TestTheFallbackReportsTheOriginalRejection(t *testing.T) {
	_, _, err := tools.ApplyEdits("hello\n", []tools.Edit{
		{OldString: "nothing like this", NewString: "x"},
	})
	if err == nil {
		t.Fatal("expected a rejection")
	}
	var ee *tools.EditError
	if !asEditError(err, &ee) {
		t.Fatalf("want *tools.EditError, got %T", err)
	}
	if ee.Phase != "not_found" {
		t.Fatalf("phase = %q, want not_found", ee.Phase)
	}
}

// TestAPartiallyRescuedBatchIsRejected.
//
// A batch where the fold saves some edits and not others is not the batch the
// model meant. Applying the subset is the silent partial application the
// phase rules exist to prevent.
func TestAPartiallyRescuedBatchIsRejected(t *testing.T) {
	content := "a := ‘x’\nb := 2\n"
	_, _, err := tools.ApplyEdits(content, []tools.Edit{
		{OldString: "a := 'x'", NewString: "a := 'y'"}, // rescuable
		{OldString: "no such line", NewString: "boom"}, // not
	})
	if err == nil {
		t.Fatal("a partially rescuable batch must be rejected whole")
	}
}

// TestFoldConfusablesCoversTheRenderedSet.
func TestFoldConfusablesCoversTheRenderedSet(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"‘a’", "'a'"},
		{"“a”", `"a"`},
		{"a–b", "a-b"},
		{"a—b", "a-b"},
		{"a−b", "a-b"},
		{"a b", "a b"},
		{"a　b", "a b"},
		{"a\u200bb", "ab"}, // zero-width folds to nothing
		{"a\ufeffb", "ab"}, // BOM mid-string
		{"…", "..."},       // ellipsis
		{"ﬁx", "fix"},      // fi ligature
		{"ａｂ", "ab"},       // fullwidth
		{"plain ascii", "plain ascii"},
	} {
		if got := tools.FoldConfusables(tc.in); got != tc.want {
			t.Errorf("FoldConfusables(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFoldLeavesUnlistedRunesAlone. A fold that guessed at characters outside
// the confusable set would start changing meaning rather than presentation.
func TestFoldLeavesUnlistedRunesAlone(t *testing.T) {
	for _, s := range []string{"café", "naïve", "日本語", "Ω", "emoji 🙂"} {
		if got := tools.FoldConfusables(s); got != s {
			t.Errorf("FoldConfusables(%q) = %q; unlisted runes must be left alone", s, got)
		}
	}
}

func asEditError(err error, target **tools.EditError) bool {
	for err != nil {
		if e, ok := err.(*tools.EditError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
