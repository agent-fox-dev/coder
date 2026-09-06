//go:build !race

package agentkit

import (
	"testing"
	"time"
)

// This file is NFR-PERF-09's CI threshold: the half that can actually fail.
//
// A Benchmark function prints a number nobody reads. These tests run the same
// code through testing.Benchmark and assert the result, so a regression breaks
// the build rather than appearing in output someone might scroll past.
//
// WHY `//go:build !race`: the race detector inflates every measurement by
// roughly an order of magnitude. A threshold loose enough to pass under -race
// is too loose to catch anything, and one tight enough to be useful fails the
// race build for a reason that has nothing to do with correctness. The default
// gate is `go test -race ./...`; these budgets are checked by the plain run.
//
// The thresholds carry deliberate headroom over the numbers measured here,
// because CI hardware is slower and noisier than a developer's machine and a
// budget that flakes gets deleted. Headroom is stated per budget rather than
// applied silently.

// budget runs a benchmark and reports ns/op.
//
// The whole file costs roughly fifteen seconds, which is why it honours
// -short: a budget check that makes the ordinary edit-test loop painful gets
// skipped by hand, and then it is not a gate.
func budget(t *testing.T, name string, f func(*testing.B)) time.Duration {
	t.Helper()
	if testing.Short() {
		t.Skip("performance budgets are skipped under -short")
	}
	r := testing.Benchmark(f)
	if r.N == 0 {
		t.Fatalf("%s did not run", name)
	}
	d := time.Duration(r.NsPerOp())
	t.Logf("%s: %v/op (%d iterations)", name, d, r.N)
	return d
}

// TestLoopOverheadBudget is NFR-PERF-01: under 1 ms per turn, excluding model
// API latency and tool execution time.
//
// Measured at ~55 µs. The budget is the requirement's own 1 ms, which is ~18x
// headroom — enough that this fails on a real regression and not on a busy
// runner.
func TestLoopOverheadBudget(t *testing.T) {
	if d := budget(t, "loop turn", BenchmarkLoopTurnOverhead); d > time.Millisecond {
		t.Fatalf("loop overhead is %v per turn, over the NFR-PERF-01 budget of 1ms", d)
	}
}

// TestCacheHitBudget is NFR-PERF-06: a Level 2 hit adds under 0.5 ms over a
// direct response return.
//
// It is measured as a DIFFERENCE, which is what the requirement says. An
// absolute threshold on the hit alone would pass or fail on how expensive the
// direct return happens to be, which is not what is being budgeted.
func TestCacheHitBudget(t *testing.T) {
	direct := budget(t, "direct return", BenchmarkDirectResponse)
	hit := budget(t, "cache hit", BenchmarkCacheHit)
	if over := hit - direct; over > 500*time.Microsecond {
		t.Fatalf("a cache hit adds %v over a direct return, past the NFR-PERF-06 budget "+
			"of 0.5ms. The fingerprint is a SHA-256 over the whole serialized request, so "+
			"this cost grows with the transcript rather than staying constant.", over)
	}
}

// TestCacheControlStampingBudget is NFR-PERF-07: under 1 ms for 128 tools and
// 1000 messages, on EVERY request, with no breakpoint cache and no
// structural-hash recomputation path.
//
// Measured at ~34 ns, which is the point: the rolling breakpoint is three
// pointer writes. The requirement's 1 ms budget exists to rule out the
// "optimization" of caching it behind a structural hash — and this number is
// the evidence that caching it would be trading a static, prefix-only
// breakpoint for nothing at all.
func TestCacheControlStampingBudget(t *testing.T) {
	if d := budget(t, "cache_control stamping", BenchmarkCacheControlStamping); d > time.Millisecond {
		t.Fatalf("stamping takes %v at 128 tools / 1000 messages, over the NFR-PERF-07 "+
			"budget of 1ms", d)
	}
}

// TestSchemaCacheActuallyPaysForItself is NFR-PERF-03's timing half.
//
// The deterministic half — that a steady-state build serializes ZERO schemas —
// is in perf_wiring_test.go, where it does not depend on a clock. This one
// guards against the cache being present and not on the path, which is exactly
// the state this benchmark found it in.
func TestSchemaCacheActuallyPaysForItself(t *testing.T) {
	uncached := budget(t, "build, uncached", BenchmarkAnthropicBuildRequestUncached)
	cached := budget(t, "build, cached", BenchmarkAnthropicBuildRequestCached)
	if cached >= uncached {
		t.Fatalf("a cached build costs %v against an uncached %v; REQ-CACHE-06 is not on "+
			"the request path", cached, uncached)
	}
}
