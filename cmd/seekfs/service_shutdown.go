package main

import (
	"sync"
	"time"
)

// Bounded service shutdown.
//
// Root cause of the STOP_PENDING hang: every wait on the stop path was
// unbounded or the stop path never ran at all:
//
//   - stopContentPlugin waited on vol.contentTasksDone with no timeout. A
//     content task is held for a whole drain tick (serial extraction across
//     thousands of files with a 2-minute per-document timeout), a fold
//     assembling gigabytes, the post-attach integrity pass hashing gigabytes,
//     restart catch-up reading the journal, or a service-owned build. Any of
//     those wedged the stop forever.
//   - persistVolumeLoop ran a final forced persistVolumeIfDue(vol, true) on
//     s.stop: a full overlay fold plus a multi-gigabyte tmp write, commit and
//     reload (observed ~15 s per volume, back to back).
//   - The SCM handler (Execute) reported STOP_PENDING with no wait hint and no
//     checkpoints, then blocked until the pipe listeners drained, so SCM could
//     not tell a slow stop from a wedged one.
//   - Shutdown never cancelled the content activations, never retired the
//     per-volume drains, and never stopped the companions; their goroutines and
//     sidecar mappings leaked until the process was killed.
//
// The fix: one shutdown sequence with a budget per phase. Signal first (every
// loop selects on s.stop), cancel the content activations up front so in-flight
// drain ticks, builds, folds and the integrity pass abort at their next
// per-document check, drain content tasks only up to a deadline, release
// mappings best-effort under bounded locks, and never do blocking persist work
// on the way out: the WAL holds every replayed batch, so skipping the final
// persist loses nothing and a mid-persist write killed mid-flight is safe
// (tmp + atomic rename; the WAL rewrite keeps the post-snapshot frames).
var (
	// serviceShutdownTimeout bounds the content/companion teardown phases.
	serviceShutdownTimeout = 8 * time.Second
	// serviceShutdownContentWait bounds the content-task drain
	// (drain tick, catch-up, builds, folds, integrity pass).
	serviceShutdownContentWait = 3 * time.Second
	// serviceShutdownCompanionWait bounds the companion stop wait.
	serviceShutdownCompanionWait = 2 * time.Second
	// serviceShutdownLockWait bounds each lock acquisition in the release
	// phase (an in-flight search can hold indexMu.RLock for a long query).
	serviceShutdownLockWait = 2 * time.Second
	// serviceShutdownListenerWait bounds the pipe-listener drain after the
	// stop signal (ConnectNamedPipe is interrupted via CloseHandle).
	serviceShutdownListenerWait = 3 * time.Second
	// serviceShutdownRemoteWait bounds the Mode L server close (in-flight
	// remote work is connection-cancelled first).
	serviceShutdownRemoteWait = 3 * time.Second
)

// shutdownService runs the bounded shutdown sequence, exactly once per service
// instance. progress, when non-nil, is called with the stage name as each
// phase starts so the SCM handler can report STOP_PENDING checkpoints. It is
// safe to call from the SCM stop handler and from servePrivileged after
// <-s.stop; whichever runs first drives, the other is a no-op.
func (s *goSearchService) shutdownService(progress func(stage string)) {
	if s == nil {
		return
	}
	s.shutdownOnce.Do(func() { s.shutdownServiceOnce(progress) })
}

func (s *goSearchService) shutdownServiceOnce(progress func(stage string)) {
	note := func(stage string) {
		serviceLog("service shutdown: %s", stage)
		if progress != nil {
			progress(stage)
		}
	}
	note("stopping")
	s.signalServiceStop()
	s.indexMu.RLock()
	volumes := append([]*serviceVolumeIndex(nil), s.volumes...)
	var companions []*companionFeature
	if f := s.features.Load(); f != nil {
		for _, c := range f.companions {
			companions = append(companions, c)
		}
	}
	s.indexMu.RUnlock()
	// Cancel first: every content loop (drain tick, build, fold, integrity
	// pass, catch-up) aborts at its next per-document/stage check once the
	// activation is cancelled, so the drain below actually terminates.
	note("cancel-content")
	for _, vol := range volumes {
		if a := vol.contentActivation.Load(); a != nil {
			a.cancel()
		}
		vol.stopContentDrain()
	}
	for _, c := range companions {
		c.requestStop()
	}
	note("drain-content")
	if !s.waitContentTasksBounded(volumes, serviceShutdownContentWait) {
		serviceLog("service shutdown: content tasks did not drain in %s; releasing anyway", serviceShutdownContentWait)
	}
	note("release-content")
	if !s.releaseContentForShutdown(volumes, serviceShutdownLockWait) {
		serviceLog("service shutdown: content release skipped (locks busy); process exit reclaims the mappings")
	}
	note("companions")
	if !waitCompanionsStopped(companions, serviceShutdownCompanionWait) {
		serviceLog("service shutdown: companions did not stop in %s; exiting anyway", serviceShutdownCompanionWait)
	}
	// Persist the usage-signal store (frecency + query history); opens also
	// save synchronously, so this only catches a trailing write.
	note("flush-signals")
	flushSignalStore()
	note("stopped")
}

// waitContentTasksBounded waits for every volume's content tasks (drain loop,
// catch-up, builds, folds, integrity pass) up to timeout. No new task can
// start once the service is stopping (beginContentTask refuses), so the set
// only shrinks. False means the deadline hit; the caller must proceed anyway.
func (s *goSearchService) waitContentTasksBounded(volumes []*serviceVolumeIndex, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		vol.contentTasksMu.Lock()
		done := vol.contentTasksDone
		count := vol.contentTasks
		vol.contentTasksMu.Unlock()
		if count <= 0 || done == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
	return true
}

// releaseContentForShutdown drops the per-volume content bases/readers and the
// sidecar locks, mirroring the release phase of stopContentPlugin but with a
// bounded wait for the write lock: an in-flight search can hold
// indexMu.RLock for a long content query, and shutdown must not queue behind
// it. On a lock timeout the mappings stay until process exit (which unmaps
// them); queries are already refused because the service is stopping.
func (s *goSearchService) releaseContentForShutdown(volumes []*serviceVolumeIndex, timeout time.Duration) bool {
	if !s.lockIndexMuWriteTimeout(timeout) {
		return false
	}
	defer s.indexMu.Unlock()
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		if !lockMutexTimeout(&vol.mu, timeout) {
			continue
		}
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
		vol.contentActivation.Store(nil)
		vol.mu.Unlock()
	}
	s.contentLocksMu.Lock()
	defer s.contentLocksMu.Unlock()
	for path, lock := range s.contentLocks {
		lock.release()
		delete(s.contentLocks, path)
	}
	return true
}

// lockIndexMuWriteTimeout acquires s.indexMu for writing within d. On timeout
// a background goroutine releases the lock if it arrives later, so the lock
// is never left held.
func (s *goSearchService) lockIndexMuWriteTimeout(d time.Duration) bool {
	done := make(chan struct{})
	go func() { s.indexMu.Lock(); close(done) }()
	return awaitLock(done, func() { s.indexMu.Unlock() }, d)
}

// lockMutexTimeout acquires mu within d, with the same late-release guard.
func lockMutexTimeout(mu *sync.Mutex, d time.Duration) bool {
	done := make(chan struct{})
	go func() { mu.Lock(); close(done) }()
	return awaitLock(done, func() { mu.Unlock() }, d)
}

func awaitLock(done chan struct{}, unlock func(), d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		go func() { <-done; unlock() }()
		return false
	}
}

// waitCompanionsStopped waits for every companion's run loop up to timeout.
// Companions exit on s.stop (already closed) and kill their child process on
// the way out, so this is normally immediate.
func waitCompanionsStopped(companions []*companionFeature, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for _, c := range companions {
		if c == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-c.stopped:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
	return true
}

// waitWaitGroupTimeout waits for wg up to d.
func waitWaitGroupTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// stopContentPluginBounded cancels the content plugin for volumes and waits
// for its tasks up to wait, then releases the bases. The plugin-reload path
// (applyPluginDefinitions) uses a generous wait so a usable base survives a
// slow fold; the shutdown path uses serviceShutdownContentWait and proceeds
// regardless.
func (s *goSearchService) stopContentPluginBounded(volumes []*serviceVolumeIndex, wait time.Duration) {
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		if a := vol.contentActivation.Load(); a != nil {
			a.cancel()
		}
		vol.stopContentDrain()
	}
	s.waitContentTasksBounded(volumes, wait)
	s.releaseContentForShutdown(volumes, serviceShutdownLockWait)
}
