//go:build uiautomation

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// uiauto_fixture.go: the test-only service fixture. SEEKFS_UIDRIVE_FIXTURE
// names a JSON file that supplies synthetic volumes and a directory to walk
// for each volume build. Nothing here runs unless that variable is set on a
// standalone service built with the uiautomation tag.

const uiDriveFixtureEnv = "SEEKFS_UIDRIVE_FIXTURE"

type uiDriveFixture struct {
	Volumes []uiDriveFixtureVolume `json:"volumes"`
}

type uiDriveFixtureVolume struct {
	Volume          string `json:"volume"`
	Label           string `json:"label,omitempty"`
	FS              string `json:"fs,omitempty"`
	TotalBytes      uint64 `json:"total_bytes,omitempty"`
	FreeBytes       uint64 `json:"free_bytes,omitempty"`
	Root            string `json:"root"`
	PerEntryDelayMS int    `json:"per_entry_delay_ms,omitempty"`
}

var uiDriveFixtureData *uiDriveFixture

var (
	fixtureWatchMu sync.Mutex
	fixtureWatched map[*serviceVolumeIndex]bool
)

func uiDriveFixtureActive() bool { return uiDriveFixtureData != nil }

// installUIAutoFixtureService loads the fixture and replaces the settings
// seams with synthetic equivalents. It is called by cmdService before the
// pipe listener starts.
func installUIAutoFixtureService() {
	path := strings.TrimSpace(os.Getenv(uiDriveFixtureEnv))
	if path == "" {
		return
	}
	fixture, err := loadUIDriveFixture(path)
	if err != nil {
		serviceLog("uidrive fixture: %v", err)
		return
	}
	uiDriveFixtureData = fixture
	settingsVolumeEnumeration = fixture.enumerateVolumes
	serviceIndexUSNVolume = func(volume string, progress *volumeBuildProgress) (*Index, error) {
		return buildUIDriveFixtureIndex(fixture, volume, progress)
	}
	// The fixture runs on a nearly full test volume. Report the content disk
	// reserve as satisfied so a small fixture build can write its sidecar under
	// E:\bench without touching the machine's real free-space policy.
	baseFreeBytes := contentFreeBytesFn
	contentFreeBytesFn = func(path string) (uint64, error) {
		free, err := baseFreeBytes(path)
		if err != nil {
			return free, err
		}
		return free + (64 << 30), nil
	}
	// A settings-added fixture volume is a walk index with no USN journal. Start
	// the same walk watcher a startup walk volume gets, so edits reach the real
	// filename-rebuild plus content-reconcile path.
	featureVolumeReadyHook = func(s *goSearchService, vol *serviceVolumeIndex) {
		if !uiDriveFixtureActive() || s == nil || vol == nil || vol.index == nil || vol.index.Source != "walk" {
			return
		}
		fixtureWatchMu.Lock()
		if fixtureWatched == nil {
			fixtureWatched = make(map[*serviceVolumeIndex]bool)
		}
		first := !fixtureWatched[vol]
		fixtureWatched[vol] = true
		fixtureWatchMu.Unlock()
		if first {
			s.startWalkWatchers(vol)
		}
	}
}

func loadUIDriveFixture(path string) (*uiDriveFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseUIDriveFixture(data)
}

func parseUIDriveFixture(data []byte) (*uiDriveFixture, error) {
	var fixture uiDriveFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return nil, fmt.Errorf("fixture JSON: %w", err)
	}
	if len(fixture.Volumes) == 0 {
		return nil, fmt.Errorf("fixture has no volumes")
	}
	for i := range fixture.Volumes {
		v := &fixture.Volumes[i]
		v.Volume = normalizeSettingsVolume(v.Volume)
		if v.Volume == "" {
			return nil, fmt.Errorf("fixture volume %d: volume is required", i)
		}
		if strings.TrimSpace(v.Root) == "" {
			return nil, fmt.Errorf("fixture volume %s: root is required", v.Volume)
		}
		if v.FS == "" {
			v.FS = "NTFS"
		}
		if v.PerEntryDelayMS < 0 {
			v.PerEntryDelayMS = 0
		}
	}
	return &fixture, nil
}

func (f *uiDriveFixture) volume(name string) *uiDriveFixtureVolume {
	want := normalizeSettingsVolume(name)
	for i := range f.Volumes {
		if strings.EqualFold(normalizeSettingsVolume(f.Volumes[i].Volume), want) {
			return &f.Volumes[i]
		}
	}
	return nil
}

func (f *uiDriveFixture) enumerateVolumes() []settingsVolumeInfo {
	out := make([]settingsVolumeInfo, 0, len(f.Volumes))
	serviceLog("uidrive fixture enumerate volumes=%d", len(f.Volumes))
	for _, v := range f.Volumes {
		out = append(out, settingsVolumeInfo{
			Volume:     v.Volume,
			FS:         v.FS,
			Label:      v.Label,
			TotalBytes: v.TotalBytes,
			FreeBytes:  v.FreeBytes,
		})
	}
	return out
}

func fixtureCapabilities(caps serviceCapabilities) serviceCapabilities {
	if uiDriveFixtureActive() && !caps.Remote {
		caps.Mutate = true
	}
	return caps
}

// buildUIDriveFixtureIndex walks the fixture root and returns a compact index
// shaped like a USN/walk build. It sleeps PerEntryDelayMS per entry so a
// settings apply that starts a build takes a predictable time.
func buildUIDriveFixtureIndex(f *uiDriveFixture, volume string, progress *volumeBuildProgress) (*Index, error) {
	v := f.volume(volume)
	if v == nil {
		return nil, fmt.Errorf("fixture has no volume %s", volume)
	}
	root, err := filepath.Abs(v.Root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fixture root is not a directory: %s", root)
	}

	idx := &Index{
		Version:      indexVersion,
		Roots:        []string{root},
		BuiltAt:      time.Now(),
		Source:       "walk",
		Volume:       v.Volume,
		Compact:      true,
		CompactAttrs: true,
	}
	idx.Records = []CompactRecord{{
		FRN: 1, ParentFRN: 1, Parent: -1, Name: ".",
		Mode: uint32(os.ModeDir), ModUnix: info.ModTime().UnixNano(),
	}}
	idByPath := map[string]int{root: 0}
	nextFRN := uint64(2)
	delay := time.Duration(v.PerEntryDelayMS) * time.Millisecond
	total := uint64(0)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && path != root {
			total++
		}
		return nil
	})
	progress.setPhase("building")
	progress.setCounts(0, total)
	done := uint64(0)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}
		parentID, ok := idByPath[filepath.Dir(path)]
		if !ok {
			parentID = 0
		}
		entryInfo, err := d.Info()
		if err != nil {
			return nil
		}
		mode := uint32(0)
		if d.IsDir() {
			mode = uint32(os.ModeDir)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       nextFRN,
			ParentFRN: idx.Records[parentID].FRN,
			Parent:    int32(parentID),
			Name:      d.Name(),
			Mode:      mode,
			Size:      entryInfo.Size(),
			ModUnix:   entryInfo.ModTime().UnixNano(),
		})
		idByPath[path] = len(idx.Records) - 1
		nextFRN++
		done++
		if done%16 == 0 {
			progress.setDone(done)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	progress.setDone(done)
	progress.setPhase("writing")
	return idx, nil
}
