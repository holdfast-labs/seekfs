//go:build !windows

package main

import "os"

// contentOpenNoRecall is a plain open off Windows; placeholder hydration is a
// Windows cloud-filesystem concern.
func contentOpenNoRecall(path string) (*os.File, error) { return os.Open(path) }
