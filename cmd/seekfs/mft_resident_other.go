//go:build !windows

package main

import "errors"

// Resident MFT reads need the Windows NTFS FSCTL; other platforms always use
// the normal open path.
var contentOpenResidentSource = func(string) (contentResidentSource, error) {
	return nil, errors.New("resident MFT reads require Windows")
}
