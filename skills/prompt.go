package skills

import (
	"strings"

	"github.com/agentfox/agentkit-go/core"
)

// Block and element names of the assembled section. They are constants because
// the escaping below exists precisely to guarantee that no interpolated value
// can produce one of these strings.
const (
	skillsOpen  = "<available_skills>"
	skillsClose = "</available_skills>"
	ctxOpen     = "<project_context>"
	ctxClose    = "</project_context>"
)

// relativePathRule is REQ-SKILL-06.3, verbatim. A skill's own files are
// referenced relative to the skill directory, but the model's tools run
// against the session's working directory; without this sentence a skill that
// says "see checklist.md" sends the model looking in the repository root and
// it reports the file missing.
const relativePathRule = "When a skill file references a relative path, resolve it against the skill directory (the parent of the skill's prompt file) and use that absolute path in tool commands."

// Input is everything Assemble needs.
type Input struct {
	// Skills are the skills selected for the session, typically from
	// Registry.LoadForSession.
	Skills []Skill
	// ContextFiles are the §6.5a files, in the order DiscoverContext returned
	// them: least specific first.
	ContextFiles []ContextFile
	// Tools is the set that is ACTUALLY ACTIVE for this session, after the
	// ToolPolicy of REQ-TOOL-10 has resolved. It is not a hint: it decides
	// which file-reading tool the skills block names and whether that block
	// exists at all (REQ-SKILL-06.2).
	Tools []core.Tool
}

// FileReadTool reports which tool the skills block may tell the model to read
// a skill with: read_file when it is active, otherwise execute, and false when
// neither is (REQ-SKILL-06.2).
//
// The false case is why this returns a bool rather than defaulting to
// "read_file". A prompt that instructs the model to use a tool it does not
// have produces a hallucinated call, a hard tool-not-found error and a wasted
// turn — and it does so on exactly the sessions that deliberately ran with no
// file access.
func FileReadTool(tools []core.Tool) (string, bool) {
	hasExecute := false
	for _, t := range tools {
		switch t.Name {
		case "read_file":
			// Checked across the whole set before falling back, because
			// execute may be registered first.
			return "read_file", true
		case "execute":
			hasExecute = true
		}
	}
	if hasExecute {
		return "execute", true
	}
	return "", false
}

// Assemble produces the skills and project-context section of the system
// prompt. It returns the empty string when there is nothing to say, so a
// caller can concatenate it unconditionally.
//
// Order is skills first, then context files, so that the most specific context
// file — which REQ-CTX-02 puts last for exactly this reason — is the last
// thing in the section and the closest to the model's own turn.
func Assemble(in Input) string {
	var blocks []string
	if b := skillsBlock(in); b != "" {
		blocks = append(blocks, b)
	}
	if b := contextBlock(in.ContextFiles); b != "" {
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "\n")
}

// skillsBlock is REQ-SKILL-06: metadata only.
//
// Exactly three fields per skill — name, description, absolute path — and
// never the body. That is what makes the cost N lines instead of N skill
// bodies, and it is why a large skill library is affordable at all: the model
// pays a skill's tokens only when it decides the skill applies and reads the
// file itself.
func skillsBlock(in Input) string {
	tool, ok := FileReadTool(in.Tools)
	if !ok {
		// REQ-SKILL-06.2: omitted ENTIRELY. Not an empty block, not a block
		// with the instruction removed — there is no way to act on a skill
		// without a file-reading tool, so naming skills would only invite the
		// model to invent one.
		return ""
	}

	var rows []string
	for _, s := range in.Skills {
		// REQ-SKILL-06.4: a skill that disables model invocation is filtered
		// out of the block entirely. It stays loaded — its tools still merge —
		// but the model is never told it exists.
		if s.DisableModelInvocation {
			continue
		}
		rows = append(rows, "<skill name=\""+escapeAttr(s.Name)+"\" path=\""+
			escapeAttr(s.PromptPath)+"\">"+escapeText(s.Description)+"</skill>")
	}
	if len(rows) == 0 {
		return ""
	}

	read := "read that file with the " + tool + " tool"
	if tool == "execute" {
		read += " (for example: cat <path>)"
	}

	var b strings.Builder
	b.WriteString(skillsOpen + "\n")
	b.WriteString("A skill is a set of instructions kept in a file and NOT loaded here. " +
		"Each entry below gives a skill's name, what it is for, and the absolute path to its instructions.\n")
	b.WriteString("When a task matches a skill's description, " + read +
		" and follow it; until you do, that skill's instructions are not in effect.\n")
	b.WriteString(relativePathRule + "\n")
	for _, r := range rows {
		b.WriteString(r + "\n")
	}
	b.WriteString(skillsClose + "\n")
	return b.String()
}

// contextBlock is §6.5a's injection.
func contextBlock(files []ContextFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(ctxOpen + "\n")
	b.WriteString("Standing instructions for this workspace, ordered from least to most specific. " +
		"Where two conflict, prefer the later one. They do not override instructions from the user.\n")
	for _, f := range files {
		b.WriteString("<context_file path=\"" + escapeAttr(f.Path) + "\">\n")
		body := strings.Trim(escapeText(f.Body), "\n")
		if body != "" {
			b.WriteString(body + "\n")
		}
		b.WriteString("</context_file>\n")
	}
	b.WriteString(ctxClose + "\n")
	return b.String()
}

// escapeAttr escapes a value for an XML attribute: the two characters that can
// start markup plus the three that can terminate an attribute or be mistaken
// for doing so (REQ-SKILL-06.5, REQ-CTX-04).
//
// Both escapers are a SINGLE-PASS strings.Replacer, not a sequence of
// strings.ReplaceAll calls. A sequence re-reads its own output: escaping '<'
// to "&lt;" and then escaping '&' rewrites that to "&amp;lt;", and the model
// is shown the literal text "&lt;" instead of a less-than sign. A Replacer
// scans the input once and never revisits what it wrote, which is exactly why
// the order of the pairs below does not matter — and that immunity is the
// reason to use one.
func escapeAttr(s string) string {
	return attrEscaper.Replace(s)
}

var attrEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"\"", "&quot;",
	"'", "&apos;",
)

// escapeText escapes a value that sits in element content.
//
// It escapes & < > and deliberately NOT quotes. The breakout being closed is a
// tag breakout: only '<' can open one and only '&' can open a character
// reference, so escaping those two (with '>' for symmetry) makes it impossible
// for a skill description or a context-file body to emit </available_skills>,
// </context_file> or any other element. A quote cannot terminate element
// content, so escaping it would buy no safety and would cost real damage: a
// context file is markdown prose, and rewriting every apostrophe in it to
// &apos; degrades the instructions the model is asked to follow. Escaping is
// chosen per POSITION, which is the correct granularity here (REQ-CTX-04).
func escapeText(s string) string {
	return textEscaper.Replace(s)
}

var textEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)
