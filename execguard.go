package agentkit

import (
	"fmt"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/guard"
)

// checkExecuteGuard is OQ-8's loud failure: a shell tool in the resolved set
// with no BeforeToolCall interceptor is core.ErrUnguardedExecute. It is
// consulted at the head of every run, before the slot is claimed, so the
// failure is an ordinary returned error and not a stream nobody reads. The
// opt-out is guard.AllowAll; the shipped starting point is guard.Restricted.
func (a *Agent) checkExecuteGuard() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.BeforeToolCall != nil {
		return nil
	}
	for _, t := range a.cfg.ToolPolicy.Resolve(a.tools) {
		if guard.IsShellTool(t.Name) {
			return fmt.Errorf("%w (tool %q)", core.ErrUnguardedExecute, t.Name)
		}
	}
	return nil
}
