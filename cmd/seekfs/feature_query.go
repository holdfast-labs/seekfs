package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"seekfs/feature"
)

type featureLeaf struct {
	Name    string
	Text    string
	matches map[uint64]struct{}
}

func parseFeatureLeaf(raw string) (featureLeaf, error) {
	name, text, ok := strings.Cut(raw, ":")
	if !ok || !validFeatureName(name) || name == "content" || strings.TrimSpace(text) == "" {
		return featureLeaf{}, fmt.Errorf("expected feature:<companion-name>:<term>; use content: for built-in content")
	}
	if text[0] == '"' || text[0] == '\'' {
		if len(text) < 2 || text[len(text)-1] != text[0] {
			return featureLeaf{}, fmt.Errorf("unterminated feature query quote")
		}
		text = contentUnquoteToken(text)
	}
	if text == "" {
		return featureLeaf{}, fmt.Errorf("empty feature query")
	}
	return featureLeaf{Name: name, Text: text}, nil
}

func queryHasFeatureToken(q string) bool {
	if !strings.Contains(q, "feature:") {
		return false
	}
	for _, token := range contentTokenizeQuery(q) {
		if strings.HasPrefix(strings.TrimLeft(token, "!-"), "feature:") {
			return true
		}
		for _, part := range featureSplitAlternatives(token) {
			if strings.HasPrefix(strings.TrimLeft(part, "!-"), "feature:") {
				return true
			}
		}
	}
	return false
}

func queryHasFeatureLeaf(pq parsedQuery) bool {
	if len(pq.Features) > 0 {
		return true
	}
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			if queryHasFeatureLeaf(alt) {
				return true
			}
		}
	}
	for _, neg := range pq.NotGroups {
		if queryHasFeatureLeaf(neg) {
			return true
		}
	}
	return false
}

func featureAllLeaves(pq parsedQuery) []featureLeaf {
	out := append([]featureLeaf(nil), pq.Features...)
	for _, group := range pq.OrGroups {
		for _, alt := range group {
			out = append(out, featureAllLeaves(alt)...)
		}
	}
	for _, neg := range pq.NotGroups {
		out = append(out, featureAllLeaves(neg)...)
	}
	return out
}

func featureSplitAlternatives(raw string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' || raw[i] == '\'' {
			i = contentSkipQuoted(raw, i) - 1
			continue
		}
		if raw[i] == ':' && i+1 < len(raw) && raw[i+1] == '/' {
			if end, ok := contentSkipRegexSpan(raw, i+2); ok {
				i = end - 1
				continue
			}
		}
		if raw[i] == '|' {
			parts = append(parts, raw[start:i])
			start = i + 1
		}
	}
	return append(parts, raw[start:])
}

func (s *goSearchService) bindFeatureQuery(ctx context.Context, vol *serviceVolumeIndex, pq *parsedQuery, answers map[string]map[uint64]struct{}, shared **featureVolumeView, remaining *int) error {
	for i := range pq.Features {
		leaf := &pq.Features[i]
		key := leaf.Name + "\x00" + leaf.Text
		set, ok := answers[key]
		if !ok {
			companion := s.features.Load().companions[leaf.Name]
			if companion == nil {
				return fmt.Errorf("feature %s is disabled or not configured", leaf.Name)
			}
			var view *featureVolumeView
			var err error
			set, view, err = companion.query(ctx, vol, leaf.Text, pq.CaseSensitive)
			if err != nil {
				return err
			}
			if len(set) > *remaining {
				return fmt.Errorf("feature query exceeds aggregate candidate budget; narrow the feature terms")
			}
			*remaining -= len(set)
			switch {
			case *shared == nil:
				*shared = view
			case (*shared).meta != view.meta || (*shared).gen != view.gen:
				return fmt.Errorf("feature query changed between predicates; retry required")
			case view.prepared && !(*shared).prepared:
				*shared = view
			}
			answers[key] = set
		}
		leaf.matches = set
	}
	for g := range pq.OrGroups {
		for a := range pq.OrGroups[g] {
			pq.OrGroups[g][a].CaseSensitive = pq.CaseSensitive
			if err := s.bindFeatureQuery(ctx, vol, &pq.OrGroups[g][a], answers, shared, remaining); err != nil {
				return err
			}
		}
	}
	for n := range pq.NotGroups {
		pq.NotGroups[n].CaseSensitive = pq.CaseSensitive
		if err := s.bindFeatureQuery(ctx, vol, &pq.NotGroups[n], answers, shared, remaining); err != nil {
			return err
		}
	}
	return nil
}

// Generic companions use bounded FRN candidates for top-level conjunctions.
// ponytail: OR/NOT-only queries scan at most MaxQueryMatches records; add a
// native planner for a feature only when its workload needs a wider fast lane.
func (s *goSearchService) featureVolumeMatches(vol *serviceVolumeIndex, v *featureVolumeView, pq parsedQuery, top *featureTopHeap, matchBudget int) (int, error) {
	if queryHasAnyContentLeaf(pq) && (!vol.contentUsableForQuery() || vol.content.healthIncomplete()) {
		return 0, contentUnavailableError()
	}
	if err := checkQueryCapabilities(pq, v.view.index); err != nil {
		return 0, err
	}
	dropSatisfiedVolumeTerms(&pq, vol.volume)
	cache := make(map[int]string, feature.PageSize)
	matcher := newContentLeafMatcher(pq)
	count, visited := 0, 0
	visit := func(entry Entry) error {
		visited++
		if visited > feature.MaxQueryMatches {
			return fmt.Errorf("feature query exceeds scan budget; add a top-level feature predicate")
		}
		if queryCanceled(pq) {
			return errQueryCanceled
		}
		if entryMatchesWithContentMatcher(v.view, entry, pq, pq.MatchPath, matcher) {
			count++
			if count > matchBudget {
				return fmt.Errorf("feature query exceeds aggregate match budget; narrow the query")
			}
			if err := top.offer(entry); err != nil {
				return err
			}
		}
		cache = boundContentPathCache(cache)
		return nil
	}
	if len(pq.Features) > 0 {
		set := pq.Features[0].matches
		for _, leaf := range pq.Features[1:] {
			if len(leaf.matches) < len(set) {
				set = leaf.matches
			}
		}
		for frn := range set {
			if entry, ok := v.entryForFRN(frn, cache); ok {
				if err := visit(entry); err != nil {
					return 0, err
				}
			}
		}
	} else {
		if v.baseCount+len(v.records) > feature.MaxQueryMatches {
			return 0, fmt.Errorf("feature OR/NOT scan exceeds budget; add a top-level feature predicate")
		}
		for id := 0; id < v.baseCount; id++ {
			if v.hidden.contains(id) {
				continue
			}
			rec := v.view.index.compactRecord(id)
			if rec.Deleted {
				continue
			}
			if entry, ok := v.entryForFRN(rec.FRN, cache); ok {
				if err := visit(entry); err != nil {
					return 0, err
				}
			}
		}
		for frn := range v.latest {
			if entry, ok := v.entryForFRN(frn, cache); ok {
				if err := visit(entry); err != nil {
					return 0, err
				}
			}
		}
	}
	return count, nil
}

func (s *goSearchService) searchFeatureQuery(opts queryOptions, countOnly bool) ([]Entry, int, error) {
	s.initializeFeatures()
	pq, err := parseQuery(opts)
	if err != nil {
		return nil, 0, err
	}
	if !queryHasFeatureLeaf(pq) {
		return nil, 0, fmt.Errorf("no feature predicate")
	}
	for _, leaf := range featureAllLeaves(pq) {
		if s.features.Load().companions[leaf.Name] == nil {
			return nil, 0, fmt.Errorf("feature %s is disabled or not configured", leaf.Name)
		}
	}
	if pq.SortColumn == "relevance" || pq.Fuzzy {
		return nil, 0, fmt.Errorf("generic feature queries do not support relevance sorting or fuzzy rewriting")
	}
	deadline := time.Now().Add(serviceQueryTimeout - 250*time.Millisecond)
	if opts.DeadlineUnix != 0 {
		deadline = time.Unix(0, opts.DeadlineUnix)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if opts.Cancel != nil {
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				if opts.Cancel() {
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	s.indexMu.RLock()
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	s.indexMu.RUnlock()
	volumes, err = serviceVolumesForQuery(volumes, opts)
	if err != nil {
		return nil, 0, err
	}
	top := &featureTopHeap{pq: pq}
	if !countOnly {
		top.limit = min(normalizedLimit(opts.Limit, false), feature.MaxQueryMatches)
	}
	remaining := feature.MaxQueryMatches
	total := 0
	for _, vol := range volumes {
		bound := cloneParsedQuery(pq)
		var view *featureVolumeView
		if err = s.bindFeatureQuery(ctx, vol, &bound, make(map[string]map[uint64]struct{}), &view, &remaining); err != nil {
			return nil, 0, err
		}
		if view != nil {
			if err := s.prepareFeatureView(vol, view); err != nil {
				return nil, 0, err
			}
		}
		s.indexMu.RLock()
		vol.mu.Lock()
		if view != nil && !s.featureVolumeCurrent(vol, view) {
			err = fmt.Errorf("feature query changed before verification; retry required")
		}
		vol.mu.Unlock()
		if err == nil && view != nil {
			var count int
			count, err = s.featureVolumeMatches(vol, view, bound, top, feature.MaxQueryMatches-total)
			total += count
		}
		s.indexMu.RUnlock()
		if err != nil {
			return nil, 0, err
		}
	}
	results := top.sorted()
	opts.Trace.setPlannerMode("feature-frn")
	opts.Trace.setSource("feature-frn", total)
	return results, total, nil
}

func (s *goSearchService) serviceCommandFeatureSearch(w io.Writer, caps serviceCapabilities, req *serviceRequest) {
	if caps.Remote {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: "feature predicates are local-only"})
		return
	}
	opts := requestToOptionsFromService(*req)
	opts.Trace = &searchTrace{}
	if req.RequestSeq > 0 {
		for {
			current := s.requestSeq.Load()
			if req.RequestSeq <= current || s.requestSeq.CompareAndSwap(current, req.RequestSeq) {
				break
			}
		}
		opts.Cancel = func() bool { return req.RequestSeq < s.requestSeq.Load() }
	}
	if req.CancelOverride != nil {
		opts.Cancel = req.CancelOverride
	}
	start := time.Now()
	results, count, err := s.searchFeatureQuery(opts, req.CountOnly)
	if err != nil {
		_ = json.NewEncoder(w).Encode(serviceResponse{OK: false, Message: err.Error()})
		return
	}
	complete := true
	resp := serviceResponse{OK: true, Count: count, SearchMS: float64(time.Since(start).Nanoseconds()) / 1e6, Source: "feature-frn", PlannerMode: "feature-frn", Complete: &complete}
	if !req.CountOnly {
		resp.Count = len(results)
		resp.Rows = entriesToJSON(results)
		for _, entry := range results {
			resp.Results = append(resp.Results, entry.Path)
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}
