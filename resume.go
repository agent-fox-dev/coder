package agentkit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/session"
)

// OpenSession opens or creates a session log and returns the resume state.
//
// It is the front door for a persisted agent, and it exists so the ONLY way to
// build one is the correct way. REQ-SESS-02 requires the recovered model and
// reasoning level to be CONSTRUCTION INPUTS, not fields patched onto a built
// agent — a distinction that is easy to state and easy to violate, so
// NewAgent rejects a non-empty store outright (ErrSessionNotEmpty) and this is
// what you reach for instead.
func OpenSession(path string, opts session.Options) (*session.Store, *session.Resume, error) {
	store, loaded, err := session.Open(path, opts)
	if errors.Is(err, fs.ErrNotExist) {
		// A session that does not exist yet is the ordinary first-run case,
		// not an error the caller should have to distinguish.
		cwd, _ := os.Getwd()
		store, err = session.Create(path, core.SessionHeader{
			Version:   core.SessionLogVersion,
			ID:        newID("sess"),
			Timestamp: time.Now(),
			CWD:       cwd,
		}, opts)
		if err != nil {
			return nil, nil, err
		}
		return store, &session.Resume{Path: path, Header: store.Header(),
			History: core.NewConversationHistory()}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	branch, err := store.Branch(store.Head())
	if err != nil {
		return nil, nil, err
	}
	r := session.Fold(store.Header(), branch)
	r.Path = path
	r.LoadRepairs = loaded.Repairs
	return store, &r, nil
}

// NewAgentFromSession constructs an agent from folded session state.
//
// The fold's outputs are applied to cfg BEFORE construction:
//
//	Model         resolved from the recovered (provider, api, model) TRIPLE
//	ThinkingLevel from the last thinking_level_change entry
//	History       the flattened active branch, with entry ids marked
//	Checkpoint    so REQ-GO-15's skip rule (c) can still fire after a resume
//
// resolve maps a recovered provenance triple back to a *core.Model. It is a
// parameter rather than a catalog call so this package does not depend on the
// catalog, and so an embedder with its own model registry can resume without
// one.
//
// If the caller already set cfg.Model and the log names a different one, the
// LOG WINS: the transcript was produced by that model, and replaying it as
// though another produced it is what REQ-PROV-11 rule 1 exists to prevent.
func NewAgentFromSession(cfg core.AgentConfig, r *session.Resume, resolve func(provider string, api core.API, modelID string) (*core.Model, error)) (*Agent, error) {
	if r == nil {
		return nil, fmt.Errorf("agentkit: NewAgentFromSession(nil resume)")
	}

	if r.ModelID != "" {
		if resolve == nil {
			if cfg.Model == nil || cfg.Model.ID != r.ModelID {
				return nil, fmt.Errorf(
					"agentkit: session was produced by model %q (provider %q, api %q) but no "+
						"resolver was supplied and cfg.Model does not match; replaying a "+
						"transcript as though another model produced it strips its reasoning "+
						"(REQ-PROV-11 rule 1)", r.ModelID, r.Provider, r.API)
			}
		} else {
			m, err := resolve(r.Provider, r.API, r.ModelID)
			if err != nil {
				return nil, fmt.Errorf("agentkit: resolving the session's model %q: %w", r.ModelID, err)
			}
			cfg.Model = m
		}
	}
	if r.ThinkingLevel != "" {
		cfg.ThinkingLevel = r.ThinkingLevel
	}

	h := r.History
	if h == nil {
		h = core.NewConversationHistory()
	}
	if r.HasCheckpoint {
		h.SetCheckpoint(r.Checkpoint)
	}

	a, err := NewAgentWithHistory(cfg, h)
	if err != nil {
		return nil, err
	}
	return a, nil
}
