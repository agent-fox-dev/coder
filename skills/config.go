package skills

import (
	"os"
	"path/filepath"

	"github.com/agentfox/agentkit-go/core"
)

// ConfigFor derives the skills discovery configuration from an agent config,
// so that AgentConfig.TrustProject (§5, REQ-SKILL-12, REQ-SEC-10) is the ONE
// place an embedder states trust and this package sees the same answer the
// loop does.
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
func ConfigFor(cfg core.AgentConfig, workDir, builtinDir string) Config {
	home := ""
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		home = h
	}
	return Config{
		BuiltinDir:   builtinDir,
		HomeDir:      home,
		WorkDir:      workDir,
		TrustProject: cfg.TrustProject,
	}
}
