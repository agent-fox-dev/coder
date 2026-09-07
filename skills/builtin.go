package skills

import (
	"os"
	"path/filepath"
	"runtime"
)

// BuiltinDirName is the SDK's own skills directory, at the module root
// (REQ-SKILL-04: `agentkit/_skills/`). The leading underscore is not
// decoration: the go tool ignores directories beginning with `_`, so the
// prompt and manifest files there are never mistaken for a package, and
// `go vet ./...` does not descend into them.
const BuiltinDirName = "_skills"

// BuiltinDir locates the SDK's built-in skills tier, or returns "" when it
// cannot.
//
// The directory is found relative to THIS SOURCE FILE, through
// runtime.Caller, because the built-in tier is part of the SDK's source tree
// and not of the embedder's: it lives in the module cache, or in a checkout,
// wherever the compiled package came from. There is no other reliable
// pointer to it — the working directory is the untrusted repository
// (REQ-SKILL-12.3) and the executable's location says nothing about where the
// module is.
//
// EMPTY IS A VALID AND EXPECTED STATE, and callers must treat it exactly as
// they treat an unresolvable home directory: skip the tier. Binaries built
// with -trimpath carry no absolute source path, source trees are stripped
// from images, and a vendored copy may not include `_skills/`. In every one
// of those cases the answer is "no built-in skills", never a fallback to a
// relative path or to the working directory — the same rule Config.HomeDir
// states for the user tier, for the same reason.
func BuiltinDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" || !filepath.IsAbs(file) {
		return ""
	}
	// file is <module>/skills/builtin.go; the tier is <module>/_skills.
	dir := filepath.Join(filepath.Dir(filepath.Dir(file)), BuiltinDirName)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return dir
}
