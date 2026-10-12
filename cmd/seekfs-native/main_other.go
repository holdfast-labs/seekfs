//go:build !windows

package main

import "errors"

func runUI(pipe string, limit int) error {
	return errors.New("seekfs-native requires Windows")
}
