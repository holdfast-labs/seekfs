package main

import (
	"bytes"
	"os"
	"testing"
)

// TestSelectiveWithCompanionMatchesInMemory pins the one-pass PNGR+PNGC builder
// against the previous behavior: buildSelectiveCompactNameGramIndex followed by
// optionalSelfNameGramIndex (which rebuilt a full index).  The section bytes
// must match in both modes for every posting cap.
func TestSelectiveWithCompanionMatchesInMemory(t *testing.T) {
	// The ~100k scan fixture guards scale-dependent paths with the same
	// assertions; it runs in the soak tier (CI) while the default tier
	// keeps the 5k/50k shapes. Slice (not map) for deterministic order.
	fixtures := []struct {
		name string
		idx  *Index
	}{
		{"dotted5k", dottedPathBenchmarkIndex(5_000)},
		{"dotted50k", dottedPathBenchmarkIndex(50_000)},
	}
	if testSoakEnabled() {
		fixtures = append(fixtures, struct {
			name string
			idx  *Index
		}{"scan100k", buildLargeScanFixture(2000, 50)})
	} else {
		t.Logf("scan100k skipped (SEEKFS_TEST_SOAK=1 runs it in CI)")
	}
	modes := []struct {
		name string
		env  string
	}{{"parallel", ""}, {"lowmem", "lowmem"}}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			if mode.env == "" {
				os.Unsetenv("SEEKFS_MEMORY_MODE")
			} else {
				os.Setenv("SEEKFS_MEMORY_MODE", mode.env)
			}
			t.Cleanup(func() { os.Unsetenv("SEEKFS_MEMORY_MODE") })
			for _, fx := range fixtures {
				name, idx := fx.name, fx.idx
				for _, maxPosting := range []int{1, 3, 17, 50, 1 << 30} {
					sel := buildSelectiveCompactNameGramIndex(idx, 3, maxPosting)
					wantCompanion := optionalSelfNameGramIndex(idx, sel)
					gotSel, gotCompanion := buildSelectiveCompactNameGramIndexWithCompanion(idx, 3, maxPosting)
					if got, want := encodeGramPostingSection(gotSel, nil), encodeGramPostingSection(sel, nil); !bytes.Equal(got, want) {
						t.Fatalf("%s cap=%d: PNGR differs (%d vs %d bytes)", name, maxPosting, len(got), len(want))
					}
					var gotC, wantC []byte
					if gotCompanion != nil {
						gotC = encodeGramPostingSection(gotCompanion, nil)
					}
					if wantCompanion != nil {
						wantC = encodeGramPostingSection(wantCompanion, nil)
					}
					if !bytes.Equal(gotC, wantC) {
						t.Fatalf("%s cap=%d: PNGC differs (%d vs %d bytes)", name, maxPosting, len(gotC), len(wantC))
					}
				}
			}
		})
	}
}
