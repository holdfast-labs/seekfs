package main

import "container/heap"

// Local ranks are a total order within one immutable base. A global top-N
// record must be in its own volume's top-N, so select IDs there first, then
// materialize at most volumes*N entries for the cross-volume comparison.
// Overlay records are merged by the caller after hidden base IDs are removed.
// Queries requiring path verification or ranks that are unavailable retain
// the existing verifier. Unfoldable records still use that verifier inline.
func collectGlobalMemoTopN(ids []globalRecordID, volumes []*serviceVolumeIndex, snapshots []*volumeSnapshot, volumePQs []parsedQuery, rankers []func(int) int, ranksComplete bool, pq parsedQuery, limit int) ([]globalRankedEntry, int, bool, error) {
	// The caller has already resolved the rank arrays. Rechecking through
	// rankForQuery could rebuild an uncached scalar/path rank a second time.
	if !ranksComplete {
		return nil, 0, false, nil
	}
	memos := make([]*queryMemo, len(volumes))
	idents := make([]*nameIdentity, len(volumes))
	for i, vol := range volumes {
		// Live descendant changes can reorder aggregate directory sizes
		// relative to the persisted size ranks. File-only order stays valid.
		if pq.SortColumn == "size" && pq.Type != "file" && vol.index.dirSizeDelta.Load() != nil {
			return nil, 0, false, nil
		}
		memos[i], idents[i] = vol.memoFor(volumePQs[i])
		if memos[i] == nil {
			return nil, 0, false, nil
		}
	}
	heaps := make([]candidateRankMaxHeap, len(volumes))
	caches := make([]map[int]string, len(volumes))
	verified := 0
	for pos, id := range ids {
		if pos&1023 == 0 && queryCanceled(pq) {
			return nil, verified, true, errQueryCanceled
		}
		if id.volume < 0 || id.volume >= len(volumes) || globalHiddenContains(snapshots, id) {
			continue
		}
		vol := volumes[id.volume]
		if id.local < 0 || id.local >= vol.index.compactRecordCount() {
			continue
		}
		if caches[id.volume] == nil {
			caches[id.volume] = make(map[int]string)
		}
		verified++
		if !vol.memoRecordMatchesFields(volumePQs[id.volume], memos[id.volume], idents[id.volume], id.local, caches[id.volume]) {
			continue
		}
		h := &heaps[id.volume]
		item := candidateRankItem{id: id.local, rank: rankers[id.volume](id.local)}
		if h.Len() < limit {
			heap.Push(h, item)
		} else if compareCandidateRank(item.id, (*h)[0].id, rankers[id.volume]) < 0 {
			(*h)[0] = item
			heap.Fix(h, 0)
		}
	}
	selected := make([]globalRecordID, 0, limit*len(volumes))
	for v, h := range heaps {
		for _, item := range h {
			selected = append(selected, globalRecordID{volume: v, local: item.id})
		}
	}
	out, err := rankedEntriesFromGlobalIDs(volumes, selected, pq)
	if err != nil {
		return nil, verified, true, err
	}
	sortGlobalRankedEntries(out, pq)
	if len(out) > limit {
		out = out[:limit]
	}
	pq.Trace.addTerm(traceTerm{Kind: "materialization", Source: "memo-rank-heap", CountHint: len(selected), Exact: true})
	return out, verified, true, nil
}
