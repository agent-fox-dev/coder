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

// ---------------------------------------------------------------- REQ-MULTI-05

// AgentDefinition is a named specialist (REQ-MULTI-05): everything needed to
// construct a child agent, registered by name so the parent model can invoke
// it as a tool call. The "tool allowlist" is the REQ-TOOL-10 ToolPolicy,
// applied uniformly to built-in and caller-supplied tools, so a specialist can
// be scoped to read-and-search-only per delegation without rebuilding the tool
// set by hand.
type AgentDefinition struct {
	Name         string
	Description  string
	SystemPrompt string
	// Model may differ from the parent's (REQ-PROV-08). Nil inherits it.
	Model      *core.Model
	ToolPolicy core.ToolPolicy
	StopPolicy core.StopPolicy
	// Tools are registered on every child built from this definition, before
	// ToolPolicy resolves them.
	Tools []core.Tool
	// BudgetFraction is SubagentOptions.BudgetFraction for this specialist.
	BudgetFraction float64
}

// AgentRegistry holds specialists by name. It is a value the embedder owns —
// never a package-level global (NFR-SEC-05) — and a name is registered once;
// a duplicate is an error rather than a silent replacement, because the tool
// the parent model sees is the name.
type AgentRegistry struct {
	mu   sync.Mutex
	defs map[string]AgentDefinition
	list []string
}

func NewAgentRegistry() *AgentRegistry { return &AgentRegistry{defs: map[string]AgentDefinition{}} }

func (r *AgentRegistry) Register(def AgentDefinition) error {
	if def.Name == "" {
		return fmt.Errorf("agentkit: AgentDefinition has no name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.defs[def.Name]; dup {
		return fmt.Errorf("agentkit: specialist %q is already registered", def.Name)
	}
	r.defs[def.Name] = def
	r.list = append(r.list, def.Name)
	return nil
}

// Lookup returns a definition by name.
func (r *AgentRegistry) Lookup(name string) (AgentDefinition, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[name]
	return d, ok
}

// Names lists registered specialists in registration order.
func (r *AgentRegistry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.list...)
}

// Tools builds one delegation tool per registered specialist, each backed by
// a factory that constructs a FRESH child from the definition and the
// parent's config (providers, credentials, plugins, tracer) on every call
// (REQ-MULTI-02/04). The child inherits the parent's model unless the
// definition names its own.
func (r *AgentRegistry) Tools(parent *Agent, maxBudgetUSD float64) []core.Tool {
	names := r.Names()
	out := make([]core.Tool, 0, len(names))
	for _, n := range names {
		def, ok := r.Lookup(n)
		if !ok {
			continue
		}
		factory := func(context.Context) (*Agent, error) { return NewAgentFromDefinition(parent, def) }
		out = append(out, SubagentTool(parent, factory, SubagentOptions{
			Name:           def.Name,
			Description:    def.Description,
			BudgetFraction: def.BudgetFraction,
			MaxBudgetUSD:   maxBudgetUSD,
		}))
	}
	return out
}

// NewAgentFromDefinition constructs a fresh child from a definition. The
// parent's infrastructure fields carry over; its history, session store,
// queues and system prompt do not (REQ-MULTI-02).
func NewAgentFromDefinition(parent *Agent, def AgentDefinition) (*Agent, error) {
	parent.mu.Lock()
	pcfg := parent.cfg
	parent.mu.Unlock()

	cfg := core.AgentConfig{
		Model:          pcfg.Model,
		Provider:       pcfg.Provider,
		MaxTokens:      pcfg.MaxTokens,
		Temperature:    pcfg.Temperature,
		TopP:           pcfg.TopP,
		SystemPrompt:   def.SystemPrompt,
		StopPolicy:     def.StopPolicy,
		ParallelTools:  pcfg.ParallelTools,
		ThinkingLevel:  pcfg.ThinkingLevel,
		ToolPolicy:     def.ToolPolicy,
		BeforeToolCall: pcfg.BeforeToolCall,
		AfterToolCall:  pcfg.AfterToolCall,
		Middleware:     pcfg.Middleware,
		Plugins:        pcfg.Plugins,
		Tracer:         pcfg.Tracer,
		Attribution:    pcfg.Attribution,
		CacheRetention: pcfg.CacheRetention,
		RequestOptions: pcfg.RequestOptions,
		Providers:      pcfg.Providers,
		TrustProject:   pcfg.TrustProject,
	}
	if def.Model != nil {
		cfg.Model = def.Model
	}
	if cfg.StopPolicy == nil {
		cfg.StopPolicy = pcfg.StopPolicy
	}
	child, err := NewAgent(cfg)
	if err != nil {
		return nil, err
	}
	for _, t := range def.Tools {
		if err := child.RegisterTool(t); err != nil {
			return nil, err
		}
	}
	return child, nil
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

	// The first failure cancels the siblings; every task's own outcome is
	// still reported in its slot, so the caller sees which child failed and
	// which were cancelled because of it.
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it T) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("agentkit: panic in parallel task %d: %v", i, r)
					cancel()
				}
			}()
			r, err := fn(ctx, it)
			results[i] = r
			if err != nil {
				errs[i] = err
				cancel()
			}
		}(i, it)
	}
	wg.Wait()
	return results, errs
}
