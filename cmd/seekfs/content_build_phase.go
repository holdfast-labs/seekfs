package main

// Build-phase wall-time capture for content index assembly.
//
// contentAssemblePhaseCapture is a test-only hook (local profiling harnesses):
// when non-nil, assembleContentIndexStreamed and the section builders
// accumulate per-phase wall time into it with atomic adds. Production builds
// leave it nil, so each site pays one predictable branch on a cold path
// (batch/spill/finish granularity — never per document, term or gram).
// Single-flight only: concurrent builds sharing one capture would merge
// their times.

import (
	"sync/atomic"
	"time"
)

// contentAssemblePhaseTimes accumulates per-phase wall nanos for one
// assembleContentIndexStreamed run. Read via snapshot.
type contentAssemblePhaseTimes struct {
	// tokenize covers the parallel normalize/hash/tokenize stage per batch.
	tokenize int64
	// emit covers the serial emit loop per batch: doc-table appends, text
	// Append (including zstd flushes, also counted in encodeText) and
	// term/gram Adds (including mid-emit spills, also counted in
	// spillSort). A subset relation, not an addition.
	emit int64
	// train covers the v8 per-shard dictionary training (one call).
	train int64
	// spillSort covers every external-builder spill (sort + run write)
	// across the gram and term builders; spills counts those spills
	// (one spilled run each).
	spillSort int64
	spills    int64
	// mergeTerm/mergeGram cover the FinishFile k-way merges (run reads +
	// block encode); writeTerm/writeGram cover the section file writes.
	mergeTerm int64
	writeTerm int64
	mergeGram int64
	writeGram int64
	// finishText covers the CXST FinishFile (tail flush + section file).
	finishText int64
	// encodeText covers textWriter.flush (zstd EncodeAll + frame spool
	// write); a subset of emit + finishText.
	encodeText int64
	// pathsEncode covers encodeContentPaths; saveFsync covers the final
	// temp-file Sync in contentSaveAssembled.
	pathsEncode int64
	saveFsync   int64
}

// contentAssemblePhaseCapture, when non-nil, receives per-phase wall time.
var contentAssemblePhaseCapture *contentAssemblePhaseTimes

// contentPhaseAdd adds d to the capture's field. Each call site guards on
// the capture being non-nil first, so production pays one branch.
func contentPhaseAdd(dst *int64, d time.Duration) {
	atomic.AddInt64(dst, int64(d))
}

// contentAssemblePhases is a plain snapshot of the capture for logging.
type contentAssemblePhases struct {
	Tokenize   time.Duration
	Emit       time.Duration
	Train      time.Duration
	SpillSort  time.Duration
	Spills     int64
	MergeTerm  time.Duration
	WriteTerm  time.Duration
	MergeGram  time.Duration
	WriteGram  time.Duration
	FinishText time.Duration
	EncodeText time.Duration
	Paths      time.Duration
	SaveFsync  time.Duration
}

func (c *contentAssemblePhaseTimes) snapshot() contentAssemblePhases {
	if c == nil {
		return contentAssemblePhases{}
	}
	get := func(p *int64) time.Duration { return time.Duration(atomic.LoadInt64(p)) }
	return contentAssemblePhases{
		Tokenize:   get(&c.tokenize),
		Emit:       get(&c.emit),
		Train:      get(&c.train),
		SpillSort:  get(&c.spillSort),
		Spills:     atomic.LoadInt64(&c.spills),
		MergeTerm:  get(&c.mergeTerm),
		WriteTerm:  get(&c.writeTerm),
		MergeGram:  get(&c.mergeGram),
		WriteGram:  get(&c.writeGram),
		FinishText: get(&c.finishText),
		EncodeText: get(&c.encodeText),
		Paths:      get(&c.pathsEncode),
		SaveFsync:  get(&c.saveFsync),
	}
}
