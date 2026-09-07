package agentkit

import (
	"os"
	"path/filepath"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/skills"
)

// SkillsConfigFor derives the skills discovery configuration from an agent
// config, so that AgentConfig.TrustProject (§5, REQ-SKILL-12, REQ-SEC-10) is
// the ONE place an embedder states trust and the skills package sees the
// same answer the loop does.
//
// The two-step shape — derive the config, discover, hand the rendered block
// to the prompt — is deliberate: discovery is the embedder's affirmative act
// (REQ-SKILL-04) and the loop never performs it on its own. This helper only
// guarantees that the act, when taken, carries the trust decision recorded on
// the agent config rather than a second copy that can disagree with it.
//
// The user-global tier follows REQ-SKILL-12.3: when the home directory is
// unresolvable — containers, CI, cron — it is skipped entirely, never
// resolved relatively. The built-in tier is opt-in through builtinDir; empty
// skips it.
// LoadSkills selects the skills for this run, records every one of them in
// the session audit event (REQ-SKILL-11 / REQ-OBS-04), and returns the
// selection ready for SkillBlocks.
//
// It is one call rather than three because the audit is the step an embedder
// forgets: with `LoadForSession` + `AuditSkills` + `SkillBlocks` written out
// separately, dropping the middle one is a refactor away and nothing fails —
// the skills still reach the model and the audit trail silently stops saying
// which. Here the selection cannot be obtained without the event being
// emitted.
//
// Discovery stays the embedder's affirmative act (REQ-SKILL-04, REQ-SEC-10):
// this takes a registry the caller discovered, it does not go looking.
func (a *Agent) LoadSkills(reg *skills.Registry, archetype, taskPrompt string, cfg skills.Config) []skills.Skill {
	if reg == nil {
		return nil
	}
	return skills.Record(a, reg.LoadForSession(archetype, taskPrompt, cfg))
}

func SkillsConfigFor(cfg core.AgentConfig, workDir, builtinDir string) skills.Config {
	home := ""
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		home = h
	}
	return skills.Config{
		BuiltinDir:   builtinDir,
		HomeDir:      home,
		WorkDir:      workDir,
		TrustProject: cfg.TrustProject,
	}
}
