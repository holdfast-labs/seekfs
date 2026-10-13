package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMain keeps the suite hermetic: no test may read or write the machine's
// real %ProgramData%\seekfs (config, plugin.toml, indexes, spools, logs).
// Before the fix, `go test ./cmd/seekfs/` appended test output to the real
// C:\ProgramData\seekfs\service.log (133 MB, mostly lines like
// `seekfs-test-nonexistent-volume` and content builds of temp dirs) and
// created a real `gram-spool` directory, because serviceLog resolved
// %ProgramData% directly and the name-gram spool defaulted under it.
//
// The redirect: SEEKFS_DIR (honored by defaultSeekFSDir, hence by
// serviceLog, the config/plugin paths, and nameGramSpoolDir) plus
// SEEKFS_NAME_GRAM_SPOOL_DIR and ProgramData itself all point into a fresh
// temp root for the whole test process. Per-test t.Setenv overrides compose
// normally on top.
//
// The guard: a live seekfs service may be running on this machine (it appends
// to the real service.log on its own cadence), so the check cannot demand a
// byte-identical directory. Instead it fails loudly on test-attributable
// evidence: any new top-level entry in the real directory, or any appended
// service.log bytes that reference this test run (the temp root, the fake
// ProgramData, or known historical test markers).
func TestMain(m *testing.M) {
	// Service-build tests write small sidecars into temp dirs; the production
	// free-space reserve would fail them on machines with little free disk.
	// Tests that exercise the reserve set it explicitly.
	contentDiskReserveBytes = 0

	realDir := ""
	if base, ok := os.LookupEnv("ProgramData"); ok && strings.TrimSpace(base) != "" {
		realDir = filepath.Join(base, "seekfs")
	}
	beforeEntries := listTopLevelNames(realDir)
	beforeLogSize := fileSizeIfExists(filepath.Join(realDir, "service.log"))

	testRoot, err := os.MkdirTemp("", "seekfs-test-home-*")
	if err != nil {
		os.Stderr.WriteString("TestMain: cannot create temp test root: " + err.Error() + "\n")
		os.Exit(1)
	}
	// Explicit removal before os.Exit below (defers never run on Exit).
	fakeProgramData := filepath.Join(testRoot, "ProgramData")
	seekfsDir := filepath.Join(testRoot, "seekfs")
	spoolDir := filepath.Join(seekfsDir, "gram-spool")
	for _, dir := range []string{fakeProgramData, seekfsDir, spoolDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			os.Stderr.WriteString("TestMain: cannot create " + dir + ": " + err.Error() + "\n")
			os.Exit(1)
		}
	}
	// Setenv (not t.Setenv: TestMain has no *testing.T). The process-wide
	// ProgramData override is deliberate defense in depth: any code that still
	// resolves %ProgramData% directly lands in the temp root instead of the
	// machine's real directory.
	os.Setenv("SEEKFS_DIR", seekfsDir)
	os.Setenv("SEEKFS_NAME_GRAM_SPOOL_DIR", spoolDir)
	os.Setenv("ProgramData", fakeProgramData)

	code := m.Run()

	failed := false
	failf := func(format string, args ...any) {
		failed = true
		os.Stderr.WriteString("TEST HERMETICITY GUARD: " + fmt.Sprintf(format, args...) + "\n")
	}
	if got := os.Getenv("SEEKFS_DIR"); got != seekfsDir {
		failf("SEEKFS_DIR changed during the suite: now %q; the redirect must stay active", got)
	}
	if realDir != "" {
		afterEntries := listTopLevelNames(realDir)
		for _, name := range afterEntries {
			if !containsString(beforeEntries, name) { // planner_bench_test.go

				failf("new entry %q appeared in the real %s during tests; tests must stay under the temp root", name, realDir)
			}
		}
		logPath := filepath.Join(realDir, "service.log")
		if afterSize := fileSizeIfExists(logPath); afterSize > beforeLogSize && beforeLogSize >= 0 {
			if tail, err := readFileRange(logPath, beforeLogSize, afterSize); err != nil {
				failf("cannot read appended bytes of %s: %v", logPath, err)
			} else if marker := findTestMarker(tail, []string{testRoot, fakeProgramData, seekfsDir, "seekfs-test-nonexistent-volume", "seekfs-test-home-"}); marker != "" {
				failf("real %s grew by %d bytes with test-attributable content (marker %q); serviceLog must stay under the temp root", logPath, afterSize-beforeLogSize, marker)
			}
		}
	}

	// Best-effort cleanup of the temp root (defers never run on Exit).
	_ = os.RemoveAll(testRoot)
	if failed {
		os.Stderr.WriteString("TEST HERMETICITY GUARD: failing the suite\n")
		os.Exit(1)
	}
	os.Exit(code)
}

func listTopLevelNames(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func fileSizeIfExists(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func readFileRange(path string, from, to int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, to-from)
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil {
			break
		}
	}
	return buf[:n], nil
}

func findTestMarker(haystack []byte, markers []string) string {
	text := string(haystack)
	for _, marker := range markers {
		if marker != "" && strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

