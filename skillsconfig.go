package agentkit

import "github.com/agentfox/agentkit-go/skills"

// LoadSkills selects the skills for this run, records every one of them in
// the session audit event (REQ-SKILL-11 / REQ-OBS-04), and returns the
// selection ready for prompt.SkillBlocks.
//
// It is one call rather than three because the audit is the step an embedder
// forgets: with `LoadForSession` + `AuditSkills` + `prompt.SkillBlocks` written out
// separately, dropping the middle one is a refactor away and nothing fails —
// the skills still reach the model and the audit trail silently stops saying
// which. Here the selection cannot be obtained without the event being
// emitted.
//
// Discovery stays the embedder's affirmative act (REQ-SKILL-04, REQ-SEC-10):
// this takes a registry the caller discovered, it does not go looking.
//
// Injection is a second call, and it is SetPromptBlocks rather than a field
// write, because the selection is only half of what reaches the model — the
// project-context files of REQ-CTX-01 come from DiscoverContext and the tool
// the block names comes from the resolved tool set:
//
//	cfg := skills.ConfigFor(agentCfg, workDir, skills.BuiltinDir())
//	sel := agent.LoadSkills(skills.Discover(cfg), archetype, task, cfg)
//	files, _ := skills.DiscoverContext(cfg)
//	err := agent.SetPromptBlocks(prompt.SkillBlocks(sel, files, agent.Tools()))
func (a *Agent) LoadSkills(reg *skills.Registry, archetype, taskPrompt string, cfg skills.Config) []skills.Skill {
	if reg == nil {
		return nil
	}
	return skills.Record(a, reg.LoadForSession(archetype, taskPrompt, cfg))
}
