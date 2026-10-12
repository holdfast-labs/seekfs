package main

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ResultRow is one search hit as shown in the results list.
type ResultRow struct {
	Path     string
	Name     string
	Dir      string
	IsDir    bool
	Size     int64
	HasSize  bool
	Modified time.Time
	HasMod   bool
	Type     string // extension without dot, lowercased; "" for dirs
	Snippet  string
	Exists   bool
}

func rowFromJSON(row jsonResult) ResultRow {
	r := ResultRow{
		Path:    row.Path,
		Name:    row.Name,
		IsDir:   row.IsDir,
		Snippet: row.Snippet,
		Exists:  true,
	}
	if r.Name == "" {
		r.Name = filepath.Base(row.Path)
	}
	r.Dir = filepath.Dir(row.Path)
	if row.Size != nil {
		r.Size = *row.Size
		r.HasSize = true
	}
	if row.Modified != "" {
		if t, err := time.Parse(time.RFC3339Nano, row.Modified); err == nil {
			r.Modified = t
			r.HasMod = true
		} else if t, err := time.Parse(time.RFC3339, row.Modified); err == nil {
			r.Modified = t
			r.HasMod = true
		}
	}
	if r.IsDir {
		r.Type = ""
	} else {
		r.Type = strings.ToLower(strings.TrimPrefix(filepath.Ext(r.Name), "."))
	}
	if row.Exists != nil {
		r.Exists = *row.Exists
	}
	return r
}

func rowsFromServiceResponse(resp serviceResponse) []ResultRow {
	if len(resp.Rows) > 0 {
		out := make([]ResultRow, 0, len(resp.Rows))
		for _, row := range resp.Rows {
			out = append(out, rowFromJSON(row))
		}
		return out
	}
	out := make([]ResultRow, 0, len(resp.Results))
	for _, path := range resp.Results {
		out = append(out, ResultRow{
			Path: path,
			Name: filepath.Base(path),
			Dir:  filepath.Dir(path),
			Type: strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")),
		})
	}
	return out
}

// Sort columns for the Details view. SortKey values are persisted.
type SortKey int

const (
	SortName SortKey = iota
	SortFolder
	SortSize
	SortModified
	SortType
	// SortSnippet is the Match (content snippet) column. It is display-only:
	// clicking its header keeps the current sort.
	SortSnippet
)

func (k SortKey) String() string {
	switch k {
	case SortFolder:
		return "folder"
	case SortSize:
		return "size"
	case SortModified:
		return "modified"
	case SortType:
		return "type"
	case SortSnippet:
		return "match"
	default:
		return "name"
	}
}

func sortKeyFromString(s string) SortKey {
	switch strings.ToLower(s) {
	case "folder":
		return SortFolder
	case "size":
		return SortSize
	case "modified":
		return SortModified
	case "type":
		return SortType
	default:
		return SortName
	}
}

// sortRows sorts rows in place: directories first for name/type sorts (like
// Explorer), then the column key, with path as the final tiebreak so the
// order is deterministic.
func sortRows(rows []ResultRow, key SortKey, ascending bool) {
	less := func(a, b ResultRow) bool {
		switch key {
		case SortFolder:
			if d := strings.Compare(strings.ToLower(a.Dir), strings.ToLower(b.Dir)); d != 0 {
				return d < 0
			}
			return compareRowsByName(a, b) < 0
		case SortSize:
			as, bs := effectiveSize(a), effectiveSize(b)
			if as != bs {
				return as < bs
			}
			return compareRowsByName(a, b) < 0
		case SortModified:
			am, bm := effectiveMod(a), effectiveMod(b)
			if !am.Equal(bm) {
				return am.After(bm) // newest first reads naturally; flipped below when ascending
			}
			return compareRowsByName(a, b) < 0
		case SortType:
			if a.IsDir != b.IsDir {
				return a.IsDir
			}
			if d := strings.Compare(a.Type, b.Type); d != 0 {
				return d < 0
			}
			return compareRowsByName(a, b) < 0
		default: // SortName
			if a.IsDir != b.IsDir {
				return a.IsDir
			}
			return compareRowsByName(a, b) < 0
		}
	}
	// Modified is newest-first by default in Explorer; the header click flips
	// it, so invert the sense for that column only.
	if key == SortModified {
		ascending = !ascending
	}
	if ascending {
		sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
	} else {
		sort.SliceStable(rows, func(i, j int) bool { return less(rows[j], rows[i]) })
	}
}

// effectiveSize ranks directories by 0 (their stored size) — the service
// already folds subtree totals into dir sizes for sort:size, but for
// client-side re-sorts the stored value is what we have.
func effectiveSize(r ResultRow) int64 {
	if !r.HasSize {
		return -1
	}
	return r.Size
}

func effectiveMod(r ResultRow) time.Time {
	return r.Modified
}

func compareRowsByName(a, b ResultRow) int {
	if d := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); d != 0 {
		return d
	}
	if d := strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)); d != 0 {
		return d
	}
	return strings.Compare(a.Path, b.Path)
}

// Group groups rows by folder for grouped-by-folder mode, preserving each
// group's first-seen order.
type Group struct {
	Dir  string
	Rows []int // indices into the row slice
}

func groupByFolder(rows []ResultRow) []Group {
	var groups []Group
	index := map[string]int{}
	for i, r := range rows {
		if g, ok := index[r.Dir]; ok {
			groups[g].Rows = append(groups[g].Rows, i)
		} else {
			index[r.Dir] = len(groups)
			groups = append(groups, Group{Dir: r.Dir, Rows: []int{i}})
		}
	}
	return groups
}

// DBVolume describes one loaded index from the service info response, for
// the sidebar's volume list.
type DBVolume struct {
	Path    string
	Volume  string
	Entries int
	State   string
}

func dbVolumesFrom(resp serviceResponse) []DBVolume {
	out := make([]DBVolume, 0, len(resp.DBs))
	for _, db := range resp.DBs {
		out = append(out, DBVolume{
			Path:    db.Path,
			Volume:  db.Volume,
			Entries: db.Entries,
			State:   db.State,
		})
	}
	return out
}

// contentStatusText describes the content index behind a content query,
// mirroring contentStatusText in the Wails frontend.
func contentStatusText(c *contentState) string {
	if c == nil || c.State == "" {
		return ""
	}
	if c.State == contentStateIndexing {
		pct := 0
		if c.BuildTotal > 0 {
			pct = (100 * c.BuildDone) / c.BuildTotal
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		return itoa(pct) + "% indexing contents"
	}
	if err := firstNonEmpty(c.BuildError, c.FoldError); err != "" {
		return "Contents: " + err
	}
	if c.Incomplete || c.Partial {
		return "Contents: partial results"
	}
	if c.State == contentStateReady {
		if c.Docs > 0 {
			return formatCount(c.Docs) + " files indexed"
		}
		return ""
	}
	return "Contents " + c.State
}

// contentUnavailable reports whether err is the service's content-unavailable
// refusal (no usable content index), as opposed to a real search failure.
func contentUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "content") &&
		(strings.Contains(msg, "unavailable") || strings.Contains(msg, "disabled") ||
			strings.Contains(msg, "not built") || strings.Contains(msg, "still building"))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func formatCount(n int) string {
	s := itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}
