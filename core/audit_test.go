package core

import "testing"

// TestHashArgumentsIsStableAndLabelled: the same call hashes the same way, so
// an auditor can correlate; a different one does not; and no arguments means
// no hash, not the hash of nothing.
func TestHashArgumentsIsStableAndLabelled(t *testing.T) {
	same := HashArguments([]byte(`{"a":1}`))
	if same != HashArguments([]byte(`{"a":1}`)) {
		t.Fatal("the hash must be stable, or it correlates nothing")
	}
	if same == HashArguments([]byte(`{"a":2}`)) {
		t.Fatal("different arguments must hash differently")
	}
	if HashArguments(nil) != "" {
		t.Fatal("no arguments means no hash, not the hash of nothing")
	}
	if got := HashArguments([]byte(`{}`)); len(got) < 8 || got[:7] != "sha256:" {
		t.Fatalf("hash = %q, want a labelled digest", got)
	}
}

func TestMCPServerOf(t *testing.T) {
	cases := map[string]string{
		"mcp__github__create_issue": "github",
		"mcp__db__query":            "db",
		"read_file":                 "",
		"mcp__malformed":            "",
		"mcp__":                     "",
		"":                          "",
		// A LOCAL tool whose name happens to contain the separator. Without
		// the prefix check this reports a server called "my", inventing an MCP
		// origin for a tool that has none — and an audit trail that attributes
		// a local call to a remote server is worse than one that omits the
		// field.
		"my__local__tool": "",
		"__leading":       "",
	}
	for in, want := range cases {
		if got := MCPServerOf(in); got != want {
			t.Errorf("MCPServerOf(%q) = %q, want %q", in, got, want)
		}
	}
}
