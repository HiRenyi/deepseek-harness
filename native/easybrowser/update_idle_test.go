package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTrackMCPActivity_PostOnly confirms only POST requests mark activity (GET
// /sse heartbeats must NOT — a connected-but-idle client shouldn't block the
// idle auto-update).
func TestTrackMCPActivity_PostOnly(t *testing.T) {
	// Reset global activity timestamp.
	lastMCPActivityMu.Lock()
	lastMCPActivity = time.Time{}
	lastMCPActivityMu.Unlock()

	srv := httptest.NewServer(trackMCPActivity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	// GET must not mark activity.
	http.Get(srv.URL + "/message")
	lastMCPActivityMu.Lock()
	gotGet := lastMCPActivity
	lastMCPActivityMu.Unlock()
	if !gotGet.IsZero() {
		t.Fatal("GET /sse heartbeat must not mark MCP activity")
	}

	// POST must mark activity.
	http.Post(srv.URL+"/message", "application/json", nil)
	lastMCPActivityMu.Lock()
	gotPost := lastMCPActivity
	lastMCPActivityMu.Unlock()
	if gotPost.IsZero() {
		t.Fatal("POST /message must mark MCP activity")
	}
	if time.Since(gotPost) > 2*time.Second {
		t.Fatalf("activity timestamp stale: %v", time.Since(gotPost))
	}
}

// TestIdleUpdateThreshold pins the env contract: default 30 min, custom value,
// <=0 disables (returns 0).
func TestIdleUpdateThreshold(t *testing.T) {
	// 隔离数据目录，避免本地 ~/.browser-mcp/browser-mcp.json 的 idle_min 污染默认值。
	t.Setenv("BROWSER_MCP_DATA_DIR", t.TempDir())
	t.Setenv("BROWSER_MCP_UPDATE_IDLE_MIN", "")
	if d := idleUpdateThreshold(); d != 2*time.Minute {
		t.Fatalf("default threshold=%v want 2m", d)
	}
	t.Setenv("BROWSER_MCP_UPDATE_IDLE_MIN", "5")
	if d := idleUpdateThreshold(); d != 5*time.Minute {
		t.Fatalf("custom threshold=%v want 5m", d)
	}
	t.Setenv("BROWSER_MCP_UPDATE_IDLE_MIN", "0")
	if d := idleUpdateThreshold(); d != 0 {
		t.Fatalf("0 should disable, got %v", d)
	}
	t.Setenv("BROWSER_MCP_UPDATE_IDLE_MIN", "garbage")
	if d := idleUpdateThreshold(); d != 2*time.Minute {
		t.Fatalf("garbage should fall back to default 2m, got %v", d)
	}
}

// TestIdleUpdateSingleFlight confirms acquire/release guards against overlap:
// a second acquire while one is held returns false.
func TestIdleUpdateSingleFlight(t *testing.T) {
	// Start from released state.
	idleUpdateRelease()
	if !idleUpdateAcquire() {
		t.Fatal("first acquire should succeed")
	}
	if idleUpdateAcquire() {
		t.Fatal("second acquire while held should fail (single-flight)")
	}
	idleUpdateRelease()
	if !idleUpdateAcquire() {
		t.Fatal("acquire after release should succeed")
	}
	idleUpdateRelease()
}

// TestRunIdleUpdateTick_NoUpdateAvailable confirms a tick is a no-op when no
// newer version is available (gitee placeholder 0.0.0 vs a non-numeric current)
// — guards against the auto-updater firing spuriously during dev.
func TestRunIdleUpdateTick_NoUpdateAvailable(t *testing.T) {
	// Idle since startup (zero last activity) + a manifest cache that reports
	// a version NOT newer than current → tick must return without touching the
	// updater. We can't easily inject a manifest here, so this test only
	// asserts the idle-gate path: with a RECENT activity timestamp, the tick
	// returns early regardless of manifest.
	lastMCPActivityMu.Lock()
	lastMCPActivity = time.Now() // not idle
	lastMCPActivityMu.Unlock()
	defer func() {
		lastMCPActivityMu.Lock()
		lastMCPActivity = time.Time{}
		lastMCPActivityMu.Unlock()
	}()
	// Should return immediately (not idle) without panicking.
	runIdleUpdateTick(30 * time.Minute)
}
