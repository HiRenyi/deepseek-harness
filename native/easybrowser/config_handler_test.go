package main

// config_handler_test.go — F2.6 /api/config + /api/config/reload handler tests.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/browser-mcp/bridge/config"
)

// writeFileBytes writes data to path, creating parent dirs (0700) + file 0600.
// Does NOT touch the config cache (simulates an external process writing the
// file while the bridge's cache is stale).
func writeFileBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// isolateConfigDataDir sets BROWSER_MCP_DATA_DIR to a temp dir + invalidates the
// config cache (shared helper for handler tests).
func isolateConfigDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dir)
	config.Reload() // invalidate + re-read (no file → defaults)
	return dir
}

// TestConfigHandler_ReturnsEffectiveWithSources: GET /api/config returns the
// merged config with `from` labels per field.
func TestConfigHandler_ReturnsEffectiveWithSources(t *testing.T) {
	isolateConfigDataDir(t)
	// Set an env override so one field is from "env".
	t.Setenv("BROWSER_MCP_UPDATE_SOURCE", "http")

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	configHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	src, ok := resp["update_source"].(map[string]any)
	if !ok {
		t.Fatalf("update_source missing or wrong type: %v", resp["update_source"])
	}
	if src["value"] != "http" || src["from"] != "env" {
		t.Errorf("update_source=%+v want env/http", src)
	}
}

// TestConfigReloadHandler_HotReload: POST /api/config/reload re-reads the file
// and returns changed fields + needs_restart for data_dir.
func TestConfigReloadHandler_HotReload(t *testing.T) {
	isolateConfigDataDir(t)
	// Write + cache an initial config file.
	cfg := &config.BrowserMCPConfig{UpdateSource: "gitee", IdleMin: 30, AutoStart: true}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	config.Reload() // cache the initial state

	// Write a changed file directly (simulates external GUI write — does NOT
	// invalidate the bridge's cache, so Reload detects the diff).
	changed := &config.BrowserMCPConfig{UpdateSource: "http", IdleMin: 5, AutoStart: true}
	data, _ := json.MarshalIndent(changed, "", "  ")
	path := config.ConfigPath()
	if err := writeFileBytes(path, data); err != nil {
		t.Fatalf("write file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/config/reload", nil)
	rec := httptest.NewRecorder()
	configReloadHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp struct {
		OK            bool     `json:"ok"`
		Reloaded      []string `json:"reloaded"`
		NeedsRestart  bool     `json:"needs_restart"`
		RestartReason string   `json:"restart_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if !resp.OK {
		t.Fatal("ok=false want true")
	}
	if resp.NeedsRestart {
		t.Error("needs_restart=true want false (data_dir unchanged)")
	}
	foundSrc, foundIdle := false, false
	for _, f := range resp.Reloaded {
		if f == "update_source" {
			foundSrc = true
		}
		if f == "idle_min" {
			foundIdle = true
		}
	}
	if !foundSrc {
		t.Error("update_source not in reloaded fields")
	}
	if !foundIdle {
		t.Error("idle_min not in reloaded fields")
	}
}

// TestConfigReloadHandler_DataDirNeedsRestart: changing data_dir sets
// needs_restart=true.
func TestConfigReloadHandler_DataDirNeedsRestart(t *testing.T) {
	// For data_dir to be effective from the file (not overridden by env), we
	// can't use BROWSER_MCP_DATA_DIR for isolation. Instead, use a config file
	// at the env-data-dir location with data_dir set in the file — the env
	// still wins, so we test the needs_restart flag by changing a non-data_dir
	// field that... actually, data_dir from env always wins. So to test
	// data_dir change detection, we change the FILE's data_dir while the env
	// stays the same — the effective value doesn't change (env wins), so
	// needs_restart would be false.
	//
	// This is a known limitation: when BROWSER_MCP_DATA_DIR is set (for test
	// isolation), data_dir changes in the file are masked by env. The
	// needs_restart=true path for data_dir is covered by config_test.go
	// (TestReload_DataDirChangeNeedsRestart) which uses setTempHome (no env).
	// Here we just verify the handler returns ok + the restart_reason field.
	isolateConfigDataDir(t)
	cfg := &config.BrowserMCPConfig{UpdateSource: "gitee"}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	config.Reload()

	req := httptest.NewRequest(http.MethodPost, "/api/config/reload", nil)
	rec := httptest.NewRecorder()
	configReloadHandler(rec, req)

	var resp struct {
		OK            bool   `json:"ok"`
		NeedsRestart  bool   `json:"needs_restart"`
		RestartReason string `json:"restart_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK {
		t.Fatal("ok=false want true")
	}
	// restart_reason is always "data_dir" (the only non-hot-reloadable field).
	if resp.RestartReason != "data_dir" {
		t.Errorf("restart_reason=%q want data_dir", resp.RestartReason)
	}
}

// TestConfigReloadHandler_MethodGuard: non-POST returns 405.
func TestConfigReloadHandler_MethodGuard(t *testing.T) {
	isolateConfigDataDir(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config/reload", nil)
	rec := httptest.NewRecorder()
	configReloadHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rec.Code)
	}
}
