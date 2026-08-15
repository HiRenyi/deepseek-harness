//go:build !windows

package transport

import (
	"os/exec"
	"testing"
)

// TestPidAliveForeignLiveProcess pins pidAlive for a real, separate live
// process on non-Windows. The pre-existing TestAcquireLockExclusive only
// exercises pid==os.Getpid() (an early-return shortcut); this test exercises
// the actual foreign-PID probe path (syscall.Signal(0)).
func TestPidAliveForeignLiveProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("foreign-pid test spawns a subprocess")
	}
	cmd := exec.Command("sleep", "30")
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
