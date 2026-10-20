//go:build !windows

package main

import "os/exec"

func prepareNativeServiceCommand(cmd *exec.Cmd) {}
