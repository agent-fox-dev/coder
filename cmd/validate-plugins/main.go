// Command validate-plugins is REQ-PLUGIN-10's `--validate-plugins`.
//
// The requirement names `nightshift --validate-plugins`; nightshift is a
// daemon built ON this SDK, so the SDK ships the check as a library function
// (plugins.Validate) and this binary as the reference driver. A host embeds
// the same call behind its own flag.
//
// It loads every configured manifest, runs interface conformance checks and
// REQ-PLUGIN-09's import lint, and reports violations WITHOUT starting
// anything. A registry is not populated here, so every manifest reports as
// unregistered — which is correct for a standalone binary and is why a host
// with plugins linked in should call plugins.Validate directly, passing its
// own registry.
//
//	validate-plugins -path ./plugins [-path ./more] [-disable name]
//
// Exit 0 when clean, 1 when any error-severity violation is found.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/agentfox/agentkit-go/plugins"
)

type list []string

func (l *list) String() string     { return fmt.Sprint(*l) }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	var paths, disabled list
	flag.Var(&paths, "path", "directory to search for plugin.toml (repeatable)")
	flag.Var(&disabled, "disable", "plugin name to skip (repeatable)")
	flag.Parse()

	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "validate-plugins: at least one -path is required; there is "+
			"no implicit search path (REQ-PLUGIN-05)")
		os.Exit(2)
	}

	rep := plugins.Validate(plugins.Config{Paths: paths, Disabled: disabled}, plugins.NewRegistry())
	fmt.Print(rep.String())
	os.Exit(rep.ExitCode())
}
