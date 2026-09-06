package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/mcp"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
)

// TestTheProviderLedgerMatchesTheCode is NFR-COMPAT-07 as an executable
// invariant rather than prose (G12).
//
// docs/PROVIDERS.md records the pinned version of every moving external
// surface. Those strings are COPIES of constants, and a copy of a version
// number is the single most likely thing in the repository to go stale: the
// constant gets bumped in the same commit that fixes the bug, and the ledger
// is updated in the commit after that, or never.
//
// This does not check that the ledger is CORRECT about the vendor — nothing in
// this repository can, which is what the file's own "reviewed is not
// implemented" section says. It checks that the ledger and the code agree
// about what this build targets, which is the part a test can hold.
func TestTheProviderLedgerMatchesTheCode(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "PROVIDERS.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("NFR-COMPAT-07 requires this ledger to exist: %v", err)
	}
	doc := string(raw)

	for _, tc := range []struct {
		what  string
		value string
	}{
		{"anthropic API version", anthropic.APIVersion},
		{"anthropic compaction beta", anthropic.BetaCompaction},
		{"anthropic base URL", anthropic.DefaultBaseURL},
		{"openai-completions base URL", openai.DefaultBaseURL},
		{"openai-completions path", openai.Path},
		{"openai-responses base URL", openairesponses.DefaultBaseURL},
		{"openai-responses path", openairesponses.Path},
		{"google base URL", google.DefaultBaseURL},
		{"ollama base URL", ollama.DefaultBaseURL},
		{"ollama path", ollama.Path},
		{"MCP protocol version", mcp.ProtocolVersion},
	} {
		if !strings.Contains(doc, tc.value) {
			t.Errorf("docs/PROVIDERS.md does not mention the %s (%q).\n"+
				"The ledger's version strings are copies of constants; when they "+
				"drift, the ledger is the thing a reader trusts and the code is the "+
				"thing that runs.", tc.what, tc.value)
		}
	}

	// Every API this build registers must have a row. A new wire API that
	// nobody added to the ledger is exactly the omission NFR-COMPAT-07 exists
	// to prevent — the row is where its pin and its capture date live.
	for _, api := range []string{
		string(anthropic.API), string(openai.API), string(openairesponses.API),
		string(google.API), string(ollama.API),
	} {
		if !strings.Contains(doc, "`"+api+"`") {
			t.Errorf("docs/PROVIDERS.md has no row for the %q wire API", api)
		}
	}
}
