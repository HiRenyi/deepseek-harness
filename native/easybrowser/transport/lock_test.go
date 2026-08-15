package transport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAcquireLockExclusive verifies the occupancy lock prevents a second live
// bridge from claiming the same data dir, and releases cleanly. This is the
// protection against the multi-instance stomp that orphaned nm-host.
func TestAcquireLockExclusive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)

	// First acquire succeeds.
	release1, err := AcquireLock()
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}

	// Second acquire (same dir, owner pid == os.Getpid() is "alive") must fail.
	if _, err := AcquireLock(); err == nil {
		t.Fatal("second AcquireLock should fail while first holds the lock")
	}

	// After release, a fresh acquire succeeds.
	release1()
	release2, err := AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	release2()

	// Lock file must be cleaned up.
	if _, err := os.Stat(filepath.Join(dir, lockName)); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed, stat err=%v", err)
	}
}

// TestAcquireLockReclaimsStale verifies a lock pointing at a dead pid is reclaimed.
func TestAcquireLockReclaimsStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write a stale lock for a pid that is definitely not alive (a huge number).
	stale := filepath.Join(dir, lockName)
	if err := os.WriteFile(stale, []byte(`{"pid":99999999,"started":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock should reclaim stale lock: %v", err)
	}
	release()
}

// TestAcquireLockReclaimsRecycledPid is the end-to-end regression for the storm
// root cause: a lock pointing at a LIVE but non-bridge process (a recycled pid)
// must be reclaimed, NOT refused with "already running". Pre-fix this returned
// "bridge already running" and the new bridge exited silently, leaving
// addr.json stale and triggering the "Native host has exited" reconnect storm.
func TestAcquireLockReclaimsRecycledPid(t *testing.T) {
	if testing.Short() {
		t.Skip("foreign-pid test spawns a subprocess")
	}
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Hold a live foreign process whose pid will be written into the lock.
	cmd := exec.Command("cmd", "/c", "timeout", "/t", "30", "/nobreak")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Lock points at a live process that is NOT bridge.exe (recycled-pid shape).
	body := fmt.Sprintf(`{"pid":%d,"started":"2026-01-01T00:00:00Z"}`, cmd.Process.Pid)
	stale := filepath.Join(dir, lockName)
	if err := os.WriteFile(stale, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock should reclaim lock pointing at live non-bridge pid: %v", err)
	}
	release()
}
