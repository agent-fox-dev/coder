package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/schema"
)

func TestToolPolicyResolution(t *testing.T) {
	builtin := func(n string) Tool {
		return Tool{Name: n, Description: n, Builtin: true, InputSchema: schema.Object(),
			Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	}
	custom := func(n string) Tool {
		return Tool{Name: n, Description: n, InputSchema: schema.Object(),
			Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	}
	reg := []Tool{builtin("read"), builtin("write"), builtin("exec")}

	names := func(ts []Tool) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return strings.Join(out, ",")
	}

	// The four non-obvious consequences of REQ-TOOL-10, each its own row.
	cases := []struct {
		name   string
		policy ToolPolicy
		want   string
	}{
		{`NoTools "all" disables CUSTOM tools too`,
			ToolPolicy{NoTools: NoToolsAll, CustomTools: []Tool{custom("mine")}}, ""},
		{`NoTools "builtin" leaves custom tools alive`,
			ToolPolicy{NoTools: NoToolsBuiltin, CustomTools: []Tool{custom("mine")}}, "mine"},
		{`a ToolNames allowlist constrains custom tools`,
			ToolPolicy{ToolNames: []string{"read"}, CustomTools: []Tool{custom("mine")}}, "read"},
		{`ExcludeTools applies to custom tools`,
			ToolPolicy{ExcludeTools: []string{"mine"}, CustomTools: []Tool{custom("mine")}}, "read,write,exec"},
		{`Tools non-nil bypasses everything`,
			ToolPolicy{Tools: []Tool{custom("only")}, ToolNames: []string{"read"}, NoTools: NoToolsAll}, "only"},
		{`Tools non-nil but EMPTY means no tools, deliberately`,
			ToolPolicy{Tools: []Tool{}}, ""},
		{`nil ToolNames means the default set`,
			ToolPolicy{}, "read,write,exec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := names(tc.policy.Resolve(reg)); got != tc.want {
				t.Fatalf("resolved %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCustomToolOverridesBuiltinInPlace: overriding must not reorder the tool
// list, because the tool list is part of the cached prompt prefix.
func TestCustomToolOverridesBuiltinInPlace(t *testing.T) {
	reg := []Tool{
		{Name: "a", Builtin: true}, {Name: "read", Builtin: true, Description: "builtin"}, {Name: "z", Builtin: true},
	}
	got := ToolPolicy{
		CustomTools: []Tool{{Name: "read", Description: "custom"}},
	}.Resolve(reg)
	if len(got) != 3 || got[1].Name != "read" || got[1].Description != "custom" {
		t.Fatalf("override did not happen in place: %+v", got)
	}
}
