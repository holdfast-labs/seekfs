//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// oldAccumulateMFT is the pre-compact map accumulation, kept as the reference
// implementation for the equivalence test.
func oldAccumulateMFT(records []synthOpts) map[uint64]mftEntry {
	out := make(map[uint64]mftEntry, len(records))
	for i, opts := range records {
		rec := synthMFTRecord(opts)
		if entry, ok := parseMFTRecord(rec, uint64(i+1), 512); ok {
			out[entry.frn] = entry
		}
	}
	return out
}

// oldFilterMFTExclusions is the former map[uint64][]uint64 child-index filter.
func oldFilterMFTExclusions(entries map[uint64]mftEntry, vol string, exclusions []string) int {
	if len(exclusions) == 0 || len(entries) == 0 {
		return 0
	}
	children := make(map[uint64][]uint64, len(entries)/2)
	for frn, e := range entries {
		if e.parentFRN != 0 && e.parentFRN != frn {
			children[e.parentFRN] = append(children[e.parentFRN], frn)
		}
	}
	var roots []uint64
	volRoot := strings.ToUpper(strings.TrimRight(vol, `\`)) + `\`
	for _, exclusion := range exclusions {
		canonical, err := filepath.Abs(exclusion)
		if err != nil {
			continue
		}
		canonical = filepath.Clean(canonical)
		rel, err := filepath.Rel(volRoot, canonical)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		components := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
		cur := uint64(mftRootFRN)
		resolved := true
		for _, component := range components {
			var next uint64
			for _, frn := range children[cur] {
				if strings.EqualFold(entries[frn].name, component) {
					next = frn
					break
				}
			}
			if next == 0 {
				resolved = false
				break
			}
			cur = next
		}
		if resolved {
			roots = append(roots, cur)
		}
	}
	if len(roots) == 0 {
		return 0
	}
	excluded := make(map[uint64]bool, len(roots))
	queue := roots
	for len(queue) > 0 {
		frn := queue[0]
		queue = queue[1:]
		if excluded[frn] {
			continue
		}
		excluded[frn] = true
		queue = append(queue, children[frn]...)
	}
	removed := 0
	for frn := range excluded {
		if _, ok := entries[frn]; ok {
			delete(entries, frn)
			removed++
		}
	}
	return removed
}

func mftEntriesEqual(a, b mftEntry) bool {
	return a.frn == b.frn && a.parentFRN == b.parentFRN && a.name == b.name &&
		a.attr == b.attr && a.size == b.size && a.modUnix == b.modUnix &&
		a.isDir == b.isDir && a.inUse == b.inUse && a.seq == b.seq && a.parentSeq == b.parentSeq
}

func TestMFTTableAccumulationMatchesMap(t *testing.T) {
	records := make([]synthOpts, 0, 512)
	for i := 0; i < 500; i++ {
		isDir := i%13 == 0
		var size int64
		if !isDir {
			size = int64(i % 100_000)
		}
		name := fmt.Sprintf("entry-%04d%s", i, []string{".txt", ".bin", ".go"}[i%3])
		opts := synthOpts{
			inUse:       true,
			isDir:       isDir,
			name:        name,
			namespace:   fileNameWin32,
			parentFRN:   uint64(1 + i/8),
			modUnixNano: int64(1_700_000_000_000_000_000 + int64(i)*1_000_000),
			size:        size,
			nonResident: !isDir,
		}
		if i == 7 {
			// Win32 long name plus the DOS alias: the parser must keep Win32.
			opts.name = "long-hardlink-name.txt"
			opts.altName = "LONGHA~1.TXT"
			opts.altNamespace = fileNameDOS
		}
		if i == 9 {
			// DOS name first, Win32 long name second: Win32 must win.
			opts.name = "DOSNAME~1.TXT"
			opts.namespace = fileNameDOS
			opts.altName = "second-hardlink-name.txt"
			opts.altNamespace = fileNameWin32
		}
		records = append(records, opts)
	}

	oldEntries := oldAccumulateMFT(records)
	newEntries := newMFTTable(uint64(len(records)))
	for i, opts := range records {
		rec := synthMFTRecord(opts)
		if entry, ok := parseMFTRecord(rec, uint64(i+1), 512); ok {
			newEntries.add(entry)
		}
	}
	if newEntries.count != len(oldEntries) {
		t.Fatalf("new count = %d, old count = %d", newEntries.count, len(oldEntries))
	}
	for frn, want := range oldEntries {
		got, ok := newEntries.get(frn)
		if !ok {
			t.Fatalf("frn %d missing from compact table", frn)
		}
		if !mftEntriesEqual(got, want) {
			t.Fatalf("frn %d: compact %+v != map %+v", frn, got, want)
		}
	}
}

func TestMFTTableExclusionFilterMatchesMap(t *testing.T) {
	records := []synthOpts{
		{inUse: true, isDir: true, name: ".", parentFRN: 5},
		{inUse: true, isDir: true, name: "ProgramData", parentFRN: 5},
		{inUse: true, isDir: true, name: "seekfs", parentFRN: 6},
		{inUse: true, name: "seekfs.toml", parentFRN: 7, size: 10},
		{inUse: true, isDir: true, name: "indexes", parentFRN: 7},
		{inUse: true, name: "seekfs_c.gsi", parentFRN: 9, size: 20},
		{inUse: true, name: "keep.txt", parentFRN: 5, size: 30},
		{inUse: true, isDir: true, name: "System Volume Information", parentFRN: 5},
		{inUse: true, isDir: true, name: "{GUID}", parentFRN: 8},
	}
	// FRNs 5..13, matching the record order.
	frns := []uint64{5, 6, 7, 8, 9, 10, 11, 12, 13}
	oldEntries := make(map[uint64]mftEntry, len(records))
	table := newMFTTable(14)
	for i, opts := range records {
		rec := synthMFTRecord(opts)
		entry, ok := parseMFTRecord(rec, frns[i], 512)
		if !ok {
			t.Fatalf("record %d did not parse", i)
		}
		oldEntries[frns[i]] = entry
		table.add(entry)
	}
	exclusions := []string{
		`C:\ProgramData\seekfs`,
		`C:\System Volume Information`,
		`D:\Other-Volume\seekfs`,
		`C:\NoSuch\Thing`,
	}
	oldRemoved := oldFilterMFTExclusions(oldEntries, `C:`, exclusions)
	newRemoved := filterMFTExclusions(table, `C:`, exclusions)
	if oldRemoved != newRemoved {
		t.Fatalf("removed old=%d new=%d", oldRemoved, newRemoved)
	}
	if table.count != len(oldEntries) {
		t.Fatalf("table count = %d, map count = %d", table.count, len(oldEntries))
	}
	for frn, want := range oldEntries {
		got, ok := table.get(frn)
		if !ok {
			t.Fatalf("frn %d present in map but not table", frn)
		}
		if !mftEntriesEqual(got, want) {
			t.Fatalf("frn %d after filter: compact %+v != map %+v", frn, got, want)
		}
	}
}
