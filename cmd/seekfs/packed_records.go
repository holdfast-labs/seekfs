package main

import (
	"encoding/binary"
	"os"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

func (idx *Index) packCompactRecords(dropRecords bool) {
	if idx == nil || !idx.Compact || idx.PackedRecords != nil || idx.MMapRecords != nil {
		return
	}
	idx.PackedRecords = newPackedRecords(idx.Records)
	if dropRecords {
		idx.Records = nil
		idx.CompactNameOrder = nil
	}
}

func (idx *Index) repackCompactRecords() {
	if idx == nil || !idx.Compact || idx.PackedRecords == nil {
		return
	}
	count := idx.PackedRecords.Len()
	records := make([]CompactRecord, count)
	for i := range records {
		records[i] = idx.PackedRecords.At(i)
	}
	idx.PackedRecords = newPackedRecords(records)
	idx.Records = nil
	idx.CompactNameOrder = nil
}

func (idx *Index) packedNameBlobLooksBloated() bool {
	if idx == nil || idx.PackedRecords == nil {
		return false
	}
	recordCount := idx.PackedRecords.Len()
	if recordCount == 0 {
		return false
	}
	limit := recordCount * 64
	if limit < 512*1024*1024 {
		limit = 512 * 1024 * 1024
	}
	return len(idx.PackedRecords.NameBlob) > limit
}

func (p *PackedRecords) Len() int {
	if p == nil {
		return 0
	}
	return len(p.FRNs)
}

func (m *MMapRecords) Len() int {
	if m == nil {
		return 0
	}
	return m.count
}

func (m *MMapRecords) At(i int) CompactRecord {
	if m == nil || i < 0 || i >= m.count {
		return CompactRecord{}
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return CompactRecord{}
	}
	refBytes := 6
	if m.wideRefs {
		refBytes = 8
	}
	parent, nameID := m.recordRefs(base + 16)
	modeOff := base + 16 + refBytes
	sizeOff := modeOff + 4
	modOff := sizeOff + 8
	delOff := modOff + 8
	rec := CompactRecord{
		FRN:       binary.LittleEndian.Uint64(m.recordData[base:]),
		ParentFRN: binary.LittleEndian.Uint64(m.recordData[base+8:]),
		NameOff:   nameID,
		Mode:      binary.LittleEndian.Uint32(m.recordData[modeOff:]),
		Size:      int64(binary.LittleEndian.Uint64(m.recordData[sizeOff:])),
		ModUnix:   int64(binary.LittleEndian.Uint64(m.recordData[modOff:])),
		Deleted:   m.recordData[delOff] != 0,
	}
	if (!m.wideRefs && parent == compactNarrowParentSentinel) || (m.wideRefs && parent == compactWideParentSentinel) {
		rec.Parent = -1
	} else {
		rec.Parent = int32(parent)
	}
	rec.Name, rec.NameLen = m.nameByID(nameID)
	return rec
}

// lowerNameByID lowercases a name that was already resolved from its token.
// Callers that have parsed a record once can reuse its Name/NameOff instead of
// re-parsing the record refs via lowerNameAt.
func (m *MMapRecords) lowerNameByID(nameID uint32, name string) string {
	if m == nil {
		return ""
	}
	derived := m.fileDerived()
	if len(derived.LowerOffs) > 0 && len(derived.LowerLens) == len(derived.LowerOffs) {
		token := int(nameID)
		if token >= 0 && token < len(derived.LowerOffs) {
			off := derived.LowerOffs[token]
			if off == packedLowerSameAsName {
				return name
			}
			length := derived.LowerLens[token]
			end := int(off) + int(length)
			if end >= int(off) && end <= len(derived.LowerBlob) {
				return stringView(derived.LowerBlob[int(off):end])
			}
		}
	}
	if name == "" {
		return ""
	}
	return strings.ToLower(name)
}

func (m *MMapRecords) lowerNameAt(i int) string {
	if m == nil || i < 0 || i >= m.count {
		return ""
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return ""
	}
	_, nameID := m.recordRefs(base + 16)
	name, _ := m.nameByID(nameID)
	return m.lowerNameByID(nameID, name)
}

// deletedAt reports a record's deleted flag without materializing the whole
// record or its name.
func (m *MMapRecords) deletedAt(i int) bool {
	if m == nil || i < 0 || i >= m.count {
		return false
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return false
	}
	refBytes := 6
	if m.wideRefs {
		refBytes = 8
	}
	delOff := base + 16 + refBytes + 20
	if delOff >= len(m.recordData) {
		return false
	}
	return m.recordData[delOff] != 0
}

func (m *MMapRecords) nameAtRecord(i int) string {
	name, _ := m.nameAtRecordWithLen(i)
	return name
}

func (m *MMapRecords) nameAtRecordWithLen(i int) (string, uint16) {
	if m == nil || i < 0 || i >= m.count {
		return "", 0
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return "", 0
	}
	_, nameID := m.recordRefs(base + 16)
	return m.nameByID(nameID)
}

func (m *MMapRecords) fileDerived() indexDerivedSections {
	if m == nil || m.file == nil {
		return indexDerivedSections{}
	}
	return m.file.derived
}

func (m *MMapRecords) recordOffset(i int) (int, bool) {
	if m == nil || i < 0 || i >= m.count {
		return 0, false
	}
	size := compactDiskRecordBytes
	if m.wideRefs {
		size = compactWideDiskRecordBytes
	}
	base := i * size
	if base < 0 || base+size > len(m.recordData) {
		return 0, false
	}
	return base, true
}

// parentNameAt fetches a record's parent id and name with a single offset
// decode for parent-chain walks, instead of a full record assembly plus a
// second decode for the name.
func (m *MMapRecords) parentNameAt(i int) (int32, string) {
	if m == nil || i < 0 || i >= m.count {
		return -1, ""
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return -1, ""
	}
	parent, nameID := m.recordRefs(base + 16)
	var p int32
	if (!m.wideRefs && parent == compactNarrowParentSentinel) || (m.wideRefs && parent == compactWideParentSentinel) {
		p = -1
	} else {
		p = int32(parent)
	}
	name, _ := m.nameByID(nameID)
	return p, name
}

func (m *MMapRecords) recordRefs(off int) (uint32, uint32) {
	if m.wideRefs {
		if off+8 > len(m.recordData) {
			return compactWideParentSentinel, 0
		}
		return binary.LittleEndian.Uint32(m.recordData[off:]), binary.LittleEndian.Uint32(m.recordData[off+4:])
	}
	if off+6 > len(m.recordData) {
		return compactNarrowParentSentinel, 0
	}
	parent := uint32(m.recordData[off]) | uint32(m.recordData[off+1])<<8 | uint32(m.recordData[off+2])<<16
	nameID := uint32(m.recordData[off+3]) | uint32(m.recordData[off+4])<<8 | uint32(m.recordData[off+5])<<16
	return parent, nameID
}

func (m *MMapRecords) nameByID(nameID uint32) (string, uint16) {
	if m == nil {
		return "", 0
	}
	token := int(nameID) * 6
	if token < 0 || token+6 > len(m.tokenTable) {
		return "", 0
	}
	off := binary.LittleEndian.Uint32(m.tokenTable[token:])
	length := binary.LittleEndian.Uint16(m.tokenTable[token+4:])
	end := int(off) + int(length)
	if end < int(off) || end > len(m.nameBlob) {
		return "", 0
	}
	return stringView(m.nameBlob[int(off):end]), length
}

func (m *MMapRecords) scanCapabilities() {
	if m == nil {
		return
	}
	refBytes := 6
	if m.wideRefs {
		refBytes = 8
	}
	for i := 0; i < m.count; i++ {
		base, ok := m.recordOffset(i)
		if !ok {
			return
		}
		sizeOff := base + 16 + refBytes + 4
		modOff := sizeOff + 8
		if binary.LittleEndian.Uint64(m.recordData[sizeOff:]) != 0 {
			m.hasSize = true
		}
		if binary.LittleEndian.Uint64(m.recordData[modOff:]) != 0 {
			m.hasModUnix = true
		}
		if m.hasSize && m.hasModUnix {
			return
		}
	}
}

func (p *PackedRecords) At(i int) CompactRecord {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return CompactRecord{}
	}
	name := p.nameAt(i)
	return CompactRecord{
		FRN:       p.FRNs[i],
		ParentFRN: p.parentFRNAt(i),
		Parent:    p.Parents[i],
		Name:      name,
		NameOff:   p.NameOffs[i],
		NameLen:   p.NameLens[i],
		Mode:      p.modeAt(i),
		Size:      p.sizeAt(i),
		ModUnix:   p.modUnixAt(i),
		Deleted:   p.deletedAt(i),
	}
}

func (p *PackedRecords) Set(i int, rec CompactRecord) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	p.FRNs[i] = rec.FRN
	p.Parents[i] = rec.Parent
	p.setParentFRN(i, rec.ParentFRN)
	if rec.Name != p.nameAt(i) {
		p.setName(i, rec.Name)
		p.setLowerName(i, rec.Name)
	}
	p.setMode(i, rec.Mode)
	p.setSize(i, rec.Size)
	p.setModUnix(i, rec.ModUnix)
	p.setDeleted(i, rec.Deleted)
}

func (p *PackedRecords) Append(rec CompactRecord) {
	if p == nil {
		return
	}
	p.FRNs = append(p.FRNs, rec.FRN)
	p.Parents = append(p.Parents, rec.Parent)
	p.NameOffs = append(p.NameOffs, 0)
	p.NameLens = append(p.NameLens, 0)
	p.LowerOffs = append(p.LowerOffs, 0)
	if need := (len(p.FRNs) + 63) / 64; len(p.DirBits) < need {
		p.DirBits = append(p.DirBits, 0)
	}
	if p.Size32 != nil {
		p.Size32 = append(p.Size32, 0)
		p.setSize(len(p.FRNs)-1, rec.Size)
	} else if rec.Size != 0 {
		p.Size32 = make([]uint32, len(p.FRNs))
		p.setSize(len(p.FRNs)-1, rec.Size)
	}
	if p.ModUnix != nil {
		p.ModUnix = append(p.ModUnix, rec.ModUnix)
	} else if rec.ModUnix != 0 {
		p.ModUnix = make([]int64, len(p.FRNs))
		p.ModUnix[len(p.ModUnix)-1] = rec.ModUnix
	}
	if need := (len(p.FRNs) + 63) / 64; len(p.DeletedBits) < need {
		p.DeletedBits = append(p.DeletedBits, 0)
	}
	p.setName(len(p.FRNs)-1, rec.Name)
	p.setLowerName(len(p.FRNs)-1, rec.Name)
	p.setParentFRN(len(p.FRNs)-1, rec.ParentFRN)
	p.setDeleted(len(p.FRNs)-1, rec.Deleted)
	p.setMode(len(p.FRNs)-1, rec.Mode)
}

func (p *PackedRecords) parentFRNAt(i int) uint64 {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return 0
	}
	if value, ok := p.ParentFRNExtras[i]; ok {
		return value
	}
	parent := int(p.Parents[i])
	if parent >= 0 && parent < len(p.FRNs) {
		return p.FRNs[parent]
	}
	return p.FRNs[i]
}

func (p *PackedRecords) setParentFRN(i int, parentFRN uint64) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	parent := int(p.Parents[i])
	derived := uint64(0)
	if parent >= 0 && parent < len(p.FRNs) {
		derived = p.FRNs[parent]
	} else {
		derived = p.FRNs[i]
	}
	if parentFRN == derived {
		if p.ParentFRNExtras != nil {
			delete(p.ParentFRNExtras, i)
		}
		return
	}
	if p.ParentFRNExtras == nil {
		p.ParentFRNExtras = make(map[int]uint64, 4)
	}
	p.ParentFRNExtras[i] = parentFRN
}

func (p *PackedRecords) sizeAt(i int) int64 {
	if p == nil || i < 0 || i >= len(p.Size32) {
		return 0
	}
	value := p.Size32[i]
	if value != packedSize64Sentinel {
		return int64(value)
	}
	j := sort.Search(len(p.Size64IDs), func(j int) bool { return p.Size64IDs[j] >= uint32(i) })
	if j < len(p.Size64IDs) && p.Size64IDs[j] == uint32(i) && j < len(p.Size64Values) {
		return p.Size64Values[j]
	}
	return 0
}

func (p *PackedRecords) modUnixAt(i int) int64 {
	if p == nil || i < 0 || i >= len(p.ModUnix) {
		return 0
	}
	return p.ModUnix[i]
}

func (p *PackedRecords) deletedAt(i int) bool {
	if p == nil || i < 0 {
		return false
	}
	word := i >> 6
	if word >= len(p.DeletedBits) {
		return false
	}
	return p.DeletedBits[word]&(uint64(1)<<uint(i&63)) != 0
}

func (p *PackedRecords) setDeleted(i int, value bool) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	word := i >> 6
	if word >= len(p.DeletedBits) {
		next := make([]uint64, word+1)
		copy(next, p.DeletedBits)
		p.DeletedBits = next
	}
	mask := uint64(1) << uint(i&63)
	if value {
		p.DeletedBits[word] |= mask
		return
	}
	p.DeletedBits[word] &^= mask
}

func (p *PackedRecords) modeAt(i int) uint32 {
	if p == nil || i < 0 {
		return 0
	}
	if len(p.ModeExtraIDs) > 0 {
		id := uint32(i)
		j := sort.Search(len(p.ModeExtraIDs), func(j int) bool { return p.ModeExtraIDs[j] >= id })
		if j < len(p.ModeExtraIDs) && p.ModeExtraIDs[j] == id && j < len(p.ModeExtraValues) {
			return p.ModeExtraValues[j]
		}
	}
	word := i >> 6
	if word >= len(p.DirBits) {
		return 0
	}
	if p.DirBits[word]&(uint64(1)<<uint(i&63)) != 0 {
		return uint32(os.ModeDir)
	}
	return 0
}

func (p *PackedRecords) setMode(i int, mode uint32) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	word := i >> 6
	if word >= len(p.DirBits) {
		next := make([]uint64, word+1)
		copy(next, p.DirBits)
		p.DirBits = next
	}
	mask := uint64(1) << uint(i&63)
	if mode&uint32(os.ModeDir) != 0 {
		p.DirBits[word] |= mask
	} else {
		p.DirBits[word] &^= mask
	}
	if mode == 0 || mode == uint32(os.ModeDir) {
		p.removeModeExtra(i)
		return
	}
	p.upsertModeExtra(i, mode)
}

func (p *PackedRecords) upsertModeExtra(i int, value uint32) {
	id := uint32(i)
	j := sort.Search(len(p.ModeExtraIDs), func(j int) bool { return p.ModeExtraIDs[j] >= id })
	if j < len(p.ModeExtraIDs) && p.ModeExtraIDs[j] == id {
		p.ModeExtraValues[j] = value
		return
	}
	p.ModeExtraIDs = append(p.ModeExtraIDs, 0)
	copy(p.ModeExtraIDs[j+1:], p.ModeExtraIDs[j:])
	p.ModeExtraIDs[j] = id
	p.ModeExtraValues = append(p.ModeExtraValues, 0)
	copy(p.ModeExtraValues[j+1:], p.ModeExtraValues[j:])
	p.ModeExtraValues[j] = value
}

func (p *PackedRecords) removeModeExtra(i int) {
	id := uint32(i)
	j := sort.Search(len(p.ModeExtraIDs), func(j int) bool { return p.ModeExtraIDs[j] >= id })
	if j >= len(p.ModeExtraIDs) || p.ModeExtraIDs[j] != id {
		return
	}
	copy(p.ModeExtraIDs[j:], p.ModeExtraIDs[j+1:])
	p.ModeExtraIDs = p.ModeExtraIDs[:len(p.ModeExtraIDs)-1]
	copy(p.ModeExtraValues[j:], p.ModeExtraValues[j+1:])
	p.ModeExtraValues = p.ModeExtraValues[:len(p.ModeExtraValues)-1]
}

func (p *PackedRecords) setSize(i int, value int64) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	if p.Size32 == nil {
		if value == 0 {
			return
		}
		p.Size32 = make([]uint32, len(p.FRNs))
	}
	if value >= 0 && value < int64(packedSize64Sentinel) {
		p.Size32[i] = uint32(value)
		return
	}
	p.Size32[i] = packedSize64Sentinel
	p.upsertSize64(i, value)
}

func (p *PackedRecords) upsertSize64(i int, value int64) {
	id := uint32(i)
	j := sort.Search(len(p.Size64IDs), func(j int) bool { return p.Size64IDs[j] >= id })
	if j < len(p.Size64IDs) && p.Size64IDs[j] == id {
		p.Size64Values[j] = value
		return
	}
	p.Size64IDs = append(p.Size64IDs, 0)
	copy(p.Size64IDs[j+1:], p.Size64IDs[j:])
	p.Size64IDs[j] = id
	p.Size64Values = append(p.Size64Values, 0)
	copy(p.Size64Values[j+1:], p.Size64Values[j:])
	p.Size64Values[j] = value
}

func (p *PackedRecords) setModUnix(i int, value int64) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return
	}
	if p.ModUnix == nil {
		if value == 0 {
			return
		}
		p.ModUnix = make([]int64, len(p.FRNs))
	}
	p.ModUnix[i] = value
}

func (p *PackedRecords) nameAt(i int) string {
	if p == nil || i < 0 || i >= len(p.NameOffs) {
		return ""
	}
	off := p.NameOffs[i]
	length := p.NameLens[i]
	end := int(off) + int(length)
	if end < int(off) || end > len(p.NameBlob) {
		return ""
	}
	return stringView(p.NameBlob[int(off):end])
}

// parentNameAt fetches a record's parent id and name with single bounds
// checks for parent-chain walks, instead of a full record assembly plus a
// second name lookup.
func (p *PackedRecords) parentNameAt(i int) (int32, string) {
	if p == nil || i < 0 || i >= len(p.FRNs) {
		return -1, ""
	}
	return p.Parents[i], p.nameAt(i)
}

func (p *PackedRecords) lowerNameAt(i int) string {
	if p == nil || i < 0 || i >= len(p.LowerOffs) {
		return ""
	}
	off := p.LowerOffs[i]
	if off == packedLowerSameAsName {
		return p.nameAt(i)
	}
	length := p.NameLens[i]
	end := int(off) + int(length)
	if end < int(off) || end > len(p.LowerBlob) {
		return strings.ToLower(p.nameAt(i))
	}
	return stringView(p.LowerBlob[int(off):end])
}

func (p *PackedRecords) setName(i int, name string) {
	if p == nil || i < 0 || i >= len(p.NameOffs) {
		return
	}
	if len(name) > int(^uint16(0)) {
		name = name[:int(^uint16(0))]
	}
	p.NameOffs[i] = uint32(len(p.NameBlob))
	p.NameLens[i] = uint16(len(name))
	p.NameBlob = append(p.NameBlob, name...)
}

func (p *PackedRecords) setLowerName(i int, name string) {
	if p == nil || i < 0 || i >= len(p.LowerOffs) {
		return
	}
	lower := strings.ToLower(name)
	if len(lower) > int(^uint16(0)) {
		lower = lower[:int(^uint16(0))]
	}
	if lower == name {
		p.LowerOffs[i] = packedLowerSameAsName
		return
	}
	p.LowerOffs[i] = uint32(len(p.LowerBlob))
	p.LowerBlob = append(p.LowerBlob, lower...)
}

func (p *PackedRecords) setNameDedup(i int, name string, refs map[string]struct {
	off uint32
	len uint16
}) {
	if p == nil || i < 0 || i >= len(p.NameOffs) {
		return
	}
	if len(name) > int(^uint16(0)) {
		name = name[:int(^uint16(0))]
	}
	if ref, ok := refs[name]; ok {
		p.NameOffs[i] = ref.off
		p.NameLens[i] = ref.len
		return
	}
	ref := struct {
		off uint32
		len uint16
	}{off: uint32(len(p.NameBlob)), len: uint16(len(name))}
	refs[name] = ref
	p.NameOffs[i] = ref.off
	p.NameLens[i] = ref.len
	p.NameBlob = append(p.NameBlob, name...)
}

func (p *PackedRecords) setLowerNameDedup(i int, name string, refs map[string]struct {
	off uint32
	len uint16
}) {
	if p == nil || i < 0 || i >= len(p.LowerOffs) {
		return
	}
	lower := strings.ToLower(name)
	if len(lower) > int(^uint16(0)) {
		lower = lower[:int(^uint16(0))]
	}
	if lower == name {
		p.LowerOffs[i] = packedLowerSameAsName
		return
	}
	if ref, ok := refs[lower]; ok {
		p.LowerOffs[i] = ref.off
		return
	}
	ref := struct {
		off uint32
		len uint16
	}{off: uint32(len(p.LowerBlob)), len: uint16(len(lower))}
	refs[lower] = ref
	p.LowerOffs[i] = ref.off
	p.LowerBlob = append(p.LowerBlob, lower...)
}

func stringView(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func (vol *serviceVolumeIndex) markDeleted(id int) {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return
	}
	if vol.children == nil {
		rec := vol.index.compactRecord(id)
		rec.Deleted = true
		vol.index.setCompactRecord(id, rec)
		return
	}
	stack := []int{id}
	for len(stack) > 0 {
		last := len(stack) - 1
		cur := stack[last]
		stack = stack[:last]
		if cur < 0 || cur >= vol.index.compactRecordCount() {
			continue
		}
		rec := vol.index.compactRecord(cur)
		if rec.Deleted {
			continue
		}
		rec.Deleted = true
		vol.index.setCompactRecord(cur, rec)
		for _, childID := range vol.childIDsForRecord(cur) {
			stack = append(stack, int(childID))
		}
	}
}

func (s *goSearchService) servePrivileged() {
	var wg sync.WaitGroup
	const listeners = 8
	for i := 0; i < listeners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.servePipeListener()
		}()
	}
	if s.remoteAddr != "" {
		rs := newRemoteLoopbackServer(s, s.remoteAddr)
		s.remoteSrv = rs
		if err := rs.start(); err != nil {
			serviceLog("remote loopback listener disabled: %v", err)
			s.remoteSrv = nil
		} else {
			serviceLog("remote loopback (Mode L) listening on %s", s.remoteAddr)
		}
	}
	<-s.stop
	if s.remoteSrv != nil {
		s.remoteSrv.close()
		s.remoteSrv = nil
	}
	wg.Wait()
}
