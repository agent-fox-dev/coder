package difftest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/difftest"
)

func diffs(t *testing.T, ref, act string, sensitive ...string) []difftest.Difference {
	t.Helper()
	d, err := difftest.Compare([]byte(ref), []byte(act), sensitive)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func kinds(ds []difftest.Difference) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Path+":"+string(d.Kind))
	}
	return strings.Join(out, " ")
}

// TestNumberLiteralsAreNotNormalized is NFR-TEST-06.4's sharpest clause.
//
// 1024, 1024.0 and 1e3 are the same float64 and different bytes. A comparator
// that decodes into float64 launders all three into one value and reports PASS
// on a provider that emits a max_tokens the vendor rejects. The literal is the
// thing under test.
func TestNumberLiteralsAreNotNormalized(t *testing.T) {
	for _, c := range []struct{ ref, act string }{
		{`{"max_tokens":1024}`, `{"max_tokens":1024.0}`},
		{`{"max_tokens":1000}`, `{"max_tokens":1e3}`},
		{`{"temperature":0}`, `{"temperature":0.0}`},
	} {
		got := diffs(t, c.ref, c.act)
		if len(got) != 1 || got[0].Kind != difftest.KindValue {
			t.Fatalf("Compare(%s, %s) = %v; the number LITERAL must be diffed",
				c.ref, c.act, kinds(got))
		}
	}
	// And identical literals are identical.
	if got := diffs(t, `{"n":1024}`, `{"n":1024}`); len(got) != 0 {
		t.Fatalf("identical literals differ: %v", kinds(got))
	}
}

// TestNullVersusAbsentIsADifference is what makes REQ-PROV-16 enforceable.
//
// `omitempty` on a field whose zero value is meaningful produces "absent"
// where the reference produces an explicit value. A comparator that treats
// null and absent as the same passes the exact bug the requirement exists to
// prevent, and does so silently.
func TestNullVersusAbsentIsADifference(t *testing.T) {
	got := diffs(t, `{"content":null}`, `{}`)
	if len(got) != 1 || got[0].Kind != difftest.KindMissing || got[0].Path != "$.content" {
		t.Fatalf("null vs absent = %v, want one missing at $.content", kinds(got))
	}
	got = diffs(t, `{}`, `{"content":null}`)
	if len(got) != 1 || got[0].Kind != difftest.KindExtra {
		t.Fatalf("absent vs null = %v, want one extra", kinds(got))
	}
	// An explicit null on both sides agrees.
	if got := diffs(t, `{"content":null}`, `{"content":null}`); len(got) != 0 {
		t.Fatalf("two explicit nulls differ: %v", kinds(got))
	}
	// And null is not the empty string.
	got = diffs(t, `{"content":null}`, `{"content":""}`)
	if len(got) != 1 || got[0].Kind != difftest.KindType {
		t.Fatalf("null vs \"\" = %v, want a type difference", kinds(got))
	}
}

func TestKeyOrderIsNormalizedByDefault(t *testing.T) {
	if got := diffs(t, `{"a":1,"b":2}`, `{"b":2,"a":1}`); len(got) != 0 {
		t.Fatalf("key order differed without being declared sensitive: %v", kinds(got))
	}
}

// TestOrderSensitivePathsFail is NFR-TEST-06.5's other half. Key order is moved
// to a side channel, never discarded — so a scenario can declare the paths
// where insertion order is observable to the model.
func TestOrderSensitivePathsFail(t *testing.T) {
	ref := `{"messages":[{"content":[{"input":{"zebra":1,"apple":2}}]}]}`
	act := `{"messages":[{"content":[{"input":{"apple":2,"zebra":1}}]}]}`

	if got := diffs(t, ref, act); len(got) != 0 {
		t.Fatalf("undeclared path reported an order difference: %v", kinds(got))
	}
	got := diffs(t, ref, act, "$.messages[].content[].input")
	if len(got) != 1 || got[0].Kind != difftest.KindOrder {
		t.Fatalf("declared order-sensitive path = %v, want one order difference. "+
			"Model-authored tool-call arguments are exactly this case: reordering them "+
			"changes the text the model is conditioned on.", kinds(got))
	}
}

func TestStringEscapingIsNormalized(t *testing.T) {
	if got := diffs(t, `{"s":"Aé"}`, `{"s":"Aé"}`); len(got) != 0 {
		t.Fatalf("escaping is normalized on purpose: %v", kinds(got))
	}
	if got := diffs(t, `{"s":"a"}`, `{"s":"b"}`); len(got) != 1 {
		t.Fatalf("different strings must differ: %v", kinds(got))
	}
}

// TestArrayOrderIsNeverNormalized: message order and content-block order are
// the prompt.
func TestArrayOrderIsNeverNormalized(t *testing.T) {
	got := diffs(t, `{"m":["a","b"]}`, `{"m":["b","a"]}`)
	if len(got) != 2 {
		t.Fatalf("array reordering = %v, want both elements to differ", kinds(got))
	}
	got = diffs(t, `{"m":["a","b"]}`, `{"m":["a"]}`)
	if len(got) == 0 || got[0].Kind != difftest.KindLength {
		t.Fatalf("array length = %v, want a length difference", kinds(got))
	}
}

func TestKeyOrderSideChannelWalksIdenticalPaths(t *testing.T) {
	a, err := difftest.KeyOrderLines([]byte(`{"b":{"y":1,"x":2},"a":3}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := difftest.KeyOrderLines([]byte(`{"a":3,"b":{"x":2,"y":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("side channels have different lengths: %v vs %v", a, b)
	}
	for i := range a {
		pa := strings.SplitN(a[i], "\t", 2)[0]
		pb := strings.SplitN(b[i], "\t", 2)[0]
		if pa != pb {
			t.Fatalf("line %d walks different paths: %q vs %q. Both sides must traverse "+
				"objects by sorted key, or the side channels cannot be diffed at all.",
				i, pa, pb)
		}
	}
	if a[0] == b[0] {
		t.Fatal("the side channel must RECORD the original order, not the sorted one")
	}
}

// ------------------------------------------------------------------ ledger

func writeLedger(t *testing.T, dir string, entries any) string {
	t.Helper()
	p := filepath.Join(dir, "known-divergences.json")
	b, _ := json.Marshal(entries)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnIncompleteLedgerEntryIsRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeLedger(t, dir, []map[string]any{{"scenario": "s", "path": "$.a", "kind": "value"}})
	if _, err := difftest.LoadLedger(p); err == nil {
		t.Fatal("an entry with no `why` cannot be reviewed and must be rejected " +
			"(NFR-TEST-07.4): an entry buys time, it does not close the defect")
	}
}

func TestAMissingLedgerIsEmptyNotAnError(t *testing.T) {
	l, err := difftest.LoadLedger(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a repository with no accepted divergences is the goal state: %v", err)
	}
	if len(l.Entries) != 0 {
		t.Fatal("want an empty ledger")
	}
}

func TestClassificationAndStaleEntries(t *testing.T) {
	dir := t.TempDir()
	p := writeLedger(t, dir, []map[string]any{
		{"scenario": "s1", "path": "$.a", "kind": "value", "why": "vendor SDK emits a float"},
		{"scenario": "s1", "path": "$.never", "kind": "value", "why": "fixed upstream, not yet removed"},
	})
	l, err := difftest.LoadLedger(p)
	if err != nil {
		t.Fatal(err)
	}

	if st, _ := l.Classify("s1", "anthropic-messages", nil); st != difftest.StatePass {
		t.Fatalf("no differences = %s, want PASS", st)
	}
	st, _ := l.Classify("s1", "anthropic-messages",
		[]difftest.Difference{{Path: "$.a", Kind: difftest.KindValue}})
	if st != difftest.StateKnown {
		t.Fatalf("a covered difference = %s, want KNOWN", st)
	}
	st, un := l.Classify("s1", "anthropic-messages", []difftest.Difference{
		{Path: "$.a", Kind: difftest.KindValue},
		{Path: "$.b", Kind: difftest.KindValue},
	})
	if st != difftest.StateFail || len(un) != 1 || un[0].Path != "$.b" {
		t.Fatalf("state=%s unaccepted=%v; EVERY difference must be covered for KNOWN", st, un)
	}
	// Wrong kind at a covered path is NOT covered: an entry must not widen to
	// a different defect at the same place.
	if st, _ := l.Classify("s1", "x", []difftest.Difference{
		{Path: "$.a", Kind: difftest.KindMissing}}); st != difftest.StateFail {
		t.Fatalf("a different KIND at a covered path = %s, want FAIL", st)
	}

	stale := l.Stale()
	if len(stale) != 1 || stale[0].Path != "$.never" {
		t.Fatalf("stale = %v, want the entry that never fired", stale)
	}
}

// ------------------------------------------------------------------ exit machine

// syntheticRun builds a scenario directory with a HAND-AUTHORED reference.
//
// NFR-TEST-06.3 forbids that for the real corpus, and this is not the real
// corpus: these fixtures test the HARNESS — the comparator, the ledger and the
// exit machine — not any provider. A hand-authored reference proves nothing
// about a provider precisely because it encodes the same mental model; it
// proves plenty about a comparator, whose job is to notice bytes.
func syntheticRun(t *testing.T, reference string, ledger any) difftest.Run {
	t.Helper()
	dir := t.TempDir()
	sdir := filepath.Join(dir, "scenarios", "basic")
	if err := os.MkdirAll(filepath.Join(sdir, "reference"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "scenario.json"), []byte(syntheticScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "reference", "anthropic-messages.json"),
		[]byte(reference), 0o644); err != nil {
		t.Fatal(err)
	}

	lp := filepath.Join(dir, "none.json")
	if ledger != nil {
		lp = writeLedger(t, dir, ledger)
	}
	run, err := difftest.Execute(context.Background(), difftest.Options{
		ScenarioDir: filepath.Join(dir, "scenarios"), LedgerPath: lp,
		Targets: []difftest.Target{difftest.Targets()[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

const syntheticScenario = `{
  "name": "basic",
  "config": {"model": "anthropic/claude-sonnet-4-5", "max_tokens": 1024},
  "messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}]
}`

// actualBody captures what the Anthropic provider really produces for the
// synthetic scenario, so the PASS case compares against reality rather than
// against a guess about it — which is the same argument NFR-TEST-06.3 makes
// about goldens, applied to a fixture.
func actualBody(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(p, []byte(syntheticScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := difftest.LoadScenario(p)
	if err != nil {
		t.Fatal(err)
	}
	req, m, err := sc.Request()
	if err != nil {
		t.Fatal(err)
	}
	b, err := difftest.Capture(context.Background(), difftest.Targets()[0].Provider, m, req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestACleanRunExitsZero(t *testing.T) {
	run := syntheticRun(t, actualBody(t), nil)
	if len(run.Results) != 1 || run.Results[0].State != difftest.StatePass {
		t.Fatalf("run = %+v, want one PASS", run.Results)
	}
	if run.ExitCode() != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.ExitCode(), run.Summary())
	}
}

func TestAFailExitsOne(t *testing.T) {
	run := syntheticRun(t, `{"model":"something-else","max_tokens":1,"messages":[],"stream":true}`, nil)
	if run.ExitCode() != 1 {
		t.Fatalf("exit = %d, want 1\n%s", run.ExitCode(), run.Summary())
	}
	if !strings.Contains(run.Summary(), "FAIL") {
		t.Fatalf("summary must name the failure:\n%s", run.Summary())
	}
}

// TestAStaleLedgerEntryExitsThree is NFR-TEST-07.1.
//
// A stale entry is a live, unattended permission slip: the day someone
// reintroduces exactly that regression the harness reports KNOWN and exits 0,
// and the defect ships with the harness's blessing. FIXED takes its own code
// so "got worse" stays distinguishable from "got better, paperwork behind".
func TestAStaleLedgerEntryExitsThree(t *testing.T) {
	run := syntheticRun(t, actualBody(t), []map[string]any{
		{"scenario": "basic", "path": "$.nothing", "kind": "value", "why": "long since fixed"},
	})
	if run.ExitCode() != 3 {
		t.Fatalf("exit = %d, want 3\n%s", run.ExitCode(), run.Summary())
	}
	if !strings.Contains(run.Summary(), "FIXED") {
		t.Fatalf("summary must name the stale entry:\n%s", run.Summary())
	}
}

// TestADarkRunPrintsNoTally is NFR-TEST-07.3, and it is the requirement that
// protects every other one here. Zero compared scenarios is not a result.
func TestADarkRunPrintsNoTally(t *testing.T) {
	run, err := difftest.Execute(context.Background(), difftest.Options{
		ScenarioDir: filepath.Join(t.TempDir(), "empty"),
		LedgerPath:  filepath.Join(t.TempDir(), "none.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Dark {
		t.Fatal("an empty corpus is DARK, not passing")
	}
	if run.ExitCode() != 1 {
		t.Fatalf("exit = %d, want 1", run.ExitCode())
	}
	s := run.Summary()
	if !strings.HasPrefix(s, "DARK:") {
		t.Fatalf("a dark run must say so first:\n%s", s)
	}
	if tallyLine.MatchString(s) {
		t.Fatalf("a dark run must print NO TALLY; a count of zero beside the word PASS "+
			"is exactly the failure this guards:\n%s", s)
	}
}

// TestAnUnknownScenarioOptionIsAHardError is NFR-TEST-06.6. Without it both
// arms ignore the unmapped key and agree for the wrong reason.
func TestAnUnknownScenarioOptionIsAHardError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(p, []byte(`{"name":"x","reasoning_efort":"high"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := difftest.LoadScenario(p); err == nil {
		t.Fatal("a misspelled option must be a hard error, or both sides ignore it and " +
			"the harness reports PASS on a scenario neither side ran")
	}
}

// tallyLine is the summary's counting line. A dark run must not print one.
var tallyLine = regexp.MustCompile(`(?m)^\d+ compared: \d+ PASS`)
