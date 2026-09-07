// Command streaming renders a run as it happens and lets you interrupt it.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/streaming "Write a haiku about mutexes, then count its syllables."
//
// Press Ctrl-C while it is thinking: the run stops at the next checkpoint, the
// partial answer stays in the transcript, and the program exits cleanly.
//
// Two things here are easy to get wrong and are the reason this example
// exists. Events come in two classes — incremental deltas that may be
// coalesced or absent entirely, and authoritative events that are complete and
// final for the item they name. A consumer that applies BOTH for the same item
// double-counts, so an authoritative event REPLACES what you accumulated. And
// the abort is out-of-band: it is a method on the agent, callable from a
// signal handler that does not own the goroutine that called Stream.
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/schema"
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
		prompt = "Write a haiku about mutexes, then count its syllables with the tool."
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
	cfg.StopPolicy = agentkit.StopAny(
		agentkit.StopAfterTurns(10),
		agentkit.StopOverBudget(1.00),
	)

	if err := checkCredentials(model); err != nil {
		return err
	}

	agent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return err
	}
	// A tool, so the run has something to stream beyond text. Its schema is a
	// value built with combinators rather than a JSON literal, because the
	// schema is rewritten before it reaches the wire and is used to coerce and
	// validate what comes back.
	if err := agent.RegisterTool(syllableTool()); err != nil {
		return err
	}

	// 1. Abort is out-of-band. Agent.Abort takes no context precisely so a
	//    caller that does not own the Run goroutine — a signal handler, an
	//    RPC handler, a UI event loop — can stop the turn. It is idempotent
	//    and a no-op when idle.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\n[interrupt: stopping at the next checkpoint]")
		agent.Abort()
	}()

	// 2. Stream returns immediately. The producer never blocks on this
	//    consumer, so a slow terminal cannot stall the model call or trip the
	//    provider's idle timeout.
	stream, err := agent.Stream(context.Background(), prompt)
	if err != nil {
		return err
	}

	var textSoFar strings.Builder
	for event := range stream.Events() {
		switch e := event.(type) {

		// ---- incremental: an optimization, never the source of truth. A
		// non-streaming provider emits none of these at all.
		case core.TextDeltaEvent:
			fmt.Print(e.Delta)
			textSoFar.WriteString(e.Delta)

		case core.ThinkingDeltaEvent:
			fmt.Fprint(os.Stderr, dim(e.Delta))

		// ---- authoritative: complete and final for the item it names,
		// exactly one per item. Receiving this means DISCARD the deltas you
		// accumulated for that item and take this payload whole.
		case core.TextEndEvent:
			textSoFar.Reset()
			textSoFar.WriteString(e.Text)

		case core.ToolCallStartEvent:
			fmt.Printf("\n[model is calling %s]\n", e.Name)

		case core.ToolExecutionEndEvent:
			status := "ok"
			if e.IsError {
				status = "error"
			}
			fmt.Printf("[%s finished: %s, %dms]\n", e.Name, status, e.ElapsedMS)

		case core.TurnEndEvent:
			fmt.Fprintf(os.Stderr, "\n[turn %d: %s, %d output tokens]\n",
				e.TurnIndex, e.Message.StopReason, e.Message.Usage.OutputTokens)

		case core.ErrorEvent:
			fmt.Fprintf(os.Stderr, "\n[stream error: %s]\n", e.Message)
		}
	}

	// 3. The result is fed by the terminal event, not by consumption, which is
	//    what makes abandoning a stream safe: a caller that reads no event at
	//    all can still ask for the result here.
	res, err := stream.RunResult()

	// An abort is a normal outcome, not a failure. The partial answer is
	// already in the transcript.
	if errors.Is(err, core.ErrAborted) || errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "\n[aborted after %d turns; $%.5f spent]\n",
			res.TurnCount, res.Usage.CostUSD)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n[done: %s · %d turns · $%.5f]\n",
		res.StopReason, res.TurnCount, res.Usage.CostUSD)
	return nil
}

func syllableTool() core.Tool {
	return core.Tool{
		Name:        "count_syllables",
		Description: "Count the syllables in a line of English text",
		InputSchema: schema.Object(
			schema.Prop("text", schema.String("The line to count")),
		),
		PromptGuidelines: []string{"Use count_syllables rather than counting by ear."},
		Execute: func(_ context.Context, in json.RawMessage) core.ToolResult {
			var args struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			return core.OKResult(map[string]any{"syllables": syllables(args.Text)})
		},
	}
}

// syllables is a crude vowel-group count. It is deliberately simple: the point
// of the example is the event stream, not English phonology.
func syllables(s string) int {
	total := 0
	for _, word := range strings.Fields(strings.ToLower(s)) {
		inVowel, n := false, 0
		for _, r := range word {
			isVowel := strings.ContainsRune("aeiouy", r)
			if isVowel && !inVowel {
				n++
			}
			inVowel = isVowel
		}
		if strings.HasSuffix(word, "e") && n > 1 {
			n--
		}
		if n == 0 {
			n = 1
		}
		total += n
	}
	return total
}

func dim(s string) string { return "\033[2m" + s + "\033[0m" }

// modelSpec is "vendor/model-id", or a bare id when it is unambiguous.
func modelSpec() string {
	if s := os.Getenv("AGENTKIT_MODEL"); s != "" {
		return s
	}
	return "anthropic/claude-sonnet-4-5"
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
