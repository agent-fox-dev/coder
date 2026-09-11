package guard

import (
	"context"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestRestrictedPolicy pins the reference interceptor's decisions.
func TestRestrictedPolicy(t *testing.T) {
	p := Restricted(Options{AllowedPrograms: []string{"go", "/usr/bin/git"}})
	call := func(tool string, args map[string]any) core.BeforeToolCallDecision {
		return p(context.Background(), core.BeforeToolCallContext{ToolName: tool, Arguments: args})
	}
	cases := []struct {
		tool  string
		args  map[string]any
		block bool
		why   string
	}{
		{"execute", map[string]any{"command": "go test ./..."}, false, "allowed program"},
		{"execute", map[string]any{"command": "GOFLAGS=-mod=mod go build"}, false, "env assignment prefix"},
		{"execute", map[string]any{"command": "git log 'a;b'"}, false, "operator inside single quotes"},
		{"execute", map[string]any{"command": "go test | tee out"}, true, "pipe"},
		{"execute", map[string]any{"command": "go test; rm -rf /"}, true, "list operator"},
		{"execute", map[string]any{"command": "echo $(whoami)"}, true, "command substitution"},
		{"execute", map[string]any{"command": "git log \"$HOME\""}, true, "expansion inside double quotes"},
		{"execute", map[string]any{"command": "rm -rf /"}, true, "program not allowed"},
		{"execute", map[string]any{"command": ""}, true, "empty"},
		{"run_command", map[string]any{"argv": []any{"go", "vet", "a;b"}}, false, "argv is not re-parsed"},
		{"run_command", map[string]any{"argv": []any{"curl", "x"}}, true, "argv program not allowed"},
		{"powershell", map[string]any{"command": "Get-ChildItem"}, true, "no PowerShell grammar: refused outright"},
		{"read_file", map[string]any{"path": "x"}, false, "non-shell tools pass"},
	}
	for _, c := range cases {
		if got := call(c.tool, c.args).Block; got != c.block {
			t.Errorf("%s %v: block=%v, want %v (%s)", c.tool, c.args, got, c.block, c.why)
		}
	}
	if !call("execute", map[string]any{"command": "go test | tee"}).Block {
		t.Fatal("pipe")
	}
	loose := Restricted(Options{AllowedPrograms: []string{"go"}, AllowShellOperators: true})
	if loose(context.Background(), core.BeforeToolCallContext{ToolName: "execute",
		Arguments: map[string]any{"command": "go test | tee"}}).Block {
		t.Fatal("AllowShellOperators must permit the pipe")
	}
	term := Restricted(Options{TerminateOnBlock: true})
	if d := term(context.Background(), core.BeforeToolCallContext{ToolName: "execute",
		Arguments: map[string]any{"command": "ls"}}); !d.Block || !d.Terminate {
		t.Fatal("TerminateOnBlock must cast the REQ-TOOL-13.2 vote")
	}
}
