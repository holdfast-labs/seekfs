package main

import (
	"fmt"
	"os"
)

// gdebug.go: file logging gated by SEEKFS_NATIVE_DEBUGLOG. Used to verify
// rendering and search delivery without a debugger.

func dbgLog(format string, args ...any) {
	path := os.Getenv("SEEKFS_NATIVE_DEBUGLOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}
