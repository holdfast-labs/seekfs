//go:build !windows

package main

import (
	"errors"
	"time"
)

func pipeRoundTrip(pipeName string, req serviceRequest, timeout time.Duration) (serviceResponse, error) {
	return serviceResponse{}, errors.New("seekfs-native requires Windows")
}
