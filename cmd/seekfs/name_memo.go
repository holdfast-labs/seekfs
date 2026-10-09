package main

// Name-memo query lane (items 1-3 of the fsearch-inspired rounds).
//
// fsearch scores distinct names once per query instead of verifying entries
// one by one. This file maps that design onto seekfs:
//
//   - nameIdentity (per index, built once in the background): dense ids for
//     distinct lowercased names, a u64 char mask per name, one exemplar
//     record per name, per-record name ids, and the parentless roots.
//     Token (mmap) volumes use the token itself as the name id; packed
//     volumes group records by blob (off,len), which the deduped builder
//     guarantees equals string equality. Plain-Records indexes decline.
//   - queryMemo (per index, single-entry cache): slot assignment for one
//     query plus per-name term bits and, in path mode, dir bits folded down
//     the tree. An entry matches iff its own bits plus its parent's folded
//     bits cover every required slot, exactly mirroring entryMatches for the
//     supported query family (plain terms, dir: terms, one extension,
//     globs, single-plain-term negations, scalar filters).
//
// Base mutations that can change names (set/appendCompactRecord) reset the
// identity; order-preserving repacks keep it. Records the identity cannot
// see (cycles, dangling parents, post-build mutations mid-flight) fall back
// to the exact legacy verify path per record, so the lane is exact under
// every graph shape.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// memoUnknownName is the recName value for records whose name postdates the
// identity (or that the identity never covered). Such records always take
// the legacy verify path.
const memoUnknownName = ^uint32(0)

// memoMaxPosSlots caps the positive bit slots (terms + extension + globs)
// in one uint16; memoMaxNegSlots caps the negation slots in another.
const memoMaxPosSlots = 12
const memoMaxNegSlots = 4

// ---------------------------------------------------------------------------
// Char masks (item 2)
// ---------------------------------------------------------------------------

// memoMaskAdd folds one byte into a char mask. The alphabet mirrors the
// lowercased names and terms the memo compares: a-z, 0-9, '.', '-', '_',
// ' ', non-ASCII, and everything else. ASCII uppercase folds to lowercase so
// raw and folded strings produce identical masks; the subset test is a
// necessary condition for substring containment and therefore never
// false-negatives.
func memoMaskAdd(m uint64, b byte) uint64 {
	switch {
	case b >= 'a' && b <= 'z':
		return m | (1 << (b - 'a'))
	case b >= 'A' && b <= 'Z':
		return m | (1 << (b - 'A'))
	case b >= '0' && b <= '9':
		return m | (1 << (26 + b - '0'))
	case b == '.':
		return m | (1 << 36)
	case b == '-' || b == '_':
		return m | (1 << 37)
	case b == ' ':
		return m | (1 << 38)
	case b >= 0x80:
		return m | (1 << 39)
	default:
		return m | (1 << 40)
	}
}

func memoMaskOf(s string) uint64 {
	var m uint64
	for i := 0; i < len(s); i++ {
		m = memoMaskAdd(m, s[i])
	}
	return m
}

// ---------------------------------------------------------------------------
// Distinct-name identity (item 1, substrate)
// ---------------------------------------------------------------------------

type nameIdentity struct {
	// token is true for mmap volumes, where the name id is the token itself.
	token   bool
	count   int      // distinct names
	records int      // record count at build
	masks   []uint64 // per distinct name
	// exemplar maps a distinct name to one record carrying it (packed
	// only; token volumes resolve strings via nameByID).
	exemplar []uint32
	// recName maps a record to its distinct name id (packed), or its token
	// (mmap). memoUnknownName when unknown.
	recName []uint32
	roots   []uint32
	// Lowercased volume/root prefix strings: a term contained in either is
	// a substring of every reconstructed path (same guard as
	// plainTermMayOccurInPathPrefix, granted here instead of declined).
	volLower  string
	rootLower string
}

func (idx *Index) resetNameIdentity() {
	if idx == nil || idx.nameIdent.Load() == nil {
		return
	}
	idx.nameIdentMu.Lock()
	idx.nameIdent.Store(nil)
	idx.memoTopo.Store(nil)
	idx.nameMemo.resetLocked(idx.memoGen.Add(1))
	idx.nameIdentMu.Unlock()
}

// memoLaneEnabled is the kill switch for the name-memo lane (escape hatch
// and A/B baseline): SEEKFS_NAME_MEMO=0 disables identity build and every
// memo entry point, restoring legacy paths exactly.
func memoLaneEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SEEKFS_NAME_MEMO")))
	return v != "0" && v != "false" && v != "no" && v != "off"
}

// ensureNameIdentity returns the identity, building it once under a mutex.
// It returns nil for shapes without intrinsic name ids (plain Records) or
// when anything is missing. In low-memory mode the per-record name array is
// skipped and the lane stays off. Callers treat nil as "memo lane
// unavailable".
func (idx *Index) ensureNameIdentity() *nameIdentity {
	if idx == nil || serviceLowMemoryMode() || !memoLaneEnabled() {
		return nil
	}
	if ident := idx.nameIdent.Load(); ident != nil {
		if ident.records == idx.compactRecordCount() {
			return ident
		}
		// Count changed under us (append without reset raced a query):
		// drop it rather than serve a short recName.
		idx.resetNameIdentity()
		return nil
	}
	idx.nameIdentMu.Lock()
	defer idx.nameIdentMu.Unlock()
	if ident := idx.nameIdent.Load(); ident != nil {
		return ident
	}
	var ident *nameIdentity
	if idx.MMapRecords != nil {
		ident = buildTokenNameIdentity(idx)
	} else if idx.PackedRecords != nil {
		ident = buildPackedNameIdentity(idx)
	}
	if ident != nil {
		idx.nameIdent.Store(ident)
	}
	return ident
}

func memoVolumeRootLower(idx *Index) (string, string) {
	volLower := ""
	if idx.Volume != "" {
		volLower = strings.ToLower(idx.Volume)
	}
	rootLower := ""
	if len(idx.Roots) > 0 {
		rootLower = strings.ToLower(idx.Roots[0])
		// Roots[0] replaces Volume during path reconstruction.
		volLower = ""
	}
	return volLower, rootLower
}

// buildTokenNameIdentity assigns one mask per token and reads every record's
// token once for the recName array and exemplar-free lookup. Masks come from
// the derived lowercase names when present, falling back to folding the raw
// name (one transient allocation per token at build time only).
func buildTokenNameIdentity(idx *Index) *nameIdentity {
	m := idx.MMapRecords
	if m == nil {
		return nil
	}
	n := idx.compactRecordCount()
	if n == 0 {
		return nil
	}
	tokenCount := len(m.tokenTable) / 6
	if tokenCount <= 0 {
		return nil
	}
	ident := &nameIdentity{token: true, masks: make([]uint64, tokenCount), recName: make([]uint32, n)}
	for t := 0; t < tokenCount; t++ {
		name, _ := m.nameByID(uint32(t))
		lower := m.lowerNameByID(uint32(t), name)
		if lower == "" && name == "" {
			continue
		}
		if lower == "" {
			lower = strings.ToLower(name)
		}
		ident.masks[t] = memoMaskOf(lower)
	}
	for i := 0; i < n; i++ {
		tok, ok := m.tokenAt(i)
		if !ok {
			ident.recName[i] = memoUnknownName
			continue
		}
		ident.recName[i] = tok
		if parent, _ := m.parentNameAt(i); parent < 0 || int(parent) >= n || int(parent) == i {
			ident.roots = append(ident.roots, uint32(i))
		}
	}
	ident.count = tokenCount
	ident.records = n
	ident.volLower, ident.rootLower = memoVolumeRootLower(idx)
	return ident
}

// buildPackedNameIdentity groups records by blob (off,len) — equal for equal
// names under the deduped builder — sorts the groups, and numbers them
// densely. Masks and exemplars come from one record per group. No name
// strings are materialized: grouping is pure integer sorting.
func buildPackedNameIdentity(idx *Index) *nameIdentity {
	p := idx.PackedRecords
	if p == nil {
		return nil
	}
	n := len(p.FRNs)
	if n == 0 {
		return nil
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		oa, ob := p.NameOffs[order[a]], p.NameOffs[order[b]]
		if oa != ob {
			return oa < ob
		}
		la, lb := p.NameLens[order[a]], p.NameLens[order[b]]
		if la != lb {
			return la < lb
		}
		return order[a] < order[b]
	})
	ident := &nameIdentity{recName: make([]uint32, n)}
	var lastOff uint32
	var lastLen uint16
	first := true
	for _, id := range order {
		off, ln := p.NameOffs[id], p.NameLens[id]
		if first || off != lastOff || ln != lastLen {
			lower := p.lowerNameAt(id)
			ident.masks = append(ident.masks, memoMaskOf(lower))
			ident.exemplar = append(ident.exemplar, uint32(id))
			lastOff, lastLen, first = off, ln, false
		}
		ident.recName[id] = uint32(len(ident.masks) - 1)
		if parent := p.Parents[id]; parent < 0 || int(parent) >= n || int(parent) == id {
			ident.roots = append(ident.roots, uint32(id))
		}
	}
	ident.count = len(ident.masks)
	ident.records = n
	ident.volLower, ident.rootLower = memoVolumeRootLower(idx)
	return ident
}

// tokenAt reads just a record's name token (name id) without assembling the
// record: one offset decode for memo lookups.
func (m *MMapRecords) tokenAt(i int) (uint32, bool) {
	if m == nil || i < 0 || i >= m.count {
		return 0, false
	}
	base, ok := m.recordOffset(i)
	if !ok {
		return 0, false
	}
	_, nameID := m.recordRefs(base + 16)
	return nameID, true
}

// identName returns the folded name string for a distinct name id.
func (ident *nameIdentity) identName(idx *Index, id uint32) string {
	if ident == nil || int(id) >= ident.count {
		return ""
	}
	if ident.token {
		if m := idx.MMapRecords; m != nil {
			name, _ := m.nameByID(id)
			if lower := m.lowerNameByID(id, name); lower != "" {
				return lower
			}
			return strings.ToLower(name)
		}
		return ""
	}
	if int(id) >= len(ident.exemplar) {
		return ""
	}
	return idx.compactLowerNameAt(int(ident.exemplar[id]))
}

// ---------------------------------------------------------------------------
// Per-query memo (item 3)
// ---------------------------------------------------------------------------

// nameMemoCache is the single-entry per-query memo cache on Index. Hits
// require an exact signature match plus a record-count check; anything else
// rebuilds. It carries its own lock so readers never touch volume mutexes.
type nameMemoCache struct {
	mu      sync.Mutex
	sig     string
	records int
	gen     uint64
	memo    *queryMemo
	ident   *nameIdentity
}

func (c *nameMemoCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sig = ""
	c.records = 0
	c.gen = 0
	c.memo = nil
	c.ident = nil
	c.mu.Unlock()
}

// resetLocked clears the entry while the caller holds no cache lock; the
// generation passed in is the post-reset generation, so any concurrent
// builder holding an older one cannot store afterwards.
func (c *nameMemoCache) resetLocked(gen uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sig = ""
	c.records = 0
	c.gen = gen
	c.memo = nil
	c.ident = nil
	c.mu.Unlock()
}

// queryMemo is one query's scored names plus folded dir bits.
type queryMemo struct {
	sig       string
	records   int
	matchPath bool
	terms     []string // union slots (plain terms + dir: terms)
	termMask  []uint64
	prefixOK  []bool
	hasExt    bool
	ext       string
	extBit    uint16
	globs     []string
	globBit   []uint16
	negs      []string
	negMask   []uint64
	// impossible is set when the query provably matches nothing (differing
	// required extensions, or a negated term in the volume/root prefix).
	impossible bool
	// negPrefixHit forces empty (a negated term is a substring of every path).
	negPrefixHit bool
	nameBits     []uint16 // per distinct name: covered positive slots
	nameNeg      []uint16 // per distinct name: hit negation slots
	reqMask      uint16   // positive term slots that must be covered
	// folded reports whether dir bits were folded (path mode with
	// ancestor-dependent slots). When false, no slot can match through
	// ancestors, so own bits decide alone and no visited set exists.
	folded  bool
	dirBits []uint16 // per record: folded-down term bits (path mode)
	dirNeg  []uint16 // per record: folded-down negation bits
	visited []uint64 // per-record bitset: folded (memo applies)
}

// memoSigFor canonicalizes the match-relevant part of a (volume-dropped)
// parsed query. Scalar filters (type/size/date/attr/mod) are record-level
// and evaluated in the scan, so they are excluded: queries differing only
// there share the memo.
func memoSigFor(pq parsedQuery) string {
	var b strings.Builder
	if pq.MatchPath {
		b.WriteByte('P')
	}
	for _, t := range nonVolumeTerms(pq.Terms) {
		b.WriteByte('T')
		b.WriteString(t)
		b.WriteByte(0)
	}
	for _, d := range pq.Dirs {
		b.WriteByte('D')
		b.WriteString(d)
		b.WriteByte(0)
	}
	for _, e := range pq.Exts {
		b.WriteByte('E')
		b.WriteString(e)
		b.WriteByte(0)
	}
	for _, g := range pq.Globs {
		b.WriteByte('G')
		b.WriteString(g)
		b.WriteByte(0)
	}
	for _, n := range pq.NotGroups {
		b.WriteByte('N')
		b.WriteString(memoNegKey(n))
		b.WriteByte(0)
	}
	return b.String()
}

// memoNegKey returns the single plain term of a negation subquery, or "" if
// the group is not memo-shapable (the whole query then declines the lane).
func memoNegKey(neg parsedQuery) string {
	if len(neg.Terms) != 1 || isVolumeQueryTerm(neg.Terms[0]) || neg.CaseSensitive || neg.Fuzzy || len(neg.Exts) != 0 || len(neg.Dirs) != 0 ||
		len(neg.Globs) != 0 || len(neg.Regexps) != 0 || len(neg.RegexTerms) != 0 ||
		len(neg.Parents) != 0 || neg.Under != "" || neg.Exists || neg.HasModAfter ||
		len(neg.SizeFilters) != 0 || len(neg.DateFilters) != 0 || len(neg.AttrFilters) != 0 ||
		len(neg.OrGroups) != 0 || len(neg.NotGroups) != 0 || neg.Type != "" {
		return ""
	}
	return nonVolumeTerms(neg.Terms)[0]
}

// memoSupported reports whether the memo lane can answer pq exactly.
// Anything exotic declines and the legacy lanes run unchanged.
func memoSupported(pq parsedQuery) bool {
	if pq.CaseSensitive || pq.Fuzzy {
		return false
	}
	// Only satisfied volume terms may be removed by the caller. A remaining
	// volume term is a real constraint, which a name identity cannot decide.
	for _, term := range pq.Terms {
		if isVolumeQueryTerm(term) {
			return false
		}
	}
	if pq.Under != "" || pq.Exists {
		return false
	}
	if len(pq.Regexps) != 0 || len(pq.RegexTerms) != 0 {
		return false
	}
	if len(pq.Parents) != 0 || len(pq.OrGroups) != 0 {
		return false
	}
	if pq.RootBias != "" || pq.CWDBias != "" {
		return false
	}
	if len(pq.Globs) > 0 {
		// Glob matching re-parses the pattern per call and cannot ride
		// the mask prefilter; the dedicated glob lanes (glob-literal
		// postings, ext reduction) already serve these well.
		return false
	}
	pos := len(nonVolumeTerms(pq.Terms)) + len(pq.Dirs) + len(pq.Globs)
	if len(pq.Exts) > 0 {
		pos++
	}
	if pos == 0 || pos > memoMaxPosSlots || len(pq.NotGroups) > memoMaxNegSlots {
		return false
	}
	for _, t := range nonVolumeTerms(pq.Terms) {
		if isVolumeQueryTerm(t) || strings.ContainsAny(t, `\/*?[]:`) {
			return false
		}
	}
	for _, d := range pq.Dirs {
		if strings.ContainsAny(d, `\/*?[]:`) {
			return false
		}
	}
	for _, n := range pq.NotGroups {
		if n.MatchPath && !pq.MatchPath {
			return false
		}
		t := memoNegKey(n)
		if t == "" || isVolumeQueryTerm(t) || strings.ContainsAny(t, `\/*?[]:`) {
			return false
		}
	}
	if !pq.MatchPath && len(pq.Dirs) > 0 {
		// dir: filters read the full path even in filename mode.
		return false
	}
	return true
}

// memoFor returns the cached memo for pq on this volume with the identity
// it was built from, building on a miss. It returns nils when the lane
// cannot serve the query. The caller must already have dropped satisfied
// volume terms.
func (vol *serviceVolumeIndex) memoFor(pq parsedQuery) (*queryMemo, *nameIdentity) {
	if vol == nil || vol.index == nil || !memoSupported(pq) {
		return nil, nil
	}
	idx := vol.index
	gen := idx.memoGen.Load()
	ident := idx.ensureNameIdentity()
	if ident == nil {
		return nil, nil
	}
	sig := memoSigFor(pq)
	n := idx.compactRecordCount()
	c := &idx.nameMemo
	c.mu.Lock()
	if c.memo != nil && c.sig == sig && c.records == n && c.gen == gen && c.ident != nil {
		m, ident := c.memo, c.ident
		c.mu.Unlock()
		return m, ident
	}
	c.mu.Unlock()
	m := buildQueryMemo(vol, idx, ident, pq, sig, n)
	if m == nil {
		return nil, nil
	}
	c.mu.Lock()
	// A reset raced the build: drop the memo rather than serve bits from
	// a dead identity.
	if idx.memoGen.Load() != gen {
		c.mu.Unlock()
		return nil, nil
	}
	c.sig, c.records, c.gen, c.memo, c.ident = sig, n, gen, m, ident
	c.mu.Unlock()
	return m, ident
}

// buildQueryMemo scores every distinct name once (in parallel) and, in path
// mode, folds the bits down the directory tree.
func buildQueryMemo(vol *serviceVolumeIndex, idx *Index, ident *nameIdentity, pq parsedQuery, sig string, n int) *queryMemo {
	m := &queryMemo{sig: sig, records: n, matchPath: pq.MatchPath}
	terms := append(append([]string(nil), nonVolumeTerms(pq.Terms)...), pq.Dirs...)
	m.terms = terms
	m.termMask = make([]uint64, len(terms))
	m.prefixOK = make([]bool, len(terms))
	for i, t := range terms {
		m.termMask[i] = memoMaskOf(t)
		// A term inside the volume/root prefix is a substring of every
		// reconstructed path: the slot is auto-satisfied (the granted twin
		// of plainTermMayOccurInPathPrefix's decline).
		if pq.MatchPath && ident.volLower != "" && strings.Contains(ident.volLower, t) {
			m.prefixOK[i] = true
			continue
		}
		if pq.MatchPath && ident.rootLower != "" && strings.Contains(ident.rootLower, t) {
			m.prefixOK[i] = true
		}
	}
	if len(pq.Exts) > 0 {
		m.hasExt = true
		m.ext = pq.Exts[0]
		for _, e := range pq.Exts[1:] {
			if e != m.ext {
				// One name carries one extension: differing required
				// extensions match nothing, exactly.
				m.impossible = true
				break
			}
		}
		m.extBit = 1 << uint(len(terms))
	}
	m.globs = append([]string(nil), pq.Globs...)
	m.globBit = make([]uint16, len(m.globs))
	for i := range m.globs {
		m.globBit[i] = 1 << uint(len(terms)+1+i)
	}
	for i := range terms {
		if !m.prefixOK[i] {
			m.reqMask |= 1 << uint(i)
		}
	}
	for _, ng := range pq.NotGroups {
		t := memoNegKey(ng)
		m.negs = append(m.negs, t)
		m.negMask = append(m.negMask, memoMaskOf(t))
		if pq.MatchPath && ((ident.volLower != "" && strings.Contains(ident.volLower, t)) ||
			(ident.rootLower != "" && strings.Contains(ident.rootLower, t))) {
			// A negated term in every path excludes every entry.
			m.negPrefixHit = true
		}
	}
	if m.impossible || m.negPrefixHit {
		m.nameBits = make([]uint16, ident.count)
		m.nameNeg = make([]uint16, ident.count)
		return m
	}
	m.nameBits = make([]uint16, ident.count)
	m.nameNeg = make([]uint16, ident.count)
	if !scoreDistinctNames(idx, ident, m, pq) {
		return nil
	}
	if pq.MatchPath && (m.reqMask != 0 || len(m.negs) > 0) {
		// Only fold when some slot can match through ancestors.
		// Pure own-name queries (extension/glob only) skip the fold:
		// every record decides from its own bits alone.
		topo := idx.ensureDirTopo(vol)
		if topo == nil {
			// No child source to fold over: decline rather than scan
			// every record through the legacy path under a memo label.
			return nil
		}
		if !foldDirTopo(idx, ident, topo, m, n, pq) {
			return nil
		}
		m.folded = true
	}
	return m
}

// scoreDistinctNames evaluates every distinct name once, in parallel across
// chunks. Mask misses skip the substring work; only mask-passing names pay
// for exemplar verification.
func scoreDistinctNames(idx *Index, ident *nameIdentity, m *queryMemo, pq parsedQuery) bool {
	count := ident.count
	if count == 0 {
		return !queryCanceled(pq)
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, count/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		return scoreNameRange(idx, ident, m, 0, count, pq)
	}
	var wg sync.WaitGroup
	var canceled atomic.Bool
	for w := 0; w < workers; w++ {
		start := w * count / workers
		end := (w + 1) * count / workers
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			if !scoreNameRange(idx, ident, m, start, end, pq) {
				canceled.Store(true)
			}
		}(start, end)
	}
	wg.Wait()
	return !canceled.Load() && !queryCanceled(pq)
}

func scoreNameRange(idx *Index, ident *nameIdentity, m *queryMemo, start, end int, pq parsedQuery) bool {
	for id := start; id < end; id++ {
		if (id-start)&1023 == 0 && queryCanceled(pq) {
			return false
		}
		s := ident.identName(idx, uint32(id))
		if s == "" {
			continue
		}
		mask := ident.masks[id]
		var bits uint16
		for t, term := range m.terms {
			if m.matchPath && s == "." {
				continue
			}
			if m.prefixOK[t] {
				continue
			}
			if m.termMask[t]&^mask != 0 {
				continue
			}
			if strings.Contains(s, term) {
				bits |= 1 << uint(t)
			}
		}
		if m.hasExt && memoNameExt(s) == m.ext {
			bits |= m.extBit
		}
		for g, glob := range m.globs {
			if ok, err := filepath.Match(glob, s); err == nil && ok {
				bits |= m.globBit[g]
			}
		}
		m.nameBits[id] = bits
		var neg uint16
		for j, nt := range m.negs {
			if m.matchPath && s == "." {
				continue
			}
			if m.negMask[j]&^mask != 0 {
				continue
			}
			if strings.Contains(s, nt) {
				neg |= 1 << uint(j)
			}
		}
		m.nameNeg[id] = neg
	}
	return !queryCanceled(pq)
}

// memoNameExt mirrors entryMatches' extension derivation on an already
// folded name: filepath.Ext is case-transparent, so folding first is exact.
func memoNameExt(lower string) string {
	return strings.TrimPrefix(filepath.Ext(lower), ".")
}

// dirTopo is the parents-first order of child-bearing records, built once
// per identity. The per-query fold is a single linear pass over it: no
// queue, no per-query traversal, no visited bookkeeping for the pass
// itself. Records absent from it (childless files and dirs, unreachable
// cycles) consult their parent's folded row in the scan instead.
type dirTopo struct {
	order []uint32
	n     int
}

// ensureDirTopo returns the cached topo, building it once under the
// identity lock. It needs a volume for map-backed child tables. Without
// any child source it returns nil, and path-mode memos needing a fold
// decline.
func (idx *Index) ensureDirTopo(vol *serviceVolumeIndex) *dirTopo {
	if idx == nil || vol == nil {
		return nil
	}
	if t := idx.memoTopo.Load(); t != nil {
		if t.n == idx.compactRecordCount() {
			return t
		}
	}
	d := &idx.Derived
	hasSource := (len(d.ChildOffsets) > 0 && len(d.ChildIDs) > 0) ||
		(len(vol.childOffsets) > 0 && len(vol.childIDs) > 0) ||
		vol.children != nil
	if !hasSource {
		return nil
	}
	idx.nameIdentMu.Lock()
	defer idx.nameIdentMu.Unlock()
	if t := idx.memoTopo.Load(); t != nil && t.n == idx.compactRecordCount() {
		return t
	}
	ident := idx.nameIdent.Load()
	if ident == nil {
		return nil
	}
	n := idx.compactRecordCount()
	seen := make([]uint64, (n+63)/64)
	var order []uint32
	stack := append([]uint32(nil), ident.roots...)
	mark := func(id uint32) bool {
		if int(id) >= n {
			return false
		}
		wi, bit := int(id)/64, uint(id)%64
		if seen[wi]&(1<<bit) != 0 {
			return false
		}
		seen[wi] |= 1 << bit
		return true
	}
	for _, r := range stack {
		mark(r)
	}
	for len(stack) > 0 {
		last := len(stack) - 1
		d := stack[last]
		stack = stack[:last]
		kids := memoFoldChildren(vol, idx, int(d), n)
		if len(kids) == 0 {
			continue
		}
		order = append(order, d)
		for _, c := range kids {
			if int(c) >= n {
				continue
			}
			if mark(c) {
				stack = append(stack, c)
			}
		}
	}
	t := &dirTopo{order: order, n: n}
	idx.memoTopo.Store(t)
	return t
}

// memoParentOf reads just a record's parent id.
func memoParentOf(idx *Index, id int) int32 {
	n := idx.compactRecordCount()
	if id < 0 || id >= n {
		return -2
	}
	if p := idx.PackedRecords; p != nil && id < len(p.Parents) {
		return p.Parents[id]
	}
	if m := idx.MMapRecords; m != nil {
		if p, ok := m.parentAt(id); ok {
			return p
		}
		return -2
	}
	if id < len(idx.Records) {
		return idx.Records[id].Parent
	}
	return -2
}

// foldDirTopo runs the per-query fold as one linear pass over the topo
// order: each node merges its own bits with its parent's already-final row.
// Nodes whose parent is unvisited or invalid stay unvisited and slow-path
// (with their descendants, which consult the unvisited mark), keeping the
// lane exact under every graph shape.
func foldDirTopo(idx *Index, ident *nameIdentity, topo *dirTopo, m *queryMemo, n int, pq parsedQuery) bool {
	m.dirBits = make([]uint16, n)
	m.dirNeg = make([]uint16, n)
	m.visited = make([]uint64, (n+63)/64)
	for pos, d := range topo.order {
		if pos&1023 == 0 && queryCanceled(pq) {
			return false
		}
		if int(d) >= n {
			continue
		}
		p := memoParentOf(idx, int(d))
		var prow, pneg uint16
		if p < 0 || int(p) >= n || p == int32(d) {
			// No ancestors above this node.
		} else if m.visited[int(p)/64]&(1<<uint(int(p)%64)) == 0 {
			continue
		} else {
			prow, pneg = m.dirBits[p], m.dirNeg[p]
		}
		own, ownNeg := uint16(0), uint16(0)
		if int(d) < len(ident.recName) {
			if nid := ident.recName[d]; nid != memoUnknownName && int(nid) < len(m.nameBits) {
				own = m.nameBits[nid]
				if int(nid) < len(m.nameNeg) {
					ownNeg = m.nameNeg[nid]
				}
			} else {
				continue
			}
		} else {
			continue
		}
		m.dirBits[d] = own | prow
		m.dirNeg[d] = ownNeg | pneg
		m.visited[int(d)/64] |= 1 << uint(int(d)%64)
	}
	return !queryCanceled(pq)
}

// memoFoldChildren reads a record's children without allocating on the
// CSR paths (derived tables, then volume-built ones). Map-backed volumes
// fall back to childIDsForRecord.
func memoFoldChildren(vol *serviceVolumeIndex, idx *Index, id, n int) []uint32 {
	if idx == nil || id < 0 || id >= n {
		return nil
	}
	d := &idx.Derived
	if id+1 < len(d.ChildOffsets) {
		start, end := d.ChildOffsets[id], d.ChildOffsets[id+1]
		if start <= end && int(end) <= len(d.ChildIDs) {
			return d.ChildIDs[start:end]
		}
	}
	if vol == nil {
		return nil
	}
	return vol.childIDsForRecord(id)
}

// memoScalarFields verifies the record-level (path-free) part of pq from
// light fields. Name, ancestor and extension/glob bits come from the memo;
// everything here is numeric or mode-derived. It mirrors
// compactRecordPrecheck minus the name substring branch, which the bits
// already decided.
func memoScalarFields(f memoRecFields, pq parsedQuery) bool {
	if !f.ok || f.deleted {
		return false
	}
	if pq.Type == "file" && f.mode&uint32(os.ModeDir) != 0 {
		return false
	}
	if pq.Type == "dir" && f.mode&uint32(os.ModeDir) == 0 {
		return false
	}
	if !attrFiltersMatch(f.mode, pq.AttrFilters) {
		return false
	}
	if pq.HasModAfter {
		if f.mod == 0 || !time.Unix(0, f.mod).After(pq.ModifiedAfter) {
			return false
		}
	}
	for _, sf := range pq.SizeFilters {
		if !sf.matches(f.size) {
			return false
		}
	}
	for _, df := range pq.DateFilters {
		if !df.matches(f.mod) {
			return false
		}
	}
	return true
}

// memoMatchesFields decides one record from light fields: exact for the
// supported family, undecided (slow path) for anything the memo cannot see.
// No record is decoded and no string is touched on this path.
func (m *queryMemo) memoMatchesFields(idx *Index, ident *nameIdentity, id int, f memoRecFields, pq parsedQuery) (bool, bool) {
	if m.impossible || m.negPrefixHit {
		return false, true
	}
	if !f.ok {
		return false, false
	}
	// Directory size filters use the same aggregate, including live deltas,
	// as materialized entries rather than the directory's raw record size.
	if f.mode&uint32(os.ModeDir) != 0 && id >= 0 && id < len(idx.Derived.SubtreeBytes) {
		f.size = int64(idx.Derived.SubtreeBytes[id])
		if delta := idx.dirSizeDelta.Load(); delta != nil {
			f.size += (*delta)[id]
		}
		f.size = max(0, f.size)
	}
	if !memoScalarFields(f, pq) {
		return false, true
	}
	if f.nameID == memoUnknownName || int(f.nameID) >= len(m.nameBits) {
		return false, false
	}
	own := m.nameBits[f.nameID]
	var ownNeg uint16
	if int(f.nameID) < len(m.nameNeg) {
		ownNeg = m.nameNeg[f.nameID]
	}
	if !m.folded {
		// No ancestor-dependent slot exists: own bits decide alone.
		if ownNeg != 0 {
			return false, true
		}
		if own&m.reqMask != m.reqMask {
			return false, true
		}
		if m.hasExt && own&m.extBit == 0 {
			return false, true
		}
		for _, gb := range m.globBit {
			if own&gb == 0 {
				return false, true
			}
		}
		return true, true
	}
	// Folded path mode. Topo-processed records read their complete row;
	// everything else consults its parent's row, slow-pathing when the
	// parent is unfoldable (unvisited, invalid graph shape) or missing.
	var terms, neg uint16
	if id >= 0 && id < m.records && len(m.visited) > 0 && m.visited[id/64]&(1<<uint(id%64)) != 0 && id < len(m.dirBits) {
		terms = own | m.dirBits[id]
		neg = ownNeg | m.dirNeg[id]
	} else {
		p := memoParentOf(idx, id)
		if p < 0 || int(p) >= m.records || p == int32(id) {
			// No ancestors exist above this record.
			terms, neg = own, ownNeg
		} else if len(m.visited) > 0 && int(p) < len(m.dirBits) && m.visited[int(p)/64]&(1<<uint(int(p)%64)) != 0 {
			terms = own | m.dirBits[p]
			neg = ownNeg | m.dirNeg[p]
		} else {
			return false, false
		}
	}
	if neg != 0 {
		return false, true
	}
	if terms&m.reqMask != m.reqMask {
		return false, true
	}
	if m.hasExt && own&m.extBit == 0 {
		// Extension is own-name-only: ancestors cannot satisfy it.
		return false, true
	}
	for _, gb := range m.globBit {
		if own&gb == 0 {
			return false, true
		}
	}
	return true, true
}

// startBackgroundNameIdentityBuilds warms the distinct-name identity for
// resident volumes next to the trigram/order builds. Queries also build it
// lazily on first use; the background pass just moves the one-time cost off
// the query path.
func (s *goSearchService) startBackgroundNameIdentityBuilds(volumes []*serviceVolumeIndex) {
	if len(volumes) == 0 || serviceLowMemoryMode() {
		return
	}
	go func() {
		for _, vol := range volumes {
			if vol == nil {
				continue
			}
			// Retain the mapped base until the build finishes. Persist/rebuild
			// takes indexMu exclusively before unmapping or replacing it.
			s.indexMu.RLock()
			vol.mu.Lock()
			if vol.index != nil {
				vol.index.ensureNameIdentity()
			}
			vol.mu.Unlock()
			s.indexMu.RUnlock()
		}
	}()
}

// memoRecFields are the scalar decision fields for one record, read
// without assembling a CompactRecord or resolving any name.
type memoRecFields struct {
	deleted bool
	mode    uint32
	size    int64
	mod     int64
	parent  int32
	nameID  uint32
	ok      bool
}

// memoFields reads the scalar fields for the memo scan. Packed volumes
// serve every field from columnar arrays; mmap volumes from fixed record
// offsets plus the identity's name array; plain Records fall back to a
// full decode.
func memoFields(idx *Index, ident *nameIdentity, id int) memoRecFields {
	n := idx.compactRecordCount()
	if id < 0 || id >= n {
		return memoRecFields{}
	}
	if p := idx.PackedRecords; p != nil {
		var nameID uint32 = memoUnknownName
		if ident != nil && id < len(ident.recName) {
			nameID = ident.recName[id]
		}
		return memoRecFields{
			deleted: p.deletedAt(id),
			mode:    p.modeAt(id),
			size:    p.sizeAt(id),
			mod:     p.modUnixAt(id),
			parent:  p.Parents[id],
			nameID:  nameID,
			ok:      true,
		}
	}
	if m := idx.MMapRecords; m != nil {
		base, ok := m.recordOffset(id)
		if !ok {
			return memoRecFields{}
		}
		parent, token := m.recordRefs(base + 16)
		var p int32
		if (!m.wideRefs && parent == compactNarrowParentSentinel) || (m.wideRefs && parent == compactWideParentSentinel) {
			p = -1
		} else {
			p = int32(parent)
		}
		// The identity's name array is authoritative when present (it
		// was read from these same refs at build); the live token is
		// the fallback.
		nameID := token
		if ident != nil && id < len(ident.recName) {
			if nid := ident.recName[id]; nid != memoUnknownName {
				nameID = nid
			}
		}
		mode, _ := m.modeAt(id)
		size, _ := m.sizeAt(id)
		mod, _ := m.modUnixAt(id)
		return memoRecFields{
			deleted: m.deletedAt(id),
			mode:    mode,
			size:    size,
			mod:     mod,
			parent:  p,
			nameID:  nameID,
			ok:      true,
		}
	}
	rec := idx.compactRecord(id)
	var nameID uint32 = memoUnknownName
	if ident != nil && id < len(ident.recName) {
		nameID = ident.recName[id]
	}
	return memoRecFields{
		deleted: rec.Deleted,
		mode:    rec.Mode,
		size:    rec.Size,
		mod:     rec.ModUnix,
		parent:  rec.Parent,
		nameID:  nameID,
		ok:      true,
	}
}

// ---------------------------------------------------------------------------
// Scan and count entry points
// ---------------------------------------------------------------------------

// memoScanTop returns the first limit memo-matching ids in scan order,
// mirroring boundedScanHiddenTop's contract (hidden and the membership
// filter apply identically). Unvisited records take the legacy verify path
// inline so the result is exact under every graph shape.
func (vol *serviceVolumeIndex) memoScanTop(pq parsedQuery, hidden hiddenBaseIDs, limit int, filter *boundedScanMembershipFilter, order []uint32, orderLen int) ([]int, bool) {
	if limit <= 0 {
		return nil, false
	}
	m, ident := vol.memoFor(pq)
	if m == nil || ident == nil {
		return nil, false
	}
	pq.Trace.addTerm(traceTerm{Term: memoSigFor(pq), Kind: "memo", Source: "memo-scan", Exact: true})
	if m.impossible || m.negPrefixHit {
		return []int{}, true
	}
	if orderLen < 2*serviceTrigramParallelVerifyMinIDs {
		out, _, ok := vol.memoScanTopRange(pq, m, ident, hidden, filter, order, 0, orderLen, nil, nil, 0, limit)
		return out, ok
	}
	const serialPrefixPositions = 4 * serviceTrigramParallelVerifyMinIDs
	prefixEnd := min(orderLen, serialPrefixPositions)
	out, _, ok := vol.memoScanTopRange(pq, m, ident, hidden, filter, order, 0, prefixEnd, nil, nil, 0, limit)
	if !ok || len(out) >= limit || prefixEnd >= orderLen {
		return out, ok
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, (orderLen-prefixEnd)/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		rest, _, ok := vol.memoScanTopRange(pq, m, ident, hidden, filter, order, prefixEnd, orderLen, nil, nil, 0, limit-len(out))
		if !ok {
			return nil, false
		}
		return append(out, rest...), true
	}
	var stopped atomic.Bool
	counts := make([]atomic.Int64, workers)
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := prefixEnd + w*(orderLen-prefixEnd)/workers
		end := prefixEnd + (w+1)*(orderLen-prefixEnd)/workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			parts[w], _, _ = vol.memoScanTopRange(pq, m, ident, hidden, filter, order, start, end, &stopped, counts, w, limit)
		}(w, start, end)
	}
	wg.Wait()
	if stopped.Load() {
		return nil, false
	}
	for _, part := range parts {
		for _, id := range part {
			out = append(out, id)
			if len(out) >= limit {
				return out, true
			}
		}
	}
	return out, true
}

// memoScanTopRange scans one order slice for the memo lane, with cooperative
// early termination shared across workers.
func (vol *serviceVolumeIndex) memoScanTopRange(pq parsedQuery, m *queryMemo, ident *nameIdentity, hidden hiddenBaseIDs, filter *boundedScanMembershipFilter, order []uint32, start, end int, stopped *atomic.Bool, counts []atomic.Int64, workerIndex, limit int) ([]int, int, bool) {
	out := make([]int, 0, min(limit, 1024))
	cache := make(map[int]string)
	scanned := 0
	for pos := start; pos < end; pos++ {
		if pos&1023 == 0 && (stopped != nil && stopped.Load() || queryCanceled(pq)) {
			if stopped != nil {
				stopped.Store(true)
			}
			return nil, scanned, false
		}
		if counts != nil && pos&1023 == 0 && pos > start {
			var earlier int64
			for k := 0; k < workerIndex && earlier < int64(limit); k++ {
				earlier += counts[k].Load()
			}
			if earlier >= int64(limit) {
				return out, scanned, true
			}
		}
		id := compactUint32OrderAt(order, pos)
		scanned++
		if filter != nil && !filter.contains(id) {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if vol.memoRecordMatchesFields(pq, m, ident, id, cache) {
			out = append(out, id)
			if counts != nil {
				counts[workerIndex].Add(1)
			}
			if len(out) >= limit {
				break
			}
		}
	}
	return out, scanned, true
}

// memoScanAll collects every memo-matching id in scan order (the unordered
// full-scan contract). Used where callers sort or count afterwards.
func (vol *serviceVolumeIndex) memoScanAll(pq parsedQuery, hidden hiddenBaseIDs, filter *boundedScanMembershipFilter) ([]int, bool) {
	m, ident := vol.memoFor(pq)
	if m == nil || ident == nil {
		return nil, false
	}
	pq.Trace.addTerm(traceTerm{Term: memoSigFor(pq), Kind: "memo", Source: "memo-scan", Exact: true})
	if m.impossible || m.negPrefixHit {
		return []int{}, true
	}
	n := vol.index.compactRecordCount()
	order := vol.orderForQuery(pq)
	orderLen := compactUint32OrderLen(order, n)
	if orderLen < 2*serviceTrigramParallelVerifyMinIDs {
		out, _, ok := vol.memoScanAllRange(pq, m, ident, hidden, filter, order, 0, orderLen, nil)
		return out, ok
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, orderLen/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		out, _, ok := vol.memoScanAllRange(pq, m, ident, hidden, filter, order, 0, orderLen, nil)
		return out, ok
	}
	var stopped atomic.Bool
	parts := make([][]int, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * orderLen / workers
		end := (w + 1) * orderLen / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			parts[w], _, _ = vol.memoScanAllRange(pq, m, ident, hidden, filter, order, start, end, &stopped)
		}(w, start, end)
	}
	wg.Wait()
	if stopped.Load() {
		return nil, false
	}
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]int, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out, true
}

func (vol *serviceVolumeIndex) memoScanAllRange(pq parsedQuery, m *queryMemo, ident *nameIdentity, hidden hiddenBaseIDs, filter *boundedScanMembershipFilter, order []uint32, start, end int, stopped *atomic.Bool) ([]int, int, bool) {
	out := make([]int, 0, 256)
	cache := make(map[int]string)
	for pos := start; pos < end; pos++ {
		if pos&1023 == 0 && (stopped != nil && stopped.Load() || queryCanceled(pq)) {
			if stopped != nil {
				stopped.Store(true)
			}
			return nil, 0, false
		}
		id := compactUint32OrderAt(order, pos)
		if filter != nil && !filter.contains(id) {
			continue
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if vol.memoRecordMatchesFields(pq, m, ident, id, cache) {
			out = append(out, id)
		}
	}
	return out, len(out), true
}

// memoCount counts memo-matching records without materializing anything:
// bit checks plus scalar record reads. Hidden ids are skipped exactly like
// the scan variants.
func (vol *serviceVolumeIndex) memoCount(pq parsedQuery, hidden hiddenBaseIDs) (int, bool) {
	m, ident := vol.memoFor(pq)
	if m == nil || ident == nil {
		return 0, false
	}
	pq.Trace.addTerm(traceTerm{Term: memoSigFor(pq), Kind: "memo", Source: "memo-count", Exact: true})
	if m.impossible || m.negPrefixHit {
		return 0, true
	}
	n := vol.index.compactRecordCount()
	if n < 2*serviceTrigramParallelVerifyMinIDs {
		return vol.memoCountRange(pq, m, ident, hidden, 0, n, nil)
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, n/serviceTrigramParallelVerifyMinIDs))
	if workers <= 1 {
		return vol.memoCountRange(pq, m, ident, hidden, 0, n, nil)
	}
	var stopped atomic.Bool
	counts := make([]int, workers)
	oks := make([]bool, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * n / workers
		end := (w + 1) * n / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			c, ok := vol.memoCountRange(pq, m, ident, hidden, start, end, &stopped)
			counts[w], oks[w] = c, ok
		}(w, start, end)
	}
	wg.Wait()
	if stopped.Load() {
		return 0, false
	}
	total := 0
	for w := range counts {
		if !oks[w] {
			return 0, false
		}
		total += counts[w]
	}
	return total, true
}

func (vol *serviceVolumeIndex) memoCountRange(pq parsedQuery, m *queryMemo, ident *nameIdentity, hidden hiddenBaseIDs, start, end int, stopped *atomic.Bool) (int, bool) {
	count := 0
	cache := make(map[int]string)
	for id := start; id < end; id++ {
		if id&1023 == 0 && (stopped != nil && stopped.Load() || queryCanceled(pq)) {
			if stopped != nil {
				stopped.Store(true)
			}
			return 0, false
		}
		if !hidden.empty() && hidden.contains(id) {
			continue
		}
		if vol.memoRecordMatchesFields(pq, m, ident, id, cache) {
			count++
		}
	}
	return count, true
}

// memoRecordMatchesFields decides one record through the memo using light
// field reads (no record decode), slow-pathing anything the memo cannot see
// (unvisited graph regions, unknown names) via the exact legacy verifier.
// The slow path builds a throwaway Entry; hits are re-verified by the
// caller's own materialization, so the boolean alone is what matters here.
func (vol *serviceVolumeIndex) memoRecordMatchesFields(pq parsedQuery, m *queryMemo, ident *nameIdentity, id int, cache map[int]string) bool {
	if id < 0 || id >= vol.index.compactRecordCount() {
		return false
	}
	f := memoFields(vol.index, ident, id)
	if ok, decided := m.memoMatchesFields(vol.index, ident, id, f, pq); decided {
		return ok
	}
	if _, match := compactCandidateEntryIfMatch(vol.index, pq, id, cache, true, false); match {
		return true
	}
	return false
}
