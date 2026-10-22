package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sidebarHasVolume(u *UI, volume string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, r := range u.sideRows {
		if r.kind == sideScope && strings.Contains(r.label, volume) {
			return true
		}
	}
	return false
}

func TestStatusRefresherLoadingToReady(t *testing.T) {
	u := testUI(t, false)
	var infoReady atomic.Bool
	var searchCalls atomic.Int32
	u.client.exchange = func(req serviceRequest) (serviceResponse, error) {
		switch req.Command {
		case "info":
			if !infoReady.Load() {
				return serviceResponse{OK: true, Loading: true}, nil
			}
			return serviceResponse{
				OK: true, Health: serviceHealthOK, Entries: 120,
				DBs: []dbInfo{{Volume: "X:", Entries: 120, State: "ready"}},
			}, nil
		case "search":
			searchCalls.Add(1)
			return serviceResponse{OK: true, Rows: []jsonResult{{Path: `X:\a.txt`, Name: "a.txt"}}}, nil
		}
		return serviceResponse{OK: true}, nil
	}
	u.mu.Lock()
	u.query = "needle"
	u.mu.Unlock()

	change := u.refreshStatusOnce()
	if u.health != serviceHealthDegraded || !strings.Contains(u.healthMsg, "Loading indexes") {
		t.Fatalf("loading status = %q/%q", u.health, u.healthMsg)
	}
	if change.RerunQuery() {
		t.Fatal("loading transition must not rerun before a finish or volume change")
	}
	if sidebarHasVolume(u, "X:") {
		t.Fatal("sidebar listed X: before the volume loaded")
	}

	infoReady.Store(true)
	change = u.refreshStatusOnce()
	if !change.VolumesChanged || !change.RerunQuery() {
		t.Fatalf("ready change = %+v", change)
	}
	if u.health != serviceHealthOK || u.entries != 120 {
		t.Fatalf("ready status = %q entries=%d", u.health, u.entries)
	}
	if !sidebarHasVolume(u, "X:") {
		t.Fatalf("sidebar did not list X: after ready: %+v", u.sideRows)
	}
	deadline := time.Now().Add(3 * time.Second)
	for searchCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if searchCalls.Load() == 0 {
		t.Fatal("volume change did not trigger a re-query")
	}
}

func TestStatusRefresherLoadingFinishedReruns(t *testing.T) {
	u := testUI(t, false)
	var loading atomic.Bool
	loading.Store(true)
	var searchCalls atomic.Int32
	u.client.exchange = func(req serviceRequest) (serviceResponse, error) {
		switch req.Command {
		case "info":
			return serviceResponse{
				OK: true, Loading: loading.Load(), Entries: 50,
				DBs: []dbInfo{{Volume: "X:", Entries: 50, State: "ready"}},
			}, nil
		case "search":
			searchCalls.Add(1)
			return serviceResponse{OK: true}, nil
		}
		return serviceResponse{OK: true}, nil
	}
	u.mu.Lock()
	u.query = "doc"
	u.mu.Unlock()
	u.refreshStatusOnce()
	loading.Store(false)
	change := u.refreshStatusOnce()
	if !change.LoadingFinished || !change.RerunQuery() {
		t.Fatalf("finish change = %+v", change)
	}
	deadline := time.Now().Add(3 * time.Second)
	for searchCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if searchCalls.Load() == 0 {
		t.Fatal("loading finish did not trigger a re-query")
	}
}

func TestStatusRefreshIntervalSelection(t *testing.T) {
	cases := []struct {
		name      string
		health    string
		reachable bool
		building  bool
		want      time.Duration
	}{
		{"unreachable", serviceHealthError, false, false, time.Second},
		{"loading", serviceHealthDegraded, true, false, time.Second},
		{"empty", serviceHealthEmpty, true, false, time.Second},
		{"error", serviceHealthError, true, false, time.Second},
		{"unknown", "", true, false, time.Second},
		{"building", serviceHealthOK, true, true, 2 * time.Second},
		{"idle", serviceHealthOK, true, false, 10 * time.Second},
	}
	for _, tc := range cases {
		if got := statusRefreshInterval(tc.health, tc.reachable, tc.building); got != tc.want {
			t.Fatalf("%s interval = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestStatusRefresherUnreachableDoesNotSpawn(t *testing.T) {
	prev := nativeServiceSpawnHook
	var spawns atomic.Int32
	nativeServiceSpawnHook = func(serviceSpawnSpec) error {
		spawns.Add(1)
		return nil
	}
	defer func() { nativeServiceSpawnHook = prev }()

	u := testUI(t, false)
	u.client.exchange = func(serviceRequest) (serviceResponse, error) {
		return serviceResponse{}, errors.New("pipe missing")
	}
	u.refreshStatusOnce()
	if spawns.Load() != 0 {
		t.Fatalf("status refresher spawned %d service(s), want 0", spawns.Load())
	}
	if u.health != serviceHealthError {
		t.Fatalf("unreachable health = %q", u.health)
	}
}

func TestStatusRefresherNoInvalidateWhenUnchanged(t *testing.T) {
	u := testUI(t, false)
	var invalidations atomic.Int32
	u.invalidate = func() { invalidations.Add(1) }
	u.client.exchange = func(req serviceRequest) (serviceResponse, error) {
		return serviceResponse{
			OK: true, Health: serviceHealthOK, Entries: 12,
			DBs: []dbInfo{{Volume: "X:", Entries: 12, State: "ready"}},
		}, nil
	}
	u.refreshStatusOnce()
	first := invalidations.Load()
	if first == 0 {
		t.Fatal("first status apply did not invalidate")
	}
	u.refreshStatusOnce()
	if invalidations.Load() != first {
		t.Fatalf("unchanged status invalidated %d more time(s)", invalidations.Load()-first)
	}
}
