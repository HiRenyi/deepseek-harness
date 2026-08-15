//go:build !windows

package main

import "os/exec"

// detach_other.go — non-Windows no-op for spawned GUI detachment.
//
// On Mac/Linux, exec.Cmd.Start() already runs the child as a detached
// process (no shared process group unless Setpgid is set), so no
// SysProcAttr configuration is required. The Windows-specific flags live in
// detach_windows.go.

// detachSpawn applies platform-specific detachment flags to cmd. No-op on
// non-Windows (Start() is already detached).
func detachSpawn(cmd *exec.Cmd) {}
