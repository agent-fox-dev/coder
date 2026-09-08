package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// makeTargetRe finds a target definition at the start of a line.
func makeTargetRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:`)
}

// DetectVerifyCommand picks the command that decides whether this run
// succeeded. It reads the repository the way a new contributor would: the
// Makefile first, because a project that ships one has already answered the
// question, then the language's default.
//
// It returns "" when it cannot tell. That is a first-class outcome, not a
// failure — the pipeline then refuses to claim the fix is verified, and says
// so, instead of inventing a command and reporting its absence as success.
func DetectVerifyCommand(dir string) string {
	read := func(name string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		return string(b), err == nil
	}

	if mk, ok := read("Makefile"); ok {
		for _, target := range []string{"check", "test"} {
			if makeTargetRe(target).MatchString(mk) {
				return "make " + target
			}
		}
	}
	if _, ok := read("go.mod"); ok {
		return "go test ./..."
	}
	if pkg, ok := read("package.json"); ok {
		if strings.Contains(pkg, `"test"`) {
			return "npm test"
		}
	}
	if _, ok := read("pyproject.toml"); ok {
		if _, uv := read("uv.lock"); uv {
			return "uv run pytest -q"
		}
		return "pytest -q"
	}
	if _, ok := read("Cargo.toml"); ok {
		return "cargo test"
	}
	return ""
}

// Verify runs the quality command and reports what happened.
//
// The command is split on whitespace and executed directly — there is no shell
// between this process and the program. That rules out `make check && lint`
// style compound commands, and the trade is deliberate: a verification step is
// the one place in this program where "what exactly ran" must be unambiguous.
func Verify(ctx context.Context, run Runner, dir, command string, timeout time.Duration) VerifyResult {
	if strings.TrimSpace(command) == "" {
		return VerifyResult{Command: "", Skipped: true}
	}
	argv := strings.Fields(command)
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, code, err := run(ctx, dir, argv)
	res := VerifyResult{
		Command:  command,
		ExitCode: code,
		OK:       err == nil && code == 0,
		Output:   tail(out, 40),
		Elapsed:  time.Since(start),
	}
	if err != nil {
		res.ExitCode = -1
		res.Output = strings.TrimSpace(err.Error() + "\n" + res.Output)
	}
	return res
}

// verifyProgram is the first word of the verify command — the program the
// implementation phase's shell allowlist has to include, or the agent cannot
// run the suite it is being judged by.
func verifyProgram(command string) string {
	if f := strings.Fields(command); len(f) > 0 {
		return filepath.Base(f[0])
	}
	return ""
}
