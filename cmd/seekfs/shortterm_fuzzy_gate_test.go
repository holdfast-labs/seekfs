package main

import (
	"testing"
)

func TestShortTermAutoFuzzyGate(t *testing.T) {
	vol := buildFuzzyTestVolume(t)
	exact := []Entry{{Path: `Q:\exact-tonic.txt`, Name: "exact-tonic.txt"}}
	// Unmarked query with a partial exact set: must NOT top up.
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, queryOptions{Query: "tonics", Limit: 10}, exact); added {
		t.Fatal("unmarked partial exact set was topped up; want exact-only")
	}
	// Unmarked zero-result query: must still backfill.
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, queryOptions{Query: "tonics", Limit: 10}, nil); !added {
		t.Fatal("unmarked zero-result query did not backfill")
	}
	// Explicit marker on partial exact: must still top up.
	if _, added := appendFuzzyServiceMatches([]*serviceVolumeIndex{vol}, queryOptions{Query: "tonics~", Limit: 10}, exact); !added {
		t.Fatal("explicitly marked partial exact set did not top up")
	}
}
