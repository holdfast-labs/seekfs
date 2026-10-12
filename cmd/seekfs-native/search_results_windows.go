//go:build windows

package main

import (
	"strings"
	"syscall"
)

// search_results_windows.go: delivering Searcher results to the UI thread —
// sequence check, history push, row refresh, status update, unmatched-content
// error handling.

// deliverPendingResult runs on the UI thread via msgSearchResult.
func (a *app) deliverPendingResult() {
	a.mu.Lock()
	res := a.pendingResult
	a.pendingResult = nil
	a.mu.Unlock()
	if res == nil {
		return
	}
	a.mu.Lock()
	if res.Seq < a.seq {
		a.mu.Unlock()
		return // stale
	}
	a.seq = res.Seq
	if res.Err != nil {
		if contentUnavailable(res.Err) {
			a.contentUnavailableMsg = res.Message
			a.rows = nil
			a.sortedRows = nil
		} else {
			a.fuzzyMsg = res.Message
			a.rows = nil
			a.sortedRows = nil
		}
	} else if !res.OK {
		if contentUnavailableString(res.Message) {
			a.contentUnavailableMsg = res.Message
		} else {
			a.fuzzyMsg = res.Message
		}
		a.rows = nil
		a.sortedRows = nil
		a.content = res.Content
	} else {
		a.rows = res.Rows
		a.fuzzyMsg = res.Message
		a.elapsedMS = res.ElapsedMS
		a.searchMS = res.SearchMS
		a.source = res.Source
		a.content = res.Content
		a.contentUnavailableMsg = ""
		// Push history for committed (Enter) searches and settled live
		// searches alike — Back walks what the user saw.
		if strings.TrimSpace(res.Query) != "" {
			a.history.Push(HistoryEntry{Query: res.Query, Under: a.under, ContentsMode: res.ContentMode})
		}
	}
	a.mu.Unlock()
	a.refreshRows()
	a.refreshStatus()
	a.updateNavButtons()
}

func (a *app) onSearchResultDelivered() {
	_, _, _ = syscall.SyscallN(pSetFocus.Addr(), a.results)
}
