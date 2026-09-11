// Command session shows a durable session: one process writes an append-only
// log, the next folds it back into an agent that remembers.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/session "Pick a number between 1 and 100 and tell me what it is."
//	go run ./examples/session "What number did you pick?"
//
// The second command answers from the first one's transcript, which lives on
// disk and not in this process. Point --session anywhere, and start over with
// --reset:
//
//	go run ./examples/session --session ./session.jsonl "hello"
//	go run ./examples/session --session ./session.jsonl --reset
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/session"
	"github.com/agentfox/agentkit-go/stop"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultPath := filepath.Join(os.TempDir(), "agentkit-example-session.jsonl")
	path := flag.String("session", defaultPath, "path to the session log")
	reset := flag.Bool("reset", false, "delete the session log and start over")
	flag.Parse()

	if *reset {
		if err := os.Remove(*path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "removed %s\n", *path)
	}

	model, err := catalog.ResolveModel(modelSpec())
	if err != nil {
		return err
	}

	cfg := core.AgentConfig{Model: model}
	agentkit.RegisterDefaults(&cfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)
	cfg.StopPolicy = stop.Any(
		stop.AfterTurns(10),
		stop.OverBudget(1.00),
	)
	cfg.SystemPrompt = "You are concise. Answer in plain prose, no preamble."

	// 1. session.OpenOrCreate creates the log or opens an existing one, and hands back
	//    both halves: the store to keep writing to, and the fold of what is
	//    already there. It is the front door precisely because the wrong way
	//    to resume — build an agent, then patch the recovered model onto it —
	//    is easy to write and impossible to spot in review.
	//
	//    DurabilityPerEntry fsyncs after every entry. That is roughly one disk
	//    flush per message, and it is the right trade whenever losing the
	//    transcript would lose real work; DurabilityBuffered (the default)
	//    survives process death but not machine death.
	store, resume, err := session.OpenOrCreate(*path, session.Options{
		Durability: session.DurabilityPerEntry,
	})
	if err != nil {
		return err
	}
	defer store.Close()
	fmt.Fprintf(os.Stderr, "session: %s (cat it: it is JSONL, one entry per line)\n", *path)

	// 2. Repairs are reported, never silent. A log truncated by a kill -9 or
	//    a full disk still loads — the loader drops what it cannot parse —
	//    but the caller is told what was dropped, so a deployment that must
	//    not resume from a damaged transcript can refuse instead of guessing.
	//    LoadRepairs is the loader's half; resume.Repairs() adds the fold's.
	if len(resume.LoadRepairs) > 0 {
		fmt.Fprintf(os.Stderr, "%d repair(s) on load:\n", len(resume.LoadRepairs))
		for _, rep := range resume.LoadRepairs {
			lost := ""
			if rep.LostData() {
				lost = " (data was discarded)"
			}
			fmt.Fprintf(os.Stderr, "  %s%s\n", rep, lost)
		}
	}

	prompt := strings.Join(flag.Args(), " ")

	var agent *agentkit.Agent
	if len(resume.Messages) == 0 {
		// 3a. First run: nothing to fold. The store goes on the config and
		//     NewAgent subscribes to the loop, so every message from here on
		//     is appended as it happens rather than saved at the end — a run
		//     that dies mid-turn still leaves the turns before it on disk.
		fmt.Fprintln(os.Stderr, "first run: new session")
		if prompt == "" {
			prompt = "Pick a number between 1 and 100 and tell me what it is."
		}
		cfg.SessionStore = store
		agent, err = agentkit.NewAgent(cfg)
		if err != nil {
			return err
		}
	} else {
		// 3b. Later runs: the log has messages, so NewAgent would reject this
		//     store with core.ErrSessionNotEmpty. That rejection is the point.
		//     The recovered model and thinking level are CONSTRUCTION inputs:
		//     they have to be in place before the first request is built,
		//     because a resumed run that quietly reverts to the default model
		//     or the default reasoning level produces a regression that only
		//     appears after a restart and that no single-process test sees.
		fmt.Fprintf(os.Stderr, "resuming: %d message(s) recovered from the log\n", len(resume.Messages))
		fmt.Fprintf(os.Stderr, "  provenance: provider=%q api=%q model=%q thinking=%q\n",
			resume.Provider, resume.API, resume.ModelID, resume.ThinkingLevel)
		if prompt == "" {
			prompt = "What number did you pick? Answer with just the number."
		}
		cfg.SessionStore = store
		agent, err = agentkit.NewAgentFromSession(cfg, resume, resolveModel)
		if err != nil {
			return err
		}
	}

	if err := checkCredentials(model); err != nil {
		return err
	}

	res, err := agent.Run(context.Background(), prompt)
	if err != nil {
		return err
	}

	fmt.Println(res.FinalText())
	u := res.Usage
	fmt.Fprintf(os.Stderr, "\n[%s · %d turns · in %d / out %d tokens · $%.5f]\n",
		model.ID, res.TurnCount, u.InputTokens, u.OutputTokens, u.CostUSD)
	fmt.Fprintln(os.Stderr, "run it again to ask a follow-up against this transcript")
	return nil
}

// 4. resolveModel maps the recovered provenance TRIPLE back to a descriptor.
//
// All three fields matter, and dropping any one of them is a silent bug.
// The model id alone does not say who served it: the same id under a
// different vendor is a different endpoint, different pricing and often a
// different wire dialect. And "same model" is computed over the whole triple
// — provider, api and id — so recovering two of three makes the first
// post-resume request look like a model change, which downgrades every signed
// thinking block in the transcript to plain text and strips the signatures
// that made them replayable.
//
// It is a callback rather than a catalog call inside the SDK so that resume
// works for an embedder with its own model registry, and so the session
// package does not depend on the catalog.
func resolveModel(vendor string, api core.API, modelID string) (*core.Model, error) {
	m, err := catalog.ResolveModel(vendor + "/" + modelID)
	if err != nil {
		return nil, err
	}
	// The log wins over the catalog on the wire API: the transcript was
	// produced over that dialect, and the descriptor here is a deep copy, so
	// correcting it is safe and affects nothing else.
	if api != "" && m.API != api {
		m.API = api
	}
	return m, nil
}

// modelSpec is "vendor/model-id", or a bare id when it is unambiguous. On a
// resume it is only the fallback: the log's own model wins.
func modelSpec() string {
	if s := os.Getenv("AGENTKIT_MODEL"); s != "" {
		return s
	}
	return "anthropic/claude-sonnet-5"
}

// checkCredentials fails BEFORE the request with a message naming the variable
// to set, rather than after a 401 that names none of them.
//
// The three-state check matters: a deployment using an instance role or ADC
// has no key this process can read and a transport that will nonetheless
// authenticate, so "ambient" must pass a pre-flight that "none" fails.
func checkCredentials(m *core.Model) error {
	auth := provider.ResolveAuth(authFor(m), provider.Env{})
	if auth.State != provider.CredentialNone {
		return nil
	}
	return fmt.Errorf("no credential for vendor %q: set one of %s (see examples/README.md)",
		m.Provider, strings.Join(varNames(authFor(m)), ", "))
}

func authFor(m *core.Model) provider.VendorAuth {
	switch m.API {
	case anthropic.API:
		return anthropic.VendorAuth
	case google.API:
		return google.VendorAuth
	case ollama.API:
		return ollama.VendorAuth
	default:
		// openai-completions and openai-responses share one per-vendor table,
		// keyed on the VENDOR: "openai", "openrouter", "groq", "deepseek"…
		return openai.AuthFor(m.Provider)
	}
}

func varNames(v provider.VendorAuth) []string {
	out := make([]string, 0, len(v.Vars)+1)
	for _, e := range v.Vars {
		out = append(out, e.Name)
	}
	if v.BaseURLVar != "" {
		out = append(out, v.BaseURLVar+" (for a gateway or a local server)")
	}
	if len(out) == 0 {
		out = append(out, "a vendor-specific API key")
	}
	return out
}
