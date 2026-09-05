package agentkit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// AgentFactory builds a fresh child agent for one delegation.
//
// It is a FACTORY, not an *Agent, and that is the whole design of REQ-MULTI.
// Handing SubagentTool a single agent value would look correct and fail under
// exactly the condition delegation exists for: the orchestrator emits two
// parallel calls to the same specialist, the second finds the run slot taken
// and returns ErrBusy (REQ-LOOP-15). "Each child is an independent value" is
// what makes parallel delegation safe BY CONSTRUCTION (REQ-MULTI-04), and a
// shared instance is not one.
type AgentFactory func(ctx context.Context) (*Agent, error)

// SubagentOptions configures a delegation tool.
type SubagentOptions struct {
	Name        string
	Description string
	// PromptField names the argument carrying the child's prompt.
	PromptField string
	// BudgetFraction of the parent's REMAINING budget to grant the child, in
	// (0,1]. Zero means no budget is propagated.
	//
	// It is propagated as an explicit child config field, never as a
	// context.Context value (REQ-MULTI-03). A budget smuggled through ctx is
	// invisible to the type system and silently absent whenever a caller
	// passes a bare context.Background() — see the context convention in §5.
	BudgetFraction float64
	// MaxBudgetUSD caps the parent's total spend, and is what
	// BudgetFraction is a fraction of.
	MaxBudgetUSD float64
}

// SubagentTool wraps an agent factory as a tool the orchestrator can call
// (REQ-MULTI-01).
//
// The child ALWAYS starts with fresh, empty history (REQ-MULTI-02). Sharing
// the parent's transcript is prohibited for two independent reasons: it is a
// prompt-injection surface — anything a tool result put in the parent's
// history reaches the child's system context — and it inflates the child's
// input by the whole parent conversation, which is the cost delegation was
// supposed to avoid.
func SubagentTool(parent *Agent, factory AgentFactory, opts SubagentOptions) core.Tool {
	if opts.PromptField == "" {
		opts.PromptField = "prompt"
	}
	desc := opts.Description
	if desc == "" {
		desc = "Delegate a task to the " + opts.Name + " specialist."
	}

	return core.Tool{
		Name:        opts.Name,
		Description: desc,
		InputSchema: schema.Object(
			schema.Prop(opts.PromptField, schema.String(
				"The complete task for the specialist. It sees NONE of this "+
					"conversation, so include every detail it needs.")),
		),
		// Parallel is correct here precisely because the factory hands out an
		// independent agent per call.
		ExecutionMode: core.Parallel,
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var args map[string]any
			if err := json.Unmarshal(in, &args); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			prompt, _ := args[opts.PromptField].(string)
			if prompt == "" {
				return core.ErrResult("invalid_arguments",
					fmt.Sprintf("%q is required and must be a non-empty string", opts.PromptField))
			}

			child, err := factory(ctx)
			if err != nil {
				return core.ErrResult("subagent_construction_failed", err.Error())
			}
			if child.history.Len() != 0 {
				// A factory that returned a pre-populated agent has defeated
				// REQ-MULTI-02. Fail loudly rather than leak the transcript.
				return core.ErrResult("subagent_history_not_empty",
					"the agent factory returned an agent with non-empty history; "+
						"a child must always start fresh (REQ-MULTI-02)")
			}

			if opts.BudgetFraction > 0 && opts.MaxBudgetUSD > 0 {
				remaining := opts.MaxBudgetUSD - parent.Usage().CostUSD
				if remaining <= 0 {
					return core.ErrResult("budget_exhausted",
						"the parent has no remaining budget to delegate")
				}
				slice := remaining * opts.BudgetFraction
				child.mu.Lock()
				existing := child.cfg.StopPolicy
				child.cfg.StopPolicy = StopAny(existing, StopOverBudget(slice))
				child.mu.Unlock()
			}

			res, err := child.Run(ctx, prompt)
			if err != nil {
				// A child that failed is not a parent that failed. The
				// orchestrator sees an error result and can try something
				// else; propagating would end the whole run.
				return core.ErrResult("subagent_failed", err.Error())
			}
			out := core.OKResult(map[string]any{
				"result": res.FinalText(),
				"turns":  res.TurnCount,
			})
			out.Metadata = &core.ToolMetadata{DurationMS: 0}
			return out
		},
	}
}

// RunParallel runs fn over items concurrently and returns the results in INPUT
// ORDER, with a per-item error slot.
//
// This is the fan-out REQ-MULTI-04 describes. The PRD names errgroup for it,
// and errgroup is genuinely appropriate HERE — each child is an independent
// run with its own transcript, so cancelling siblings on the first failure is
// a defensible policy — but errgroup lives in golang.org/x/sync, which the
// dependency gate forbids. Hand-rolling it is a few lines, and it lets the
// semantics differ from the intra-batch tool executor deliberately rather than
// by accident: there, a failing tool must NOT cancel its peers (REQ-GO-04);
// here, an abandoned delegation tree should stop burning budget.
func RunParallel[T, R any](ctx context.Context, items []T, fn func(context.Context, T) (R, error)) ([]R, []error) {
	results := make([]R, len(items))
	errs := make([]error, len(items))
	if len(items) == 0 {
		return results, errs
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i, it := range items {
		wg.Add(1)
		go func(i int, it T) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("agentkit: panic in parallel task %d: %v", i, r)
					once.Do(func() { firstErr = errs[i]; cancel() })
				}
			}()
			r, err := fn(ctx, it)
			results[i] = r
			if err != nil {
				errs[i] = err
				once.Do(func() { firstErr = err; cancel() })
			}
		}(i, it)
	}
	wg.Wait()
	_ = firstErr
	return results, errs
}
