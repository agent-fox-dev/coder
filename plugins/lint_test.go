package plugins_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/plugins"
)

func writeGo(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// REQ-SKILL-09: skill plugin code may not import an LLM client library or a
// model API package. A skill holding its own model client escapes the
// session's accounting, hooks and policy entirely.
func TestSkillLintFlagsModelClientImports(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "tools.go", `package p

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/agentfox/agentkit-go/provider"
)
`)
	bad, err := plugins.LintSkillImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 2 {
		t.Fatalf("violations = %+v, want the model SDK and the backend package", bad)
	}
	joined := bad[0].Import + "," + bad[1].Import
	if !strings.Contains(joined, "anthropic-sdk-go") || !strings.Contains(joined, "agentkit-go/provider") {
		t.Fatalf("imports = %q", joined)
	}
}

// The skill lint is a SUPERSET of REQ-PLUGIN-09's, not a replacement: the
// internal-package rule still applies to skill plugin code.
func TestSkillLintStillFlagsAgentkitInternals(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "x.go", `package p

import "github.com/agentfox/agentkit-go/internal/toml"
`)
	bad, err := plugins.LintSkillImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 {
		t.Fatalf("violations = %+v", bad)
	}
}

// The plugin lint must NOT gain the skill prefixes: REQ-PLUGIN-09 forbids
// agentkit internals and nothing else, and a plugin is allowed to be a
// backend — that is what BackendPlugin is for.
func TestThePluginLintIsUnchangedByTheSkillRules(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "x.go", `package p

import "github.com/anthropics/anthropic-sdk-go"
`)
	bad, err := plugins.LintImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("REQ-PLUGIN-09 flagged %+v; a backend plugin holds a model client by design", bad)
	}
}

// Prefixes match at a path-segment boundary, never as a bare substring.
func TestSkillLintMatchesWholePathSegments(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "x.go", `package p

import "github.com/openai/openai-go-community-fork/util"
`)
	bad, err := plugins.LintSkillImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("flagged %+v; a different module that shares a prefix is not this rule's business", bad)
	}
}

// REQ-SEC-06 makes skill allowlist extensions ADDITIVE ONLY. The prohibited
// set must therefore not be reachable for deletion: the accessor hands out a
// copy, and mutating it cannot turn the check off.
func TestTheProhibitedSetCannotBeShortenedByACaller(t *testing.T) {
	got := plugins.SkillForbiddenImportPrefixes()
	if len(got) == 0 {
		t.Fatal("the prohibited set is empty")
	}
	for i := range got {
		got[i] = "example.com/harmless"
	}
	if again := plugins.SkillForbiddenImportPrefixes(); again[0] == "example.com/harmless" {
		t.Fatal("a caller mutated the package's own prohibited set")
	}
}
