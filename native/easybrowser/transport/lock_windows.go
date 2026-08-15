//go:build windows

package transport

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// processAlive reports whether a process with the given pid is alive on Windows.
//
// We use windows.OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION rather
// than os.Process.Signal(syscall.Signal(0)): on Windows the latter is
// unreliable and returns a non-nil error ("not supported by windows") even for
// live processes. That made pidAlive misjudge a live bridge as stale, causing a
// second bridge to reclaim its lock and overwrite addr.json — the root cause of
// the "Native host has exited" infinite reconnect loop.
//
// Liveness is NOT identity: OpenProcess succeeds for a RECYCLED pid (a dead
// bridge's pid number reused by an unrelated live process such as chrome.exe),
// so processAlive returns true for a pid that is no longer bridge. AcquireLock
// therefore pairs this with pidIsOurs (image-name match) before refusing — see
// lock.go. The earlier comment claiming "a dead or recycled pid yields
// ERROR_INVALID_PARAMETER" was true for dead pids but FALSE for recycled ones;
// that false assumption was the root cause of the storm's second recurrence.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil || handle == 0 {
		return false
	}
	_ = windows.CloseHandle(handle)
	return true
}

var (
	exeBasenameOnce sync.Once
	exeBasename     string
)

// currentExeBasename returns the running process's executable basename
// (e.g. "bridge.exe"), lowercased and cached for the process lifetime. It is
// the identity reference for pidIsOurs — deliberately NOT hardcoded
// "bridge.exe" so that test binaries and renamed distributions self-identify
// correctly.
func currentExeBasename() string {
	exeBasenameOnce.Do(func() {
		if exe, err := os.Executable(); err == nil {
			exeBasename = strings.ToLower(filepath.Base(exe))
		}
	})
	return exeBasename
}

// processImageName returns the full image path of the process at pid via
// QueryFullProcessImageName. It needs PROCESS_QUERY_LIMITED_INFORMATION — the
// same access OpenProcess uses in processAlive — and fails (non-nil error) for
// a pid we cannot query (e.g. dead, or higher-protected), which pidIsOurs
// treats as "not ours".
func processImageName(pid int) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil || handle == 0 {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var buf [windows.MAX_PATH]uint16
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// pidIsOurs reports whether the process at pid is owned by the same executable
// as the current process. This is the second reclaim gate after pidAlive: a
// recycled pid is alive (OpenProcess succeeds) but belongs to an unrelated
// process, so its image name won't match ours — AcquireLock reclaims it
// instead of refusing with "already running", which was the root cause of the
// "Native host has exited" storm's second recurrence (memory
// nm-host-debug-lessons).
func pidIsOurs(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	name, err := processImageName(pid)
	if err != nil || name == "" {
		return false
	}
	return strings.EqualFold(filepath.Base(name), currentExeBasename())
}
