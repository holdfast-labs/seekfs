package main

import "os"

// testSoakEnabled reports whether the opt-in soak tier runs.
//
// The default `go test ./...` tier must stay fast on a loaded box, so the
// genuinely long differential matrices run a reduced-but-representative
// subset unless SEEKFS_TEST_SOAK=1, which selects the full matrix with the
// same assertions. CI sets it for the long step (see scripts/ci.ps1 and the
// "Test tiers" section in docs/BENCHMARKS.md), so no coverage is lost: every
// shape the reduced matrix skips still runs in CI on every push.
func testSoakEnabled() bool {
	return os.Getenv("SEEKFS_TEST_SOAK") == "1"
}
