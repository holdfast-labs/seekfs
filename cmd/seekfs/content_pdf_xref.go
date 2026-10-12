package main

// PDF document model: cross-reference resolution, object parsing, object streams
// and the page tree. It never trusts a declared offset, length, count or /Size —
// each is clamped to the bytes actually present, and every loop that walks a
// /Prev chain, a /Kids tree or an object stream carries a hard iteration bound.
//
// Both xref forms are supported: the classic `xref` table (with a `trailer`
// dict) and the cross-reference stream (/Type /XRef, /W, /Index). Objects stored
// in /ObjStm streams are resolved through /N and /First, with /Extends followed
// one level at a time. A /Prev cycle is treated as a malformed document.

import (
	"bytes"
	"context"
	"sort"
	"strconv"
)

const (
	contentPDFMaxXRefSections = 64
	contentPDFMaxXRefEntries  = 1 << 20
	contentPDFMaxObjects      = 1 << 18
	contentPDFMaxObjStm       = 1 << 16
	contentPDFMaxPages        = 1 << 15
	contentPDFMaxPageDepth    = 64
	contentPDFMaxFonts        = 256
)

type contentPDFCompRef struct {
	stream int
	index  int
}

type contentPDFDoc struct {
	ctx           context.Context
	buf           []byte
	maxRaw        int64
	maxText       int64
	offsets       map[int]int64
	compressed    map[int]contentPDFCompRef
	trailer       map[string]contentPDFValue
	cache         map[int]contentPDFValue
	objStmCache   map[int]map[int]contentPDFValue
	objCount      int
	encrypted     bool
	endstreams    []int
	endstreamsSet bool
}

type contentPDFObject struct {
	num       int
	gen       int
	value     contentPDFValue
	raw       []byte
	hasStream bool
}

func contentPDFFindStartXRef(buf []byte) (int64, bool) {
	idx := bytes.LastIndex(buf, []byte("startxref"))
	if idx < 0 {
		return 0, false
	}
	l := &contentPDFLexer{buf: buf, pos: idx + len("startxref")}
	l.skipSpace()
	n, err := strconv.ParseInt(l.readWord(), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// contentPDFLoadDoc builds the object map by following the xref chain. ok=false
// means the file is malformed enough that no text can be trusted out of it.
func contentPDFLoadDoc(ctx context.Context, buf []byte, maxRaw, maxText int64) (*contentPDFDoc, bool) {
	d := &contentPDFDoc{
		ctx:         ctx,
		buf:         buf,
		maxRaw:      maxRaw,
		maxText:     maxText,
		offsets:     map[int]int64{},
		compressed:  map[int]contentPDFCompRef{},
		trailer:     map[string]contentPDFValue{},
		cache:       map[int]contentPDFValue{},
		objStmCache: map[int]map[int]contentPDFValue{},
	}
	off, ok := contentPDFFindStartXRef(buf)
	if !ok {
		return nil, false
	}
	visited := map[int64]bool{}
	for sections := 0; sections < contentPDFMaxXRefSections; sections++ {
		if off < 0 || off >= int64(len(buf)) {
			return nil, false
		}
		if visited[off] {
			// A /Prev cycle means the chain is corrupt; treat the document as
			// malformed rather than guessing which section wins.
			return nil, false
		}
		visited[off] = true
		sec, ok := d.parseXRefSection(off)
		if !ok {
			if sections == 0 {
				return nil, false
			}
			break
		}
		for num, e := range sec.entries {
			switch e.kind {
			case contentPDFXRefOffset:
				if _, exists := d.offsets[num]; !exists {
					d.offsets[num] = e.offset
				}
			case contentPDFXRefCompressed:
				if _, exists := d.compressed[num]; !exists {
					d.compressed[num] = contentPDFCompRef{stream: e.stream, index: e.index}
				}
			}
		}
		for k, v := range sec.trailer {
			if _, exists := d.trailer[k]; !exists {
				d.trailer[k] = v
			}
		}
		if !sec.hasPrev {
			break
		}
		off = sec.prev
	}
	if len(d.offsets) == 0 && len(d.compressed) == 0 {
		return nil, false
	}
	if _, ok := d.trailer["Encrypt"]; ok {
		d.encrypted = true
	}
	return d, true
}

type contentPDFXRefKind uint8

const (
	contentPDFXRefOffset contentPDFXRefKind = iota
	contentPDFXRefCompressed
)

type contentPDFXRefEntry struct {
	kind   contentPDFXRefKind
	offset int64
	stream int
	index  int
}

type contentPDFXRefSection struct {
	entries map[int]contentPDFXRefEntry
	trailer map[string]contentPDFValue
	prev    int64
	hasPrev bool
}

func (d *contentPDFDoc) parseXRefSection(off int64) (*contentPDFXRefSection, bool) {
	if off < 0 || off >= int64(len(d.buf)) {
		return nil, false
	}
	l := &contentPDFLexer{buf: d.buf, pos: int(off), budget: contentPDFMaxValues}
	l.skipSpace()
	if l.peekWord() == "xref" {
		return d.parseClassicXRef(l)
	}
	obj, ok := d.parseIndirectAt(int(off), 0)
	if !ok || (obj.value.kind != contentPDFDict && obj.value.kind != contentPDFStream) {
		return nil, false
	}
	return d.parseXRefStream(obj)
}

func (d *contentPDFDoc) parseClassicXRef(l *contentPDFLexer) (*contentPDFXRefSection, bool) {
	l.readWord() // xref
	sec := &contentPDFXRefSection{
		entries: map[int]contentPDFXRefEntry{},
		trailer: map[string]contentPDFValue{},
	}
	total := 0
	for {
		l.skipSpace()
		if l.pos >= len(l.buf) {
			return nil, false
		}
		if l.peekWord() == "trailer" {
			break
		}
		start, err := strconv.Atoi(l.readWord())
		if err != nil || start < 0 {
			return nil, false
		}
		l.skipSpace()
		count, err := strconv.Atoi(l.readWord())
		if err != nil || count < 0 {
			return nil, false
		}
		if count > contentPDFMaxXRefEntries {
			count = contentPDFMaxXRefEntries
		}
		for i := 0; i < count; i++ {
			l.skipSpace()
			offTok := l.readWord()
			if offTok == "" {
				return nil, false
			}
			l.skipSpace()
			_ = l.readWord() // generation
			l.skipSpace()
			typ := l.readWord()
			o, err := strconv.ParseInt(offTok, 10, 64)
			if err != nil || o < 0 {
				return nil, false
			}
			if typ == "n" && start+i <= contentPDFMaxXRefEntries {
				sec.entries[start+i] = contentPDFXRefEntry{kind: contentPDFXRefOffset, offset: o}
			}
			total++
			if total > contentPDFMaxXRefEntries {
				return nil, false
			}
		}
	}
	l.readWord() // trailer
	v, ok := contentPDFParseValue(l, 0)
	if !ok || v.kind != contentPDFDict {
		return nil, false
	}
	sec.trailer = v.dict
	d.xrefPrev(sec, v.dict)
	return sec, true
}

func (d *contentPDFDoc) xrefPrev(sec *contentPDFXRefSection, dict map[string]contentPDFValue) {
	if p, ok := dict["Prev"]; ok {
		if n, ok := contentPDFIntOf(p); ok && n >= 0 {
			sec.prev = n
			sec.hasPrev = true
		}
	}
}

func (d *contentPDFDoc) parseXRefStream(obj *contentPDFObject) (*contentPDFXRefSection, bool) {
	dict := obj.value.dict
	wv := d.resolve(dict["W"])
	if wv.kind != contentPDFArray || len(wv.arr) < 3 {
		return nil, false
	}
	w := make([]int, len(wv.arr))
	entryLen := 0
	for i, e := range wv.arr {
		n, ok := contentPDFIntOf(e)
		if !ok || n < 0 || n > 8 {
			return nil, false
		}
		w[i] = int(n)
		entryLen += int(n)
	}
	if entryLen <= 0 || entryLen > 64 {
		return nil, false
	}
	size := int64(0)
	if n, ok := contentPDFIntOf(d.resolve(dict["Size"])); ok && n > 0 {
		size = n
	}
	if size > contentPDFMaxXRefEntries {
		size = contentPDFMaxXRefEntries
	}
	var index []int64
	if iv := d.resolve(dict["Index"]); iv.kind == contentPDFArray {
		for _, e := range iv.arr {
			if n, ok := contentPDFIntOf(e); ok && n >= 0 {
				index = append(index, n)
			}
		}
	}
	if len(index) == 0 {
		index = []int64{0, size}
	}
	data, _, err := contentPDFDecodeStream(d.ctx, d, dict, obj.raw, d.maxRaw)
	if err != nil {
		return nil, false
	}
	sec := &contentPDFXRefSection{
		entries: map[int]contentPDFXRefEntry{},
		trailer: dict,
	}
	pos := 0
	vals := make([]int, len(w))
	for k := 0; k+1 < len(index); k += 2 {
		start := index[k]
		count := index[k+1]
		if start < 0 || count <= 0 {
			continue
		}
		if count > contentPDFMaxXRefEntries {
			count = contentPDFMaxXRefEntries
		}
		for i := int64(0); i < count; i++ {
			if pos+entryLen > len(data) {
				break
			}
			p := pos
			for fi, fw := range w {
				vals[fi] = contentPDFReadBE(data[p:], fw)
				p += fw
			}
			pos += entryLen
			typ := 1
			if w[0] != 0 {
				typ = vals[0]
			}
			num := int(start + i)
			switch typ {
			case 1:
				sec.entries[num] = contentPDFXRefEntry{kind: contentPDFXRefOffset, offset: int64(vals[1])}
			case 2:
				if len(vals) >= 3 {
					sec.entries[num] = contentPDFXRefEntry{kind: contentPDFXRefCompressed, stream: vals[1], index: vals[2]}
				}
			}
		}
	}
	d.xrefPrev(sec, dict)
	return sec, true
}

func contentPDFReadBE(b []byte, n int) int {
	v := 0
	for i := 0; i < n && i < len(b); i++ {
		v = v<<8 | int(b[i])
	}
	return v
}

func contentPDFReadIntWord(l *contentPDFLexer) (int, bool) {
	l.skipSpace()
	n, err := strconv.Atoi(l.readWord())
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseIndirectAt parses `num gen obj <value> [stream ... endstream] [endobj]`.
func (d *contentPDFDoc) parseIndirectAt(off, depth int) (*contentPDFObject, bool) {
	if off < 0 || off >= len(d.buf) || depth > contentPDFMaxResolveDepth {
		return nil, false
	}
	l := &contentPDFLexer{buf: d.buf, pos: off, budget: contentPDFMaxValues}
	l.skipSpace()
	num, err := strconv.Atoi(l.readWord())
	if err != nil || num < 0 {
		return nil, false
	}
	l.skipSpace()
	gen, err := strconv.Atoi(l.readWord())
	if err != nil || gen < 0 {
		return nil, false
	}
	l.skipSpace()
	if l.peekWord() != "obj" {
		return nil, false
	}
	l.readWord()
	v, ok := contentPDFParseValue(l, 0)
	if !ok {
		return nil, false
	}
	o := &contentPDFObject{num: num, gen: gen, value: v}
	l.skipSpace()
	if v.kind == contentPDFDict && l.peekWord() == "stream" {
		l.readWord()
		raw, ok := d.readStreamBytes(l, v.dict, depth)
		if !ok {
			return nil, false
		}
		o.raw = raw
		o.hasStream = true
		o.value = contentPDFValue{kind: contentPDFStream, dict: v.dict, raw: raw}
	}
	return o, true
}

// readStreamBytes returns the raw stream payload. An indirect or absurd /Length
// is clamped to the bytes present; when it cannot be trusted, the `endstream`
// keyword delimits the payload instead.
func (d *contentPDFDoc) readStreamBytes(l *contentPDFLexer, dict map[string]contentPDFValue, depth int) ([]byte, bool) {
	if l.pos < len(d.buf) && d.buf[l.pos] == '\r' {
		l.pos++
	}
	if l.pos < len(d.buf) && d.buf[l.pos] == '\n' {
		l.pos++
	}
	start := l.pos
	length := int64(-1)
	if lv, ok := dict["Length"]; ok {
		v := lv
		if lv.kind == contentPDFKindRef {
			if rv, ok := d.objectDepth(lv.ref.num, depth+1); ok {
				v = rv
			}
		}
		if n, ok := contentPDFIntOf(v); ok {
			length = n
		}
	}
	if length >= 0 && length <= int64(len(d.buf)-start) {
		end := start + int(length)
		l.pos = end
		return d.buf[start:end], true
	}
	k := d.endstreamAt(start)
	if k < 0 {
		return nil, false
	}
	data := d.buf[start:k]
	for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == '\r') {
		data = data[:len(data)-1]
	}
	l.pos = k
	return data, true
}

// endstreamAt returns the absolute offset of the first `endstream` keyword at or
// after start, or -1. Occurrences are indexed once (O(n)) so many streams whose
// /Length is absent do not each rescan the whole buffer.
func (d *contentPDFDoc) endstreamAt(start int) int {
	const kw = "endstream"
	if !d.endstreamsSet {
		d.endstreamsSet = true
		for off := 0; ; {
			i := bytes.Index(d.buf[off:], []byte(kw))
			if i < 0 {
				break
			}
			d.endstreams = append(d.endstreams, off+i)
			off += i + len(kw)
		}
	}
	i := sort.SearchInts(d.endstreams, start)
	if i < len(d.endstreams) {
		return d.endstreams[i]
	}
	return -1
}

// object resolves an indirect object by number, through the xref offset map or
// its object-stream container. depth bounds the /Length->object recursion.
func (d *contentPDFDoc) objectDepth(num, depth int) (contentPDFValue, bool) {
	if depth > contentPDFMaxResolveDepth {
		return contentPDFNullValue(), false
	}
	if v, ok := d.cache[num]; ok {
		return v, true
	}
	d.objCount++
	if d.objCount > contentPDFMaxObjects {
		return contentPDFNullValue(), false
	}
	if off, ok := d.offsets[num]; ok {
		o, ok := d.parseIndirectAt(int(off), depth)
		if !ok {
			return contentPDFNullValue(), false
		}
		d.cache[num] = o.value
		return o.value, true
	}
	if c, ok := d.compressed[num]; ok {
		objs, ok := d.objStreamObjects(c.stream, depth+1)
		if !ok {
			return contentPDFNullValue(), false
		}
		if v, ok := objs[num]; ok {
			d.cache[num] = v
			return v, true
		}
	}
	return contentPDFNullValue(), false
}

func (d *contentPDFDoc) resolve(v contentPDFValue) contentPDFValue {
	for depth := 0; v.kind == contentPDFKindRef; depth++ {
		if depth > contentPDFMaxResolveDepth {
			return contentPDFNullValue()
		}
		nv, ok := d.objectDepth(v.ref.num, depth)
		if !ok {
			return contentPDFNullValue()
		}
		v = nv
	}
	return v
}

// objStreamObjects decodes one /ObjStm and parses its contained objects. A
// missing /Type is tolerated (some writers omit it) when /N and /First are
// present.
func (d *contentPDFDoc) objStreamObjects(streamNum, depth int) (map[int]contentPDFValue, bool) {
	if depth > contentPDFMaxResolveDepth {
		return nil, false
	}
	if m, ok := d.objStmCache[streamNum]; ok {
		return m, true
	}
	v, ok := d.objectDepth(streamNum, depth)
	if !ok || v.kind != contentPDFStream {
		return nil, false
	}
	if t, ok := contentPDFNameOf(v.dict["Type"]); ok && t != "ObjStm" {
		return nil, false
	}
	n, ok := contentPDFIntOf(d.resolve(v.dict["N"]))
	if !ok || n <= 0 || n > contentPDFMaxObjStm {
		return nil, false
	}
	first, ok := contentPDFIntOf(d.resolve(v.dict["First"]))
	if !ok || first < 0 {
		return nil, false
	}
	data, _, err := contentPDFDecodeStream(d.ctx, d, v.dict, v.raw, d.maxRaw)
	if err != nil {
		return nil, false
	}
	l := &contentPDFLexer{buf: data, budget: contentPDFMaxValues}
	nums := make([]int, n)
	offs := make([]int, n)
	for i := 0; i < int(n); i++ {
		a, ok1 := contentPDFReadIntWord(l)
		b, ok2 := contentPDFReadIntWord(l)
		if !ok1 || !ok2 || a < 0 || b < 0 {
			return nil, false
		}
		nums[i] = a
		offs[i] = b
	}
	objs := make(map[int]contentPDFValue, n)
	for i := 0; i < int(n); i++ {
		p := int(first) + offs[i]
		if p < 0 || p >= len(data) {
			continue
		}
		l2 := &contentPDFLexer{buf: data, pos: p, budget: contentPDFMaxValues}
		val, ok := contentPDFParseValue(l2, 0)
		if !ok {
			continue
		}
		objs[nums[i]] = val
		d.cache[nums[i]] = val
	}
	if ev, ok := v.dict["Extends"]; ok {
		if en, ok := contentPDFIntOf(d.resolve(ev)); ok && int(en) != streamNum {
			if ext, ok := d.objStreamObjects(int(en), depth+1); ok {
				for k, val := range ext {
					if _, exists := objs[k]; !exists {
						objs[k] = val
					}
				}
			}
		}
	}
	d.objStmCache[streamNum] = objs
	return objs, true
}

// ----- page tree -----

type contentPDFPage struct {
	resources contentPDFValue
	contents  []contentPDFValue
}

func (d *contentPDFDoc) collectPages() []contentPDFPage {
	rv, ok := d.trailer["Root"]
	if !ok {
		return nil
	}
	root := d.resolve(rv)
	if root.kind != contentPDFDict {
		return nil
	}
	pv, ok := root.dict["Pages"]
	if !ok {
		return nil
	}
	var out []contentPDFPage
	visited := map[int]bool{}
	var walk func(node, resources contentPDFValue, depth int)
	walk = func(node, resources contentPDFValue, depth int) {
		if depth > contentPDFMaxPageDepth || len(out) >= contentPDFMaxPages {
			return
		}
		if d.ctx.Err() != nil {
			return
		}
		node = d.resolve(node)
		if node.kind != contentPDFDict {
			return
		}
		if res, ok := node.dict["Resources"]; ok {
			resources = d.resolve(res)
		}
		typ, _ := contentPDFNameOf(node.dict["Type"])
		kids := d.resolve(node.dict["Kids"])
		if typ == "Pages" || (typ == "" && kids.kind == contentPDFArray) {
			if kids.kind != contentPDFArray {
				return
			}
			for _, kid := range kids.arr {
				if kid.kind == contentPDFKindRef {
					if visited[kid.ref.num] {
						continue
					}
					visited[kid.ref.num] = true
				}
				walk(kid, resources, depth+1)
			}
			return
		}
		pg := contentPDFPage{resources: resources}
		if cv, ok := node.dict["Contents"]; ok {
			pg.contents = d.contentRefs(cv)
		}
		out = append(out, pg)
	}
	walk(pv, contentPDFNullValue(), 0)
	return out
}

func (d *contentPDFDoc) contentRefs(v contentPDFValue) []contentPDFValue {
	v = d.resolve(v)
	switch v.kind {
	case contentPDFArray:
		return v.arr
	case contentPDFStream:
		return []contentPDFValue{v}
	}
	return nil
}
