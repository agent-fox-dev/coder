// Command interactive lets you type while the agent is working.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/interactive
//
// It is a small REPL. Ask for something slow — "list every Go file here and
// summarise each one" — and then, while it is still working, type:
//
//	just the loop files, skip the rest      steer the RUNNING turn
//	/follow now write it as a table         queue a follow-up for when it finishes
//	/abort                                  stop the turn, keep what it produced
//	/phase                                  what is it doing right now
//	/snapshot                               a consistent view, safe mid-turn
//
// These are the seams of section 6.13, and they are the part a consumer
// cannot add from outside. Transport, framing and RPC are ordinary code
// anybody can write on top; a phase predicate, a snapshot with producer
// identity, an abort reachable without owning the Run goroutine, and mid-run
// message queues with defined drain points all require changing the loop —
// which means forking. So they are here, and AgentKit still ships no daemon.
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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
		agentkit.StopAfterTurns(20),
		agentkit.StopOverBudget(2.00),
	)
	cfg.SystemPrompt = "You are a helpful assistant. Use the slow_work tool when " +
		"asked to do something that takes a while, so the user can watch it happen."

	// 1. Both queues default to one-at-a-time: a single poll yields at most one
	//    message. QueueDrainAll delivers everything waiting instead. The
	//    default is the conservative one because two steering messages
	//    delivered into a single turn is rarely what a typist meant.
	cfg.SteeringQueueMode = core.QueueOneAtATime
	cfg.FollowUpQueueMode = core.QueueOneAtATime

	if err := checkCredentials(model); err != nil {
		return err
	}
	agent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return err
	}
	if err := agent.RegisterTool(slowWorkTool()); err != nil {
		return err
	}

	fmt.Println("Type a prompt and press enter. While a run is in flight, plain text")
	fmt.Println("steers it; /follow queues a follow-up; /abort stops it; /phase and")
	fmt.Println("/snapshot inspect it. /quit exits.")

	// 2. One goroutine owns stdin for the whole program, not just between
	//    runs. That is the entire point: a REPL that only reads input when it
	//    is idle cannot deliver anything INTO a turn, and every mid-run
	//    control below would be unreachable.
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for line := range lines {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}

		// 3. Every one of these is safe to call from this goroutine while a
		//    turn is in flight on another (REQ-LIFE-03). None of them blocks,
		//    and none of them needs the context that started the run.
		switch {
		case text == "/quit":
			agent.Abort()
			return nil

		case text == "/abort":
			// Abort takes no arguments and no context precisely so a caller
			// that does not own the Run goroutine can stop the turn. It is
			// idempotent and a no-op when idle.
			agent.Abort()
			continue

		case text == "/phase":
			// Phase must be cheap, non-blocking and callable from anywhere,
			// including from inside a hook. It is backed by an atomic, never
			// by a lock that could be held across a model call.
			fmt.Printf("[phase: %s · idle: %v]\n", agent.Phase(), agent.Idle())
			continue

		case text == "/snapshot":
			printSnapshot(agent)
			continue

		case strings.HasPrefix(text, "/follow "):
			// A follow-up is polled only when the inner loop is EXHAUSTED. It
			// restarts the outer loop inside the same run: no second start
			// event, no done event in between, one RunResult.
			if err := agent.FollowUpText(strings.TrimPrefix(text, "/follow ")); err != nil {
				fmt.Fprintln(os.Stderr, "queue:", err)
				continue
			}
			fmt.Println("[queued as a follow-up]")
			continue
		}

		// 4. Plain text while a run is active is STEERING: it is drained at
		//    the head of the next iteration, immediately before the provider
		//    request — never between an assistant response and its tool
		//    results, which would leave a tool_use with no answer and make
		//    the request invalid.
		//
		//    Pending steering also keeps the inner loop alive when the
		//    assistant produced no tool calls, so a steer that lands on the
		//    last turn still gets a reply rather than arriving after the run
		//    has ended.
		if !agent.Idle() {
			if err := agent.SteerText(text); err != nil {
				fmt.Fprintln(os.Stderr, "steer:", err)
				continue
			}
			fmt.Println("[steering the running turn]")
			continue
		}

		// 5. Idle: this is a new run. Run and Stream FAIL rather than queue
		//    when a turn is already active (ErrBusy) — a prompt queued behind
		//    a running turn was written against a transcript the user could
		//    see, and by the time it ran the transcript would have changed
		//    underneath it. Whether to retry, queue or reject is the caller's
		//    decision, which is why the library does not make it.
		stream, err := agent.Stream(context.Background(), text)
		if err != nil {
			if errors.Is(err, core.ErrBusy) {
				fmt.Fprintln(os.Stderr, "[busy: the previous turn is still finishing]")
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			consume(stream)
		}()
	}
	return nil
}

// consume renders one run. It is on its own goroutine so the input loop above
// keeps reading — which is what makes steering possible at all.
func consume(stream *core.EventStream) {
	for event := range stream.Events() {
		switch e := event.(type) {
		case core.TextDeltaEvent:
			fmt.Print(e.Delta)
		case core.ToolExecutionStartEvent:
			fmt.Printf("\n[%s…]\n", e.Name)
		case core.ToolExecutionEndEvent:
			fmt.Printf("[%s done in %dms]\n", e.Name, e.ElapsedMS)
		case core.TurnEndEvent:
			fmt.Printf("\n[turn %d ended: %s]\n", e.TurnIndex, e.Message.StopReason)
		}
	}
	res, err := stream.RunResult()
	switch {
	case errors.Is(err, core.ErrAborted), errors.Is(err, context.Canceled):
		// An abort is a normal outcome. The partial answer is already in the
		// transcript, and the run ends at a turn boundary with every tool
		// result in place — so the session stays resumable.
		fmt.Printf("\n[aborted · %d turns · $%.5f]\n", res.TurnCount, res.Usage.CostUSD)
	case err != nil:
		fmt.Fprintf(os.Stderr, "\n[run failed: %v]\n", err)
	default:
		fmt.Printf("\n[%s · %d turns · $%.5f] > ", res.StopReason, res.TurnCount, res.Usage.CostUSD)
	}
}

// printSnapshot shows the REQ-LIFE-02 resync target.
func printSnapshot(agent *agentkit.Agent) {
	snap, err := agent.Snapshot(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "snapshot:", err)
		return
	}
	// Idle false means the snapshot is CONSISTENT, not COMPLETE: the in-flight
	// turn is not in it. A caller persisting for resume either waits for idle
	// or accepts that the interrupted turn replays from its last completed
	// turn boundary.
	fmt.Printf("[snapshot: %d messages · revision %d · phase %s · complete: %v]\n",
		len(snap.Messages), snap.Revision, snap.Phase, snap.Idle)
	// ProducerID must be compared BEFORE Revision. Revisions from two
	// producers are unordered, and a snapshot restored into a new Agent
	// restarts the counter — so a strict revision-monotonicity guard is right
	// for a live stream and wrong at the moment the stream restarts.
	fmt.Printf("[producer: %s]\n", snap.ProducerID)
}

// slowWorkTool exists so there is a turn long enough to type into. A real
// agent's slow turns are model latency and tool execution; this just makes
// them reproducible.
func slowWorkTool() core.Tool {
	return core.Tool{
		Name:        "slow_work",
		Description: "Perform a slow unit of work. Call it once per item you are processing.",
		InputSchema: schema.Object(
			schema.Prop("item", schema.String("What is being worked on")),
			schema.Opt("seconds", schema.Int("How long it takes, 1-5")),
		),
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			var args struct {
				Item    string `json:"item"`
				Seconds int    `json:"seconds"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return nil, err
			}
			if args.Seconds <= 0 || args.Seconds > 5 {
				args.Seconds = 2
			}
			select {
			case <-time.After(time.Duration(args.Seconds) * time.Second):
			case <-ctx.Done():
				// The handler receives the run's context, so an abort kills
				// work in progress. What an abort must NOT do is re-decide
				// whether a handler runs: that decision is made once, before
				// the batch starts, or the scheduler splits the batch and
				// leaves phantom side effects behind.
				return nil, ctx.Err()
			}
			return json.Marshal(map[string]any{"item": args.Item, "status": "done"})
		},
	}
}

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
