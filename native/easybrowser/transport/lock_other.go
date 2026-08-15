//go:build !windows

package transport

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether a process with the given pid is alive on non-Windows.
//
// A null signal (syscall.Signal(0)) is the standard POSIX "am I allowed to
// signal / does it exist" probe: it returns nil if the process exists and the
// caller has permission, an error otherwise. The zero signal performs no
// delivery. This is reliable on Unix; the Windows branch (lock_windows.go) uses
// OpenProcess instead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// pidIsOurs reports whether the process at pid is owned by the same executable
// as the current process — the identity gate AcquireLock pairs with pidAlive
// (see lock.go). Best-effort on non-Windows: read /proc/<pid>/exe readlink and
// compare its basename to os.Executable(). /proc unavailable (e.g. macOS) ->
// true (conservative: don't block, rely on pidAlive's Signal(0); Unix pid
// recycling is less of an issue for this Windows-primary project).
func pidIsOurs(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	return strings.EqualFold(filepath.Base(exe), filepath.Base(self))
}
