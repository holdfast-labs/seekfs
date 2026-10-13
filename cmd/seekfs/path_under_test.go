package main

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanPathIfNeededMatchesClean proves the allocation-free Clean fast
// path returns exactly filepath.Clean over a corpus of tricky inputs.
func TestCleanPathIfNeededMatchesClean(t *testing.T) {
	fixed := []string{
		"",
		`E:\`,
		`E:\a\b\c.txt`,
		`E:\a\b\`,
		`E:\a\\b\c.txt`,
		`E:\a\b\.`,
		`E:\a\.\b`,
		`E:\a\..\b`,
		`E:\a\b\..\..`,
		`E:\...\a`,
		`E:\.hidden\file`,
		`E:\a b\c d\e.txt`,
		`E:/a/b/c.txt`,
		`E:\a/b\c.txt`,
		`\\host\share`,
		`\\host\share\`,
		`\\host\share\a\b`,
		`\\host\share\\a`,
		`\\host\share\a\`,
		`\\?\C:\a\b`,
		`\\.\C:\a\b`,
		`relative\path`,
		`..\up`,
		`.\here`,
		`a\..\..\b`,
		`C:`,
		`C:foo`,
		`C:\`,
		`C:\a`,
		`\`,
		`\a\b`,
		`E:\a\.`,
		`E:\a\..`,
		`E:\..`,
		`E:\a\...\b`,
		`E:\a....\b`,
		`E:\trailing..`,
	}
	for _, p := range fixed {
		if got, want := cleanPathIfNeeded(p), filepath.Clean(p); got != want {
			t.Fatalf("cleanPathIfNeeded(%q) = %q, want %q", p, got, want)
		}
	}
	// Deterministic pseudo-random corpus over hostile components.
	rng := rand.New(rand.NewSource(42))
	comps := []string{"a", "BB", ".hidden", ".", "..", "...", "x y", "e.txt", "", "Cc", "a..b", "..x", "x.."}
	roots := []string{`E:\`, `E:`, `\\h\s\`, `rel`, ``, `C:\base`}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		b.WriteString(roots[rng.Intn(len(roots))])
		n := 1 + rng.Intn(5)
		for j := 0; j < n; j++ {
			sep := `\`
			switch rng.Intn(6) {
			case 0:
				sep = `/`
			case 1:
				sep = `\\`
			}
			b.WriteString(sep)
			b.WriteString(comps[rng.Intn(len(comps))])
		}
		if rng.Intn(4) == 0 {
			b.WriteString(`\`)
		}
		p := b.String()
		if got, want := cleanPathIfNeeded(p), filepath.Clean(p); got != want {
			t.Fatalf("cleanPathIfNeeded(%q) = %q, want %q", p, got, want)
		}
	}
}

// TestPathUnderMatchesSlowPath proves the cached/fast pathUnder agrees with
// the original Abs+Clean+Rel implementation over tricky inputs, including
// case-only differences (which must fall through to the slow path unchanged).
func TestPathUnderMatchesSlowPath(t *testing.T) {
	paths := []string{
		`E:\bench\chromium\base\test.cc`,
		`E:\bench\chromium\base`,
		`E:\bench\chromium`,
		`E:\bench\chromiumBASE`,
		`E:\bench\chromium\base\`,
		`E:\bench\chromium\.\base\test.cc`,
		`E:\bench\chromium\sub\..\base\test.cc`,
		`E:/bench/chromium/base/test.cc`,
		`E:\BENCH\CHROMIUM\BASE\TEST.CC`,
		`E:\`,
		`F:\other\file.txt`,
		`\\host\share\dir\file.txt`,
		`\\host\share`,
		`\\host\share\`,
		`\\HOST\SHARE\dir`,
		`relative\path\file.txt`,
		`E:\bench\chromiumify\file.txt`,
	}
	roots := []string{
		`E:\bench\chromium`,
		`E:\bench\chromium\base`,
		`E:\bench\chromium\base\`,
		`E:\BENCH\chromium`,
		`e:\bench\chromium\base`,
		`E:\`,
		`E:\bench`,
		`F:\`,
		`\\host\share`,
		`\\host\share\dir`,
		`\\HOST\SHARE`,
		`relative\path`,
		`E:\bench\chromium\base\test.cc`,
		`E:\missing`,
	}
	slowOK := func(p, r string) bool {
		// filepath.Rel hangs (infinite loop) on some UNC pairs, e.g.
		// Rel(`\\host\share`, `\\host\share\`) in Go 1.26. The fast path
		// avoids Rel there; only compare against the slow path where Rel
		// terminates. UNC pairs are covered by fast-only expectations
		// below instead.
		return !(strings.HasPrefix(p, `\\`) || strings.HasPrefix(r, `\\`))
	}
	for _, p := range paths {
		for _, r := range roots {
			if !slowOK(p, r) {
				continue
			}
			if got, want := pathUnder(p, r), pathUnderSlow(p, r); got != want {
				t.Fatalf("pathUnder(%q, %q) = %v, slow = %v", p, r, got, want)
			}
		}
	}
	// Exercise the normalization cache across repeats (same roots twice).
	for _, p := range paths {
		for _, r := range roots {
			if !slowOK(p, r) {
				continue
			}
			if got, want := pathUnder(p, r), pathUnderSlow(p, r); got != want {
				t.Fatalf("pathUnder repeat(%q, %q) = %v, slow = %v", p, r, got, want)
			}
		}
	}
	// UNC pairs the slow path cannot evaluate (stdlib Rel hang): assert the
	// fast path's expected answers directly.
	uncCases := []struct {
		path, root string
		want       bool
	}{
		{`\\host\share`, `\\host\share`, true},
		{`\\host\share\`, `\\host\share`, true},
		{`\\host\share\dir\file.txt`, `\\host\share`, true},
		{`\\host\share\dir`, `\\host\share\dir`, true},
		{`\\host\share`, `\\host\share\dir`, false},
		{`\\host\share\other`, `\\host\share\dir`, false},
		// NOTE: UNC pairs without an exact prefix/equality (e.g.
		// case-only differences) still reach the stdlib Rel, which hangs
		// on some UNC inputs (pre-existing; the fast path adds no new
		// hangs). They are deliberately not exercised here.
	}
	for _, c := range uncCases {
		if got := pathUnder(c.path, c.root); got != c.want {
			t.Fatalf("pathUnder(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
		}
	}
}

// TestPathUnderFastPathFires ensures the exact-prefix shortcut actually
// engages for the common already-clean case (otherwise the optimization
// would be dead code that only tests the slow path).
func TestPathUnderFastPathFires(t *testing.T) {
	clean := `E:\bench\chromium\base\test.cc`
	root := `E:\bench\chromium\base`
	if !pathLooksClean(clean) {
		t.Fatalf("expected clean path %q to look clean", clean)
	}
	if normalizeFilterPathCached(root) != normalizeFilterPath(root) {
		t.Fatal("cached normalization disagrees with direct")
	}
	if !pathUnder(clean, root) {
		t.Fatalf("pathUnder(%q, %q) = false, want true", clean, root)
	}
}

// TestNormalizeFilterPathCacheIsBounded keeps the resident service from
// growing the root-normalization cache by one entry per typed under: root.
func TestNormalizeFilterPathCacheIsBounded(t *testing.T) {
	for i := 0; i < 3*filterPathNormCacheMax; i++ {
		root := filepath.Join(`E:\bench\roots`, strings.Repeat("d", 1+i%7), filepath.Base(strings.Repeat("x", 1+i/7)))
		if got, want := normalizeFilterPathCached(root), normalizeFilterPath(root); got != want {
			t.Fatalf("cached(%q) = %q; want %q", root, got, want)
		}
	}
	n := 0
	filterPathNormCache.Range(func(_, _ any) bool { n++; return true })
	if n > filterPathNormCacheMax {
		t.Fatalf("cache holds %d roots; want at most %d", n, filterPathNormCacheMax)
	}
}
