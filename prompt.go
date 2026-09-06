package agentkit

import (
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/skills"
	"github.com/agentfox/agentkit-go/tools"
)

// The assembled system prompt (NFR-TEST-08a, REQ-TOOL-04e, REQ-SKILL-06).
//
// Until this existed, core.Tool.PromptGuidelines was a field nothing read and
// the loop sent AgentConfig.SystemPrompt verbatim — so a tool could declare
// guidance the model never saw. NFR-TEST-08(a) asks for a golden of the
// assembled prompt "built through the real tool resolver", which needs a real
// assembler to build it.

// PromptSection names a block of the assembled prompt. The order of this
// declaration is the order they appear, and the golden pins it.
type PromptSection string

const (
	SectionBase       PromptSection = "base"
	SectionTools      PromptSection = "tools"
	SectionGuidelines PromptSection = "guidelines"
	SectionSkills     PromptSection = "skills"
)

// BaseInstructions is the built-in opening section.
//
// Deliberately short. Everything specific to what the agent can actually do
// comes from the resolved tool set, which is the part that changes; a long
// fixed preamble is tokens paid on every request to say things the tool
// descriptions already say.
const BaseInstructions = `You are an AI assistant with access to tools.

Work from evidence: read before you edit, and verify a change rather than
assuming it worked. When a tool reports an error, read the error — it names
what to do differently. Prefer one correct call to several speculative ones.`

// UniversalGuidelines are appended after the per-tool ones (NFR-TEST-08a:
// "appends the universal guidelines last").
//
// Last because they are the weakest: a per-tool guideline is specific advice
// about a tool the model is looking at, and burying it under general
// exhortations is how it stops being read.
var UniversalGuidelines = []string{
	"Do not guess at file contents or APIs; read them.",
	"Report what you actually did, including what failed.",
}

// PromptInput is everything the assembler needs.
type PromptInput struct {
	// Custom replaces the BUILT-IN sections when non-empty (AgentConfig.SystemPrompt).
	Custom string
	// Tools is the set actually active after the REQ-TOOL-10 policy resolved,
	// never the registry. The guidelines the model sees must describe the
	// tools it has.
	Tools []core.Tool
	// ExtraBlocks are appended after the built-in sections, in order. This is
	// where the skills and project-context block goes (SkillBlocks builds it).
	//
	// It is an opaque []string rather than typed skill values because
	// AgentConfig lives in core, and core cannot import skills without
	// inverting the package graph. Handing the assembler finished text also
	// keeps discovery where REQ-SKILL-04 puts it: an affirmative act by the
	// embedder, not something the loop does on its own.
	ExtraBlocks []string
}

// BuildSystemPrompt assembles the prompt.
//
// A CUSTOM prompt replaces the built-in base and the built-in guidelines, and
// nothing else. Skills and project context still append: an embedder enables
// those by a separate affirmative act — discovery, and REQ-SEC-10's project
// trust — and having a custom prompt silently switch them off would mean the
// trust decision quietly stopped applying.
func BuildSystemPrompt(in PromptInput) string {
	var blocks []string

	if in.Custom != "" {
		blocks = append(blocks, strings.TrimRight(in.Custom, "\n"))
	} else {
		blocks = append(blocks, BaseInstructions)
		if g := guidelinesBlock(in.Tools); g != "" {
			blocks = append(blocks, g)
		}
	}

	for _, b := range in.ExtraBlocks {
		if b = strings.TrimRight(b, "\n"); b != "" {
			blocks = append(blocks, b)
		}
	}
	return strings.Join(blocks, "\n\n")
}

// SkillBlocks renders the REQ-SKILL-06 skills block and the REQ-CTX project
// context block, ready for PromptInput.ExtraBlocks.
//
// tools must be the ACTIVE set: the block names the tool the model should read
// a skill with, and naming one the model does not have produces a hallucinated
// call and a wasted turn (REQ-SKILL-06.2).
func SkillBlocks(sk []skills.Skill, ctxFiles []skills.ContextFile, active []core.Tool) []string {
	s := skills.Assemble(skills.Input{Skills: sk, ContextFiles: ctxFiles, Tools: active})
	if s == "" {
		return nil
	}
	return []string{s}
}

// guidelinesBlock is NFR-TEST-08a's collection.
//
// Deduplicated while PRESERVING FIRST-SEEN ORDER. Sorting would be tidier and
// wrong: the order tools were resolved in is the order the model reads them,
// and an alphabetical list separates a guideline from the tool it is about.
func guidelinesBlock(active []core.Tool) string {
	var (
		lines []string
		seen  = map[string]bool{}
	)
	add := func(g string) {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		lines = append(lines, "- "+g)
	}

	for _, t := range active {
		for _, g := range t.PromptGuidelines {
			add(g)
		}
	}
	// REQ-TOOL-04e: emitted when the file-navigation tools are ABSENT and
	// execute is present. It cannot be a PromptGuidelines entry on any tool,
	// because a per-tool field can only fire when its tool is there — which is
	// the opposite of the condition.
	if !anyPresent(active, tools.FileNavigationTools()) && hasTool(active, "execute") {
		add(tools.ExecuteFallbackGuideline)
	}
	for _, g := range UniversalGuidelines {
		add(g)
	}

	if len(lines) == 0 {
		return ""
	}
	return "Guidelines:\n" + strings.Join(lines, "\n")
}

func hasTool(set []core.Tool, name string) bool {
	for _, t := range set {
		if t.Name == name {
			return true
		}
	}
	return false
}

func anyPresent(set []core.Tool, names []string) bool {
	for _, n := range names {
		if hasTool(set, n) {
			return true
		}
	}
	return false
}
