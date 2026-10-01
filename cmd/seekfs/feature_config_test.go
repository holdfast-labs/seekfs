package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFeatureConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seekfs.toml")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	text := "[features.content]\nenabled = true\n[features.tags]\nenabled = true\ncommand = '" + exe + "'\nargs = ['a,b', \"c\\\"d\", 'C:\\work']\ntimeout_seconds = 3\n[content]\nmode = 'explicit'\nroots = ['C:\\work']\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Features["content"].Enabled || !reflect.DeepEqual(cfg.Features["tags"].Args, []string{"a,b", `c"d`, `C:\work`}) || cfg.Content.Mode != "explicit" {
		t.Fatalf("config = %+v", cfg)
	}
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	if featureContentEnabled(cfg) {
		t.Fatal("environment disable must override config")
	}
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	if !featureContentEnabled(appConfig{}) {
		t.Fatal("legacy environment opt-in stopped working")
	}
	for _, bad := range []string{
		"[features.BAD]\nenabled = true", "[features.tags]\nenabled = maybe", "[features.tags]\nenabled = true\ncommand = 'tags.exe'",
		"[features.content]\ncommand = 'content.exe'", "[features.tags]\ntimeout_seconds = 0", "[features.tags]\nargs = ['unterminated]",
	} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Errorf("accepted invalid config %q", bad)
		}
	}
}

func TestFeatureQueryParsing(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "1")
	for _, query := range []string{`feature:tags:blue`, `feature:tags:"two words"`, `!feature:tags:blue`, `ext:go|feature:tags:blue`, `feature:tags:"a|b"`, `ext:go|feature:tags:/a|b/`, `content:/a|b/|feature:tags:"two words"`} {
		pq, err := parseQuery(queryOptions{Query: query})
		if err != nil || !queryHasFeatureLeaf(pq) {
			t.Errorf("parse(%q): %+v, %v", query, pq, err)
		}
	}
	for _, query := range []string{`feature:tags:`, `feature:content:x`, `feature:tags:"unterminated`} {
		if _, err := parseQuery(queryOptions{Query: query}); err == nil {
			t.Errorf("accepted %q", query)
		}
	}
	if _, err := searchAll(nil, queryOptions{Query: "feature:tags:blue"}, false); err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("offline feature query: %v", err)
	}
	pq, err := parseQuery(queryOptions{Query: `foo feature:tags:"term folder/name"`})
	if err != nil || pq.MatchPath {
		t.Fatalf("opaque feature term changed filename matching: %+v, %v", pq, err)
	}
}

func TestFeatureTokenDetectionIgnoresContentLiterals(t *testing.T) {
	for _, q := range []string{`content:"has feature:tags:blue"`, `content:/word|feature:tags:x/`, `regex:feature:tags:x`, `C:\folder\feature:tags:x`} {
		if queryHasFeatureToken(q) {
			t.Errorf("literal content dispatched as a feature: %q", q)
		}
	}
	for _, q := range []string{`feature:tags:"has content:needle"`, `foo|feature:tags:/a|b/`, `content:/a|b/|feature:tags:blue`} {
		if !queryHasFeatureToken(q) {
			t.Errorf("feature predicate not detected: %q", q)
		}
	}
}

func TestDisabledFeaturesHaveNoWorkersOrFeed(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "0")
	s := &goSearchService{stop: make(chan struct{})}
	vol := newServiceVolumeIndex("disabled.gsi", &Index{Source: "usn", Volume: "C:", Compact: true})
	s.prepareFeatureVolume(vol)
	defer s.signalServiceStop()
	if vol.content != nil || vol.contentCoord != nil || vol.featureFeed != nil || len(s.features.Load().companions) != 0 {
		t.Fatal("disabled feature allocated per-volume state/workers")
	}
}

func TestContentFeatureConfigOptIn(t *testing.T) {
	t.Setenv("SEEKFS_CONTENT_SEARCH", "")
	if err := os.Unsetenv("SEEKFS_CONTENT_SEARCH"); err != nil {
		t.Fatal(err)
	}
	previous := serviceContentSelection.Load()
	t.Cleanup(func() { serviceContentSelection.Store(previous) })
	s := &goSearchService{stop: make(chan struct{}), contentCfg: appConfig{Features: map[string]featureConfig{"content": {Enabled: true}}}}
	vol := newServiceVolumeIndex("configured.gsi", &Index{Source: "usn", Volume: "C:", Compact: true})
	s.prepareFeatureVolume(vol)
	defer s.signalServiceStop()
	if vol.content == nil || vol.contentCoord == nil || !contentSearchEnabled() {
		t.Fatal("config did not enable built-in content")
	}
	if _, err := parseQuery(queryOptions{Query: "content:needle"}); err != nil {
		t.Fatal(err)
	}
}
