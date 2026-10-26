//go:build !windows

package main

import "errors"

func runElevatedSettings(patch *serviceSettings, pipeName string, installService bool, _ func(string)) error {
	return errors.New("elevated settings require Windows")
}
