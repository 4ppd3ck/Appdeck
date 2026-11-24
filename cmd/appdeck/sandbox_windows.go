//go:build windows

// sandbox_windows.go — the Job Object side of sandbox.go. Windows has no
// in-process filesystem sandbox a launcher can hand a child, so the honest
// equivalent runs after start: a Job Object with a 2 GB commit cap and
// kill-on-close. The child cannot outlive the launcher and runaway memory
// dies; nothing here pretends to be filesystem isolation.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

const sandboxJobMemoryLimit = uint64(2 << 30) // 2 GiB commit cap per sandboxed app

func wrapSandboxPlatform(a *App, cmd *exec.Cmd, appDir, dataDir, profileDir string) error {
	// No argv wrap on windows — the Job Object attaches after start.
	return nil
}

func attachSandboxPlatform(a *App, pid int, logPath string) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		sandboxLogNote(logPath, fmt.Sprintf("job object create failed: %v", err))
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_JOB_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		JobMemoryLimit: uintptr(sandboxJobMemoryLimit),
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		sandboxLogNote(logPath, fmt.Sprintf("job object limits failed: %v", err))
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		sandboxLogNote(logPath, fmt.Sprintf("job object attach failed: %v", err))
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		sandboxLogNote(logPath, fmt.Sprintf("job object assign failed: %v", err))
	}
}

// sandboxLogNote appends a best-effort note to the app's log.
func sandboxLogNote(logPath, note string) {
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "appdeck: sandbox: %s\n", note)
}
