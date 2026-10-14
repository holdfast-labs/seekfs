package main

import "testing"

// uncappedDiskVerify lifts the per-query disk verification budget for the
// test (restored on cleanup). Logic-differential tests prove two paths
// answer identically over thousand-doc corpora; with the production budget
// both arms would cap at different verify orders and diverge on the
// completeness flag alone. Boundedness itself is proven separately by
// small-budget cap tests (TestContentDiskVerifyBudget*).
func uncappedDiskVerify(t testing.TB) {
	t.Helper()
	prev := contentDiskVerifyBudget
	contentDiskVerifyBudget = 1 << 30
	t.Cleanup(func() { contentDiskVerifyBudget = prev })
}

// stubContentDiskText substitutes fixture text for disk reads until the test
// ends, keyed by the exact path the verifier would open. A missing key reads
// as absent, like a missing file. v10 verifies from disk, so synthetic
// in-memory corpora (fake paths) serve their fixture text through this seam
// while the gate/predicate/ordering logic runs exactly; only the read syscall
// is doubled. Tests that prove disk behavior itself (freshness, caps,
// offline CLI) use real files. Production never sets the seam.
func stubContentDiskText(t testing.TB, texts map[string]string) {
	t.Helper()
	prev := contentDiskReadForVerify
	contentDiskReadForVerify = func(path string, s contentExtractSettings) ([]byte, bool) {
		if text, ok := texts[path]; ok {
			return contentStoredText([]byte(text)), true
		}
		return prev(path, s)
	}
	t.Cleanup(func() { contentDiskReadForVerify = prev })
}

// stubContentDiskPathMap remaps synthetic verify paths to real files for
// every content read (queries, fold re-reads, builds): the remapped reads go
// through the real extractor, so churned content stays fresh. It covers
// fixtures whose record index resolves to synthetic paths (e.g. a `C:\`
// volume over temp-dir files) while the bytes live elsewhere. Unmapped
// paths read as given.
func stubContentDiskPathMap(t testing.TB, remap map[string]string) {
	t.Helper()
	prev := contentResolveForRead
	contentResolveForRead = func(path string) string {
		if real, ok := remap[path]; ok {
			return real
		}
		if prev != nil {
			return prev(path)
		}
		return path
	}
	t.Cleanup(func() { contentResolveForRead = prev })
}

// stubContentDiskTextForIndex maps every record's reconstructed index path to
// fixture text given parallel to the record list ("" = absent), so v10 disk
// verification serves a synthetic in-memory corpus through the service path.
func stubContentDiskTextForIndex(t testing.TB, idx *Index, texts []string) {
	t.Helper()
	m := make(map[string]string, len(texts))
	cache := make(map[int]string, len(texts))
	for i, text := range texts {
		if text == "" {
			continue
		}
		if p := idx.reconstructCompactPathCached(i, cache); p != "" {
			m[p] = text
		}
	}
	stubContentDiskText(t, m)
}
