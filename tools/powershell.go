package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// PowerShell is REQ-TOOL-06's second shell dialect, as a SEPARATELY NAMED
// tool rather than a mode of `execute`.
//
// Two decisions the requirement makes explicitly, both about the tool LIST
// rather than about PowerShell:
//
//  1. It is registered on EVERY platform, including ones that cannot run it.
//  2. The platform check is deferred to execution.
//
// Together those keep the tool list platform-stable, and the tool list is the
// head of the cached prompt prefix. A set that differed between a Linux CI
// runner and a developer's Windows box would produce a different prefix hash
// on each, so neither would ever hit the other's provider-side cache — and the
// symptom would be a bill, not an error.
//
// A separate tool rather than a `shell: "powershell"` argument for the same
// reason `execute` does not take one: the model chooses a tool by name, and a
// dialect selected by an argument is a dialect the model gets wrong silently.
func PowerShell(opts Options) core.Tool {
	return core.Tool{
		Name:    "powershell",
		Builtin: true,
		Description: "Run a PowerShell command. Windows-only; on other platforms this " +
			"reports that it is unavailable rather than falling back to another shell.",
		// Sequential, like execute: a command has process-wide side effects.
		ExecutionMode: core.Sequential,
		InputSchema: schema.Object(
			schema.Prop("command", schema.String("The PowerShell command to run")),
			schema.Opt("timeout_s", schema.Int("Seconds before the process tree is killed")),
		),
		PromptGuidelines: []string{
			"powershell runs only on Windows; prefer execute for portable commands.",
		},
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Command  string `json:"command"`
				TimeoutS int    `json:"timeout_s"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if a.Command == "" {
				return core.ErrResult("invalid_arguments", "command is required")
			}
			// REQ-TOOL-06: timeout_s is optional with NO default, and when
			// supplied must be positive.
			if a.TimeoutS < 0 {
				return core.ErrResult("invalid_arguments", "timeout_s must be positive")
			}

			// THE DEFERRED CHECK. It happens here, on the call, and not at
			// registration — see the type comment.
			bin, err := ResolvePowerShell()
			if err != nil {
				return core.ErrResult("unsupported_platform", err.Error())
			}

			argv := []string{bin, "-NoProfile", "-NonInteractive", "-Command", a.Command}
			res, err := RunArgv(ctx, argv, ExecOptions{
				Dir:      workspaceRoot(opts),
				Timeout:  time.Duration(a.TimeoutS) * time.Second,
				MaxBytes: DefaultByteLimit,
				SpillDir: opts.SpillDir,
				Env:      opts.Env,
			})
			if err != nil {
				return core.ErrResult("exec_failed", err.Error())
			}
			return execResultToTool(res)
		},
	}
}

func workspaceRoot(opts Options) string {
	if opts.Workspace == nil {
		return ""
	}
	return opts.Workspace.Root
}

// ResolvePowerShell is the ladder: pwsh (PowerShell 7+, cross-platform) then
// Windows PowerShell.
//
// pwsh is tried FIRST and on every platform, because it is the version that is
// still developed and it genuinely runs on Linux and macOS. The Windows-only
// half is the 5.1 fallback. A host that has installed pwsh on Linux gets a
// working tool; one that has not gets an error naming what was searched, which
// is the same contract ResolveShell offers.
func ResolvePowerShell() (string, error) {
	if p, ok := lookPathAny("pwsh", "pwsh.exe"); ok {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		if p, ok := lookPathAny("powershell.exe", "powershell"); ok {
			return p, nil
		}
		return "", fmt.Errorf("tools: no PowerShell found. Searched pwsh and " +
			"powershell.exe on PATH")
	}
	return "", fmt.Errorf("tools: powershell is not available on %s: pwsh is not on "+
		"PATH. This tool is registered on every platform so the tool list — and "+
		"therefore the cached prompt prefix — stays platform-stable; install "+
		"PowerShell 7 (pwsh) to use it here", runtime.GOOS)
}
