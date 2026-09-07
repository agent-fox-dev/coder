// Command chat is the smallest useful AgentKit program: resolve a model, ask
// it one question, print the answer and what it cost.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/chat "Explain the Go memory model in three sentences."
//
// Any model in the catalog works, and so does one that is not in it — an
// unknown id under a known vendor inherits that vendor's row:
//
//	AGENTKIT_MODEL=openai/gpt-5.6-terra        go run ./examples/chat "hello"
//	AGENTKIT_MODEL=anthropic/some-unreleased-id go run ./examples/chat "hello"
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"fmt"
	"os"
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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "In one sentence: what is an agent loop?"
	}

	// 1. Resolve the model. This is what supplies the wire API, base URL,
	//    context window, pricing and compatibility profile — none of which the
	//    model-ID string carries. It is also where an unknown id under a known
	//    vendor inherits a sibling's row rather than being rejected.
	model, err := catalog.ResolveModel(modelSpec())
	if err != nil {
		return err
	}

	// 2. Register the wire APIs. Nothing is registered by import side effect,
	//    so a program that only wants the loop never drags net/http in. All
	//    five are registered here so any AGENTKIT_MODEL resolves; a real
	//    application registers only the ones it uses.
	cfg := core.AgentConfig{Model: model}
	agentkit.RegisterDefaults(&cfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)

	// 3. A stop policy is required in practice, not optional: without one a
	//    tool-using agent has no upper bound. Even here, where no tool is
	//    registered, it is the honest default.
	cfg.StopPolicy = agentkit.StopAny(
		agentkit.StopAfterTurns(10),
		agentkit.StopOverBudget(1.00), // dollars, cumulative for the run
	)
	cfg.SystemPrompt = "You are concise. Answer in plain prose, no preamble."

	if err := checkCredentials(model); err != nil {
		return err
	}

	agent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return err
	}

	res, err := agent.Run(context.Background(), prompt)
	if err != nil {
		return err
	}

	fmt.Println(res.FinalText())

	// Usage is reported by the provider and priced against the model that
	// actually served the request, so this is the real number and not an
	// estimate.
	u := res.Usage
	fmt.Fprintf(os.Stderr, "\n[%s · %d turns · in %d / out %d tokens · cached %d · $%.5f]\n",
		model.ID, res.TurnCount, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CostUSD)
	return nil
}

// modelSpec is "vendor/model-id", or a bare id when it is unambiguous.
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
