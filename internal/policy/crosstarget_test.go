package policy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// crossTargets is the supported matrix of NFR-COMPAT-06. It is a list in a
// test, not a line in a Makefile, for the same reason allowedModules is: the
// README promised a cross-target gate and nothing ran it, and a gate that
// exists only as prose is indistinguishable from no gate.
var crossTargets = []struct{ goos, goarch string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

// TestCrossTargetBuildAndVet runs `go build ./...` and `go vet ./...` for every
// supported GOOS/GOARCH, UNCONDITIONALLY — not only when a build-constrained
// file changes.
//
// The unconditional part is the requirement's whole argument. Platform-
// constrained files are called from unconstrained code, so an ordinary rename
// breaks a target while touching no constrained file and leaving the host
// suite fully green. A gate that keys on "did a _windows.go file change" cannot
// see that; only building the target can.
//
// CGO_ENABLED=0 is forced. A cross build needs it anyway (there is no cross C
// toolchain here), and it is the honest setting: NFR-COMPAT-06 is a promise
// about the pure-Go build. Whether a cgo dependency has crept in is
// TestNoCgoOutsideStdlib's job, and that one runs with cgo ON for the reason
// its comment gives.
func TestCrossTargetBuildAndVet(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-target build gate skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH; the cross-target gate needs the toolchain")
	}
	root := repoRoot(t)

	for _, target := range crossTargets {
		target := target
		t.Run(target.goos+"/"+target.goarch, func(t *testing.T) {
			for _, verb := range []string{"build", "vet"} {
				cmd := exec.Command("go", verb, "./...")
				cmd.Dir = root
				cmd.Env = append(os.Environ(),
					"GOOS="+target.goos,
					"GOARCH="+target.goarch,
					"CGO_ENABLED=0",
				)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf(`go %s ./... failed for %s/%s: %v
%s
NFR-COMPAT-06 requires every release gate to build and vet all four supported
targets. This target is broken on the host's green suite, which is exactly the
failure the requirement names: platform-constrained files are called from
unconstrained code, so the host build cannot see a break in another target.
Fix the target; do not narrow the matrix.`, verb, target.goos, target.goarch, err,
						strings.TrimSpace(string(out)))
					return
				}
			}
		})
	}
}
