package transport

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// lockEntry is the JSON body of bridge.lock.
type lockEntry struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

// AcquireLock claims the occupancy lock for the bridge process.
//
// Contract: ensures at most one live bridge per data dir. On a stale lock
// (owner PID no longer alive) it reclaims; on a live owner it refuses.
// Returns a release func on success.
func AcquireLock() (func(), error) {
	dir, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data dir for lock: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	lockPath, err := lockFilePath()
	if err != nil {
		return nil, err
	}

	pid := os.Getpid()
	entry := lockEntry{PID: pid, Started: time.Now()}
	body, _ := json.Marshal(entry)

	// First try the optimistic exclusive-create path.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if _, werr := f.Write(body); werr != nil {
			f.Close()
			_ = os.Remove(lockPath)
			return nil, fmt.Errorf("write lock %s: %w", lockPath, werr)
		}
		f.Close()
		return func() { _ = os.Remove(lockPath) }, nil
	}

	// Lock exists — inspect owner; reclaim only if stale.
	existing, rerr := readLock(lockPath)
	if rerr != nil {
		// Corrupt/unreadable lock: reclaim.
		_ = os.Remove(lockPath)
		return writeLockReclaim(lockPath, body)
	}
	if !pidAlive(existing.PID) || !pidIsOurs(existing.PID) {
		// Dead pid, OR a recycled pid (alive but no longer bridge.exe): reclaim
		// either way. Without pidIsOurs, a recycled pid made us refuse "already
		// running" and the new bridge exited silently, leaving addr.json stale
		// and triggering the "Native host has exited" reconnect storm.
		_ = os.Remove(lockPath)
		return writeLockReclaim(lockPath, body)
	}
	return nil, fmt.Errorf("bridge already running (pid=%d, lock=%s); stop it first", existing.PID, lockPath)
}

func writeLockReclaim(lockPath string, body []byte) (func(), error) {
	if err := os.WriteFile(lockPath, body, 0600); err != nil {
		return nil, fmt.Errorf("reclaim lock %s: %w", lockPath, err)
	}
	return func() { _ = os.Remove(lockPath) }, nil
}

func readLock(path string) (lockEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return lockEntry{}, err
	}
	var e lockEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return lockEntry{}, err
	}
	return e, nil
}

// pidAlive reports whether a process with the given pid exists and is alive.
//
// The platform-specific probe (OpenProcess on Windows, Signal(0) elsewhere)
// lives in build-tagged files (lock_windows.go / lock_other.go). We must NOT
// rely on os.Process.Signal(syscall.Signal(0)) on Windows — it returns a
// non-nil error regardless of liveness, which made every existing lock look
// stale. That caused auto-launched bridges to reclaim a live bridge's lock,
// overwrite addr.json with their own (soon-to-die) pipe UUID, and trigger the
// "Native host has exited" infinite reconnect loop.
//
// Liveness is NOT identity: AcquireLock pairs pidAlive with pidIsOurs
// (image-name match) to distinguish a live bridge from a recycled pid that the
// probe reports as alive but belongs to an unrelated process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	return processAlive(pid)
}
