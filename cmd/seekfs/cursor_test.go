package main

import (
	"reflect"
	"testing"
)

func TestSearchCursorRoundTripMismatchMalformed(t *testing.T) {
	opts := queryOptions{Query: "needle", Under: `C:\data`, MatchPath: true, Limit: 2}
	token := encodeSearchCursor(opts, 12)
	got, err := decodeSearchCursor(token, opts)
	if err != nil || got != 12 {
		t.Fatalf("round trip = %d, %v", got, err)
	}
	if _, err := decodeSearchCursor(token, queryOptions{Query: "other", Under: opts.Under, MatchPath: true}); err == nil || err.Error() != "cursor does not match this query" {
		t.Fatalf("mismatch error = %v", err)
	}
	if _, err := decodeSearchCursor("%%%", opts); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}

func TestNormalizeSearchArgsCursorFlag(t *testing.T) {
	got := normalizeSearchArgs([]string{"needle", "--cursor", "abc"})
	if !reflect.DeepEqual(got, []string{"--cursor", "abc", "needle"}) {
		t.Fatalf("normalized args = %#v", got)
	}
}

func TestServiceCommandSearchCursorNamePaging(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{{2, "a.txt", "x"}, {3, "b.txt", "x"}, {4, "c.txt", "x"}, {5, "d.txt", "x"}, {6, "e.txt", "x"}})
	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	query := "ext:txt"
	var all []string
	cursor := ""
	for page := 0; page < 3; page++ {
		r := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 2, Cursor: cursor})
		if !r.OK {
			t.Fatalf("page %d: %s", page, r.Message)
		}
		all = append(all, r.Results...)
		if page < 2 && r.NextCursor == "" {
			t.Fatalf("page %d missing next cursor", page)
		}
		if page == 2 && r.NextCursor != "" {
			t.Fatalf("last page has next cursor %q", r.NextCursor)
		}
		cursor = r.NextCursor
	}
	full := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: query, Limit: 10})
	if !reflect.DeepEqual(all, full.Results) {
		t.Fatalf("paged %v != full %v", all, full.Results)
	}
}

func TestServiceCommandSearchCursorContentPaging(t *testing.T) {
	vol := newContentQueryVolume(t, []contentFixtureFile{{2, "a.txt", "needle"}, {3, "b.txt", "needle"}, {4, "c.txt", "needle"}})
	s := &goSearchService{volumes: []*serviceVolumeIndex{vol}, indexes: []*Index{vol.index}}
	one := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 1})
	if !one.OK || one.NextCursor == "" {
		t.Fatalf("first page: %+v", one)
	}
	two := callTestServiceCommand(t, s, serviceRequest{Command: "search", Query: "content:needle", Limit: 1, Cursor: one.NextCursor})
	if !two.OK || len(two.Results) != 1 {
		t.Fatalf("second page: %+v", two)
	}
}
