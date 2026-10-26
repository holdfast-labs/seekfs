package main

// Resident small-file extraction from MFT records.
//
// Files whose name-index size is at most contentMFTResidentMaxBytes can often
// be read without opening them: NTFS stores such a file's unnamed $DATA
// directly in its MFT record. FSCTL_GET_NTFS_FILE_RECORD returns that record
// (metadata only, never data clusters); a record is usable only when the
// unnamed $DATA is resident and the file is not compressed, encrypted,
// sparse, a reparse point, or an attribute-list/extension record, and when the
// record sequence and data length match the name index. Anything doubtful
// falls back to the normal open, which stays the only read path for every
// other file.

import (
	"context"
	"sync"
	"sync/atomic"
)

// contentMFTResidentMaxBytes bounds the small-file optimization. Larger files
// are always opened (resident data that large is not a useful win).
const contentMFTResidentMaxBytes = 1024

// Fallback reasons reported in content health. Stable strings.
const (
	contentResidentReasonNonResident   = "nonresident"
	contentResidentReasonCompressed    = "compressed"
	contentResidentReasonEncrypted     = "encrypted"
	contentResidentReasonSparse        = "sparse"
	contentResidentReasonReparse       = "reparse"
	contentResidentReasonAttributeList = "attribute_list"
	contentResidentReasonExtension     = "extension_record"
	contentResidentReasonNotInUse      = "not_in_use"
	contentResidentReasonDirectory     = "directory"
	contentResidentReasonNoData        = "no_data"
	contentResidentReasonParse         = "parse"
	contentResidentReasonSequence      = "sequence_mismatch"
	contentResidentReasonSize          = "size_mismatch"
	contentResidentReasonFSCTL         = "fsctl"
	contentResidentReasonUnavailable   = "unavailable"
)

// contentResidentCounters is one build's resident-read telemetry.
type contentResidentCounters struct {
	Hits      int64
	Fallbacks map[string]int64
}

// contentResidentFallbackTotal sums the per-reason fallback counts.
func contentResidentFallbackTotal(c contentResidentCounters) int64 {
	var n int64
	for _, v := range c.Fallbacks {
		n += v
	}
	return n
}

// contentResidentSource supplies resident $DATA bytes for one volume. Tests
// inject a fake through contentOpenResidentSource; production uses the NTFS
// FSCTL. A miss returns a stable fallback reason and the caller opens the file.
type contentResidentSource interface {
	ResidentText(frn uint64, seq uint16, size int64) (data []byte, reason string, ok bool)
	Counters() contentResidentCounters
	Close()
}

// contentResidentContext carries the build's resident source to extraction.
type contentResidentContextKey struct{}

func contentWithResidentSource(ctx context.Context, src contentResidentSource) context.Context {
	if src == nil {
		return ctx
	}
	return context.WithValue(ctx, contentResidentContextKey{}, src)
}

func contentResidentFromContext(ctx context.Context) (contentResidentSource, bool) {
	if ctx == nil {
		return nil, false
	}
	src, ok := ctx.Value(contentResidentContextKey{}).(contentResidentSource)
	return src, ok && src != nil
}

// contentResidentStats accumulates hits and per-reason fallbacks. Sources
// embed it so the FSCTL and test sources share the counting code.
type contentResidentStats struct {
	hits      atomic.Int64
	mu        sync.Mutex
	fallbacks map[string]int64
}

func (s *contentResidentStats) hit() { s.hits.Add(1) }

func (s *contentResidentStats) fallback(reason string) {
	if reason == "" {
		reason = contentResidentReasonParse
	}
	s.mu.Lock()
	if s.fallbacks == nil {
		s.fallbacks = make(map[string]int64)
	}
	s.fallbacks[reason]++
	s.mu.Unlock()
}

func (s *contentResidentStats) snapshot() contentResidentCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := contentResidentCounters{Hits: s.hits.Load()}
	if len(s.fallbacks) > 0 {
		out.Fallbacks = make(map[string]int64, len(s.fallbacks))
		for reason, n := range s.fallbacks {
			out.Fallbacks[reason] = n
		}
	}
	return out
}
