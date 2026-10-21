//go:build !windows

package main

import "errors"

func runElevatedSettings(patch *serviceSettings, pipeName string, installService bool) error {
	return errors.New("elevated settings require Windows")
}
