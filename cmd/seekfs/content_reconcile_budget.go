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

// contentReconcileLimits are the two limits a reconcile holds new files to,
// matching the build: text is the transient disk allowance in extracted-text
// bytes (the reserve on the sidecar's volume), index is what remains of an
// explicit budget_bytes in projected index bytes (contentIndexCostEstimate).
// MaxInt64 means unlimited.
type contentReconcileLimits struct {
	text  int64
	index int64
}

const contentReconcileUnlimited = int64(^uint64(0) >> 1)

// contentReconcileNewLimits returns the limits for files a reconcile may add
// to base on vol: the reserve allowance on the sidecar's volume, and the
// explicit budget minus the base's projected index bytes.
//
// The reserve allowance applies ONLY when the base was itself reserve-truncated
// (Policy.ReserveBytes > 0, stamped by the build that hit the reserve). The
// gate exists so such a base does not read its own drops as drift and loop
// through a rebuild that truncates the same way (see the file header). A base
// that fit its selection must not drop newly admitted in-scope files just
// because the disk filled between the build and the reconcile: the drain adds
// docs without the build's whole-corpus spool peak, and the fold keeps
// contentCheckDiskReserve as the write-time guard. Recomputing the live
// allowance for a non-truncated base would silently trim every drive-root file
// the auto scope admits (real E: index, 2026-10-18: 327,669 files left out).
func contentReconcileNewLimits(vol *serviceVolumeIndex, base *contentIndex, scope *contentScopeResolved) contentReconcileLimits {
	lim := contentReconcileLimits{text: contentReconcileUnlimited, index: contentReconcileUnlimited}
	if vol != nil && vol.dbPath != "" && base != nil && base.Policy.ReserveBytes > 0 {
		lim.text = contentScopeAllowance(contentIndexPathForDB(vol.dbPath), 0)
	}
	if scope != nil && scope.BudgetExplicit && scope.BudgetBytes > 0 {
		var used int64
		if base != nil {
			for i := range base.Docs {
				used += contentIndexCostEstimate(int64(base.Docs[i].TextLen))
			}
		}
		lim.index = 0
		if used < scope.BudgetBytes {
			lim.index = scope.BudgetBytes - used
		}
	}
	return lim
}

// contentReconcileTrimNew keeps the keys of newKeys that fit allowance (in
// extracted-text bytes) in build priority order; see contentReconcileTrimNewLimits.
func contentReconcileTrimNew(newKeys []uint64, rank map[uint64]contentScopeMember, allowance int64) (admitted []uint64, droppedFiles int, droppedBytes int64) {
	return contentReconcileTrimNewLimits(newKeys, rank, contentReconcileLimits{text: allowance, index: contentReconcileUnlimited})
}

// contentReconcileTrimNewLimits keeps the keys of newKeys that fit both
// limits in build priority order, using each file's size as its
// extracted-text estimate. rank reports a key's build ordering and size; keys
// it does not know sort last and are admitted only while room remains. It
// returns the admitted keys (ascending) and the files and bytes left out.
func contentReconcileTrimNewLimits(newKeys []uint64, rank map[uint64]contentScopeMember, lim contentReconcileLimits) (admitted []uint64, droppedFiles int, droppedBytes int64) {
	if lim.text == contentReconcileUnlimited && lim.index == contentReconcileUnlimited {
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
	var used, indexUsed int64
	full := false
	for _, c := range cands {
		cost := contentIndexCostEstimate(c.size)
		if !full && c.size <= lim.text-used && cost <= lim.index-indexUsed {
			used += c.size
			indexUsed += cost
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
