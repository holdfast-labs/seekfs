//go:build !windows

package main

import "errors"

func runElevatedSettings(patch *serviceSettings, pipeName string) error {
	return errors.New("elevated settings require Windows")
}
