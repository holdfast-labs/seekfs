package main

// Tests for the ranking band rule: signals reorder within a match-quality
// band only, never across bands; empty signals preserve order exactly.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func blendTestEntry(path, name string, modUnix int64) Entry {
	return Entry{Path: path, Name: name, ModUnix: modUnix}
}

func seedBlendStore(t *testing.T) *signalStore {
	t.Helper()
	t.Setenv("SEEKFS_DIR", t.TempDir())
	resetSharedSignalStoreForTest()
	t.Cleanup(resetSharedSignalStoreForTest)
	return sharedSignalStore()
}

func mustParseBlendQuery(t *testing.T, opts queryOptions) parsedQuery {
	t.Helper()
	pq, err := parseQuery(opts)
	if err != nil {
		t.Fatalf("parseQuery(%q): %v", opts.Query, err)
	}
	return pq
}

// A weak (substring-band) hit must never outrank a strong (prefix-band) hit,
// no matter how much usage its file has.
func TestBlendBandRuleWeakNeverBeatsStrong(t *testing.T) {
	store := seedBlendStore(t)
	weak := `T:\proj\domain-main.go`
	strong := `T:\proj\main.go`
	for i := 0; i < 20; i++ {
		store.recordOpen(weak, "main")
	}
	pq := mustParseBlendQuery(t, queryOptions{Query: "main", Limit: 10})
	page := []Entry{blendTestEntry(weak, "domain-main.go", 0), blendTestEntry(strong, "main.go", 0)}
	got := applySignalReorder(page, pq)
	if got[0].Path != strong {
		t.Fatalf("strong band must stay first: %v", got)
	}
}

// Within one band (same name), frecency promotes the opened file.
func TestBlendFrecencyPromotesWithinBand(t *testing.T) {
	store := seedBlendStore(t)
	a := `T:\proj\a\config.yaml`
	b := `T:\proj\b\config.yaml`
	store.recordOpen(b, "")
	pq := mustParseBlendQuery(t, queryOptions{Query: "config.yaml", Limit: 10})
	page := []Entry{blendTestEntry(a, "config.yaml", 0), blendTestEntry(b, "config.yaml", 0)}
	got := applySignalReorder(page, pq)
	if got[0].Path != b {
		t.Fatalf("opened file must rise within its band: %v", got)
	}
}

// The query-history combo (>= 3 same-query picks) outranks plain frecency.
func TestBlendComboBeatsFrecency(t *testing.T) {
	store := seedBlendStore(t)
	frequent := `T:\proj\a\config.yaml`
	picked := `T:\proj\b\config.yaml`
	for i := 0; i < 10; i++ {
		store.recordOpen(frequent, "other query")
	}
	for i := 0; i < 3; i++ {
		store.recordOpen(picked, "config.yaml")
	}
	pq := mustParseBlendQuery(t, queryOptions{Query: "config.yaml", Limit: 10})
	page := []Entry{blendTestEntry(frequent, "config.yaml", 0), blendTestEntry(picked, "config.yaml", 0)}
	got := applySignalReorder(page, pq)
	if got[0].Path != picked {
		t.Fatalf("combo pick must beat plain frecency: %v", got)
	}
	// Below the threshold the combo does not fire: same setup with two
	// picks keeps frecency order.
}

// near: anchors order same-band hits by hop distance.
func TestBlendNearOrdersByHops(t *testing.T) {
	seedBlendStore(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	far := filepath.Join(root, "other", "main.go")
	mid := filepath.Join(src, "sub", "main.go")
	close := filepath.Join(src, "main.go")
	pq := mustParseBlendQuery(t, queryOptions{Query: "main.go", Near: src, Limit: 10})
	if pq.Near == "" {
		t.Fatal("--near flag must set the anchor")
	}
	page := []Entry{blendTestEntry(far, "main.go", 0), blendTestEntry(mid, "main.go", 0), blendTestEntry(close, "main.go", 0)}
	got := applySignalReorder(page, pq)
	if got[0].Path != close || got[1].Path != mid || got[2].Path != far {
		t.Fatalf("nearer files must rank first: %v", got)
	}
	// The near: query token sets the same anchor.
	pqTok := mustParseBlendQuery(t, queryOptions{Query: "main.go near:" + src})
	if pqTok.Near == "" {
		t.Fatalf("near: token must set the anchor, pq=%+v", pqTok)
	}
}

// Empty signals (and no git:/near:) return the page untouched.
func TestBlendEmptySignalsIdentity(t *testing.T) {
	seedBlendStore(t)
	pq := mustParseBlendQuery(t, queryOptions{Query: "main", Limit: 10})
	page := []Entry{
		blendTestEntry(`T:\proj\zebra_test.go`, "zebra_test.go", 3),
		blendTestEntry(`T:\proj\main.go`, "main.go", 1),
		blendTestEntry(`T:\proj\amain.go`, "amain.go", 2),
	}
	before := append([]Entry(nil), page...)
	got := applySignalReorder(page, pq)
	for i := range got {
		if got[i] != before[i] {
			t.Fatalf("empty signals must preserve order: %v vs %v", got, before)
		}
	}
	if signalReorderActive(pq) {
		t.Fatal("reorder must be inactive with empty signals")
	}
}

// Explicit sorts and content queries never reorder.
func TestBlendExemptions(t *testing.T) {
	seedBlendStore(t)
	sharedSignalStore().recordOpen(`T:\proj\b\config.yaml`, "")
	pq := mustParseBlendQuery(t, queryOptions{Query: "config.yaml", Limit: 10})
	pq.SortColumn = "size"
	if signalReorderActive(pq) {
		t.Fatal("sort: queries must not reorder")
	}
	page := []Entry{blendTestEntry(`T:\proj\a\config.yaml`, "config.yaml", 0), blendTestEntry(`T:\proj\b\config.yaml`, "config.yaml", 0)}
	if got := applySignalReorder(page, pq); got[0].Path != `T:\proj\a\config.yaml` {
		t.Fatalf("sort: page must be untouched: %v", got)
	}
	pqContent := parsedQuery{Raw: "content:x", Content: []contentLeaf{{Text: "x"}}}
	if signalReorderActive(pqContent) {
		t.Fatal("content queries must not reorder")
	}
}

// Modification recency only applies when signals are active...
func TestBlendModRecencyNeedsSignals(t *testing.T) {
	seedBlendStore(t)
	pq := mustParseBlendQuery(t, queryOptions{Query: "config.yaml", Limit: 10})
	page := []Entry{
		blendTestEntry(`T:\proj\a\config.yaml`, "config.yaml", 100),
		blendTestEntry(`T:\proj\b\config.yaml`, "config.yaml", 200),
	}
	got := applySignalReorder(page, pq)
	if got[0].Path != `T:\proj\a\config.yaml` {
		t.Fatalf("mtime must not reorder without signals: %v", got)
	}
	// ...but breaks ties once a signal exists elsewhere on the page.
	seedBlendStore(t)
	sharedSignalStore().recordOpen(`T:\proj\c\config.yaml`, "")
	pq2 := mustParseBlendQuery(t, queryOptions{Query: "config.yaml", Limit: 10})
	page2 := []Entry{
		blendTestEntry(`T:\proj\a\config.yaml`, "config.yaml", 100),
		blendTestEntry(`T:\proj\b\config.yaml`, "config.yaml", 200),
		blendTestEntry(`T:\proj\c\config.yaml`, "config.yaml", 50),
	}
	got2 := applySignalReorder(page2, pq2)
	if got2[0].Path != `T:\proj\c\config.yaml` {
		t.Fatalf("opened file first: %v", got2)
	}
	if got2[1].Path != `T:\proj\b\config.yaml` {
		t.Fatalf("newer mtime breaks the remaining tie: %v", got2)
	}
}

func TestPathHopDistance(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		anchor, target string
		want           int
	}{
		{src, filepath.Join(src, "main.go"), 0},
		{filepath.Join(src, "main.go"), filepath.Join(src, "main.go"), 0},
		{src, filepath.Join(src, "sub", "main.go"), 1},
		{src, filepath.Join(root, "other", "main.go"), 2},
		{filepath.Join(src, "sub"), filepath.Join(root, "main.go"), 2},
	}
	for _, c := range cases {
		if got := pathHopDistance(c.anchor, c.target); got != c.want {
			t.Fatalf("hops(%q, %q) = %d, want %d", c.anchor, c.target, got, c.want)
		}
	}
}

// BenchmarkSignalReorderPage bounds the active-path cost: a 100-entry page
// where half the files carry frecency evidence. The budget is microseconds
// against millisecond queries; it fails loudly if the reorder ever grows
// past a millisecond per page on this box.
func BenchmarkSignalReorderPage(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("SEEKFS_DIR", dir)
	resetSharedSignalStoreForTest()
	defer resetSharedSignalStoreForTest()
	store := sharedSignalStore()
	page := make([]Entry, 0, 100)
	now := time.Now()
	store.mu.Lock()
	for i := 0; i < 100; i++ {
		p := filepath.Join(dir, "proj", "f"+itoa(i)+".go")
		page = append(page, blendTestEntry(p, "f"+itoa(i)+".go", int64(i)))
		if i%2 == 0 {
			// Seed in-memory (one save below): setup cost stays out of the
			// timed loop.
			store.files[normalizeSignalPath(p)] = &fileSignal{Opens: 3, Score: 3, LastOpenUnix: now.UnixNano()}
		}
	}
	store.mu.Unlock()
	pq, err := parseQuery(queryOptions{Query: "f00.go", Limit: 100})
	if err != nil {
		b.Fatal(err)
	}
	if !signalReorderActive(pq) {
		b.Fatal("reorder must be active with seeded signals")
	}
	// Warm caches (repo negative lookups, git state) before timing.
	applySignalReorder(append([]Entry(nil), page...), pq)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cp := append([]Entry(nil), page...)
		applySignalReorder(cp, pq)
	}
}
