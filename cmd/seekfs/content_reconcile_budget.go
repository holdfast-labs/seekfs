package main

// Reconcile budget: a scope or journal-wrap reconcile adds files to an
// attached base through the drain, so it must respect the same limit the
// build did. A base the reserve (or an explicit budget) truncated leaves
// every lower-priority in-scope file out on purpose; without this gate the
// next reconcile reads all of them as "new", enqueues them past the limit,
// or, past the rebuild fraction, rebuilds, truncates the same way, and
// loops. Seen on 2026-10-13 on the real E: index: a reserve-limited build of
// 41,724 docs was followed by a metadata reconcile enqueuing 925,450 files
// and a scope reconcile that rebuilt from scratch.
//
// Changed files already in the base always re-extract (that only replaces
// text the budget already admitted). Keys new to the base are admitted in
// the build's own priority order (contentBuildItemLess) while their file
// sizes fit the remaining allowance; the rest stay out, exactly as if the
// build had dropped them.

import "sort"

// contentReconcileNewAllowance returns the bytes of not-yet-indexed files a
// reconcile may add to base on vol: the explicit budget minus the text the
// base already holds, or the reserve allowance on the sidecar's volume.
// MaxInt64 means unlimited (unknown free space).
func contentReconcileNewAllowance(vol *serviceVolumeIndex, base *contentIndex, scope *contentScopeResolved) int64 {
	if scope != nil && scope.BudgetExplicit && scope.BudgetBytes > 0 {
		var used int64
		if base != nil {
			for i := range base.Docs {
				used += int64(base.Docs[i].TextLen)
			}
		}
		if used >= scope.BudgetBytes {
			return 0
		}
		return scope.BudgetBytes - used
	}
	if vol == nil || vol.dbPath == "" {
		return int64(^uint64(0) >> 1)
	}
	return contentScopeAllowance(contentIndexPathForDB(vol.dbPath), 0)
}

// contentReconcileTrimNew keeps the keys of newKeys that fit allowance in
// build priority order, using each file's size as its extracted-text
// estimate. rank reports a key's build ordering and size; keys it does not
// know sort last and are admitted only while room remains. It returns the
// admitted keys (ascending) and the files and bytes left out.
func contentReconcileTrimNew(newKeys []uint64, rank map[uint64]contentScopeMember, allowance int64) (admitted []uint64, droppedFiles int, droppedBytes int64) {
	if allowance == int64(^uint64(0)>>1) {
		return newKeys, 0, 0
	}
	type cand struct {
		item contentBuildItem
		size int64
	}
	cands := make([]cand, 0, len(newKeys))
	for _, key := range newKeys {
		m, ok := rank[key]
		if !ok {
			cands = append(cands, cand{item: contentBuildItem{frn: key, priority: int(^uint(0) >> 1)}})
			continue
		}
		cands = append(cands, cand{item: contentBuildItem{frn: key, path: m.path, priority: m.priority}, size: m.size})
	}
	sort.Slice(cands, func(i, j int) bool { return contentBuildItemLess(cands[i].item, cands[j].item) })
	var used int64
	full := false
	for _, c := range cands {
		if !full && c.size <= allowance-used {
			used += c.size
			admitted = append(admitted, c.item.frn)
			continue
		}
		// Like the build: once the budget bites, everything after it is out,
		// so a later small file never jumps a higher-priority one.
		full = true
		droppedFiles++
		droppedBytes += c.size
	}
	sort.Slice(admitted, func(i, j int) bool { return admitted[i] < admitted[j] })
	return admitted, droppedFiles, droppedBytes
}
