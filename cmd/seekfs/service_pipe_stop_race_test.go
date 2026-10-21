package main

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestCreatePipeInstanceStopAfterConnect forces the stop/connect race: a
// client connects, then stop fires and the stop watcher closes the handle
// before createPipeInstance releases it. createPipeInstance must report the
// stop instead of returning the closed handle, which the connection handler
// would otherwise read and write after the number was reused.
func TestCreatePipeInstanceStopAfterConnect(t *testing.T) {
	pipeName := fmt.Sprintf(`\\.\pipe\seekfs-stoprace-%d-%d`, os.Getpid(), time.Now().UnixNano())
	svc := &goSearchService{pipeName: pipeName, sddl: standaloneTestPipeSDDL, stop: make(chan struct{})}
	pipeConnectedTestHook = func() {
		close(svc.stop)
		time.Sleep(50 * time.Millisecond) // let the watcher take the stop branch
	}
	t.Cleanup(func() { pipeConnectedTestHook = nil })

	type result struct {
		file *os.File
		err  error
	}
	got := make(chan result, 1)
	go func() {
		file, err := svc.createPipeInstance()
		got <- result{file, err}
	}()
	var client windows.Handle
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; {
		client, err = openPipeClientWithTimeout(pipeName, 100*time.Millisecond)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		select {
		case r := <-got:
			t.Fatalf("client connect: %v (createPipeInstance: file=%v err=%v)", err, r.file != nil, r.err)
		default:
			t.Fatalf("client connect: %v", err)
		}
	}
	defer windows.CloseHandle(client)

	r := <-got
	if r.err == nil {
		_, typeErr := windows.GetFileType(windows.Handle(r.file.Fd()))
		t.Fatalf("createPipeInstance returned a handle after stop closed it (GetFileType err=%v)", typeErr)
	}
	if !errors.Is(r.err, errServicePipeStopping) {
		t.Fatalf("createPipeInstance err = %v; want errServicePipeStopping", r.err)
	}
}
