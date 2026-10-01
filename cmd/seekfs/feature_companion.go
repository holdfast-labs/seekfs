package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"seekfs/feature"
)

type companionFeature struct {
	service          *goSearchService
	name             string
	cfg              featureConfig
	gate             chan struct{}
	healthMu         sync.Mutex
	state, lastError string
	progress         *feature.Progress
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	stdout           *bufio.Scanner
	processDone      chan struct{}
	stopped          chan struct{}
	stop             chan struct{}
	stopOnce         sync.Once
	job              *featureProcessJob
	storageLock      *contentVolumeLock
	requestID        uint64
	cursors          map[*serviceVolumeIndex]feature.Volume
	retryAfter       time.Time
	failures         int
	supportsQuery    bool
}

type featureOperationError struct{ feature, op, message string }

func (e *featureOperationError) Error() string {
	return fmt.Sprintf("feature %s %s: %s", e.feature, e.op, e.message)
}

func newCompanionFeature(s *goSearchService, name string, cfg featureConfig) *companionFeature {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &companionFeature{service: s, name: name, cfg: cfg, gate: make(chan struct{}, 1), stopped: make(chan struct{}), stop: make(chan struct{}), state: "starting", cursors: make(map[*serviceVolumeIndex]feature.Volume)}
}

func (f *companionFeature) requestStop() { f.stopOnce.Do(func() { close(f.stop) }) }
func (f *companionFeature) stopping() bool {
	if f.service.serviceStopping() {
		return true
	}
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

func (f *companionFeature) health() featureHealth {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	return featureHealth{Name: f.name, Enabled: true, State: f.state, Error: f.lastError, Progress: f.progress}
}

func (f *companionFeature) setHealth(state string, err error) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	f.state, f.lastError = state, ""
	if err != nil {
		f.lastError = err.Error()
	}
}

func (f *companionFeature) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.stopping() {
		return fmt.Errorf("service stopping")
	}
	select {
	case f.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-f.service.stop:
		return fmt.Errorf("service stopping")
	case <-f.stop:
		return fmt.Errorf("plugin stopping")
	}
}

func (f *companionFeature) stopProcess() {
	f.job.close()
	f.job = nil
	if f.stdin != nil {
		_ = f.stdin.Close()
	}
	if f.cmd != nil {
		_ = f.cmd.Process.Kill()
	}
	if f.processDone != nil {
		<-f.processDone
	}
	f.cmd, f.stdin, f.stdout, f.processDone = nil, nil, nil, nil
	f.cursors = make(map[*serviceVolumeIndex]feature.Volume)
	f.supportsQuery = false
	f.healthMu.Lock()
	f.progress = nil
	f.healthMu.Unlock()
}

func (f *companionFeature) failed(err error) {
	f.stopProcess()
	f.failures++
	// Bounded restart backoff prevents a broken executable from thrashing.
	f.retryAfter = time.Now().Add(time.Duration(min(f.failures, 30)) * time.Second)
	f.setHealth("unavailable", err)
}

func (f *companionFeature) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.stopping() {
		return fmt.Errorf("service stopping")
	}
	if f.cmd != nil {
		select {
		case <-f.processDone:
			f.failed(fmt.Errorf("companion exited"))
		default:
			return nil
		}
	}
	if time.Now().Before(f.retryAfter) {
		return fmt.Errorf("feature %s is in restart backoff", f.name)
	}
	root := f.service.contentCfg.SeekFSDir
	if root == "" {
		root = defaultSeekFSDir()
	}
	dataDir := filepath.Join(root, "features", f.name)
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		f.failed(err)
		return err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		f.failed(err)
		return err
	}
	if f.storageLock == nil {
		f.storageLock, err = acquireContentVolumeLock(filepath.Join(dataDir, "owner"))
		if err != nil {
			err = fmt.Errorf("feature %s storage ownership: %w", f.name, err)
			f.failed(err)
			return err
		}
	}
	f.job, err = newFeatureProcessJob()
	if err != nil {
		f.failed(err)
		return err
	}
	cmd := exec.Command(f.cfg.Command, f.cfg.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		f.failed(err)
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		f.failed(err)
		return err
	}
	// Diagnostics never enter the protocol or block on an unread stderr pipe.
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		f.failed(err)
		return err
	}
	f.cmd, f.stdin, f.stdout = cmd, stdin, feature.NewScanner(stdout)
	f.processDone = make(chan struct{})
	processDone := f.processDone
	go func() { _ = cmd.Wait(); close(processDone) }()
	if err := f.job.attach(cmd.Process); err != nil {
		f.failed(err)
		return err
	}
	resp, err := f.call(ctx, feature.Request{Op: "hello", Feature: f.name, DataDir: dataDir})
	if err != nil {
		if f.cmd != nil {
			f.failed(err)
		}
		return err
	}
	for _, capability := range resp.Capabilities {
		if capability == "query" {
			f.supportsQuery = true
		}
	}
	f.setHealth("syncing", nil)
	return nil
}

// Only the gate owner calls this. A timeout kills the process, unblocking both
// pipe IO and the worker; an abandoned response cannot satisfy the next request.
func (f *companionFeature) call(ctx context.Context, req feature.Request) (feature.Response, error) {
	f.requestID++
	req.Version, req.ID = feature.Version, f.requestID
	deadline := time.Now().Add(f.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	req.DeadlineUnix = deadline.UnixNano()
	type result struct {
		response feature.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		var resp feature.Response
		err := feature.Write(f.stdin, req)
		if err == nil {
			if !f.stdout.Scan() {
				err = f.stdout.Err()
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
			} else {
				err = json.Unmarshal(f.stdout.Bytes(), &resp)
			}
		}
		if err == nil && (resp.Version != feature.Version || resp.ID != req.ID) {
			err = fmt.Errorf("feature protocol version/request mismatch")
		}
		if err == nil && !resp.OK {
			err = &featureOperationError{feature: f.name, op: req.Op, message: resp.Error}
		}
		if err == nil && len(resp.FRNs) > feature.MaxQueryMatches {
			err = fmt.Errorf("feature reply exceeds match budget")
		}
		if p := resp.Progress; err == nil && p != nil && (p.Current < 0 || p.Total < 0 || p.Total > 0 && p.Current > p.Total || len(p.Unit) > 64) {
			err = fmt.Errorf("invalid feature progress")
		}
		done <- result{resp, err}
	}()
	timer := time.NewTimer(f.cfg.Timeout)
	defer timer.Stop()
	drain := func(err error) (feature.Response, error) {
		_ = f.cmd.Process.Kill()
		<-done
		f.failed(err)
		return feature.Response{}, err
	}
	select {
	case r := <-done:
		if r.err != nil {
			var operationError *featureOperationError
			if errors.As(r.err, &operationError) {
				f.setHealth("degraded", r.err)
			} else {
				f.failed(r.err)
			}
		}
		if r.err == nil && r.response.Progress != nil {
			f.healthMu.Lock()
			f.progress = r.response.Progress
			f.healthMu.Unlock()
		}
		return r.response, r.err
	case <-ctx.Done():
		// Drain a quick reply before restarting: rapid query cancellation should
		// not throw away an otherwise healthy companion's warmed index.
		grace := time.NewTimer(100 * time.Millisecond)
		select {
		case r := <-done:
			grace.Stop()
			var operationError *featureOperationError
			if r.err != nil && !errors.As(r.err, &operationError) {
				f.failed(r.err)
			}
			return feature.Response{}, ctx.Err()
		case <-grace.C:
		}
		// Kill before waiting on the IO goroutine; don't clear its pipes until
		// it has finished using them. Abandoned replies cannot leak to a new call.
		return drain(ctx.Err())
	case <-f.service.stop:
		return drain(fmt.Errorf("service stopping"))
	case <-f.stop:
		return drain(fmt.Errorf("plugin stopping"))
	case <-timer.C:
		return drain(fmt.Errorf("feature %s request timed out", f.name))
	}
}

func (f *companionFeature) sendRecords(ctx context.Context, meta *feature.Volume, records []feature.Record) error {
	for len(records) > 0 {
		bytes, count := 4096, 0 // Reserve room for the request header and cursor.
		for count < len(records) && count < feature.PageSize {
			rec := records[count]
			// Six bytes per string byte bounds JSON escaping without encoding the
			// page twice. Long Windows paths may need fewer than 256 rows/frame.
			n := 6*(len(rec.Path)+len(rec.Name)) + 256
			if bytes+n >= feature.MaxFrameBytes {
				break
			}
			bytes += n
			count++
		}
		if count == 0 {
			return fmt.Errorf("feature record exceeds IPC frame budget")
		}
		if _, err := f.call(ctx, feature.Request{Op: "records", Volume: meta, Records: records[:count]}); err != nil {
			return err
		}
		records = records[count:]
	}
	return nil
}

func (f *companionFeature) run() {
	defer close(f.stopped)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer func() {
		f.gate <- struct{}{}
		f.stopProcess()
		f.storageLock.release()
		f.storageLock = nil
		f.setHealth("stopped", nil)
		<-f.gate
	}()
	for {
		select {
		case <-f.service.stop:
			return
		case <-f.stop:
			return
		default:
		}
		ctx := context.Background()
		if f.lock(ctx) == nil {
			if f.start(ctx) == nil {
				f.service.indexMu.RLock()
				volumes := append([]*serviceVolumeIndex(nil), f.service.volumes...)
				f.service.indexMu.RUnlock()
				var pending error
				active := make(map[*serviceVolumeIndex]struct{}, len(volumes))
				for _, vol := range volumes {
					active[vol] = struct{}{}
				}
				for vol, meta := range f.cursors {
					if _, ok := active[vol]; ok {
						continue
					}
					if _, err := f.call(ctx, feature.Request{Op: "volume_remove", Volume: &meta}); err != nil {
						pending = err
						break
					}
					delete(f.cursors, vol)
				}
				for _, vol := range volumes {
					if f.cmd == nil {
						break
					}
					v, err := f.syncVolume(ctx, vol)
					if err != nil {
						pending = err
						if f.cmd == nil {
							break
						}
						continue
					}
					resp, err := f.call(ctx, feature.Request{Op: "health", Volume: &v.meta})
					if err != nil {
						pending = err
						break
					}
					if !resp.Complete {
						pending = fmt.Errorf("feature %s indexing incomplete: %s", f.name, resp.Message)
					}
				}
				if f.cmd != nil {
					if pending != nil {
						f.setHealth("syncing", pending)
					} else {
						f.setHealth("ready", nil)
					}
				}
			}
			<-f.gate
		}
		select {
		case <-f.service.stop:
			return
		case <-f.stop:
			return
		case <-ticker.C:
		}
	}
}
