package difftest

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// State is NFR-TEST-07's four-valued classification. Every scenario gets
// exactly one.
type State string

const (
	// StatePass: reference and provider are identical.
	StatePass State = "PASS"
	// StateKnown: differs, and EVERY difference matches a ledger entry on
	// scenario, path and kind. Accepted, tracked debt. Not clean.
	StateKnown State = "KNOWN"
	// StateFail: differs in any way the ledger does not cover. A provider bug
	// until proven otherwise.
	StateFail State = "FAIL"
)

// Entry is one accepted divergence.
//
// Every field is required by NFR-TEST-07.4: an entry states WHAT diverges and
// WHY it is accepted. An entry buys time; it does not close the defect.
type Entry struct {
	Scenario string `json:"scenario"`
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Kind     Kind   `json:"kind"`
	Why      string `json:"why"`

	fired bool
}

// Ledger is known-divergences.json.
type Ledger struct {
	Entries []*Entry
	Path    string
}

// LoadLedger reads the ledger. A missing file is an EMPTY ledger, not an
// error: a repository with no accepted divergences is the goal state, and
// requiring a file full of nothing to express it is friction with no payoff.
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{Path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &l.Entries); err != nil {
		return nil, fmt.Errorf("difftest: parsing %s: %w", path, err)
	}
	for i, e := range l.Entries {
		if e.Scenario == "" || e.Path == "" || e.Kind == "" || e.Why == "" {
			return nil, fmt.Errorf(
				"difftest: %s entry %d is incomplete: scenario, path, kind and why are all "+
					"required — an entry that does not say what diverges and why it is "+
					"accepted cannot be reviewed (NFR-TEST-07.4)", path, i)
		}
	}
	return l, nil
}

// Accepts reports whether a difference matches an entry, and marks that entry
// fired.
func (l *Ledger) Accepts(scenario, providerName string, d Difference) bool {
	np := normalizePath(d.Path)
	for _, e := range l.Entries {
		if e.Scenario != scenario || e.Kind != d.Kind {
			continue
		}
		if e.Provider != "" && e.Provider != providerName {
			continue
		}
		if normalizePath(e.Path) != np {
			continue
		}
		e.fired = true
		return true
	}
	return false
}

// Stale returns the entries that never fired during the run — NFR-TEST-07's
// FIXED state.
//
// A stale entry FAILS the run, and the reason is worth stating plainly: it is
// a live, unattended permission slip. The day someone reintroduces exactly
// that regression, the harness reports KNOWN and exits clean, and the defect
// ships with the harness's blessing.
func (l *Ledger) Stale() []*Entry {
	var out []*Entry
	for _, e := range l.Entries {
		if !e.fired {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scenario != out[j].Scenario {
			return out[i].Scenario < out[j].Scenario
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Classify assigns a scenario its state.
func (l *Ledger) Classify(scenario, providerName string, diffs []Difference) (State, []Difference) {
	if len(diffs) == 0 {
		return StatePass, nil
	}
	var unaccepted []Difference
	for _, d := range diffs {
		if !l.Accepts(scenario, providerName, d) {
			unaccepted = append(unaccepted, d)
		}
	}
	if len(unaccepted) == 0 {
		return StateKnown, nil
	}
	return StateFail, unaccepted
}
