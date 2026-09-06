package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/tools"
)

type coreTool = core.Tool

// searchTree builds a fixture with the things that separate a real search tool
// from a regexp over filepath.Walk: an ignored directory, a nested repository,
// a binary file, and content that matches in more than one case.
func searchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(".gitignore", "node_modules/\n*.log\n")
	write("main.go", "package main\n\nfunc needle() {}\n\nfunc other() {}\n")
	write("lib/helper.go", "package lib\n\n// needle here too\nvar Needle = 1\n")
	write("README.md", "the needle is in the haystack\n")
	write("debug.log", "needle in an ignored file\n")
	write("node_modules/pkg/index.js", "var needle = require('needle')\n")

	// A nested repository with its own rules: they apply within it and do not
	// leak outward (REQ-TOOL-05.3).
	write("vendor/sub/.git/HEAD", "ref: refs/heads/main\n")
	write("vendor/sub/.gitignore", "secret.txt\n")
	write("vendor/sub/ok.go", "// needle in a nested repo\n")
	write("vendor/sub/secret.txt", "needle in an ignored nested file\n")

	// A file whose extension differs from the glob only in CASE. AgentKit's
	// glob dialect is smart-case and ripgrep's is not, which is exactly the
	// divergence the accelerated path must not inherit.
	write("UPPER.GO", "needle in an uppercase-extension file\n")

	// Binary: a NUL byte in the first block.
	if err := os.WriteFile(filepath.Join(root, "blob.bin"),
		append([]byte("needle\x00"), make([]byte, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func names(res tools.SearchResult) []string {
	out := make([]string, 0, len(res.Matches))
	for _, m := range res.Matches {
		out = append(out, fmt.Sprintf("%s:%d", m.File, m.Line))
	}
	return out
}

// ---- the declared semantics

// TestSearchSkipsIgnoredAndBinaryFiles is the difference REQ-TOOL-05 names
// between a search tool and a regexp over a walk: one returns the project's
// files, the other returns node_modules.
func TestSearchSkipsIgnoredAndBinaryFiles(t *testing.T) {
	root := searchTree(t)
	for _, backend := range bothBackends(t) {
		t.Run(string(backend.name), func(t *testing.T) {
			res, err := backend.run(t, root, tools.SearchParams{Pattern: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(names(res), " ")
			for _, forbidden := range []string{"node_modules", "debug.log", "blob.bin"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("%s must not be searched; got %s", forbidden, got)
				}
			}
			if !strings.Contains(got, "main.go") || !strings.Contains(got, "lib/helper.go") {
				t.Fatalf("expected the project's own files; got %s", got)
			}
		})
	}
}

// TestANestedRepositorysRulesDoNotLeakOutward is REQ-TOOL-05.3. The nested
// repo ignores secret.txt; the outer tree does not, and the outer tree's rules
// say nothing about it either way.
func TestANestedRepositorysRulesDoNotLeakOutward(t *testing.T) {
	root := searchTree(t)
	for _, backend := range bothBackends(t) {
		t.Run(string(backend.name), func(t *testing.T) {
			res, err := backend.run(t, root, tools.SearchParams{Pattern: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(names(res), " ")
			if strings.Contains(got, "secret.txt") {
				t.Fatalf("the nested repo ignores secret.txt; got %s", got)
			}
			if !strings.Contains(got, "vendor/sub/ok.go") {
				t.Fatalf("the nested repo's other files are still searched; got %s", got)
			}
		})
	}
}

// TestSmartCaseIsTheDefault. AgentKit's declared semantics, not ripgrep's:
// absent means smart-case, so a lowercase pattern matches any case and a
// pattern carrying an uppercase rune does not.
func TestSmartCaseIsTheDefault(t *testing.T) {
	root := searchTree(t)
	yes, no := true, false
	for _, backend := range bothBackends(t) {
		t.Run(string(backend.name), func(t *testing.T) {
			lower, err := backend.run(t, root, tools.SearchParams{Pattern: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(names(lower), " "), "lib/helper.go:4") {
				t.Fatalf("an all-lowercase pattern must match `var Needle`; got %v", names(lower))
			}

			upper, err := backend.run(t, root, tools.SearchParams{Pattern: "Needle"})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range upper.Matches {
				if !strings.Contains(m.Text, "Needle") {
					t.Fatalf("an uppercase rune makes the pattern sensitive; %q matched", m.Text)
				}
			}

			forced, err := backend.run(t, root, tools.SearchParams{
				Pattern: "Needle", CaseSensitive: &no})
			if err != nil {
				t.Fatal(err)
			}
			if len(forced.Matches) <= len(upper.Matches) {
				t.Fatal("case_sensitive=false must widen the result beyond smart-case")
			}

			strict, err := backend.run(t, root, tools.SearchParams{
				Pattern: "needle", CaseSensitive: &yes})
			if err != nil {
				t.Fatal(err)
			}
			if len(strict.Matches) >= len(lower.Matches) {
				t.Fatal("case_sensitive=true must narrow the result below smart-case; " +
					"absent and false are not the same answer")
			}
		})
	}
}

// TestContextLinesSurroundTheMatch.
func TestContextLinesSurroundTheMatch(t *testing.T) {
	root := searchTree(t)
	for _, backend := range bothBackends(t) {
		t.Run(string(backend.name), func(t *testing.T) {
			res, err := backend.run(t, root, tools.SearchParams{
				Pattern: "func needle", ContextLines: 2, FileGlob: "main.go"})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Matches) != 1 {
				t.Fatalf("want 1 match, got %v", names(res))
			}
			m := res.Matches[0]
			if len(m.Before) != 2 || m.Before[1] != "" || m.Before[0] != "package main" {
				t.Fatalf("before context wrong: %q", m.Before)
			}
			if len(m.After) != 2 || m.After[1] != "func other() {}" {
				t.Fatalf("after context wrong: %q", m.After)
			}
		})
	}
}

// TestTheFileGlobUsesAgentKitsDialect. rg's glob dialect is close but not
// identical; ours is the declared one, so the accelerated path re-filters.
func TestTheFileGlobUsesAgentKitsDialect(t *testing.T) {
	root := searchTree(t)
	for _, backend := range bothBackends(t) {
		t.Run(string(backend.name), func(t *testing.T) {
			res, err := backend.run(t, root, tools.SearchParams{
				Pattern: "needle", FileGlob: "**/*.{go,md}"})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Matches) == 0 {
				t.Fatal("brace expansion must select the .go and .md files")
			}
			var sawUpper bool
			for _, m := range res.Matches {
				ext := strings.ToLower(filepath.Ext(m.File))
				if ext != ".go" && ext != ".md" {
					t.Fatalf("%s does not match the glob", m.File)
				}
				if m.File == "UPPER.GO" {
					sawUpper = true
				}
			}
			// Smart-case globbing is AgentKit's declared dialect and ripgrep's
			// is case-sensitive, so this is the assertion that the accelerated
			// path did not inherit ripgrep's rules.
			if !sawUpper {
				t.Fatalf("an all-lowercase glob must match UPPER.GO; got %v", names(res))
			}
		})
	}
}

// TestSearchTruncatesAtMaxMatches.
func TestSearchTruncatesAtMaxMatches(t *testing.T) {
	root := searchTree(t)
	for _, backend := range bothBackends(t) {
		t.Run(string(backend.name), func(t *testing.T) {
			res, err := backend.run(t, root, tools.SearchParams{
				Pattern: "needle", MaxMatches: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Matches) != 2 || !res.Truncated {
				t.Fatalf("want 2 matches and truncated=true; got %d, %v",
					len(res.Matches), res.Truncated)
			}
		})
	}
}

// TestAnInvalidPatternIsAnArgumentError, not a failed search: the caller has to
// change the pattern, and a retry cannot help.
func TestAnInvalidPatternIsAnArgumentError(t *testing.T) {
	root := searchTree(t)
	_, _, err := tools.Search(context.Background(), root, tools.SearchParams{Pattern: "a(b"})
	if err == nil {
		t.Fatal("an unparseable pattern must be refused")
	}
	var perr *tools.SearchPatternError
	if !asPatternError(err, &perr) {
		t.Fatalf("want a SearchPatternError so the tool can report invalid_arguments; got %T", err)
	}
}

// ---- the parity test REQ-TOOL-05 requires

// TestTheTwoBackendsAgree is the requirement's own condition: the fallback
// matches AgentKit's DECLARED semantics, pinned against whichever backend is
// present.
//
// It runs the same queries through both and compares the results exactly. A
// disagreement here is the thing the requirement exists to prevent — a tool
// whose answers depend on whether ripgrep happens to be installed.
func TestTheTwoBackendsAgree(t *testing.T) {
	rg := requireRipgrep(t)
	root := searchTree(t)
	yes, no := true, false

	queries := []tools.SearchParams{
		{Pattern: "needle"},
		{Pattern: "Needle"},
		{Pattern: "needle", CaseSensitive: &yes},
		{Pattern: "needle", CaseSensitive: &no},
		{Pattern: "needle", FileGlob: "**/*.go"},
		{Pattern: "needle", FileGlob: "*.go"},
		{Pattern: "needle", FileGlob: "**/*.{go,md}"},
		{Pattern: "func \\w+", FileGlob: "**/*.go"},
		{Pattern: "needle", ContextLines: 1},
		{Pattern: "needle", ContextLines: 2, FileGlob: "main.go"},
		{Pattern: "needle", MaxMatches: 2},
		{Pattern: "no-such-string-anywhere"},
	}

	for i, q := range queries {
		t.Run(fmt.Sprintf("%d/%s", i, q.Pattern), func(t *testing.T) {
			native, err := runNative(t, root, q)
			if err != nil {
				t.Fatalf("native: %v", err)
			}
			accel, err := runRipgrep(t, rg, root, q)
			if err != nil {
				t.Fatalf("ripgrep: %v", err)
			}
			if !reflect.DeepEqual(native.Matches, accel.Matches) {
				t.Fatalf("the backends disagree.\nnative:  %s\nripgrep: %s",
					dump(native), dump(accel))
			}
			if native.Truncated != accel.Truncated {
				t.Fatalf("truncated: native %v, ripgrep %v", native.Truncated, accel.Truncated)
			}
			if native.FilesSearched != accel.FilesSearched {
				t.Fatalf("files_searched: native %d, ripgrep %d",
					native.FilesSearched, accel.FilesSearched)
			}
		})
	}
}

// TestTheAcceleratedPathIsActuallyUsed. Without this the parity test could be
// comparing the native backend against itself and passing forever.
func TestTheAcceleratedPathIsActuallyUsed(t *testing.T) {
	requireRipgrep(t)
	root := searchTree(t)
	_, backend, err := tools.Search(context.Background(), root, tools.SearchParams{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if backend != tools.BackendRipgrep {
		t.Fatalf("ripgrep is on PATH but %q answered", backend)
	}
}

// ---- the tool envelope

func TestSearchToolRejectsAPathOutsideTheWorkspace(t *testing.T) {
	root := searchTree(t)
	tool := searchTool(t, root)
	res := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle","path":"../.."}`))
	if res.OK || res.Error != "path_not_allowed" {
		t.Fatalf("want path_not_allowed, got %+v", res)
	}
}

func TestSearchToolBoundsContextLines(t *testing.T) {
	root := searchTree(t)
	tool := searchTool(t, root)
	res := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"needle","context_lines":500}`))
	if res.OK || res.Error != "invalid_arguments" {
		t.Fatalf("context must be bounded: 100 matches with 500 lines either side is "+
			"50000 lines the model pays for; got %+v", res)
	}
}

// ---- helpers

type backend struct {
	name tools.SearchBackend
	run  func(*testing.T, string, tools.SearchParams) (tools.SearchResult, error)
}

// bothBackends runs a semantics test through each implementation, so a rule is
// pinned on both rather than on whichever one happens to be installed.
func bothBackends(t *testing.T) []backend {
	t.Helper()
	out := []backend{{name: tools.BackendNative, run: runNative}}
	if path, err := exec.LookPath("rg"); err == nil {
		out = append(out, backend{
			name: tools.BackendRipgrep,
			run: func(t *testing.T, root string, p tools.SearchParams) (tools.SearchResult, error) {
				return runRipgrep(t, path, root, p)
			},
		})
	}
	return out
}

func requireRipgrep(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed; the parity test has nothing to compare against")
	}
	return path
}

func runNative(t *testing.T, root string, p tools.SearchParams) (tools.SearchResult, error) {
	t.Helper()
	restore := tools.SetRipgrepLookup(func() (string, bool) { return "", false })
	defer restore()
	res, backendUsed, err := tools.Search(context.Background(), root, p)
	if err == nil && backendUsed != tools.BackendNative {
		t.Fatalf("expected the native backend, got %q", backendUsed)
	}
	return res, err
}

func runRipgrep(t *testing.T, path, root string, p tools.SearchParams) (tools.SearchResult, error) {
	t.Helper()
	restore := tools.SetRipgrepLookup(func() (string, bool) { return path, true })
	defer restore()
	res, backendUsed, err := tools.Search(context.Background(), root, p)
	if err == nil && backendUsed != tools.BackendRipgrep {
		t.Fatalf("expected the ripgrep backend, got %q", backendUsed)
	}
	return res, err
}

func dump(r tools.SearchResult) string {
	b, _ := json.Marshal(r.Matches)
	return string(b)
}

func asPatternError(err error, target **tools.SearchPatternError) bool {
	for err != nil {
		if p, ok := err.(*tools.SearchPatternError); ok {
			*target = p
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// searchTool builds the real tool against a workspace rooted at root.
func searchTool(t *testing.T, root string) coreTool {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	all, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		if tl.Name == "search_files" {
			return tl
		}
	}
	t.Fatal("search_files is not in the default tool set")
	return coreTool{}
}

// ---- REQ-TOOL-14.6: read_file detects images by magic bytes

func readTool(t *testing.T, root string) coreTool {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	all, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		if tl.Name == "read_file" {
			return tl
		}
	}
	t.Fatal("read_file is missing")
	return coreTool{}
}

// TestReadFileDetectsAnImageByItsBytesNotItsName is REQ-TOOL-14.6. A PNG
// called .txt is still a PNG, and splitting it into "lines" hands the model
// several kilobytes of mojibake.
func TestReadFileDetectsAnImageByItsBytesNotItsName(t *testing.T) {
	root := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "screenshot.txt"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	res := readTool(t, root).Execute(context.Background(),
		json.RawMessage(`{"path":"screenshot.txt"}`))
	if !res.OK {
		t.Fatalf("read failed: %+v", res)
	}
	if len(res.Blocks) != 1 {
		t.Fatalf("want a text note plus one ImageBlock; got %d blocks", len(res.Blocks))
	}
	img2, ok := res.Blocks[0].(core.ImageBlock)
	if !ok {
		t.Fatalf("block is %T, want core.ImageBlock", res.Blocks[0])
	}
	if img2.MimeType != "image/png" {
		t.Fatalf("mime %q", img2.MimeType)
	}
	note, _ := res.Data["note"].(string)
	if !strings.Contains(note, "16") || !strings.Contains(note, "8") {
		t.Fatalf("the note must say what was read; got %q", note)
	}
	if _, isText := res.Data["content"]; isText {
		t.Fatal("an image must not also be returned as text lines")
	}
}

// TestReadFileRefusesAnAnimatedPNGAtTheTool. Forwarding it lands the failure on
// the NEXT provider request — by which time the image is in history and every
// later request fails the same way.
func TestReadFileRefusesAnAnimatedPNGAtTheTool(t *testing.T) {
	root := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	animated := insertPNGChunk(buf.Bytes(), "acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0})
	if err := os.WriteFile(filepath.Join(root, "anim.png"), animated, 0o644); err != nil {
		t.Fatal(err)
	}

	res := readTool(t, root).Execute(context.Background(), json.RawMessage(`{"path":"anim.png"}`))
	if res.OK || res.Error != "unsupported_image" {
		t.Fatalf("want unsupported_image, got %+v", res)
	}
	if !strings.Contains(res.Detail, "APNG") && !strings.Contains(res.Detail, "animated") {
		t.Fatalf("the refusal must name the problem; got %q", res.Detail)
	}
}

// TestReadFileStillReadsText. The image path must not capture ordinary files.
func TestReadFileStillReadsText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := readTool(t, root).Execute(context.Background(), json.RawMessage(`{"path":"a.go"}`))
	if !res.OK || len(res.Blocks) != 0 {
		t.Fatalf("a text file must return no image blocks; got %+v", res)
	}
}

// insertPNGChunk adds a chunk right after IHDR.
func insertPNGChunk(src []byte, typ string, payload []byte) []byte {
	const sigLen = 8
	ihdrLen := int(uint32(src[sigLen])<<24 | uint32(src[sigLen+1])<<16 |
		uint32(src[sigLen+2])<<8 | uint32(src[sigLen+3]))
	at := sigLen + 8 + ihdrLen + 4

	chunk := make([]byte, 0, 12+len(payload))
	n := uint32(len(payload))
	chunk = append(chunk, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	chunk = append(chunk, typ...)
	chunk = append(chunk, payload...)
	c := crc32.NewIEEE()
	_, _ = c.Write([]byte(typ))
	_, _ = c.Write(payload)
	crc := c.Sum32()
	chunk = append(chunk, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))

	out := make([]byte, 0, len(src)+len(chunk))
	out = append(out, src[:at]...)
	out = append(out, chunk...)
	out = append(out, src[at:]...)
	return out
}
