package main

// Per-section integrity for `.gsx` sidecars.
//
// Every section table entry carries a CRC32C (Castagnoli) of its payload in
// the entry's trailing word, flagged by contentSectionFlagCRC (see
// content_index.go). Decode verifies the small sections (doc table, paths,
// policy, scope, manifest) immediately, so a corrupt small section fails the
// load. The large text/term/gram sections are verified by a background pass
// after attach, so startup does not page in gigabytes to hash them.
//
// A mismatch must never serve results: the volume is marked stale (its reader
// is dropped, so queries are refused) and a full rebuild is scheduled (v1 has
// no per-shard rebuild). Sections without a checksum are legacy files that
// keep loading; the next build rewrites them with checksums.
//
// The offline `content` command verifies the whole file synchronously before
// searching.

import (
	"fmt"
)

// verifyContentSectionCRCs checks every checksummed section of idx against its
// table checksum and returns the first mismatch. Legacy sections (no flag)
// are skipped. It recurses into a composite manifest's shards, so one call
// covers a whole sharded sidecar.
//
// Safety: section payloads may alias a memory-mapped file that a concurrent
// base swap unmaps (Windows faults on touching an unmapped view). The service
// only calls this with the attached base reachable (the local reference keeps
// the finalizer from running, and swaps happen under indexMu while the pass
// holds a content task), and every section is probed readable before it is
// hashed, so a torn-down mapping fails the pass instead of faulting.
func verifyContentSectionCRCs(idx *contentIndex) error {
	if idx == nil {
		return fmt.Errorf("nil content index")
	}
	for tag, payload := range idx.Sections {
		want, ok := idx.SectionCRC[tag]
		if !ok {
			continue
		}
		if !contentSectionReadable(payload) {
			return fmt.Errorf("content section %s is no longer readable", contentSectionTagName(tag))
		}
		if got := contentSectionCRC(payload); got != want {
			return &contentSectionCRCError{tag: tag, want: want, got: got}
		}
	}
	for _, sh := range idx.shards {
		if sh == nil || sh.idx == nil {
			return fmt.Errorf("content shard is missing its index")
		}
		if err := verifyContentSectionCRCs(sh.idx); err != nil {
			return err
		}
	}
	return nil
}

// contentVerifyAsync runs the post-attach integrity pass off the attach path.
// A var so tests run it synchronously and deterministically.
var contentVerifyAsync = func(fn func()) { go fn() }

// contentIntegrityMaxRebuilds caps the rebuilds one integrity failure streak
// schedules: a disk that corrupts every write must leave the volume visibly
// stale with the reason surfaced, not spin rebuilds forever. A var so tests
// run without waiting.
var contentIntegrityMaxRebuilds = 3

// verifyContentAfterAttach is the background integrity pass: it hashes the
// large sections of the just-attached base (the small ones were verified at
// load) and, on mismatch, drops the base and schedules a rebuild. expect is
// the attached index; a base swapped in while the pass ran is left alone.
func (s *goSearchService) verifyContentAfterAttach(vol *serviceVolumeIndex, expect *contentIndex) {
	if s == nil || vol == nil || vol.content == nil || expect == nil {
		return
	}
	if s.contentStopped(vol) {
		return
	}
	if err := verifyContentSectionCRCs(expect); err != nil {
		s.contentIntegrityFailed(vol, expect, err.Error())
		return
	}
	vol.content.markIntegrityVerified()
}

// contentIntegrityFailed drops a checksum-failed base so it is never served
// and schedules a full rebuild (v1 has no per-shard rebuild). It only acts
// when expect is still the attached base in a serving state; a newer base (or
// an indexing/rebuilding volume) is left alone. The rebuild is scheduled at
// most contentIntegrityMaxRebuilds times per failure streak; past that the
// volume stays stale with the reason in health.
func (s *goSearchService) contentIntegrityFailed(vol *serviceVolumeIndex, expect *contentIndex, reason string) {
	if s == nil || vol == nil || vol.content == nil {
		return
	}
	schedule := vol.content.markIntegrityFailed(expect, reason)
	if !schedule {
		return
	}
	if vol.content.integrityFailureCount() > contentIntegrityMaxRebuilds {
		serviceLog("content integrity failure volume=%s: %s; rebuild cap hit, leaving stale", vol.volume, reason)
		return
	}
	serviceLog("content integrity failure volume=%s: %s; rebuilding", vol.volume, reason)
	s.scheduleContentRebuild(vol)
}
