package main

// M10: serialize writers of a volume's content sidecar (`.gsx`) across
// processes. The service holds the advisory lock for the volume's lifetime and
// the offline `content-index -db` CLI holds it for one build, so the CLI fails
// fast with a clear message instead of clobbering a live service's sidecar.

import (
	"errors"
	"os"
)

// errContentVolumeLocked reports that another process (a running service or a
// concurrent CLI build) already owns a volume's content sidecar. The offline
// builder fails fast with it instead of racing a live service's persist/swap.
var errContentVolumeLocked = errors.New("content sidecar is locked by another process")

// contentVolumeLock is an exclusive advisory lock on a volume's `.gsx.lock`.
type contentVolumeLock struct {
	file *os.File
}

// contentVolumeLockPath returns the advisory lock path for a content sidecar.
// It is a sibling `.lock` file, so the sidecar itself can be replaced by the
// atomic temp+rename without disturbing the lock.
func contentVolumeLockPath(gsx string) string {
	return gsx + ".lock"
}
