package main

import (
	"errors"
	"testing"
)

func TestGlobalMemoTopNParity(t *testing.T) {
	volumes := memoTestVolumes(t)
	ids := make([]globalRecordID, 0)
	for v, vol := range volumes {
		for id := 0; id < vol.index.compactRecordCount(); id++ {
			ids = append(ids, globalRecordID{volume: v, local: id})
		}
	}
	snapshots := []*volumeSnapshot{{tombstoneIDs: []int32{2, 6}, shadowedIDs: []int32{7}}, nil}
	for _, sort := range []string{"", "path", "size", "modified", "extension", "type"} {
		for _, query := range []string{"workspace", "workspace !control", "workspace ext:json", "zzzz-no-hit"} {
			opts := queryOptions{Query: query, MatchPath: true, Limit: 7, Trace: &searchTrace{}}
			if sort != "" {
				opts.Query += " sort:" + sort
			}
			pq := mustParsePrefilterQuery(t, opts)
			t.Setenv("SEEKFS_NAME_MEMO", "0")
			it := newGlobalIDSliceIterator(ids)
			want, _, err := collectGlobalVerifiedTopN(&it, volumes, snapshots, pq, opts.Limit)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SEEKFS_NAME_MEMO", "1")
			it = newGlobalIDSliceIterator(ids)
			got, _, err := collectGlobalVerifiedTopN(&it, volumes, snapshots, pq, opts.Limit)
			if err != nil {
				t.Fatal(err)
			}
			if !sameOrderedStrings(pathsOf(globalRankedEntriesToEntries(got)), pathsOf(globalRankedEntriesToEntries(want))) {
				t.Fatalf("query=%q got=%v want=%v", opts.Query, got, want)
			}
			engaged := false
			for _, term := range opts.Trace.Terms {
				if term.Source == "memo-rank-heap" {
					engaged = true
					if term.CountHint > opts.Limit*len(volumes) {
						t.Fatalf("materialized %d entries for limit %d", term.CountHint, opts.Limit)
					}
				}
			}
			if !engaged {
				t.Fatalf("query=%q did not defer materialization", opts.Query)
			}
		}
	}
}

func TestGlobalMemoTopNCancellation(t *testing.T) {
	volumes := memoTestVolumes(t)
	pq := mustParsePrefilterQuery(t, queryOptions{Query: "workspace", MatchPath: true})
	canceled := false
	pq.Cancel = func() bool { return canceled }
	it := &cancelAtEOFGlobalIterator{ids: []globalRecordID{{volume: 0, local: 2}}, canceled: &canceled}
	if _, _, err := collectGlobalVerifiedTopN(it, volumes, nil, pq, 7); !errors.Is(err, errQueryCanceled) {
		t.Fatalf("got %v want cancellation", err)
	}
}

func BenchmarkGlobalMemoDeferredMaterialization(b *testing.B) {
	idx := dottedPathBenchmarkIndex(50_000)
	idx.packCompactRecords(true)
	volumes := []*serviceVolumeIndex{newServiceVolumeIndex("bench.gsi", idx)}
	ids := make([]globalRecordID, idx.compactRecordCount())
	for id := range ids {
		ids[id] = globalRecordID{local: id}
	}
	pq, err := parseQuery(queryOptions{Query: "workspace", MatchPath: true, Limit: 20})
	if err != nil {
		b.Fatal(err)
	}
	for _, enabled := range []string{"0", "1"} {
		b.Run("memo="+enabled, func(b *testing.B) {
			b.Setenv("SEEKFS_NAME_MEMO", enabled)
			// Warm per-index caches outside the timed steady-state loop.
			volumes[0].memoFor(pq)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				it := newGlobalIDSliceIterator(ids)
				out, _, err := collectGlobalVerifiedTopN(&it, volumes, nil, pq, 20)
				if err != nil || len(out) != 20 {
					b.Fatalf("results=%d err=%v", len(out), err)
				}
			}
		})
	}
}
