package difftest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
)

// Target is one wire API under test.
type Target struct {
	API      string
	Provider core.APIProvider
}

// Targets returns every first-party wire API. faux is deliberately absent: it
// has no wire format, and including it would inflate the scenario count with a
// comparison that cannot fail.
func Targets() []Target {
	noEnv := func(string) string { return "" }
	return []Target{
		{API: string(anthropic.API), Provider: anthropic.Provider(anthropic.Options{Getenv: noEnv})},
		{API: string(openai.API), Provider: openai.Provider(openai.Options{Getenv: noEnv})},
		{API: string(google.API), Provider: google.Provider(google.Options{Getenv: noEnv})},
		{API: string(ollama.API), Provider: ollama.Provider(ollama.Options{Getenv: noEnv})},
	}
}

// Result is one (scenario, provider) comparison.
type Result struct {
	Scenario string
	API      string
	State    State
	Diffs    []Difference
}

// Run is a whole harness run.
type Run struct {
	Results []Result
	Stale   []*Entry
	// Dark is NFR-TEST-07.3: the run never reached the scenarios. A dark run
	// prints NO TALLY and exits 1. Zero compared scenarios is not a result,
	// and the failure mode this guards is the worst one a test harness has —
	// a green pipeline over a suite that ran nothing.
	Dark       bool
	DarkReason string
}

// Options configures Execute.
type Options struct {
	ScenarioDir string
	LedgerPath  string
	Targets     []Target
}

// Execute runs the harness.
func Execute(ctx context.Context, opts Options) (Run, error) {
	targets := opts.Targets
	if targets == nil {
		targets = Targets()
	}

	// Both capture arms run ONCE before the scenario list (NFR-TEST-07.3).
	// Either failing aborts. A harness whose own plumbing is broken must not
	// be able to report a tally of zero and exit clean.
	if err := preflight(ctx, targets); err != nil {
		return Run{Dark: true, DarkReason: err.Error()}, nil
	}

	scenarios, err := LoadScenarios(opts.ScenarioDir)
	if err != nil {
		return Run{Dark: true, DarkReason: err.Error()}, nil
	}

	ledger, err := LoadLedger(opts.LedgerPath)
	if err != nil {
		return Run{}, err
	}

	var run Run
	for _, s := range scenarios {
		req, model, err := s.Request()
		if err != nil {
			return Run{}, err
		}
		for _, t := range targets {
			ref, err := s.Reference(t.API)
			if os.IsNotExist(err) {
				// No reference for this wire API in this scenario. That is not
				// a failure of the provider; it is a gap in the corpus, and
				// NFR-TEST-06.1's "a provider with no scenarios is untested"
				// is reported by the tally, not by a fake PASS.
				continue
			}
			if err != nil {
				return Run{}, err
			}

			m := *model
			m.API = core.API(t.API)
			actual, err := Capture(ctx, t.Provider, &m, req)
			if err != nil {
				return Run{}, err
			}

			diffs, err := Compare(ref, actual, s.OrderSensitivePaths)
			if err != nil {
				return Run{}, err
			}
			state, unaccepted := ledger.Classify(s.Name, t.API, diffs)
			run.Results = append(run.Results, Result{
				Scenario: s.Name, API: t.API, State: state, Diffs: unaccepted})
		}
	}

	if len(run.Results) == 0 {
		run.Dark = true
		run.DarkReason = fmt.Sprintf(
			"no scenario was compared: %d scenario(s) in %s, none carrying a reference body. "+
				"NFR-TEST-06.3 requires an INDEPENDENTLY produced reference — a vendor SDK at a "+
				"pinned version, or recorded live traffic — never a hand-authored expectation, "+
				"because a hand-authored one encodes the same mental model as the code under test",
			len(scenarios), opts.ScenarioDir)
		return run, nil
	}

	run.Stale = ledger.Stale()
	sort.SliceStable(run.Results, func(i, j int) bool {
		if run.Results[i].Scenario != run.Results[j].Scenario {
			return run.Results[i].Scenario < run.Results[j].Scenario
		}
		return run.Results[i].API < run.Results[j].API
	})
	return run, nil
}

// preflight exercises the capture arm on a request built in code, so a broken
// arm is distinguishable from an empty corpus.
func preflight(ctx context.Context, targets []Target) error {
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "preflight"}}}}}
	for _, t := range targets {
		m := &core.Model{ID: "preflight", API: core.API(t.API), Provider: "preflight", MaxTokens: 16}
		if _, err := Capture(ctx, t.Provider, m, req); err != nil {
			return fmt.Errorf("capture arm for %s is broken: %v", t.API, err)
		}
	}
	return nil
}

// ExitCode is NFR-TEST-07's exit machine.
//
//	0  every scenario PASS or KNOWN, and every ledger entry still fires
//	1  at least one FAIL, or a dark run
//	3  no FAIL, but at least one stale ledger entry
//
// FIXED takes its OWN code so "got worse" stays distinguishable from "got
// better, paperwork behind". Both are non-zero because both need a human.
func (r Run) ExitCode() int {
	if r.Dark {
		return 1
	}
	for _, res := range r.Results {
		if res.State == StateFail {
			return 1
		}
	}
	if len(r.Stale) > 0 {
		return 3
	}
	return 0
}

// Summary renders the run. NFR-TEST-07.2: the KNOWN count is always printed
// and the summary never renders as clean while divergences are accepted.
func (r Run) Summary() string {
	var b strings.Builder
	if r.Dark {
		// A dark run prints NO TALLY. A count of zero next to the word PASS is
		// exactly the thing this requirement exists to prevent.
		fmt.Fprintf(&b, "DARK: the run never reached the scenarios.\n  %s\n", r.DarkReason)
		return b.String()
	}

	counts := map[State]int{}
	for _, res := range r.Results {
		counts[res.State]++
		if res.State == StateFail {
			fmt.Fprintf(&b, "FAIL %s [%s]\n", res.Scenario, res.API)
			for _, d := range res.Diffs {
				fmt.Fprintf(&b, "    %s\n", d)
			}
		}
	}
	for _, e := range r.Stale {
		fmt.Fprintf(&b, "FIXED %s [%s] %s %s — ledger entry no longer fires; remove it\n",
			e.Scenario, e.Provider, e.Path, e.Kind)
	}

	fmt.Fprintf(&b, "%d compared: %d PASS, %d KNOWN, %d FAIL, %d FIXED\n",
		len(r.Results), counts[StatePass], counts[StateKnown], counts[StateFail], len(r.Stale))
	if counts[StateKnown] > 0 {
		fmt.Fprintf(&b, "%d accepted divergence(s) are tracked debt, not a clean run.\n",
			counts[StateKnown])
	}
	return b.String()
}
