package main

// Sharded content sidecars. A corpus larger than contentShardTargetBytes of
// extracted text is split by folder into shard files under `<gsx>.shards`,
// and the `.gsx` itself becomes a small manifest (format version 5) naming
// them. Shards are immutable: a fold rewrites only the shards its delta
// touches and publishes a new manifest atomically, so live updates cost the
// size of the touched shards instead of the whole corpus, and build/fold peak
// memory is bounded by one shard.
//
// A shard owns a set of folder prefixes. A path belongs to the shard owning
// its longest matching prefix; the root prefix "" always exists, so every
// path (including folders created after the build) has an owner.
//
// Loaded, a manifest becomes a composite contentIndex: Docs is the
// concatenation of the shard doc tables with DocIDs rebased by each shard's
// base, so the reader, resolver and health code see one index.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

// contentManifestVersion marks a manifest `.gsx`. Older binaries reject the
// version (and rebuild) instead of attaching an empty index.
const contentManifestVersion = 5

const contentSectionShards uint32 = 'C'<<24 | 'X'<<16 | 'S'<<8 | 'H'

// contentShardTargetBytes is the extracted-text size a shard is planned to.
// A corpus at or below it stays a single-file sidecar. Assembly peaks at
// several times a shard's text, so 64 MiB keeps a fold near 1 GB of heap and
// ~15 s on Chromium (256 MiB shards peaked at 1.8 GB and ~55 s). A var so
// tests can exercise sharding with small corpora.
var contentShardTargetBytes int64 = 64 << 20

// contentShardInfo is one manifest entry.
type contentShardInfo struct {
	File     string   // file name inside the shard directory
	Docs     int      // doc count, checked on load
	Prefixes []string // normalized folder prefixes this shard owns
}

// contentShard is a loaded shard of a composite index.
type contentShard struct {
	info   contentShardInfo
	idx    *contentIndex
	reader *contentReader
	base   uint32 // global DocID of the shard's first doc
}

func contentShardDir(gsx string) string { return gsx + ".shards" }

func encodeContentShards(shards []contentShardInfo) []byte {
	var b []byte
	put32 := func(v int) { b = binary.LittleEndian.AppendUint32(b, uint32(v)) }
	putStr := func(s string) { put32(len(s)); b = append(b, s...) }
	put32(len(shards))
	for _, sh := range shards {
		putStr(sh.File)
		put32(sh.Docs)
		put32(len(sh.Prefixes))
		for _, p := range sh.Prefixes {
			putStr(p)
		}
	}
	return b
}

func decodeContentShards(b []byte) ([]contentShardInfo, error) {
	bad := errors.New("content manifest shard table truncated")
	get32 := func() (int, error) {
		if len(b) < 4 {
			return 0, bad
		}
		v := int(binary.LittleEndian.Uint32(b))
		b = b[4:]
		return v, nil
	}
	getStr := func() (string, error) {
		n, err := get32()
		if err != nil || n > len(b) {
			return "", bad
		}
		s := string(b[:n])
		b = b[n:]
		return s, nil
	}
	count, err := get32()
	if err != nil || count > 1<<20 {
		return nil, bad
	}
	out := make([]contentShardInfo, 0, count)
	for i := 0; i < count; i++ {
		var sh contentShardInfo
		if sh.File, err = getStr(); err != nil {
			return nil, err
		}
		if sh.File == "" || sh.File != filepath.Base(sh.File) {
			return nil, fmt.Errorf("content manifest shard name %q is not a plain file name", sh.File)
		}
		if sh.Docs, err = get32(); err != nil {
			return nil, err
		}
		n, err := get32()
		if err != nil || n > len(b) {
			return nil, bad
		}
		for j := 0; j < n; j++ {
			p, err := getStr()
			if err != nil {
				return nil, err
			}
			sh.Prefixes = append(sh.Prefixes, p)
		}
		out = append(out, sh)
	}
	return out, nil
}

// contentComposeShards loads the shards a decoded manifest names (from the
// directory next to gsx) and turns the manifest into a composite index. On
// error every shard loaded so far is released.
func contentComposeShards(manifest *contentIndex, gsx string) error {
	infos, err := decodeContentShards(manifest.Sections[contentSectionShards])
	if err != nil {
		return err
	}
	dir := contentShardDir(gsx)
	shards := make([]*contentShard, 0, len(infos))
	fail := func(err error) error {
		for _, sh := range shards {
			sh.idx.Release()
		}
		return err
	}
	var docs []contentDoc
	size := manifest.EncodedSize
	for _, info := range infos {
		idx, err := contentLoadFileMapped(filepath.Join(dir, info.File))
		if err != nil {
			return fail(fmt.Errorf("content shard %s: %w", info.File, err))
		}
		if idx.shards != nil || len(idx.Docs) != info.Docs {
			idx.Release()
			return fail(fmt.Errorf("content shard %s does not match its manifest entry", info.File))
		}
		reader, err := openContentReader(idx)
		if err != nil {
			idx.Release()
			return fail(fmt.Errorf("content shard %s: %w", info.File, err))
		}
		base := uint32(len(docs))
		for _, d := range idx.Docs {
			d.DocID += base
			docs = append(docs, d)
		}
		size += idx.EncodedSize
		shards = append(shards, &contentShard{info: info, idx: idx, reader: reader, base: base})
	}
	manifest.Docs = docs
	manifest.EncodedSize = size
	manifest.shards = shards
	return nil
}

// shardOf maps a global DocID to its shard and local DocID.
func (r *contentReader) shardOf(docID uint32) (*contentShard, uint32, bool) {
	shards := r.idx.shards
	i := sort.Search(len(shards), func(i int) bool { return shards[i].base > docID }) - 1
	if i < 0 {
		return nil, 0, false
	}
	sh := shards[i]
	local := docID - sh.base
	if int(local) >= len(sh.idx.Docs) {
		return nil, 0, false
	}
	return sh, local, true
}

// shardedCandidateStream concatenates the shards' streams, rebased to global
// DocIDs; shard bases ascend, so the result stays ascending. A nil mask
// includes every shard.
func (r *contentReader) shardedCandidateStream(term string, mask []bool) func() (uint32, bool) {
	shards := r.idx.shards
	i := -1
	var cur func() (uint32, bool)
	return func() (uint32, bool) {
		for {
			if cur != nil {
				if id, ok := cur(); ok {
					return shards[i].base + id, true
				}
			}
			i++
			for i < len(shards) && mask != nil && !mask[i] {
				i++
			}
			if i >= len(shards) {
				return 0, false
			}
			cur = shards[i].reader.candidateStream(term)
		}
	}
}

// shardMaskUnder returns which shards can hold documents under the normalized
// folder under, or nil when every shard can (an unsharded reader, or no
// scope). A shard qualifies if it owns a prefix inside under, or owns the
// longest prefix containing under (the folder's own owner).
func (r *contentReader) shardMaskUnder(under string) []bool {
	if r == nil || r.idx == nil || len(r.idx.shards) == 0 {
		return nil
	}
	under = contentNormPath(under)
	if under == "" {
		return nil
	}
	assign := newContentShardAssigner(r.idx.shards)
	mask := make([]bool, len(r.idx.shards))
	mask[assign.dir(under)] = true
	for i, sh := range r.idx.shards {
		for _, p := range sh.info.Prefixes {
			if p != "" && contentPathUnder(p, under) {
				mask[i] = true
				break
			}
		}
	}
	return mask
}

// contentShardAssigner maps paths to shard indexes by longest prefix.
type contentShardAssigner struct {
	owner map[string]int
	root  int
}

func newContentShardAssigner(shards []*contentShard) *contentShardAssigner {
	infos := make([]contentShardInfo, len(shards))
	for i, sh := range shards {
		infos[i] = sh.info
	}
	return newContentShardAssignerFromInfo(infos)
}

func newContentShardAssignerFromInfo(infos []contentShardInfo) *contentShardAssigner {
	a := &contentShardAssigner{owner: make(map[string]int), root: len(infos) - 1}
	for i, info := range infos {
		for _, p := range info.Prefixes {
			if p == "" {
				a.root = i
			} else {
				a.owner[p] = i
			}
		}
	}
	return a
}

// dir returns the shard owning the normalized folder d.
func (a *contentShardAssigner) dir(d string) int {
	for d != "" {
		if i, ok := a.owner[d]; ok {
			return i
		}
		parent := filepath.Dir(d)
		if parent == d || parent == "." {
			break
		}
		d = parent
	}
	return a.root
}

// file returns the shard owning the file at path.
func (a *contentShardAssigner) file(path string) int {
	return a.dir(filepath.Dir(contentNormPath(path)))
}

// contentPlanShards groups files into shards of roughly target bytes by
// folder. A folder whose subtree fits is kept whole; a larger folder keeps
// its own files as one unit and is split by child folder. Units are then
// packed into shards in path order, so a shard covers neighbouring folders
// and an under: query touches few shards. The root prefix "" lands in the
// first shard.
func contentPlanShards(paths []string, sizes []int64, target int64) [][]string {
	type node struct {
		path     string
		own      int64
		total    int64
		children map[string]*node
	}
	root := &node{children: map[string]*node{}}
	for i, p := range paths {
		dir := filepath.Dir(contentNormPath(p))
		var chain []string
		for d := dir; d != "" && d != "."; {
			chain = append(chain, d)
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
		n := root
		n.total += sizes[i]
		for j := len(chain) - 1; j >= 0; j-- {
			c, ok := n.children[chain[j]]
			if !ok {
				c = &node{path: chain[j], children: map[string]*node{}}
				n.children[chain[j]] = c
			}
			c.total += sizes[i]
			n = c
		}
		n.own += sizes[i]
	}
	type unit struct {
		prefix string
		size   int64
	}
	var units []unit
	var split func(n *node)
	split = func(n *node) {
		if n.total <= target && n.path != "" {
			units = append(units, unit{n.path, n.total})
			return
		}
		units = append(units, unit{n.path, n.own})
		for _, c := range n.children {
			split(c)
		}
	}
	split(root)
	sort.Slice(units, func(i, j int) bool { return units[i].prefix < units[j].prefix })
	var out [][]string
	var cur []string
	var used int64
	for _, u := range units {
		if len(cur) > 0 && used+u.size > target {
			out = append(out, cur)
			cur, used = nil, 0
		}
		cur = append(cur, u.prefix)
		used += u.size
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

var contentShardSeq atomic.Uint64

// contentShardFileName returns a fresh, unique shard file name; shard files
// are never rewritten in place.
func contentShardFileName() string {
	return fmt.Sprintf("%x-%x.gsx", time.Now().UnixNano(), contentShardSeq.Add(1))
}

// contentSaveShard writes one shard file into dir and returns its manifest
// entry.
func contentSaveShard(dir string, idx *contentIndex, prefixes []string) (contentShardInfo, error) {
	name := contentShardFileName()
	if err := contentSaveFile(filepath.Join(dir, name), idx); err != nil {
		return contentShardInfo{}, err
	}
	return contentShardInfo{File: name, Docs: len(idx.Docs), Prefixes: prefixes}, nil
}

// contentSaveManifest atomically publishes a manifest naming shards. header
// supplies the origin, watermark, policy and scope identity.
func contentSaveManifest(gsx string, header *contentIndex, shards []contentShardInfo) error {
	m := newContentIndex()
	m.Version = contentManifestVersion
	m.Origin = header.Origin
	m.JournalID = header.JournalID
	m.CheckpointUSN = header.CheckpointUSN
	m.Policy = header.Policy
	m.ScopeHash = header.ScopeHash
	m.Sections[contentSectionShards] = encodeContentShards(shards)
	return contentSaveFile(gsx, m)
}

// contentSweepShards deletes shard files the current manifest at gsx no
// longer names. A file still mapped by a live reader cannot be deleted on
// Windows; it is left for a later sweep. A missing or unsharded manifest
// removes the whole directory's contents.
func contentSweepShards(gsx string) {
	dir := contentShardDir(gsx)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	if data, err := os.ReadFile(gsx); err == nil {
		if m, err := contentIndexDecode(data); err == nil {
			if infos, err := decodeContentShards(m.Sections[contentSectionShards]); err == nil {
				for _, info := range infos {
					keep[info.File] = true
				}
			}
		} else {
			return // unreadable manifest: do not guess what is live
		}
	}
	for _, e := range entries {
		if !keep[e.Name()] && !e.IsDir() {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// contentShardGroup is the docs (in ascending FRN order) bound for one shard.
type contentShardGroup struct {
	prefixes []string
	docs     []int // indexes into the caller's FRN-sorted doc list
}

// contentGroupByShard assigns FRN-sorted docs to the planned shards,
// preserving FRN order within each group.
func contentGroupByShard(plan [][]string, paths []string) []contentShardGroup {
	infos := make([]contentShardInfo, len(plan))
	groups := make([]contentShardGroup, len(plan))
	for i, prefixes := range plan {
		infos[i].Prefixes = prefixes
		groups[i].prefixes = prefixes
	}
	assign := newContentShardAssignerFromInfo(infos)
	for i, p := range paths {
		g := assign.file(p)
		groups[g].docs = append(groups[g].docs, i)
	}
	return groups
}

// contentShardedFoldPlan decides, for a delta snapshot against a sharded
// base, which shards must be rewritten and which delta docs each receives.
// A doc that moved between shards (rename) is dropped from its old shard and
// added to its new one; every delta FRN is dropped from the base wherever it
// lives, so the old copy never survives next to the new one.
type contentShardedFoldPlan struct {
	dirty    []bool
	incoming [][]contentDeltaDoc // per shard, delta docs it receives
	drop     map[uint64]struct{} // every FRN the delta replaces or deletes
}

func planContentShardedFold(reader *contentReader, resolver *contentResolver, snap []contentDeltaDoc) contentShardedFoldPlan {
	shards := reader.idx.shards
	assign := newContentShardAssigner(shards)
	p := contentShardedFoldPlan{
		dirty:    make([]bool, len(shards)),
		incoming: make([][]contentDeltaDoc, len(shards)),
		drop:     make(map[uint64]struct{}, len(snap)),
	}
	for _, d := range snap {
		p.drop[d.FRN] = struct{}{}
		if gi, ok := resolver.docForFRN(d.FRN); ok {
			if sh, _, ok := reader.shardOf(uint32(gi)); ok {
				p.dirty[sh.idxIn(shards)] = true
			}
		}
		if d.Deleted {
			continue
		}
		target := assign.file(d.Path)
		p.dirty[target] = true
		p.incoming[target] = append(p.incoming[target], d)
	}
	for i := range p.incoming {
		sort.Slice(p.incoming[i], func(a, b int) bool {
			x, y := p.incoming[i][a], p.incoming[i][b]
			if x.FRN != y.FRN {
				return x.FRN < y.FRN
			}
			return x.Path < y.Path
		})
	}
	return p
}

func (sh *contentShard) idxIn(shards []*contentShard) int {
	for i, s := range shards {
		if s == sh {
			return i
		}
	}
	return -1
}

// contentShardFoldSource merges one shard's base docs (minus every FRN the
// delta touches) with the delta docs assigned to it, in ascending FRN order.
func contentShardFoldSource(sh *contentShard, incoming []contentDeltaDoc, drop map[uint64]struct{}) contentBuildSource {
	base := sh.idx.Docs
	i, j := 0, 0
	return func() (contentBuildDoc, bool, error) {
		for {
			for i < len(base) {
				if _, gone := drop[base[i].FRN]; !gone {
					break
				}
				i++
			}
			haveBase, haveDelta := i < len(base), j < len(incoming)
			switch {
			case haveDelta && (!haveBase || incoming[j].FRN <= base[i].FRN):
				d := incoming[j]
				j++
				return contentBuildDoc{frn: d.FRN, path: d.Path, text: d.Text, class: d.ContentType, version: d.ExtractorVersion}, true, nil
			case haveBase:
				doc := base[i]
				i++
				text := sh.reader.docText(doc.DocID)
				if len(text) == 0 {
					continue
				}
				return contentBuildDoc{frn: doc.FRN, path: sh.reader.docPath(doc.DocID), text: text, modUnix: doc.ModUnix, class: doc.ContentType, version: doc.ExtractorVersion}, true, nil
			default:
				return contentBuildDoc{}, false, nil
			}
		}
	}
}

// contentReshardAndSave splits an assembled single-file index into folder
// shards and publishes them with a manifest at gsx. A fold uses it when an
// unsharded base has grown well past the shard target, so a volume that
// started small does not stay one ever-larger file.
func contentReshardAndSave(gsx string, whole *contentIndex, header *contentIndex, tmpDir string) (int, error) {
	reader, err := openContentReader(whole)
	if err != nil {
		return 0, err
	}
	paths := make([]string, len(whole.Docs))
	sizes := make([]int64, len(whole.Docs))
	for i, d := range whole.Docs {
		paths[i], sizes[i] = reader.docPath(d.DocID), int64(d.TextLen)
	}
	groups := contentGroupByShard(contentPlanShards(paths, sizes, contentShardTargetBytes), paths)
	dir := contentShardDir(gsx)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	infos := make([]contentShardInfo, 0, len(groups))
	for _, g := range groups {
		k := 0
		cidx, err := assembleContentIndexStream(func() (contentBuildDoc, bool, error) {
			if k == len(g.docs) {
				return contentBuildDoc{}, false, nil
			}
			d := whole.Docs[g.docs[k]]
			k++
			return contentBuildDoc{frn: d.FRN, path: paths[d.DocID], text: reader.docText(d.DocID), modUnix: d.ModUnix, class: d.ContentType, version: d.ExtractorVersion}, true, nil
		}, tmpDir)
		if err != nil {
			return 0, err
		}
		cidx.Origin, cidx.JournalID, cidx.CheckpointUSN = header.Origin, header.JournalID, header.CheckpointUSN
		cidx.Policy, cidx.ScopeHash = header.Policy, header.ScopeHash
		info, err := contentSaveShard(dir, cidx, g.prefixes)
		if err != nil {
			return 0, err
		}
		infos = append(infos, info)
	}
	return len(infos), contentSaveManifest(gsx, header, infos)
}
