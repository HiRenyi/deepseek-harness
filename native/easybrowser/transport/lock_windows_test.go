//go:build windows

package transport

import (
	"os"
	"os/exec"
	"testing"
)

// TestPidAliveForeignLiveProcess is the regression test for the Windows
// pidAlive root cause. The pre-existing TestAcquireLockExclusive only
// exercises pid==os.Getpid() (an early-return shortcut), so it never probed a
// DIFFERENT live PID — which is exactly the case that broke: on Windows,
// os.Process.Signal(syscall.Signal(0)) returns a non-nil error regardless of
// liveness, so a live bridge's lock looked stale and got reclaimed.
//
// Here we spawn a real, separate process (cmd holding a `timeout`) and assert
// pidAlive sees it as live. After killing it, we assert pidAlive returns
// false. This pins the fix: a live foreign PID must read as alive on Windows.
func TestPidAliveForeignLiveProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("foreign-pid test spawns a subprocess")
	}
	// Hold for up to 30s; plenty of headroom for the assertions below.
	cmd := exec.Command("cmd", "/c", "timeout", "/t", "30", "/nobreak")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	if !pidAlive(pid) {
		t.Fatalf("pidAlive(live foreign pid=%d) = false, want true", pid)
	}
}

// TestPidIsOursForeignLiveProcess pins the recycled-pid fix: a live foreign
// process (cmd holding a timeout) is alive but its image is NOT bridge.exe, so
// pidIsOurs must return false. Without this gate AcquireLock would refuse
// "already running" on a lock pointing at such a pid — the root cause of the
// "Native host has exited" storm's second recurrence.
func TestPidIsOursForeignLiveProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("foreign-pid test spawns a subprocess")
	}
	cmd := exec.Command("cmd", "/c", "timeout", "/t", "30", "/nobreak")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	if pidIsOurs(pid) {
		t.Fatalf("pidIsOurs(foreign live pid=%d) = true, want false (not bridge.exe)", pid)
	}
}

// TestPidIsOursSelf confirms the current process self-identifies (image name
// matches its own os.Executable basename).
func TestPidIsOursSelf(t *testing.T) {
	if !pidIsOurs(os.Getpid()) {
		t.Fatal("pidIsOurs(self) = false, want true")
	}
}
