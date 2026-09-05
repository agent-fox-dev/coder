// Command difftest runs the NFR-TEST-06/07 wire-level differential harness.
//
//	difftest [-scenarios DIR] [-ledger FILE]
//
// Exit codes are NFR-TEST-07's: 0 clean, 1 a FAIL or a dark run, 3 a stale
// ledger entry.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/agentfox/agentkit-go/difftest"
)

func main() {
	scenarios := flag.String("scenarios", "scenarios", "directory of scenario directories")
	ledger := flag.String("ledger", "known-divergences.json", "accepted-divergence ledger")
	flag.Parse()

	run, err := difftest.Execute(context.Background(), difftest.Options{
		ScenarioDir: *scenarios, LedgerPath: *ledger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "difftest: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(run.Summary())
	os.Exit(run.ExitCode())
}
