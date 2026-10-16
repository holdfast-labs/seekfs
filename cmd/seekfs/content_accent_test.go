package main

import (
	"testing"
)

func accentVolume(t *testing.T, texts ...string) *serviceVolumeIndex {
	files := make([]contentFixtureFile, len(texts))
	for i, text := range texts {
		files[i] = contentFixtureFile{frn: uint64(i + 2), name: string(rune('a'+i)) + ".txt", text: text}
	}
	return newContentQueryVolume(t, files)
}

func TestContentAccentFoldTermAndPhrase(t *testing.T) {
	vol := accentVolume(t, "résumé", "RÉSUMÉ", "café au lait")
	for _, q := range []string{"content:resume", "content:\"cafe au lait\""} {
		got, err := contentServiceSearch(t, vol, q, false)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if q == "content:\"cafe au lait\"" {
			want = 1
		}
		if len(got) != want {
			t.Fatalf("%s hits=%d want=%d", q, len(got), want)
		}
	}
}

func TestContentAccentFoldSubstring(t *testing.T) {
	vol := accentVolume(t, "résumé")
	trace := &searchTrace{}
	got, err := searchServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:sume", Limit: 20, Trace: trace}, false)
	if err != nil || len(got) != 1 {
		t.Fatalf("search=%d err=%v", len(got), err)
	}
	n, ok, err := countServiceVolumes([]*serviceVolumeIndex{vol}, queryOptions{Query: "content:sume", Trace: &searchTrace{}})
	if err != nil || !ok || n != len(got) {
		t.Fatalf("count=%d ok=%v err=%v search=%d", n, ok, err, len(got))
	}
	if trace.DiskCapped || trace.ContentIncomplete {
		t.Fatalf("unexpected cap/incomplete: %+v", trace)
	}
}

func TestContentAccentFoldSmartAccent(t *testing.T) {
	vol := accentVolume(t, "café", "cafe")
	got, err := contentServiceSearch(t, vol, "content:café", false)
	if err != nil || len(got) != 1 || got[0].Path == "" {
		t.Fatalf("got=%v err=%v", namesOf(got), err)
	}
}

func TestContentAccentFoldCaseTrue(t *testing.T) {
	vol := accentVolume(t, "résumé")
	got, err := contentServiceSearch(t, vol, "case:true content:resume", false)
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%v err=%v", namesOf(got), err)
	}
}

func TestContentAccentFoldNFD(t *testing.T) {
	vol := accentVolume(t, "re\u0301sume\u0301")
	for _, q := range []string{"content:resume", "content:résumé"} {
		got, err := contentServiceSearch(t, vol, q, false)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: got=%v err=%v", q, namesOf(got), err)
		}
	}
}

func TestContentAccentFoldSnippetSpan(t *testing.T) {
	vol := accentVolume(t, "xx café yy")
	got, err := contentServiceSearch(t, vol, "content:cafe", false)
	if err != nil || len(got) != 1 || got[0].Snippet == "" || got[0].Snippet == "xx cafe yy" {
		t.Fatalf("snippet=%q err=%v", got[0].Snippet, err)
	}
	pqOpts := queryOptions{Query: "content:cafe"}
	pq, _ := parseQuery(pqOpts)
	lines := contentMatchingLines([]byte("xx café yy\n"), newContentLeafMatcher(pq), contentPositiveLeaves(pq), 10, 0, 0)
	if len(lines) != 1 || lines[0].Text != "xx café yy" {
		t.Fatalf("lines=%+v", lines)
	}
}

func TestContentAccentFoldRegexUnchanged(t *testing.T) {
	vol := accentVolume(t, "résumé")
	got, err := contentServiceSearch(t, vol, "content:/resume/", false)
	if err != nil || len(got) != 0 {
		t.Fatalf("regex should remain accent-sensitive: got=%v err=%v", namesOf(got), err)
	}
}

// content~ and --lines see the accent-folded words too: a fuzzy query finds
// and prints an accented spelling within the distance.
func TestContentAccentFoldFuzzyLines(t *testing.T) {
	selected := true
	servicePluginContentSelection.Store(&selected)
	t.Cleanup(func() { servicePluginContentSelection.Store(nil) })
	pq, err := parseQuery(queryOptions{Query: "content~:resume"})
	if err != nil {
		t.Fatal(err)
	}
	lines := contentMatchingLines([]byte("my résumé\nnothing\nresumes here\n"), newContentLeafMatcher(pq), contentPositiveLeaves(pq), 10, 0, 0)
	if len(lines) != 2 || lines[0].Line != 1 || lines[1].Line != 3 {
		t.Fatalf("fuzzy accent lines = %+v; want lines 1 and 3", lines)
	}
}
