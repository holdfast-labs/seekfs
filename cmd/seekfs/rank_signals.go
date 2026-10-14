package main

// Usage signals: frecency + query history (WORK_QUEUE item 1, gaps G7/G13).
//
// The store records per-file access events (`seekfs open <path>`, the UI's
// Open action, the service `open` op) and per-query picks
// (`seekfs open <path> --query "..."`). Ranking blends them as bounded
// within-band tie-breakers only (see rank_blend.go): result sets and counts
// never change, and with an empty store the order is byte-identical.
//
// Persistence: one small JSON file next to the service state
// (`defaultSeekFSDir()/ranking_signals.json`, SEEKFS_DIR-aware so tests
// redirect it). Bounded: at most signalStoreMaxFiles files and
// signalStoreMaxQueries queries (top paths each); saves compact. Local only:
// the file never leaves the machine (no network transport carries it).

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// signalStoreFileName is the persisted store next to the service state.
	signalStoreFileName = "ranking_signals.json"
	// signalStoreMaxFiles bounds the persisted per-file entries (top by
	// decayed score; the tail is evicted on save).
	signalStoreMaxFiles = 5000
	// signalStoreMaxQueries bounds the persisted per-query entries.
	signalStoreMaxQueries = 2000
	// signalStoreMaxPathsPerQuery bounds the picked paths kept per query.
	signalStoreMaxPathsPerQuery = 8
	// signalFrecencyHalfLifeDays is the frecency decay half-life: an
	// untouched file's score halves every 30 days.
	signalFrecencyHalfLifeDays = 30
	// signalComboThreshold is the query-history combo threshold (same
	// normalized query -> same pick this many times), matching fff's >= 3.
	signalComboThreshold = 3
	// signalStoreVersion is the on-disk format version.
	signalStoreVersion = 1
)

// fileSignal is one file's access record.
type fileSignal struct {
	Opens        int     `json:"opens"`
	Score        float64 `json:"score"`
	LastOpenUnix int64   `json:"last_open_unix"`
}

// queryPick is one (normalized query -> path) record.
type queryPick struct {
	Count    int   `json:"count"`
	LastUnix int64 `json:"last_unix"`
}

// signalStore is the in-memory frecency + query-history store.
type signalStore struct {
	mu      sync.Mutex
	path    string
	files   map[string]*fileSignal
	queries map[string]map[string]*queryPick
	nowFunc func() time.Time
}

// signalStoreFile is the on-disk shape.
type signalStoreFile struct {
	Version int                              `json:"version"`
	Files   map[string]*fileSignal           `json:"files"`
	Queries map[string]map[string]*queryPick `json:"queries"`
}

var (
	globalSignalStoreMu sync.Mutex
	globalSignalStore   *signalStore
)

// signalStorePath returns the store path for the current SEEKFS_DIR.
func signalStorePath() string {
	return filepath.Join(defaultSeekFSDir(), signalStoreFileName)
}

// loadSignalStore opens (or creates) the store at path.
func loadSignalStore(path string) *signalStore {
	s := &signalStore{path: path, files: make(map[string]*fileSignal), queries: make(map[string]map[string]*queryPick)}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var disk signalStoreFile
	if err := json.Unmarshal(data, &disk); err != nil || disk.Version != signalStoreVersion {
		return s
	}
	now := s.now()
	for p, f := range disk.Files {
		if p == "" || f == nil || f.Opens <= 0 {
			continue
		}
		// Apply decay up to now on load so a stale file does not keep a
		// fossil score; entries fully decayed away are dropped (compaction).
		f.Score = decaySignalScore(f.Score, time.Unix(0, f.LastOpenUnix), now)
		if f.Score < 0.01 {
			continue
		}
		s.files[p] = &fileSignal{Opens: f.Opens, Score: f.Score, LastOpenUnix: f.LastOpenUnix}
	}
	for q, picks := range disk.Queries {
		if q == "" || len(picks) == 0 {
			continue
		}
		kept := make(map[string]*queryPick, len(picks))
		for p, pick := range picks {
			if p == "" || pick == nil || pick.Count <= 0 {
				continue
			}
			kept[p] = &queryPick{Count: pick.Count, LastUnix: pick.LastUnix}
		}
		if len(kept) > 0 {
			s.queries[q] = kept
		}
	}
	s.compact()
	return s
}

// sharedSignalStore returns the process-wide store for the current
// SEEKFS_DIR. The service loads it at startup; the CLI loads it on demand
// for `open`/`recent` fallback.
func sharedSignalStore() *signalStore {
	globalSignalStoreMu.Lock()
	defer globalSignalStoreMu.Unlock()
	path := signalStorePath()
	if globalSignalStore != nil && globalSignalStore.path == path {
		return globalSignalStore
	}
	globalSignalStore = loadSignalStore(path)
	return globalSignalStore
}

// resetSharedSignalStoreForTest drops the process-wide store so tests with
// different SEEKFS_DIR values do not share state.
func resetSharedSignalStoreForTest() {
	globalSignalStoreMu.Lock()
	defer globalSignalStoreMu.Unlock()
	globalSignalStore = nil
}

func (s *signalStore) now() time.Time {
	if s != nil && s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// decaySignalScore applies half-life decay to score from last to now.
func decaySignalScore(score float64, last, now time.Time) float64 {
	if score <= 0 {
		return 0
	}
	elapsed := now.Sub(last).Hours() / 24
	if elapsed <= 0 {
		return score
	}
	return score * math.Pow(0.5, elapsed/signalFrecencyHalfLifeDays)
}

// normalizeSignalQuery maps a query to its combo key: lowercase, trimmed,
// internal whitespace collapsed.
func normalizeSignalQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(q))), " ")
}

// normalizeSignalPath cleans a file path for store keys.
func normalizeSignalPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// Fast path: already-absolute clean paths (everything the index hands
	// out, and everything recordOpen stored) need no work and no allocation.
	if filepath.IsAbs(p) && pathLooksClean(p) {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// recordOpen records one access event for path (and, when query is
// non-empty, one (query -> path) pick). It persists synchronously: opens are
// user-paced and the file is small, so crash safety beats write batching.
func (s *signalStore) recordOpen(path, query string) {
	path = normalizeSignalPath(path)
	if s == nil || path == "" {
		return
	}
	now := s.now()
	s.mu.Lock()
	f := s.files[path]
	if f == nil {
		f = &fileSignal{}
		s.files[path] = f
	}
	f.Score = decaySignalScore(f.Score, time.Unix(0, f.LastOpenUnix), now) + 1
	f.Opens++
	f.LastOpenUnix = now.UnixNano()
	if nq := normalizeSignalQuery(query); nq != "" {
		picks := s.queries[nq]
		if picks == nil {
			picks = make(map[string]*queryPick)
			s.queries[nq] = picks
		}
		pick := picks[path]
		if pick == nil {
			pick = &queryPick{}
			picks[path] = pick
		}
		pick.Count++
		pick.LastUnix = now.UnixNano()
	}
	s.compact()
	disk := s.snapshotLocked()
	storePath := s.path
	s.mu.Unlock()
	saveSignalStore(storePath, disk)
}

// comboCount returns how often normalized query q picked path (0 when the
// store is nil or the pair is unknown).
func (s *signalStore) comboCount(q, path string) int {
	nq := normalizeSignalQuery(q)
	if s == nil || nq == "" || path == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries[nq][normalizeSignalPath(path)].countOrZero()
}

func (p *queryPick) countOrZero() int {
	if p == nil {
		return 0
	}
	return p.Count
}

// comboCounts returns combo counts for (normalized query, paths) under one
// lock (batched for page reorder).
func (s *signalStore) comboCounts(q string, paths []string) map[string]int {
	out := make(map[string]int, len(paths))
	nq := normalizeSignalQuery(q)
	if s == nil || nq == "" {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	picks := s.queries[nq]
	if len(picks) == 0 {
		return out
	}
	for _, p := range paths {
		if c := picks[normalizeSignalPath(p)].countOrZero(); c > 0 {
			out[p] = c
		}
	}
	return out
}

// frecencyScore returns path's decayed score at now (0 when unknown).
func (s *signalStore) frecencyScore(path string, now time.Time) float64 {
	if s == nil || path == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[normalizeSignalPath(path)]
	if f == nil {
		return 0
	}
	last := time.Unix(0, f.LastOpenUnix)
	if now.IsZero() {
		now = time.Now()
	}
	return decaySignalScore(f.Score, last, now)
}

// empty reports whether the store holds no usable signals. The ranking fast
// path keys off this: empty means byte-identical order and ~zero overhead.
func (s *signalStore) empty() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files) == 0 && len(s.queries) == 0
}

// snapshotScores returns decayed scores for paths (missing paths score 0).
func (s *signalStore) snapshotScores(paths []string, now time.Time) map[string]float64 {
	out := make(map[string]float64, len(paths))
	if s == nil {
		return out
	}
	if now.IsZero() {
		now = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		if f := s.files[normalizeSignalPath(p)]; f != nil {
			if score := decaySignalScore(f.Score, time.Unix(0, f.LastOpenUnix), now); score > 0 {
				out[normalizeSignalPath(p)] = score
			}
		}
	}
	return out
}

// recentFileJSON is one `recent` row on the service protocol.
type recentFileJSON struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Opens    int     `json:"opens"`
	Score    float64 `json:"score"`
	LastOpen string  `json:"last_open"`
	Modified string  `json:"modified,omitempty"`
}

// recentFile is one in-memory `recent` row.
type recentFile struct {
	Path         string
	Opens        int
	Score        float64
	LastOpenUnix int64
}

// recent returns the most recently opened files, newest first, up to limit
// (limit <= 0 means all, capped at the store bound).
func (s *signalStore) recent(limit int) []recentFile {
	if s == nil {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	out := make([]recentFile, 0, len(s.files))
	for p, f := range s.files {
		out = append(out, recentFile{
			Path:         p,
			Opens:        f.Opens,
			Score:        decaySignalScore(f.Score, time.Unix(0, f.LastOpenUnix), now),
			LastOpenUnix: f.LastOpenUnix,
		})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastOpenUnix != out[j].LastOpenUnix {
			return out[i].LastOpenUnix > out[j].LastOpenUnix
		}
		return out[i].Path < out[j].Path
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// compact enforces the store bounds (caller holds mu): keep the top files by
// decayed score and the top queries by total picks, top paths per query.
func (s *signalStore) compact() {
	if len(s.files) > signalStoreMaxFiles {
		type scored struct {
			path  string
			score float64
		}
		now := s.now()
		all := make([]scored, 0, len(s.files))
		for p, f := range s.files {
			all = append(all, scored{p, decaySignalScore(f.Score, time.Unix(0, f.LastOpenUnix), now)})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
		for _, drop := range all[signalStoreMaxFiles:] {
			delete(s.files, drop.path)
		}
	}
	if len(s.queries) > signalStoreMaxQueries {
		type ranked struct {
			q     string
			total int
			last  int64
		}
		all := make([]ranked, 0, len(s.queries))
		for q, picks := range s.queries {
			r := ranked{q: q}
			for _, pick := range picks {
				r.total += pick.Count
				if pick.LastUnix > r.last {
					r.last = pick.LastUnix
				}
			}
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].total != all[j].total {
				return all[i].total > all[j].total
			}
			return all[i].last > all[j].last
		})
		for _, drop := range all[signalStoreMaxQueries:] {
			delete(s.queries, drop.q)
		}
	}
	for q, picks := range s.queries {
		if len(picks) <= signalStoreMaxPathsPerQuery {
			continue
		}
		type ranked struct {
			p    string
			n    int
			last int64
		}
		all := make([]ranked, 0, len(picks))
		for p, pick := range picks {
			all = append(all, ranked{p, pick.Count, pick.LastUnix})
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].n != all[j].n {
				return all[i].n > all[j].n
			}
			return all[i].last > all[j].last
		})
		for _, drop := range all[signalStoreMaxPathsPerQuery:] {
			delete(picks, drop.p)
		}
		if len(picks) == 0 {
			delete(s.queries, q)
		}
	}
}

// snapshotLocked deep-copies the store for persistence (caller holds mu).
func (s *signalStore) snapshotLocked() signalStoreFile {
	disk := signalStoreFile{Version: signalStoreVersion, Files: make(map[string]*fileSignal, len(s.files)), Queries: make(map[string]map[string]*queryPick, len(s.queries))}
	for p, f := range s.files {
		if f == nil {
			continue
		}
		cp := *f
		disk.Files[p] = &cp
	}
	for q, picks := range s.queries {
		cp := make(map[string]*queryPick, len(picks))
		for p, pick := range picks {
			if pick == nil {
				continue
			}
			pickCp := *pick
			cp[p] = &pickCp
		}
		disk.Queries[q] = cp
	}
	return disk
}

// saveSignalStore writes a snapshot to path atomically (tmp + rename).
func saveSignalStore(path string, disk signalStoreFile) {
	if path == "" {
		return
	}
	data, err := json.Marshal(disk)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".ranking_signals-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, path)
}

// flushSignalStore persists the shared store (service shutdown path).
func flushSignalStore() {
	globalSignalStoreMu.Lock()
	s := globalSignalStore
	globalSignalStoreMu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	disk := s.snapshotLocked()
	storePath := s.path
	s.mu.Unlock()
	saveSignalStore(storePath, disk)
}
