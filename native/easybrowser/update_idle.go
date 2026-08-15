package main

// update_idle.go — P5.5 idle version check ("不用的时候静默检查更新").
//
// When the bridge has seen no MCP traffic (POST /mcp or /message — actual
// JSON-RPC messages, NOT GET /sse heartbeats) for idleMin minutes AND a newer
// version is available, trigger a bridge self-update (B1 rename-swap + re-exec,
// B5). The deprecated admin-installer / BrowserMCPUpdater scheduled-task path
// is gone; bridge now self-updates directly. Tunable via
// BROWSER_MCP_UPDATE_IDLE_MIN (default 30; <=0 disables). The check only fires
// when genuinely idle + an update exists, so it never interrupts active agent
// work. A single-flight guard (idleUpdateAcquire) ensures two idle ticks never
// trigger overlapping updates.

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/browser-mcp/bridge/config"
)

const defaultIdleUpdateMin = 2

var (
	lastMCPActivityMu sync.Mutex
	lastMCPActivity   time.Time
	idleUpdateMu      sync.Mutex
	idleUpdateRunning bool
)

// trackMCPActivity wraps an MCP transport handler, timestamping POST requests
// (real JSON-RPC messages). GET /sse heartbeats pass through without marking
// activity — an open SSE stream with no tool calls counts as "idle" for update
// purposes, so a connected-but-idle client doesn't block updates.
func trackMCPActivity(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			lastMCPActivityMu.Lock()
			lastMCPActivity = time.Now()
			lastMCPActivityMu.Unlock()
		}
		h.ServeHTTP(w, r)
	})
}

// lastMCPActivityTime returns the timestamp of the most recent MCP POST, or
// the zero time if no MCP traffic has occurred yet (treated as "idle since
// startup" by the updater).
func lastMCPActivityTime() time.Time {
	lastMCPActivityMu.Lock()
	defer lastMCPActivityMu.Unlock()
	return lastMCPActivity
}

// idleUpdateAcquire/release is a single-flight guard so two idle ticks never
// trigger overlapping updates.
func idleUpdateAcquire() bool {
	idleUpdateMu.Lock()
	defer idleUpdateMu.Unlock()
	if idleUpdateRunning {
		return false
	}
	idleUpdateRunning = true
	return true
}

func idleUpdateRelease() {
	idleUpdateMu.Lock()
	idleUpdateRunning = false
	idleUpdateMu.Unlock()
}

// idleUpdateThreshold returns the configured idle threshold from the central
// config (config.Load().IdleMin, default 2 min). Env BROWSER_MCP_UPDATE_IDLE_MIN
// still applies via the config layer (env > config > default). Returns 0 if
// disabled (<=0).
func idleUpdateThreshold() time.Duration {
	min := config.Load().IdleMin
	if min <= 0 {
		return 0
	}
	return time.Duration(min) * time.Minute
}

// startIdleUpdater arms the background idle version checker. No-op (logs once)
// when BROWSER_MCP_UPDATE_IDLE_MIN <= 0. Ticks every 30s; on each tick, if the
// bridge is idle past the threshold + a newer version is cached, triggers a
// bridge self-update (B1 rename-swap + re-exec). Safe to call once at startup.
func startIdleUpdater() {
	threshold := idleUpdateThreshold()
	if threshold == 0 {
		log.Printf("p5.5: idle auto-update disabled (BROWSER_MCP_UPDATE_IDLE_MIN<=0)")
		return
	}
	log.Printf("p5.5: idle auto-update armed (threshold=%v)", threshold)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if !idleUpdateAcquire() {
				continue // an update is already in flight
			}
			runIdleUpdateTick(threshold)
			idleUpdateRelease()
		}
	}()
}

// runIdleUpdateTick is one idle-check iteration, factored out so the
// single-flight guard wraps it cleanly.
func runIdleUpdateTick(threshold time.Duration) {
	last := lastMCPActivityTime()
	// Idle = no MCP POST in the threshold window. A zero last (no traffic since
	// startup) is treated as idle.
	if !last.IsZero() && time.Since(last) < threshold {
		return
	}
	m, err := cachedManifest()
	if err != nil || m == nil {
		return
	}
	if !isNewerVersion(m.Version, currentVersion()) {
		return
	}
	idleFor := time.Since(last)
	if last.IsZero() {
		idleFor = 0
	}
	// B5: bridge self-updates directly (rename-swap + re-exec, B1) — no longer
	// spawns the GUI --update-prompt as the sole path. spawnUpdatePrompt is
	// retained for the future "GUI also needs update" scenario (when manifest
	// has a newer gui asset), but bridge self-update no longer depends on it.
	// The single-flight guard above ensures only one self-update runs at a
	// time; a re-trigger after a successful swap is blocked because
	// currentVersion() converges on the new version (bridge_version written
	// post-swap). A failed self-update logs + the next idle tick retries.
	res := runSelfUpdate()
	if !res.OK {
		log.Printf("p5.5: idle %v + update %s available — self-update declined/failed: %s",
			idleFor, m.Version, res.Error)
		return
	}
	log.Printf("p5.5: idle %v + update %s — self-update %s (old=%s new=%s restarting=%v)",
		idleFor, m.Version, res.Action, res.OldVersion, res.NewVersion, res.Restarting)
}
