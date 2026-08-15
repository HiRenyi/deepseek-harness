package main

// detach_windows.go — Windows detachment flags for spawned GUI processes.
//
// CREATE_NEW_PROCESS_GROUP detaches the GUI from bridge's console process
// group so it survives bridge exiting and doesn't share ctrl-C handling. This
// is the Windows half of detachSpawn; the non-Windows half lives in
// detach_other.go (no flags needed — Start() already detaches).

import (
	"os/exec"
	"syscall"
)

// detachSpawn applies platform-specific detachment flags to cmd. On Windows
// this puts the child in its own process group so a bridge exit (or ctrl-C in
// the bridge console) does not cascade to the GUI.
func detachSpawn(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
