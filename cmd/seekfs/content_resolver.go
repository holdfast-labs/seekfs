package main

// The content resolver joins content documents (keyed on stable FRN) to the
// current base record IDs, which shift on every persist/compaction. It is
// rebuilt after each base swap from the persisted FRN column so that content
// never has to be re-extracted when the record table is rewritten.

import "sort"

// contentNoRecordID marks a doc whose FRN has no live base record (deleted, or
// not yet folded into the base). 0xFFFFFFFF is chosen because 0 is a valid
// record ID.
const contentNoRecordID = ^uint32(0)

// contentResolver maps content docs to base record IDs.
//
// docs and docRecordID are parallel. docRecordID[i] is the base record ID for
// docs[i], or contentNoRecordID when dropped. docs is FRN-sorted for a
// single-file base; a sharded base is sorted only within each shard, so order
// then lists doc indexes in FRN order (nil when docs is already sorted).
type contentResolver struct {
	docs        []contentDoc
	docRecordID []uint32
	order       []uint32
}

// buildContentResolver joins the doc table with the base FRN column.
// baseFRNs must be sorted ascending with baseIDs parallel to it (this is the
// shape of the persisted FRNS section). The join is O(len(docs)+len(baseFRNs)).
func buildContentResolver(docs []contentDoc, baseFRNs []uint64, baseIDs []uint32) *contentResolver {
	return buildContentResolverSeq(docs, baseFRNs, baseIDs, nil)
}

// buildContentResolverSeq is buildContentResolver with the per-record-id NTFS
// sequence column (indexed by base record ID). When a doc's FRNSeq and the
// record's sequence are both known and differ, the doc belongs to a previous
// occupant of a reused record and is not joined. A 0 on either side is unknown
// and joins as before.
func buildContentResolverSeq(docs []contentDoc, baseFRNs []uint64, baseIDs []uint32, baseSeq []uint16) *contentResolver {
	r := &contentResolver{
		docs:        docs,
		docRecordID: make([]uint32, len(docs)),
	}
	if !sort.SliceIsSorted(docs, func(i, j int) bool { return docs[i].FRN < docs[j].FRN }) {
		r.order = make([]uint32, len(docs))
		for i := range r.order {
			r.order[i] = uint32(i)
		}
		sort.Slice(r.order, func(a, b int) bool { return docs[r.order[a]].FRN < docs[r.order[b]].FRN })
	}
	j := 0
	for k := range docs {
		i := r.at(k)
		frn := docs[i].FRN
		for j < len(baseFRNs) && baseFRNs[j] < frn {
			j++
		}
		if j < len(baseFRNs) && baseFRNs[j] == frn && j < len(baseIDs) {
			id := baseIDs[j]
			if seqDiffers(docs[i].FRNSeq, seqAt(baseSeq, id)) {
				r.docRecordID[i] = contentNoRecordID
			} else {
				r.docRecordID[i] = id
			}
		} else {
			r.docRecordID[i] = contentNoRecordID
		}
	}
	return r
}

// seqAt returns the sequence for a record id, or 0 when the column is absent.
func seqAt(col []uint16, id uint32) uint16 {
	if int(id) < len(col) {
		return col[id]
	}
	return 0
}

// recordID returns the live base record ID for a doc, and whether it is live.
func (r *contentResolver) recordID(docIndex int) (uint32, bool) {
	if r == nil || docIndex < 0 || docIndex >= len(r.docRecordID) {
		return 0, false
	}
	id := r.docRecordID[docIndex]
	if id == contentNoRecordID {
		return 0, false
	}
	return id, true
}

// docForFRN binary-searches the FRN-sorted doc table.
func (r *contentResolver) docForFRN(frn uint64) (int, bool) {
	if r == nil {
		return 0, false
	}
	k := sort.Search(len(r.docs), func(k int) bool { return r.docs[r.at(k)].FRN >= frn })
	if k < len(r.docs) && r.docs[r.at(k)].FRN == frn {
		return r.at(k), true
	}
	return 0, false
}

// at returns the doc index holding the k-th smallest FRN.
func (r *contentResolver) at(k int) int {
	if r.order == nil {
		return k
	}
	return int(r.order[k])
}

// liveDocCount reports how many docs currently map to a base record.
func (r *contentResolver) liveDocCount() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, id := range r.docRecordID {
		if id != contentNoRecordID {
			n++
		}
	}
	return n
}
