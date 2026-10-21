//go:build !windows

package main

// acquireContentVolumeLock is a no-op off Windows: the advisory sidecar lock is
// a Windows file-locking concern.
func acquireContentVolumeLock(gsx string) (*contentVolumeLock, error) {
	return &contentVolumeLock{}, nil
}

func (l *contentVolumeLock) release() {}
