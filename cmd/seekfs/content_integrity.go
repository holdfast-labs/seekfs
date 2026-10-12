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

// contentVerifySectionHook runs before each section is hashed. Tests use it
// to pause the pass mid-hash (proving a concurrent Release waits the pin out)
// or to observe the pass's progress.
var contentVerifySectionHook = func(tag uint32) {}

// verifyContentSectionCRCs checks every checksummed section of idx against its
// table checksum and returns the first mismatch. Legacy sections (no flag)
// are skipped. It recurses into a composite manifest's shards, so one call
// covers a whole sharded sidecar.
//
// Lifetime: section payloads may alias a memory-mapped file, and on Windows
// touching an unmapped view faults instead of panicking. Each index (and each
// shard index) is therefore hashed under its read pin: Release takes the
// write side, so the unmap waits for every in-flight section read. When the
// index was already released the pass stops early with an error instead of
// hashing a gone mapping.
func verifyContentSectionCRCs(idx *contentIndex) error {
	if idx == nil {
		return fmt.Errorf("nil content index")
	}
	if !idx.pinRead() {
		return fmt.Errorf("content index was released before verification")
	}
	defer idx.pinDone()
	for tag, payload := range idx.Sections {
		want, ok := idx.SectionCRC[tag]
		if !ok {
			continue
		}
		contentVerifySectionHook(tag)
		if idx.released() {
			return fmt.Errorf("content index was released during verification")
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
