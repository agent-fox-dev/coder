package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/tools"
)

func toolNamed(t *testing.T, root, name string) coreTool {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	all, err := tools.All(tools.Options{Workspace: ws, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("%s is not in the default tool set", name)
	return coreTool{}
}

// ---- REQ-TOOL-06: run_command

// TestRunCommandDoesNotGoThroughAShell is the whole reason the tool exists.
//
// `execute` hands a string to bash, so `;`, `$( )` and backticks are live. An
// argv vector has no shell between the caller and the program, so a filename
// that happens to contain a semicolon is a filename.
func TestRunCommandDoesNotGoThroughAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses /bin/echo")
	}
	root := t.TempDir()
	tl := toolNamed(t, root, "run_command")

	// If a shell saw this, it would run `id` and the output would not contain
	// the literal text.
	res := tl.Execute(context.Background(),
		json.RawMessage(`{"argv":["echo","hello; $(id)","&& rm -rf /"]}`))
	if !res.OK {
		t.Fatalf("run_command failed: %+v", res)
	}
	out, _ := res.Data["output"].(string)
	if !strings.Contains(out, "hello; $(id)") || !strings.Contains(out, "&& rm -rf /") {
		t.Fatalf("arguments were re-parsed by a shell; output = %q", out)
	}
	if strings.Contains(out, "uid=") {
		t.Fatalf("$(id) was EXECUTED: %q", out)
	}
}

// TestRunCommandReportsAMissingProgramByName. exec.Command defers the lookup
// to Start, whose error names the file and nothing else; resolving first turns
// a typo into a message that says which program.
func TestRunCommandReportsAMissingProgramByName(t *testing.T) {
	root := t.TempDir()
	tl := toolNamed(t, root, "run_command")
	res := tl.Execute(context.Background(),
		json.RawMessage(`{"argv":["definitely-not-a-real-program-xyz"]}`))
	if res.OK {
		t.Fatal("a missing program must fail")
	}
	if !strings.Contains(res.Detail, "definitely-not-a-real-program-xyz") {
		t.Fatalf("the error must name the program; got %q", res.Detail)
	}
}

// TestRunCommandRejectsAnEmptyArgv.
func TestRunCommandRejectsAnEmptyArgv(t *testing.T) {
	root := t.TempDir()
	tl := toolNamed(t, root, "run_command")
	for _, in := range []string{`{"argv":[]}`, `{"argv":["  "]}`, `{}`} {
		res := tl.Execute(context.Background(), json.RawMessage(in))
		if res.OK || res.Error != "invalid_arguments" {
			t.Fatalf("%s: want invalid_arguments, got %+v", in, res)
		}
	}
}

// TestRunCommandCarriesTheSameEnvelopeAsExecute. The REQ-TOOL-08 envelope is a
// property of having run a subprocess, not of how the command was spelled.
func TestRunCommandCarriesTheSameEnvelopeAsExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a unix shell builtin")
	}
	root := t.TempDir()
	tl := toolNamed(t, root, "run_command")
	res := tl.Execute(context.Background(), json.RawMessage(`{"argv":["false"]}`))

	if res.OK {
		t.Fatal("a non-zero exit must not be OK")
	}
	if res.Metadata == nil || res.Metadata.ExitCode == nil {
		t.Fatalf("the envelope must carry an exit code: %+v", res.Metadata)
	}
	if *res.Metadata.ExitCode == 0 {
		t.Fatalf("exit code = 0 for `false`")
	}
	if res.Metadata.Outcome == "" {
		t.Fatal("the envelope must carry an outcome")
	}
}

// ---- REQ-TOOL-06: powershell

// TestPowerShellIsRegisteredOnEveryPlatform.
//
// The requirement is about the tool LIST, not about PowerShell. The list is
// the head of the cached prompt prefix, so a set that differed between a Linux
// runner and a Windows box would give each a different prefix hash and neither
// would ever hit the other's provider-side cache. The symptom is a bill, not
// an error.
func TestPowerShellIsRegisteredOnEveryPlatform(t *testing.T) {
	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	all, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tl := range all {
		if tl.Name == "powershell" {
			found = true
		}
	}
	if !found {
		t.Fatalf("powershell must be in the default set on %s too, or the tool list "+
			"is platform-dependent and so is the cached prompt prefix", runtime.GOOS)
	}
}

// TestPowerShellDefersItsPlatformCheckToExecution.
//
// Registration must not probe the platform — that is what would make the list
// vary. The check belongs on the call, where it produces an actionable error.
func TestPowerShellDefersItsPlatformCheckToExecution(t *testing.T) {
	if _, err := tools.ResolvePowerShell(); err == nil {
		t.Skip("PowerShell is installed here, so the unavailable path cannot be exercised")
	}
	root := t.TempDir()
	tl := toolNamed(t, root, "powershell")

	res := tl.Execute(context.Background(), json.RawMessage(`{"command":"Get-Date"}`))
	if res.OK {
		t.Fatal("powershell cannot have succeeded without PowerShell installed")
	}
	if res.Error != "unsupported_platform" {
		t.Fatalf("error = %q, want unsupported_platform", res.Error)
	}
	if !strings.Contains(res.Detail, "pwsh") {
		t.Fatalf("the error must name what was searched; got %q", res.Detail)
	}
}

// TestPowerShellValidatesArgumentsBeforeProbingThePlatform.
//
// A bad argument is the caller's mistake either way, and reporting
// "unsupported platform" for a missing `command` sends them somewhere useless.
func TestPowerShellValidatesArgumentsBeforeProbingThePlatform(t *testing.T) {
	root := t.TempDir()
	tl := toolNamed(t, root, "powershell")
	for _, in := range []string{`{}`, `{"command":""}`, `{"command":"x","timeout_s":-1}`} {
		res := tl.Execute(context.Background(), json.RawMessage(in))
		if res.Error != "invalid_arguments" {
			t.Fatalf("%s: want invalid_arguments, got %q (%s)", in, res.Error, res.Detail)
		}
	}
}

// TestPowerShellRunsWhereItIsInstalled. Skipped where it is not, rather than
// asserting only the failure path — a tool whose success path is never
// exercised anywhere is a tool nobody has run.
func TestPowerShellRunsWhereItIsInstalled(t *testing.T) {
	if _, err := tools.ResolvePowerShell(); err != nil {
		t.Skipf("PowerShell is not installed: %v", err)
	}
	root := t.TempDir()
	tl := toolNamed(t, root, "powershell")
	res := tl.Execute(context.Background(),
		json.RawMessage(`{"command":"Write-Output 'from-powershell'"}`))
	if !res.OK {
		t.Fatalf("powershell failed: %+v", res)
	}
	if out, _ := res.Data["output"].(string); !strings.Contains(out, "from-powershell") {
		t.Fatalf("output = %q", out)
	}
}

// TestTheDefaultToolSetIsPlatformStable is the property all of the above
// serve: the same names, in the same order, everywhere.
func TestTheDefaultToolSetIsPlatformStable(t *testing.T) {
	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	all, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(all))
	for _, tl := range all {
		got = append(got, tl.Name)
	}
	want := []string{"read_file", "write_file", "edit_file", "list_files",
		"find_files", "search_files", "execute", "run_command", "powershell"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default tool set changed.\ngot:  %v\nwant: %v\n\n"+
			"This list is the head of the cached prompt prefix; changing it is a "+
			"cache invalidation for every consumer, so it is pinned deliberately.",
			got, want)
	}
}
