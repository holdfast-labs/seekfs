//go:build windows

package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestServiceSpawnSpecExactCommand(t *testing.T) {
	app := `C:\apps\seekfs\seekfs-native.exe`
	pipe := `\\.\pipe\seekfs-native-test`
	spec := serviceSpawnSpecFor(app, pipe)

	if spec.Executable != `C:\apps\seekfs\seekfs.exe` {
		t.Fatalf("executable = %q", spec.Executable)
	}
	wantArgs := []string{
		"service", "-lowmem", "-pipe", pipe, "-sddl", defaultServiceSDDL,
	}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", spec.Args, wantArgs)
	}
	if spec.Dir != `C:\apps\seekfs` {
		t.Fatalf("dir = %q, want app dir", spec.Dir)
	}

	cmd := newNativeServiceCommand(spec)
	if cmd.Path != spec.Executable {
		t.Fatalf("path = %q, want %q", cmd.Path, spec.Executable)
	}
	if cmd.Args[0] != spec.Executable {
		t.Fatalf("argv[0] = %q", cmd.Args[0])
	}
	if !reflect.DeepEqual(cmd.Args[1:], wantArgs) {
		t.Fatalf("argv[1:] = %#v, want %#v", cmd.Args[1:], wantArgs)
	}
	if cmd.Dir != spec.Dir {
		t.Fatalf("cmd dir = %q, want %q", cmd.Dir, spec.Dir)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatalf("hidden window flag missing: %+v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.CreationFlags&nativeCreateNoWindow == 0 {
		t.Fatalf("CREATE_NO_WINDOW missing: %#x", cmd.SysProcAttr.CreationFlags)
	}
}

func TestStartNativeServiceUsesHook(t *testing.T) {
	var got serviceSpawnSpec
	old := nativeServiceSpawnHook
	nativeServiceSpawnHook = func(spec serviceSpawnSpec) error {
		got = spec
		return nil
	}
	t.Cleanup(func() { nativeServiceSpawnHook = old })

	spec := serviceSpawnSpecFor(`C:\apps\seekfs\seekfs-native.exe`, `\\.\pipe\hook`)
	if err := startNativeService(spec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("hook spec = %#v, want %#v", got, spec)
	}
	if filepath.Base(got.Executable) != "seekfs.exe" {
		t.Fatalf("spawned %q, want sibling seekfs.exe", got.Executable)
	}
}
