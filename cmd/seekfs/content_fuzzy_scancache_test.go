package main

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// The scan memo must be exact-equivalent to a rescan: corrections with the
// cache disabled, enabled cold, and enabled warm are identical, and the warm
// run hits. The synthetic dict crosses the parallel-scan threshold so both
// scan arms are covered.
func TestContentFuzzyScanCacheParity(t *testing.T) {
	keys := []string{"receive", "receiver", "received", "receives", "retrieve", "release", "relieve"}
	df := map[string]uint32{}
	for _, k := range keys {
		df[k] = 3
	}
	for i := 0; i < 12000; i++ {
		k := fmt.Sprintf("filler%05d", i)
		keys = append(keys, k)
		df[k] = 1
	}
	sort.Strings(keys)
	d, err := decodeContentTermDict(encodeContentTermDict(keys, df))
	if err != nil {
		t.Fatalf("decode dict: %v", err)
	}
	if d.gen == 0 {
		t.Fatal("decoded dict has no content generation")
	}
	scan := func(word string) map[string]int {
		wr := []rune(word)
		wb := []byte(word)
		tau := fuzzyThresholdForTerm(word)
		keep := make(map[string]int)
		contentFuzzyScanIndex(d, wr, wb, true, fuzzyLetterMask(word), piecesOf(word, tau), tau, keep)
		return keep
	}
	oldEnabled := contentFuzzyScanCacheEnabled
	defer func() { contentFuzzyScanCacheEnabled = oldEnabled }()

	contentFuzzyScanCacheEnabled = false
	rawTypo := scan("recieve")
	rawNonsense := scan("zzqqxxjjj")
	if len(rawNonsense) != 0 {
		t.Fatalf("nonsense scan = %d terms; want empty", len(rawNonsense))
	}
	if rawTypo["receive"] == 0 {
		t.Fatalf("raw typo scan misses receive: %v", rawTypo)
	}

	hitsBefore := contentFuzzyScanCache.hits.Load()
	contentFuzzyScanCacheEnabled = true
	coldTypo := scan("recieve")
	coldNonsense := scan("zzqqxxjjj")
	if !reflect.DeepEqual(rawTypo, coldTypo) || !reflect.DeepEqual(rawNonsense, coldNonsense) {
		t.Fatalf("cold-cache scan differs:\n raw=%v\n cold=%v", rawTypo, coldTypo)
	}
	warmTypo := scan("recieve")
	warmNonsense := scan("zzqqxxjjj")
	if !reflect.DeepEqual(rawTypo, warmTypo) || !reflect.DeepEqual(rawNonsense, warmNonsense) {
		t.Fatalf("warm-cache scan differs:\n raw=%v\n warm=%v", rawTypo, warmTypo)
	}
	if got := contentFuzzyScanCache.hits.Load() - hitsBefore; got != 2 {
		t.Fatalf("cache hits = %d; want 2 (one per warm word)", got)
	}
	// A second dict with equal content but its own generation decodes to the
	// same corrections through its own memo entry.
	d2, err := decodeContentTermDict(encodeContentTermDict(keys, df))
	if err != nil {
		t.Fatalf("decode dict2: %v", err)
	}
	if d2.gen == d.gen {
		t.Fatal("distinct decodes share a content generation")
	}
	keep2 := make(map[string]int)
	contentFuzzyScanIndex(d2, []rune("recieve"), []byte("recieve"), true, fuzzyLetterMask("recieve"), piecesOf("recieve", fuzzyThresholdForTerm("recieve")), fuzzyThresholdForTerm("recieve"), keep2)
	if !reflect.DeepEqual(rawTypo, keep2) {
		t.Fatalf("second dict scan differs:\n raw=%v\n d2=%v", rawTypo, keep2)
	}
}

func piecesOf(word string, tau int) [][]byte {
	var out [][]byte
	for _, p := range fuzzyPigeonholePieces(word, tau) {
		out = append(out, []byte(p))
	}
	return out
}
