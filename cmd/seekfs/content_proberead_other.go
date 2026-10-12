//go:build !windows

package main

// Non-Windows readability probe: mappings are released with munmap, and_Go
// recovers a SIGSEGV on an unmapped page as a panic, so a plain read probe is
// sufficient. This platform file only exists so the integrity pass builds
// everywhere; production runs on Windows.



// contentSectionReadable reports whether the payload can be read.
func contentSectionReadable(payload []byte) (ok bool) {
	if len(payload) == 0 {
		return true
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	var sum byte
	const page = 4096
	for off := 0; off < len(payload); off += page {
		end := off + page
		if end > len(payload) {
			end = len(payload)
		}
		for _, b := range payload[off:end] {
			sum += b
		}
	}
	_ = sum
	return true
}
