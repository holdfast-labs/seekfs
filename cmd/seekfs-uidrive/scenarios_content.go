//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type contentHealthView struct {
	State      string `json:"state"`
	Docs       int    `json:"docs"`
	Partial    bool   `json:"partial"`
	Incomplete bool   `json:"incomplete"`
	Stale      bool   `json:"content_stale"`
	BuildDone  int    `json:"build_done"`
	BuildTotal int    `json:"build_total"`
	BuildError string `json:"build_error"`
}

type serviceInfoView struct {
	OK      bool               `json:"ok"`
	Message string             `json:"message"`
	Health  string             `json:"health"`
	Content *contentHealthView `json:"content"`
}

func (h *harness) serviceInfo() (serviceInfoView, error) {
	raw, err := serviceCall(h.svcPipe, map[string]any{"command": "info"}, 5*time.Second)
	if err != nil {
		return serviceInfoView{}, err
	}
	var resp serviceInfoView
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func (h *harness) waitContentReady(timeout time.Duration) (contentHealthView, time.Duration, error) {
	start := time.Now()
	deadline := time.Now().Add(timeout)
	for {
		if h.expired() || time.Now().After(deadline) {
			return contentHealthView{}, time.Since(start), errors.New("content index did not reach ready")
		}
		info, err := h.serviceInfo()
		if err == nil && info.Content != nil {
			c := *info.Content
			if c.State == "ready" || c.State == "degraded" {
				return c, time.Since(start), nil
			}
		}
		sleepCtx(h.ctx, 250*time.Millisecond)
	}
}

// runUIContentQuery sends one query, records action-to-first-response latency,
// then waits for the row count to settle. action is "type-query" or
// "search-now".
func (h *harness) runUIContentQuery(action, query string) (automationState, float64, error) {
	before, err := h.auto.State()
	if err != nil {
		return automationState{}, 0, err
	}
	start := time.Now()
	if err := h.auto.Action(action, query); err != nil {
		return automationState{}, 0, err
	}
	var firstLatency float64
	var last automationState
	lastRows := -1
	settled := time.Time{}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if h.expired() || time.Now().After(deadline) {
			if last.Seq == 0 {
				last, _ = h.auto.State()
			}
			return last, firstLatency, errors.New("query did not settle")
		}
		st, serr := h.auto.State()
		if serr == nil {
			if firstLatency == 0 && st.Seq > before.Seq && strings.EqualFold(st.Query, query) {
				firstLatency = millis(time.Since(start))
			}
			if st.Seq > before.Seq && strings.EqualFold(st.Query, query) {
				if st.Rows != lastRows {
					lastRows = st.Rows
					settled = time.Now()
				} else if time.Since(settled) >= 200*time.Millisecond {
					last = st
					return st, firstLatency, nil
				}
			}
		}
		sleepCtx(h.ctx, 20*time.Millisecond)
	}
}

func rowPaths(st automationState) []string {
	out := make([]string, 0, len(st.RowDetails))
	for _, r := range st.RowDetails {
		out = append(out, r.Path)
	}
	return out
}

func rowForPath(st automationState, path string) *automationRow {
	want := normalizeResultPath(path)
	for i := range st.RowDetails {
		if normalizeResultPath(st.RowDetails[i].Path) == want {
			return &st.RowDetails[i]
		}
	}
	return nil
}

func (h *harness) serviceSearchPaths(query string) ([]string, error) {
	raw, err := serviceCall(h.svcPipe, map[string]any{"command": "search", "query": query, "limit": 50}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Rows    []struct {
			Path string `json:"path"`
		} `json:"rows"`
		Results []string `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("service search: %s", resp.Message)
	}
	out := make([]string, 0, len(resp.Rows)+len(resp.Results))
	for _, r := range resp.Rows {
		out = append(out, r.Path)
	}
	if len(out) == 0 {
		out = append(out, resp.Results...)
	}
	return out, nil
}

func (h *harness) attributeContentFailure(query string, expected []string, uiActual []string) (string, *bool, []string) {
	svc, err := h.serviceSearchPaths(query)
	if err != nil {
		return "service-error: " + err.Error(), nil, nil
	}
	cmp := compareResultPaths(expected, svc, filepath.Join(h.out, "fixture"))
	pass := cmp.Pass
	attr := "service"
	if pass {
		attr = "ui"
	}
	return attr, &pass, svc
}

func (h *harness) contentScenario() *contentReport {
	rep := &contentReport{}
	start := time.Now()
	root := filepath.Join(h.out, "fixture")
	expected := h.contentSet
	if len(expected.Queries) == 0 {
		rep.Error = "content expectations missing"
		return rep
	}

	if err := h.auto.Action("set-contents-mode", true); err != nil {
		rep.Error = "set contents mode: " + err.Error()
		return rep
	}

	// 1. While the index builds, exercise the typed path and record the notice.
	if info, err := h.serviceInfo(); err == nil && info.Content != nil {
		if info.Content.State == "indexing" || info.Content.State == "reconciling" {
			rep.BuildingSeen = true
		}
	}
	// Give the status refresher one poll to publish the building state before the
	// early query, so the notice has a chance to appear.
	buildingDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(buildingDeadline) {
		if st, err := h.auto.State(); err == nil {
			if st.ContentState == "indexing" || st.ContentState == "reconciling" {
				rep.BuildingSeen = true
				break
			}
		}
		sleepCtx(h.ctx, 100*time.Millisecond)
	}
	earlyQuery := expected.Queries[0].Query
	st, _, _ := h.runUIContentQuery("type-query", earlyQuery)
	rep.BuildingRows = st.Rows
	rep.BuildingNotice = st.ResultsNotice
	rep.BuildingNoticeSeen = st.ResultsNotice != ""
	if rep.BuildingNoticeSeen {
		rep.BuildingScreenshot = filepath.Join(h.out, "shots", "content-building.png")
		_ = h.auto.Screenshot(rep.BuildingScreenshot)
	}

	// 2. Wait for the service-owned content build.
	health, _, err := h.waitContentReady(4 * time.Minute)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.ContentState = health.State
	rep.ContentDocs = health.Docs
	rep.ContentBuildError = health.BuildError
	rep.ContentBuildDone = health.BuildDone
	rep.ContentBuildTotal = health.BuildTotal
	rep.ContentReadyMS = millis(time.Since(start))

	// 3. Expected positive queries: typed path and SearchNow.
	for _, q := range expected.Queries {
		exp := expectedResultPaths(root, q.Expected)
		stTyped, typedMS, typedErr := h.runUIContentQuery("type-query", q.Query)
		stNow, nowMS, nowErr := h.runUIContentQuery("search-now", q.Query)
		stUse := stNow
		if nowErr != nil {
			stUse = stTyped
		}
		actual := rowPaths(stUse)
		cmp := compareResultPaths(exp, actual, root)
		qr := contentQueryReport{
			ID: q.ID, Query: q.Query, Expected: cmp.Expected, Actual: cmp.Actual,
			Missing: cmp.Missing, Extra: cmp.Extra, Pass: cmp.Pass, Rows: stUse.Rows,
			TypedLatencyMS: typedMS, NowLatencyMS: nowMS,
			Notice: stUse.ResultsNotice, SearchMessage: stUse.SearchMessage,
		}
		snippetNeedle := q.Snippet
		if snippetNeedle == "" {
			snippetNeedle = q.SnippetAlt
		}
		qr.SnippetPass = true
		for _, path := range exp {
			row := rowForPath(stUse, path)
			if row == nil {
				continue
			}
			if snippetNeedle != "" {
				qr.SnippetText = row.Snippet
				if !snippetContains(row.Snippet, snippetNeedle, false) {
					qr.SnippetPass = false
				}
			}
		}
		if !qr.Pass || !qr.SnippetPass {
			attr, svcPass, svcActual := h.attributeContentFailure(q.Query, exp, actual)
			qr.Attribution = attr
			qr.ServicePass = svcPass
			qr.ServiceActual = svcActual
		}
		if typedErr == nil {
			rep.Latency.Typed = append(rep.Latency.Typed, typedMS)
		}
		if nowErr == nil {
			rep.Latency.SearchNow = append(rep.Latency.SearchNow, nowMS)
		}
		rep.Queries = append(rep.Queries, qr)
	}
	rep.SnippetsScreenshot = filepath.Join(h.out, "shots", "content-snippets.png")
	_ = h.auto.Screenshot(rep.SnippetsScreenshot)

	// 4. Negative cases.
	for _, q := range expected.Negative {
		st, _, _ := h.runUIContentQuery("type-query", q.Query)
		actual := rowPaths(st)
		nr := contentNegativeReport{ID: q.ID, Query: q.Query, Rows: len(actual), Actual: actual, Pass: len(actual) == 0}
		if !nr.Pass {
			attr, svcPass, _ := h.attributeContentFailure(q.Query, nil, actual)
			nr.Attribution = attr
			nr.ServicePass = svcPass
		}
		rep.Negatives = append(rep.Negatives, nr)
		if q.ID == "binary" {
			rep.NoResultsScreenshot = filepath.Join(h.out, "shots", "content-no-results.png")
			_ = h.auto.Screenshot(rep.NoResultsScreenshot)
		}
	}

	// 5. Freshness: replace the old token with the new one on disk and poll.
	fr := &contentFreshnessReport{
		Path:        filepath.Join(root, filepath.FromSlash(expected.Freshness.Path)),
		QueryBefore: expected.Freshness.Query, QueryAfter: expected.Freshness.QueryNew,
	}
	freshStart := time.Now()
	if data, err := os.ReadFile(fr.Path); err != nil {
		fr.Attribution = "fixture-read: " + err.Error()
	} else if !strings.Contains(string(data), expected.Freshness.Before) {
		fr.Attribution = "fixture missing initial token"
	} else {
		data = []byte(strings.Replace(string(data), expected.Freshness.Before, expected.Freshness.After, 1))
		if err := os.WriteFile(fr.Path, data, 0o600); err != nil {
			fr.Attribution = "fixture-write: " + err.Error()
		}
	}
	if fr.Attribution == "" {
		deadline := time.Now().Add(60 * time.Second)
		for {
			stOld, _, _ := h.runUIContentQuery("type-query", fr.QueryBefore)
			fr.OldRows = stOld.Rows
			stNew, _, _ := h.runUIContentQuery("type-query", fr.QueryAfter)
			fr.NewRows = stNew.Rows
			fr.BeforePass = stOld.Rows == 0
			fr.AfterPass = rowForPath(stNew, fr.Path) != nil
			if fr.BeforePass && fr.AfterPass {
				break
			}
			if h.expired() || time.Now().After(deadline) {
				attr, _, _ := h.attributeContentFailure(fr.QueryAfter, []string{fr.Path}, rowPaths(stNew))
				fr.Attribution = attr
				break
			}
			sleepCtx(h.ctx, 500*time.Millisecond)
		}
		fr.ReflectMS = millis(time.Since(freshStart))
	}
	fr.Pass = fr.BeforePass && fr.AfterPass
	rep.Freshness = fr
	// Wait briefly for the post-freshness reconcile to return to ready, so the
	// recorded final state is not a transient build state.
	readyDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(readyDeadline) {
		if info, err := h.serviceInfo(); err == nil && info.Content != nil && info.Content.State == "ready" {
			break
		}
		sleepCtx(h.ctx, 200*time.Millisecond)
	}
	if info, err := h.serviceInfo(); err == nil && info.Content != nil {
		rep.ContentState = info.Content.State
		rep.ContentDocs = info.Content.Docs
		rep.ContentPartial = info.Content.Partial
		rep.ContentIncomplete = info.Content.Incomplete
		rep.ContentStale = info.Content.Stale
		rep.ContentBuildError = info.Content.BuildError
		rep.ContentBuildDone = info.Content.BuildDone
		rep.ContentBuildTotal = info.Content.BuildTotal
	}
	return rep
}
