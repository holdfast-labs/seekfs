package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"seekfs/feature"
)

// Features own their workers and storage; the service owns selection, lifecycle
// and the filename/change boundary. The registry is immutable after startup.
type serviceFeatures struct {
	content    bool
	configs    map[string]featureConfig
	companions map[string]*companionFeature
}

type featureHealth struct {
	Progress *feature.Progress `json:"progress,omitempty"`
	Name     string            `json:"name"`
	Enabled  bool              `json:"enabled"`
	State    string            `json:"state"`
	Error    string            `json:"error,omitempty"`
}

func (s *goSearchService) initializeFeatures() {
	s.featuresOnce.Do(func() {
		s.pluginSettings.Store(&pluginSettings{Definitions: s.contentCfg.Plugins})
		enabled := featureContentEnabled(s.contentCfg)
		configs := configuredFeatureMap(s.contentCfg, s.contentCfg.Plugins)
		configured := configs["content"].Enabled
		serviceContentSelection.Store(&configured)
		s.setPluginContentSelection(s.contentCfg.Plugins)
		f := &serviceFeatures{content: enabled, configs: configs, companions: make(map[string]*companionFeature)}
		for name, cfg := range configs {
			if name != "content" && cfg.Enabled {
				f.companions[name] = newCompanionFeature(s, name, cfg)
			}
		}
		s.features.Store(f)
		for _, companion := range f.companions {
			go companion.run()
		}
	})
}

// Called before replay starts. Tests that construct volumes directly retain the
// legacy environment-selected content state until this service boundary runs.
func (s *goSearchService) prepareFeatureVolume(vol *serviceVolumeIndex) {
	if vol == nil {
		return
	}
	s.initializeFeatures()
	f := s.features.Load()
	if f.content && !contentScopeForVolume(s.contentConfig(), vol.volume).disabled() {
		newActivation := vol.contentActivation.Load() == nil
		if newActivation {
			ctx, cancel := context.WithCancel(context.Background())
			vol.contentActivation.Store(&contentActivation{ctx: ctx, cancel: cancel})
		}
		if vol.content == nil {
			vol.content = newContentVolumeState(vol.volume)
			vol.contentCoord = newContentCoordinator(vol.content)
		}
		if newActivation {
			vol.contentCoord.extractCtx = vol.contentActivation.Load().ctx
		}
	} else {
		vol.stopContentDrain()
		if vol.content != nil {
			vol.content.mu.Lock()
			idx := vol.content.idx
			vol.content.idx, vol.content.reader, vol.content.resolver = nil, nil, nil
			vol.content.mu.Unlock()
			if idx != nil {
				idx.Release()
			}
		}
		vol.content, vol.contentCoord = nil, nil
	}
	if len(f.companions) > 0 && vol.featureFeed == nil {
		vol.featureFeed = &featureChangeFeed{}
		// A custom -config file may put feature data outside the default seekfs
		// directory. Register it before replay so companions cannot index their
		// own database writes. Descendant creates use the existing dynamic filter.
		root := s.contentCfg.SeekFSDir
		if root != "" {
			if absolute, err := filepath.Abs(root); err == nil && strings.EqualFold(filepath.VolumeName(absolute), vol.volume) {
				if err := os.MkdirAll(absolute, 0700); err == nil {
					if vol.ownedDirFRNs == nil {
						vol.ownedDirFRNs = make(map[uint64]struct{})
					}
					collectOwnedDirFRNs(absolute, vol.ownedDirFRNs, 0)
				}
			}
		}
	}
}

func (s *goSearchService) featuresVolumeReady(vol *serviceVolumeIndex) {
	if vol == nil {
		return
	}
	if s.features.Load() == nil {
		s.prepareFeatureVolume(vol)
	}
	if s.features.Load().content {
		s.ensureContentBuild(vol)
	}
}

func (vol *serviceVolumeIndex) observeFeatureChanges(changes []usnChange) {
	if vol.contentCoord != nil {
		vol.contentCoord.observeChanges(changes)
	}
	if vol.featureFeed != nil {
		vol.featureFeed.observe(changes)
	}
}

func replaceFeatureVolumeState(dst, src *serviceVolumeIndex) {
	if dst.featureFeed == nil {
		dst.featureFeed = src.featureFeed
	}
	if dst.featureFeed != nil {
		dst.featureFeed.reset()
	}
	if dst.content == nil {
		dst.content, dst.contentCoord = src.content, src.contentCoord
		if activation := src.contentActivation.Load(); activation != nil {
			dst.contentActivation.Store(activation)
		}
	}
	if src.contentCoord != nil && src.contentCoord != dst.contentCoord {
		src.stopContentDrain()
	}
}

func (vol *serviceVolumeIndex) featuresBaseReplaced(previousJournal uint64) {
	if previousJournal != 0 && vol.journalID != 0 && previousJournal != vol.journalID {
		invalidateContentAfterBaseReset(vol, fmt.Sprintf("journal id changed from %d to %d", previousJournal, vol.journalID))
		return
	}
	rebindContentAfterBaseSwap(vol)
}

func (s *goSearchService) featureHealthSnapshot() []featureHealth {
	f := s.features.Load()
	if f == nil {
		return nil
	}
	out := []featureHealth{{Name: "content", Enabled: f.content, State: "disabled"}}
	if f.content {
		out[0].State = "enabled"
	}
	for name, cfg := range f.configs {
		if name == "content" {
			continue
		}
		h := featureHealth{Name: name, Enabled: cfg.Enabled, State: "disabled"}
		if companion := f.companions[name]; companion != nil {
			h = companion.health()
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// A bounded replay window shared by companions. A gap explicitly requires a
// new snapshot, including after a base replacement. No callback performs IPC
// while replay holds the volume lock.
const featureChangeWindow = 4096

type featureChangeFeed struct {
	mu      sync.Mutex
	seq     uint64
	epoch   uint64
	changes []usnChange
}

func (f *featureChangeFeed) observe(changes []usnChange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, change := range changes {
		if change.Attr&0x10 != 0 && change.Reason&(usnReasonRenameOld|usnReasonRenameNew|usnReasonFileDelete) != 0 {
			// A directory move/delete changes descendant paths, not just its FRN.
			f.epoch++
			f.changes = nil
			break
		}
	}
	f.seq += uint64(len(changes))
	if len(changes) >= featureChangeWindow {
		f.changes = append(f.changes[:0], changes[len(changes)-featureChangeWindow:]...)
		return
	}
	if excess := len(f.changes) + len(changes) - featureChangeWindow; excess > 0 {
		copy(f.changes, f.changes[excess:])
		f.changes = f.changes[:len(f.changes)-excess]
	}
	f.changes = append(f.changes, changes...)
}

func (f *featureChangeFeed) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch++
	f.changes = nil
}
