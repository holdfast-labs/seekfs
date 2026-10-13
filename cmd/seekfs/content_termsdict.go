package main

// Dictionary-only terms section (format v7, CXTR v2).
//
// The v6 CXTR carried full postings (docID lists + tf) per term, but no query
// path reads term postings: candidates come from the gram (CXGR) index,
// relevance re-verifies against stored text, and typo tolerance needs only
// the term dictionary with document frequencies. The v7 CXTR is therefore
// the dictionary alone: front-coded keys (shared prefixes stored once, with
// a restart index every 32 keys for logarithmic lookup) plus a u32 document
// frequency per key in key order.
//
// Layout:
//   u32 keyCount | u32 restartInterval | u32 restartCount | u32 keyBlobLen
//     | u32 overflowCount
//   restarts: restartCount x {blobOff u32, keyLen u32, key bytes}
//   keyBlob: keyCount x {[prefixLen uvarint][suffixLen uvarint][suffix bytes]}
//   dfLo: keyCount bytes (document frequency when < 255)
//   overflow: overflowCount x {index u32, df u32} (exact df for keys with
//     df >= 255, sorted by index)
//
// Document frequencies skew tiny (measured: 58% are 1, 99.7% fit a byte), so
// one byte per key plus a small sorted overflow table replaces the u32 array
// at identical values.
//
// The decoded form keeps the blob and df aliased to the section bytes (mmap
// zero-copy like the posting codec); only the restart keys are heap copies
// (~1 MB per million keys). Lookups decode at most one restart interval;
// scans decode sequentially with a single reused buffer.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
)

// contentTermDictRestartInterval is the restart spacing: every Nth key is
// stored whole, so a lookup decodes at most N keys and a scan restarts from
// a nearby absolute key.
const contentTermDictRestartInterval = 32

// contentTermDict is a decoded CXTR v2 section.
type contentTermDict struct {
	numKeys   int
	interval  int
	restarts  []string // every interval-th full key, for positioning
	restartIx []int    // blob offset of each restart key
	blob      []byte   // front-coded keys, aliased to the section
	dfLo      []byte   // numKeys frequency bytes, aliased to the section
	ovIx      []uint32 // overflow key indexes, sorted
	ovDf      []uint32 // overflow frequencies parallel to ovIx
}

// dfAt returns the document frequency of the i-th key: the Lo byte unless
// it is 255, which marks an overflow entry carrying the exact value.
func (d *contentTermDict) dfAt(i int) int {
	if d.dfLo[i] < 255 {
		return int(d.dfLo[i])
	}
	j := sort.Search(len(d.ovIx), func(j int) bool { return d.ovIx[j] >= uint32(i) })
	if j < len(d.ovIx) && d.ovIx[j] == uint32(i) {
		return int(d.ovDf[j])
	}
	return 255
}

func encodeContentTermDict(sortedKeys []string, df map[string]uint32) []byte {
	if len(sortedKeys) == 0 {
		return nil
	}
	const interval = contentTermDictRestartInterval
	restartCount := (len(sortedKeys) + interval - 1) / interval

	var blob bytes.Buffer
	blobOffs := make([]uint32, len(sortedKeys))
	prev := ""
	for i, k := range sortedKeys {
		blobOffs[i] = uint32(blob.Len())
		// Restart records are self-contained (shared=0): a lookup or scan
		// that positions at a restart decodes forward from an empty
		// previous key, so the restart's own record must not reference its
		// predecessor.
		shared := 0
		if i%contentTermDictRestartInterval != 0 {
			for shared < len(prev) && shared < len(k) && prev[shared] == k[shared] {
				shared++
			}
		}
		var tmp [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(tmp[:], uint64(shared))
		blob.Write(tmp[:n])
		n = binary.PutUvarint(tmp[:], uint64(len(k)-shared))
		blob.Write(tmp[:n])
		blob.WriteString(k[shared:])
		prev = k
	}

	var ovIx []uint32
	var ovDf []uint32
	dfLo := make([]byte, len(sortedKeys))
	for i, k := range sortedKeys {
		if v := df[k]; v < 255 {
			dfLo[i] = byte(v)
		} else {
			dfLo[i] = 255
			ovIx = append(ovIx, uint32(i))
			ovDf = append(ovDf, v)
		}
	}

	var out bytes.Buffer
	out.Grow(20 + restartCount*40 + blob.Len() + len(sortedKeys) + len(ovIx)*8)
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(sortedKeys)))
	_ = binary.Write(&out, binary.LittleEndian, uint32(interval))
	_ = binary.Write(&out, binary.LittleEndian, uint32(restartCount))
	_ = binary.Write(&out, binary.LittleEndian, uint32(blob.Len()))
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(ovIx)))
	for r := 0; r < restartCount; r++ {
		i := r * interval
		_ = binary.Write(&out, binary.LittleEndian, blobOffs[i])
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(sortedKeys[i])))
		out.WriteString(sortedKeys[i])
	}
	out.Write(blob.Bytes())
	out.Write(dfLo)
	for i := range ovIx {
		_ = binary.Write(&out, binary.LittleEndian, ovIx[i])
		_ = binary.Write(&out, binary.LittleEndian, ovDf[i])
	}
	return out.Bytes()
}

func decodeContentTermDict(data []byte) (*contentTermDict, error) {
	if len(data) < 20 {
		return nil, errors.New("content term dict truncated")
	}
	keyCount := int(binary.LittleEndian.Uint32(data[0:]))
	interval := int(binary.LittleEndian.Uint32(data[4:]))
	restartCount := int(binary.LittleEndian.Uint32(data[8:]))
	blobLen := int(binary.LittleEndian.Uint32(data[12:]))
	overflowCount := int(binary.LittleEndian.Uint32(data[16:]))
	if keyCount < 0 || interval <= 0 || restartCount < 0 || blobLen < 0 || overflowCount < 0 {
		return nil, errors.New("content term dict negative count")
	}
	if keyCount == 0 {
		if restartCount != 0 || blobLen != 0 || overflowCount != 0 || len(data) != 20 {
			return nil, errors.New("content term dict length mismatch")
		}
		return &contentTermDict{}, nil
	}
	if restartCount != (keyCount+interval-1)/interval {
		return nil, errors.New("content term dict restart count mismatch")
	}
	off := 20
	d := &contentTermDict{
		numKeys:   keyCount,
		interval:  interval,
		restarts:  make([]string, 0, restartCount),
		restartIx: make([]int, 0, restartCount),
	}
	for r := 0; r < restartCount; r++ {
		if off+8 > len(data) {
			return nil, errors.New("content term dict restarts truncated")
		}
		blobOff := int(binary.LittleEndian.Uint32(data[off:]))
		keyLen := int(binary.LittleEndian.Uint32(data[off+4:]))
		off += 8
		if keyLen < 0 || off+keyLen > len(data) {
			return nil, errors.New("content term dict restart key truncated")
		}
		d.restarts = append(d.restarts, string(data[off:off+keyLen]))
		d.restartIx = append(d.restartIx, blobOff)
		off += keyLen
	}
	if off+blobLen+keyCount+overflowCount*8 > len(data) {
		return nil, errors.New("content term dict length mismatch")
	}
	d.blob = data[off : off+blobLen]
	off += blobLen
	d.dfLo = data[off : off+keyCount]
	off += keyCount
	if overflowCount > 0 {
		d.ovIx = make([]uint32, overflowCount)
		d.ovDf = make([]uint32, overflowCount)
		for i := 0; i < overflowCount; i++ {
			d.ovIx[i] = binary.LittleEndian.Uint32(data[off:])
			d.ovDf[i] = binary.LittleEndian.Uint32(data[off+4:])
			off += 8
			if i > 0 && d.ovIx[i] <= d.ovIx[i-1] {
				return nil, errors.New("content term dict overflow out of order")
			}
			if int(d.ovIx[i]) >= keyCount || d.ovDf[i] < 255 {
				return nil, errors.New("content term dict bad overflow entry")
			}
		}
	}
	// Every 255 byte must have its overflow entry: the counts stay exact.
	marked := 0
	for _, v := range d.dfLo {
		if v == 255 {
			marked++
		}
	}
	if marked != overflowCount {
		return nil, errors.New("content term dict overflow count mismatch")
	}
	if off != len(data) {
		return nil, errors.New("content term dict trailing bytes")
	}
	// Verify the blob decodes to strictly ascending keys with the restarts
	// landing on key boundaries: a corrupt blob fails here, not mid-query.
	if err := d.verify(); err != nil {
		return nil, err
	}
	return d, nil
}

// verify decodes every key once, checking order and restart alignment.
func (d *contentTermDict) verify() error {
	prev := ""
	pos := 0
	for i := 0; i < d.numKeys; i++ {
		if i%d.interval == 0 {
			if pos != d.restartIx[i/d.interval] {
				return errors.New("content term dict restart offset mismatch")
			}
		}
		key, next, ok := decodeFrontCodedKey(d.blob, pos, prev)
		if !ok {
			return errors.New("content term dict key truncated")
		}
		if i > 0 && key <= prev {
			return errors.New("content term dict keys out of order")
		}
		if i%d.interval == 0 && key != d.restarts[i/d.interval] {
			return errors.New("content term dict restart key mismatch")
		}
		prev = key
		pos = next
	}
	if pos != len(d.blob) {
		return errors.New("content term dict blob length mismatch")
	}
	return nil
}

// decodeFrontCodedKey decodes one key at blob[pos] with the previous key for
// its shared prefix. ok is false on truncation or a bad prefix length.
func decodeFrontCodedKey(blob []byte, pos int, prev string) (key string, next int, ok bool) {
	shared, n := binary.Uvarint(blob[pos:])
	if n <= 0 {
		return "", 0, false
	}
	pos += n
	suffixLen, n := binary.Uvarint(blob[pos:])
	if n <= 0 {
		return "", 0, false
	}
	pos += n
	if int(shared) > len(prev) || pos+int(suffixLen) > len(blob) {
		return "", 0, false
	}
	return prev[:shared] + string(blob[pos:pos+int(suffixLen)]), pos + int(suffixLen), true
}

// keyAt decodes the i-th key (for tests and range-scan positioning).
// Decoding starts at the restart's own self-contained record with an empty
// previous key and steps forward, so the restart offset needs no
// predecessor.
func (d *contentTermDict) keyAt(i int) (string, bool) {
	if d == nil || i < 0 || i >= d.numKeys {
		return "", false
	}
	r := i / d.interval
	key := ""
	pos := d.restartIx[r]
	for j := r * d.interval; j <= i; j++ {
		next, npos, ok := decodeFrontCodedKey(d.blob, pos, key)
		if !ok {
			return "", false
		}
		key, pos = next, npos
	}
	return key, true
}

// count returns the document frequency of term: the v6 entry.count the fuzzy
// ranker scores by. found is false when term is absent.
func (d *contentTermDict) count(term string) (int, bool) {
	if d == nil || d.numKeys == 0 {
		return 0, false
	}
	r := sort.Search(len(d.restarts), func(i int) bool { return d.restarts[i] > term }) - 1
	if r < 0 {
		r = 0
	}
	key := ""
	pos := d.restartIx[r]
	for i := r * d.interval; i < d.numKeys; i++ {
		next, npos, ok := decodeFrontCodedKey(d.blob, pos, key)
		if !ok {
			return 0, false
		}
		key, pos = next, npos
		if key == term {
			return d.dfAt(i), true
		}
		if key > term {
			return 0, false
		}
	}
	return 0, false
}

// scanRange invokes fn for every key in [lo, hi). The key argument reuses one
// buffer across calls: copy it to retain. Returning false stops early.
// found-section scans stay allocation-light: only true fuzzy candidates
// (which the caller copies into its map) allocate.
func (d *contentTermDict) scanRange(lo, hi int, fn func(key []byte, df int) bool) {
	if d == nil {
		return
	}
	if lo < 0 {
		lo = 0
	}
	if hi > d.numKeys {
		hi = d.numKeys
	}
	if lo >= hi {
		return
	}
	r := lo / d.interval
	// buf decodes incrementally from the restart's own self-contained
	// record (shared=0 rebuilds from empty), so every record in the range
	// — restarts included — takes the same decode path.
	var buf []byte
	pos := d.restartIx[r]
	for i := r * d.interval; i < hi; i++ {
		shared, n := binary.Uvarint(d.blob[pos:])
		if n <= 0 {
			return
		}
		pos += n
		suffixLen, n := binary.Uvarint(d.blob[pos:])
		if n <= 0 {
			return
		}
		pos += n
		if int(shared) > len(buf) || pos+int(suffixLen) > len(d.blob) {
			return
		}
		buf = append(buf[:shared], d.blob[pos:pos+int(suffixLen)]...)
		pos += int(suffixLen)
		if i >= lo {
			if !fn(buf, d.dfAt(i)) {
				return
			}
		}
	}
}

// keyCount reports the dictionary size.
func (d *contentTermDict) keyCount() int {
	if d == nil {
		return 0
	}
	return d.numKeys
}
