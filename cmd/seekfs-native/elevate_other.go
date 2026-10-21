//go:build !windows

package main

import "errors"

func runElevatedSettings(patch *serviceSettings) error {
	return errors.New("elevated settings require Windows")
}
