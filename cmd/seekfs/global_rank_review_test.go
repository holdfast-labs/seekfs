package main

import (
	"errors"
	"runtime"
	"testing"
)

type cancelAtEOFGlobalIterator struct {
	ids      []globalRecordID
	pos      int
	canceled *bool
}

func (it *cancelAtEOFGlobalIterator) Next() (globalRecordID, bool) {
	if it.pos >= len(it.ids) {
		*it.canceled = true
		return globalRecordID{}, false
	}
	id := it.ids[it.pos]
	it.pos++
	return id, true
}

func (it *cancelAtEOFGlobalIterator) SeekGE(target globalRecordID) (globalRecordID, bool) {
	for it.pos < len(it.ids) && compareGlobalRecordID(it.ids[it.pos], target) < 0 {
		it.pos++
	}
	return it.Next()
}

func (it *cancelAtEOFGlobalIterator) CountHint() int { return len(it.ids) }

func TestGlobalVerifiedIteratorPropagatesCancellationAfterDrain(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "small serial candidates", n: 1},
		{name: "large serial candidates", n: 2 * serviceTrigramParallelVerifyMinIDs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canceled := false
			ids := make([]globalRecordID, tc.n)
			for i := range ids {
				// Invalid volume IDs are skipped by verification, keeping this
				// regression focused on cancellation propagation.
				ids[i] = globalRecordID{volume: -1, local: i}
			}
			pq := parsedQuery{Cancel: func() bool { return canceled }}
			newIterator := func() globalIDIterator {
				canceled = false
				return &cancelAtEOFGlobalIterator{ids: ids, canceled: &canceled}
			}

			if _, _, err := collectGlobalVerifiedTopN(newIterator(), nil, nil, pq, 1); !errors.Is(err, errQueryCanceled) {
				t.Fatalf("top-N cancellation error = %v, want %v", err, errQueryCanceled)
			}
			if _, _, err := countGlobalVerifiedIterator(newIterator(), nil, nil, pq); !errors.Is(err, errQueryCanceled) {
				t.Fatalf("count cancellation error = %v, want %v", err, errQueryCanceled)
			}
		})
	}
}
