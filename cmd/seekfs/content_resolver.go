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
// docs and docRecordID are parallel and docs is sorted by FRN. docRecordID[i]
// is the base record ID for docs[i], or contentNoRecordID when dropped.
type contentResolver struct {
	docs        []contentDoc
	docRecordID []uint32
}

// buildContentResolver joins the FRN-sorted doc table with the base FRN column.
// baseFRNs must be sorted ascending with baseIDs parallel to it (this is the
// shape of the persisted FRNS section). The join is O(len(docs)+len(baseFRNs)).
func buildContentResolver(docs []contentDoc, baseFRNs []uint64, baseIDs []uint32) *contentResolver {
	r := &contentResolver{
		docs:        docs,
		docRecordID: make([]uint32, len(docs)),
	}
	j := 0
	for i := range docs {
		frn := docs[i].FRN
		for j < len(baseFRNs) && baseFRNs[j] < frn {
			j++
		}
		if j < len(baseFRNs) && baseFRNs[j] == frn && j < len(baseIDs) {
			r.docRecordID[i] = baseIDs[j]
		} else {
			r.docRecordID[i] = contentNoRecordID
		}
	}
	return r
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
	i := sort.Search(len(r.docs), func(i int) bool { return r.docs[i].FRN >= frn })
	if i < len(r.docs) && r.docs[i].FRN == frn {
		return i, true
	}
	return 0, false
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
