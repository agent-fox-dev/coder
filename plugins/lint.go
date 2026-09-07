package plugins

import "strings"

// This file is REQ-SKILL-09's half of the import lint.
//
// It is the SAME walk, the same parser and the same BadImport type as
// REQ-PLUGIN-09's LintImports, run against a LARGER prohibited set: skill
// plugin code may not import agentkit internals (REQ-PLUGIN-09), an LLM client
// library, or a model API package. A second linter would drift from the first
// one — different directory skips, different unparseable-file handling — and
// then the two requirements would be enforced differently for no reason, so
// both predicates share lintImports.
//
// REQ-SEC-07's honesty applies here verbatim and is worth restating where a
// reader will meet it: with build-time Go module linkage (REQ-PLUGIN-08) skill
// plugin code runs in this process with these privileges. This is an
// IMPORT-PATH LINT, NOT A SANDBOX. It catches a skill reaching for an
// Anthropic or OpenAI client; it does not stop one from opening a socket with
// net/http and talking to the same API by hand.

// skillForbiddenPrefixes is REQ-SKILL-09's addition to the prohibited set: the
// LLM client libraries and model API packages a skill plugin has no business
// importing, plus AgentKit's own model path.
//
// The list is deliberately SHORT and NAMED. It is not an attempt to enumerate
// every LLM library in existence — that race cannot be won, and the lint's
// honest claim (above) is not that it makes exfiltration impossible but that a
// skill which calls a model directly is caught by review rather than shipped.
// What it does enumerate is the set an actual skill would reach for first.
//
// Why a skill must not hold a model client at all: a skill's tools run inside
// the host's session, on the host's credentials and budget. A skill that opens
// its own model connection escapes every accounting, hook and policy the SDK
// applies to the session's own calls (§6.10, REQ-TOOL-10), and the transcript
// the host persists no longer describes what the run did.
var skillForbiddenPrefixes = []string{
	// First-party model SDKs.
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/openai/openai-go",
	"github.com/sashabaranov/go-openai",
	"google.golang.org/genai",
	"github.com/google/generative-ai-go",
	"cloud.google.com/go/vertexai",
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
	// Frameworks that embed a model client.
	"github.com/tmc/langchaingo",
	// AgentKit's own backend and session packages. REQ-SKILL-08 says skill
	// plugin code may not directly invoke internal session or backend
	// packages; these are those packages, and they are not under
	// `internal/`, so REQ-PLUGIN-09's prefix does not cover them.
	"github.com/agentfox/agentkit-go/provider",
	"github.com/agentfox/agentkit-go/session",
}

// SkillForbiddenImportPrefixes returns the extra import prefixes REQ-SKILL-09
// forbids skill plugin code, for a caller that wants to report or document
// them.
//
// It returns a COPY, and the underlying list is unexported, because REQ-SEC-06
// makes skill allowlist extensions additive only: a skill (or an embedder
// acting on a skill's behalf) must not be able to shorten this list. An
// exported slice variable would let any package in the process delete an entry
// at init time and quietly turn the check off.
func SkillForbiddenImportPrefixes() []string {
	return append([]string(nil), skillForbiddenPrefixes...)
}

// LintSkillImports is REQ-SKILL-09: LintImports' rules plus the model-client
// prefixes above. A non-empty result must reject the skill at load time.
func LintSkillImports(dir string) ([]BadImport, error) {
	return lintImports(dir, forbiddenSkillImport)
}

// forbiddenSkillImport matches a prefix at a PATH SEGMENT boundary, never as a
// bare substring: `github.com/openai/openai-go` and everything under it is
// refused, while an unrelated `github.com/openai/openai-go-community-fork`
// is a different module and is not this rule's business.
func forbiddenSkillImport(path string) bool {
	if forbiddenImport(path) {
		return true
	}
	for _, p := range skillForbiddenPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
