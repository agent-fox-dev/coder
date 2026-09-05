package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// walkAll drives the engine the way find_files does: enter each directory
// before matching its children. A test that calls match() without ever
// entering a subdirectory would pass against a root-only engine, which is
// exactly the implementation this replaces.
func walkAll(t *testing.T, e *ignoreEngine, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var rec func(rel string)
	rec = func(rel string) {
		abs := root
		if rel != "" {
			abs = filepath.Join(root, filepath.FromSlash(rel))
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		for _, d := range entries {
			childRel := d.Name()
			if rel != "" {
				childRel = rel + "/" + d.Name()
			}
			ignored := e.match(childRel, d.IsDir())
			out[childRel] = ignored
			if d.IsDir() && !ignored {
				e.enter(childRel, filepath.Join(root, filepath.FromSlash(childRel)))
				rec(childRel)
			}
		}
	}
	e.enter("", root)
	rec("")
	return out
}

// TestADeeperGitignoreOverridesAShallowerOne is REQ-TOOL-05.2's precedence
// rule, and the case a root-only engine cannot express.
//
// The root says ignore every .log; a subdirectory says keep them. Loading only
// the root file gets this exactly backwards, and the symptom is a listing that
// silently omits the files the subdirectory's author asked to see.
func TestADeeperGitignoreOverridesAShallowerOne(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "*.log\n")
	writeFile(t, dir, "logs/.gitignore", "!important.log\n")
	writeFile(t, dir, "app.log", "")
	writeFile(t, dir, "logs/important.log", "")
	writeFile(t, dir, "logs/noise.log", "")

	got := walkAll(t, newIgnoreEngine(dir, ignoreOptions{Getenv: func(string) string { return "" },
		Home: func() (string, error) { return dir, nil }, GitConfig: func() string { return "" }}), dir)

	if !got["app.log"] {
		t.Error("the root *.log rule must still apply at the root")
	}
	if !got["logs/noise.log"] {
		t.Error("the root *.log rule must still apply inside logs/")
	}
	if got["logs/important.log"] {
		t.Error("logs/.gitignore re-includes important.log; a DEEPER file overrides a " +
			"shallower one, and a root-only engine cannot see this file at all")
	}
}

// TestGitInfoExcludeIsHonoured is the second REQ-TOOL-05.2 source. It is where
// a developer puts personal excludes they do not want in the shared
// .gitignore, so a tool that skips it shows them the files they hid.
func TestGitInfoExcludeIsHonoured(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".git/info/exclude", "scratch/\n")
	writeFile(t, dir, "scratch/notes.md", "")
	writeFile(t, dir, "main.go", "")

	got := walkAll(t, newIgnoreEngine(dir, ignoreOptions{Getenv: func(string) string { return "" },
		Home: func() (string, error) { return dir, nil }, GitConfig: func() string { return "" }}), dir)

	if !got["scratch"] {
		t.Error(".git/info/exclude must be read")
	}
	if got["main.go"] {
		t.Error("main.go is not excluded")
	}
}

// TestGlobalExcludesResolutionOrder pins REQ-TOOL-05.2's first source and the
// order of its three arms.
//
// core.excludesFile is checked FIRST rather than as a fallback because it is
// the only arm that reflects an explicit choice; the XDG paths are git's own
// defaults, and consulting them first silently ignores a user who pointed the
// setting elsewhere.
func TestGlobalExcludesResolutionOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cfg/explicit-ignore", "secret*\n")
	writeFile(t, dir, "xdg/git/ignore", "xdgonly*\n")
	writeFile(t, dir, "home/.config/git/ignore", "homeonly*\n")
	writeFile(t, dir, "work/secret.txt", "")
	writeFile(t, dir, "work/xdgonly.txt", "")
	writeFile(t, dir, "work/homeonly.txt", "")

	work := filepath.Join(dir, "work")

	t.Run("core.excludesFile wins", func(t *testing.T) {
		e := newIgnoreEngine(work, ignoreOptions{
			GitConfig: func() string { return filepath.Join(dir, "cfg", "explicit-ignore") },
			Getenv:    func(string) string { return filepath.Join(dir, "xdg") },
			Home:      func() (string, error) { return filepath.Join(dir, "home"), nil },
		})
		if !e.match("secret.txt", false) {
			t.Error("the configured core.excludesFile must be read")
		}
		if e.match("xdgonly.txt", false) {
			t.Error("XDG must not be consulted once core.excludesFile resolved")
		}
	})

	t.Run("XDG is second", func(t *testing.T) {
		e := newIgnoreEngine(work, ignoreOptions{
			GitConfig: func() string { return "" },
			Getenv: func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return filepath.Join(dir, "xdg")
				}
				return ""
			},
			Home: func() (string, error) { return filepath.Join(dir, "home"), nil },
		})
		if !e.match("xdgonly.txt", false) {
			t.Error("$XDG_CONFIG_HOME/git/ignore is the second source")
		}
		if e.match("homeonly.txt", false) {
			t.Error("~/.config/git/ignore must not be consulted once XDG resolved")
		}
	})

	t.Run("home is third", func(t *testing.T) {
		e := newIgnoreEngine(work, ignoreOptions{
			GitConfig: func() string { return "" },
			Getenv:    func(string) string { return "" },
			Home:      func() (string, error) { return filepath.Join(dir, "home"), nil },
		})
		if !e.match("homeonly.txt", false) {
			t.Error("~/.config/git/ignore is the third source")
		}
	})
}

// TestANestedRepositoryIsItsOwnIgnoreRoot is REQ-TOOL-05.3.
//
// A vendored dependency that is itself a git checkout must not inherit the
// outer project's rules. Without the boundary, a rule the outer project wrote
// about ITS build output deletes files from the listing of a repository that
// has never heard of it — and the deletion is silent.
func TestANestedRepositoryIsItsOwnIgnoreRoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "dist/\n")
	writeFile(t, dir, "dist/outer.js", "")
	// A nested checkout that ships a dist/ directory as source.
	writeFile(t, dir, "vendor/lib/.git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, "vendor/lib/dist/bundle.js", "")
	writeFile(t, dir, "vendor/lib/.gitignore", "tmp/\n")
	writeFile(t, dir, "vendor/lib/tmp/x", "")

	got := walkAll(t, newIgnoreEngine(dir, ignoreOptions{Getenv: func(string) string { return "" },
		Home: func() (string, error) { return dir, nil }, GitConfig: func() string { return "" }}), dir)

	if !got["dist"] {
		t.Error("the outer repository's own dist/ rule still applies to itself")
	}
	if got["vendor/lib/dist"] {
		t.Error("the outer repository's dist/ rule must NOT reach inside a nested " +
			"repository: a nested checkout's ignore rules are its own")
	}
	if !got["vendor/lib/tmp"] {
		t.Error("the nested repository's own rules DO apply within it")
	}
}

// TestANestedRepositorysRulesDoNotLeakOutward is the other half of
// REQ-TOOL-05.3.
//
// The layers are entered EXPLICITLY rather than by walking, because a walk
// visits directories in lexical order and would reach an outer "build/" before
// it ever loaded "vendor/lib/.gitignore" — so the assertion would hold for a
// reason that has nothing to do with the boundary. An outer directory sorting
// after the nested repository is the case that actually bites.
func TestANestedRepositorysRulesDoNotLeakOutward(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vendor/lib/.git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, "vendor/lib/.gitignore", "zbuild/\n")
	writeFile(t, dir, "zbuild/app", "") // OUTSIDE the nested repo, and sorts after it

	e := newIgnoreEngine(dir, ignoreOptions{Getenv: func(string) string { return "" },
		Home: func() (string, error) { return dir, nil }, GitConfig: func() string { return "" }})
	e.enter("vendor", filepath.Join(dir, "vendor"))
	e.enter("vendor/lib", filepath.Join(dir, "vendor", "lib"))

	if e.match("zbuild", true) {
		t.Error("a nested repository's zbuild/ rule must not ignore the OUTER project's " +
			"directory of that name: its layer applies only to paths beneath it")
	}
	if !e.match("vendor/lib/zbuild", true) {
		t.Error("the nested repository's own rule DOES apply within it")
	}
}

// TestNoRepositoryIsRequired is REQ-TOOL-05.4's documented answer, made
// executable. A scan of a plain directory still honours the .gitignore it
// finds there.
func TestNoRepositoryIsRequired(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "*.tmp\n")
	writeFile(t, dir, "a.tmp", "")
	writeFile(t, dir, "a.go", "")

	e := newIgnoreEngine(dir, ignoreOptions{Getenv: func(string) string { return "" },
		Home: func() (string, error) { return dir, nil }, GitConfig: func() string { return "" }})
	if !e.match("a.tmp", false) {
		t.Error("a .gitignore outside any repository is still honoured")
	}
	if e.match("a.go", false) {
		t.Error("a.go is not ignored")
	}
}

// TestFindFilesHonoursANestedGitignore is the end-to-end check: the engine is
// only useful if the walk actually feeds it every directory it enters.
func TestFindFilesHonoursANestedGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/keep.go", "")
	writeFile(t, dir, "src/gen/.gitignore", "*.go\n")
	writeFile(t, dir, "src/gen/generated.go", "")

	files := runFindFiles(t, dir, "**/*.go")
	for _, f := range files {
		if filepath.ToSlash(f) == "src/gen/generated.go" {
			t.Fatalf("find_files returned %v; src/gen/.gitignore excludes *.go and the "+
				"walk must load it on the way down", files)
		}
	}
	var sawKeep bool
	for _, f := range files {
		if filepath.ToSlash(f) == "src/keep.go" {
			sawKeep = true
		}
	}
	if !sawKeep {
		t.Fatalf("find_files returned %v, want src/keep.go", files)
	}
}

// runFindFiles drives the real find_files tool, through the real resolver, so
// the test exercises the shipped path rather than a fixture (NFR-TEST-08.3).
func runFindFiles(t *testing.T, dir, pattern string) []string {
	t.Helper()
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, err := All(Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		if tl.Name != "find_files" {
			continue
		}
		args, _ := json.Marshal(map[string]any{"pattern": pattern})
		res := tl.Execute(context.Background(), args)
		if !res.OK {
			t.Fatalf("find_files failed: %s", res.Error)
		}
		raw, _ := res.Data["files"].([]string)
		return raw
	}
	t.Fatal("find_files is not in the default tool set")
	return nil
}
