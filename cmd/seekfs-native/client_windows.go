package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// pipeRoundTrip opens the service named pipe and exchanges one JSON
// request/response pair, mirroring callServiceWithTimeout +
// exchangeServiceJSON in cmd/seekfs. A fresh connection per request matches
// the Wails UI behavior (the service accepts one request per connection).

func pipeRoundTrip(pipeName string, req serviceRequest, timeout time.Duration) (serviceResponse, error) {
	handle, err := openPipeClientWithTimeout(pipeName, timeout)
	if err != nil {
		return serviceResponse{}, err
	}
	file := os.NewFile(uintptr(handle), pipeName)
	defer file.Close()
	return exchangeServiceJSON(file, req, timeout)
}

func openPipeClientWithTimeout(pipeName string, timeout time.Duration) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return 0, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		handle, err := windows.CreateFile(
			ptr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			return handle, nil
		}
		if err != windows.ERROR_PIPE_BUSY || time.Now().After(deadline) {
			return 0, err
		}
		sleep := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep <= 0 {
			return 0, err
		}
		time.Sleep(sleep)
	}
}

func exchangeServiceJSON(conn io.ReadWriteCloser, req serviceRequest, timeout time.Duration) (serviceResponse, error) {
	if timeout <= 0 {
		return exchangeServiceJSONBlocking(conn, req)
	}
	type result struct {
		resp serviceResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := exchangeServiceJSONBlocking(conn, req)
		done <- result{resp: resp, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.resp, res.err
	case <-timer.C:
		_ = conn.Close()
		return serviceResponse{}, fmt.Errorf("seekfs service %q request timed out after %s", req.Command, timeout)
	}
}

func exchangeServiceJSONBlocking(conn io.ReadWriteCloser, req serviceRequest) (serviceResponse, error) {
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return serviceResponse{}, err
	}
	var resp serviceResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return serviceResponse{}, err
	}
	return resp, nil
}
