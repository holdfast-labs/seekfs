package main

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// readConfigFile reads seekfs.toml or plugin.toml. Settings writes replace
// these files atomically (temp file + MoveFileEx), and on Windows a reader
// that opens the file at the instant of replacement gets a sharing violation
// (or access denied on the delete-pending target). Retry those briefly so a
// concurrent read sees either the old or the new file, never an error.
func readConfigFile(path string) ([]byte, error) {
	var data []byte
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		data, err = os.ReadFile(path)
		if err == nil || !configReadTransient(err) {
			return data, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return data, err
}

func configReadTransient(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case 5, 32, 33: // ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION
		return true
	}
	return false
}
