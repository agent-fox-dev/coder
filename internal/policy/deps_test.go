// Package policy holds AgentKit's executable invariants. It carries no
// non-test source and nothing imports it.
//
// REQ-GO-13: the dependency budget of REQ-GO-11 ships as a test, not as prose.
// Adding a dependency is an edit to allowedModules below, and that edit is the
// review gate.
package policy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// allowedModules is the dependency budget. Every entry states, in prose, why
// that module is allowed. The root module requires nothing outside the Go
// standard library (REQ-GO-11), so this map holds exactly one entry: AgentKit
// itself.
//
// Before adding an entry, answer in the reason string: what does this module
// buy, is hand-rolling credible, and where is the ruling recorded? A module
// that needs cgo is rejected outright by TestNoCgoOutsideStdlib regardless of
// what this map says — cgo-freedom, not module count, is the property that
// determines whether AgentKit cross-compiles (NFR-COMPAT-06).
var allowedModules = map[string]string{
	"github.com/agentfox/agentkit-go": "the module under test",
}

// goListDeps returns one line per package in the transitive build graph of
// ./... as "importPath|modulePath|numCgoFiles".
//
// CGO_ENABLED=1 is forced into the child environment and that is load-bearing,
// not incidental. With cgo disabled the toolchain excludes cgo files by build
// constraint, so a cgo-requiring dependency reports zero CgoFiles and this
// check passes while the dependency is present. Verified against the standard
// library on go1.24.7: `net` reports 5 CgoFiles under CGO_ENABLED=1 and 0
// under CGO_ENABLED=0. A cgo-off gate is not a weaker version of this check;
// it cannot see the thing it claims to check.
func goListDeps(t *testing.T) []string {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps",
		"-f", "{{.ImportPath}}|{{with .Module}}{{.Path}}{{end}}|{{len .CgoFiles}}",
		"./...")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")

	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps failed: %v\n%s", err, stderr)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("locating module root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestNoUnapprovedModules fails on any module in the transitive build graph
// that is not in allowedModules. Standard library packages carry an empty
// module path and are always permitted.
func TestNoUnapprovedModules(t *testing.T) {
	seen := map[string][]string{}
	for _, line := range goListDeps(t) {
		f := strings.Split(line, "|")
		if len(f) != 3 {
			continue
		}
		importPath, modPath := f[0], f[1]
		if modPath == "" {
			continue // standard library
		}
		if _, ok := allowedModules[modPath]; !ok {
			seen[modPath] = append(seen[modPath], importPath)
		}
	}
	for modPath, importers := range seen {
		t.Errorf(`unapproved dependency: %s

	pulled in by: %s

REQ-GO-11 holds the root module to the Go standard library. To resolve, pick one:

  1. Remove the dependency. Usually the right answer; most of what a small
     module buys is a few dozen lines of stdlib.
  2. Move the code needing it into a NESTED module (its own go.mod, its own
     tag series). Build tags and sub-packages do NOT confine a dependency —
     it still appears in go.mod, go.sum, go list -m all, and every downstream
     SBOM. A nested module is the only mechanism in Go that does.
  3. If it genuinely belongs in the root, add it to allowedModules in this
     file with a reason stating what it buys and why hand-rolling is not
     credible, and record the ruling in docs/DEPS.md. That edit is the review
     gate — it is meant to be visible in a diff.`, modPath, strings.Join(importers, ", "))
	}
}

// TestNoCgoOutsideStdlib fails on any non-stdlib package that ships cgo files.
// A cgo-requiring dependency breaks cross-compilation for the platform matrix
// of NFR-COMPAT-06 even when it is otherwise approved.
func TestNoCgoOutsideStdlib(t *testing.T) {
	for _, line := range goListDeps(t) {
		f := strings.Split(line, "|")
		if len(f) != 3 {
			continue
		}
		importPath, modPath, cgoFiles := f[0], f[1], f[2]
		if modPath == "" || cgoFiles == "0" {
			continue
		}
		t.Errorf(`cgo dependency: %s (module %s) ships %s cgo file(s)

cgo breaks the cross-target build gate of NFR-COMPAT-06 (linux/amd64,
linux/arm64, darwin/arm64, windows/amd64). Remove it, or move it behind a
nested module that the cross-target gate does not build.`, importPath, modPath, cgoFiles)
	}
}

// TestCgoProbeIsArmed guards the guard. If CGO_ENABLED did not reach the child
// process, TestNoCgoOutsideStdlib would silently pass on a cgo dependency. The
// standard library always has at least one cgo-carrying package reachable
// under CGO_ENABLED=1, so observing zero of them means the probe is dark.
func TestCgoProbeIsArmed(t *testing.T) {
	var withCgo int
	for _, line := range goListDeps(t) {
		f := strings.Split(line, "|")
		if len(f) == 3 && f[2] != "0" {
			withCgo++
		}
	}
	if withCgo == 0 {
		t.Fatal(`the cgo probe is dark: no package in the build graph reports cgo files.

Either CGO_ENABLED=1 is not reaching the child process, or the build graph no
longer reaches a cgo-carrying stdlib package. Until this passes,
TestNoCgoOutsideStdlib proves nothing.`)
	}
}
