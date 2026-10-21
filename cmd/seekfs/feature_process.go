package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A Windows Job kills the companion and its descendants if the host exits,
// including an abrupt host crash. Companions wait for hello before doing work.
type featureProcessJob struct{ handle windows.Handle }

func newFeatureProcessJob() (*featureProcessJob, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &featureProcessJob{handle: h}, nil
}

func (j *featureProcessJob) attach(process *os.Process) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(j.handle, h)
}

func (j *featureProcessJob) close() {
	if j != nil {
		_ = windows.CloseHandle(j.handle)
	}
}
