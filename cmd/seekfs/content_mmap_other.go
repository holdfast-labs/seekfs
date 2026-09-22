//go:build !windows

package main

// Off Windows there is no mmap helper here; fall back to a heap read so a load
// that could have succeeded never fails for lack of a mapping. A nil release
// tells the caller the index owns no mapping.

import "os"

func contentMapFile(path string) (data []byte, release func(), err error) {
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return data, nil, nil
}
