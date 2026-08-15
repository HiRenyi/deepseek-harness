package config

// config.go — F2.6 central bridge configuration (browser-mcp.json).
//
// bridge self-describes its runtime knobs (update source/base, idle threshold,
// data dir, GUI autostart) via a single JSON file at <dataDir>/browser-mcp.json.
// Precedence is **env > config > default**: the file overlays defaults, then
// non-empty env vars overlay the file (env stays highest to preserve the
// existing CLAUDE.md env contract + ops tooling). The GUI is the sole writer;
// bridge only reads + reloads.
//
// The file read is cached (keyed by ConfigPath, so env-driven dataDir changes
// invalidate automatically); env is re-applied on every Load() call so runtime
// env changes (and test t.Setenv) are reflected immediately without a Reload.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/browser-mcp/bridge/profile"
)

// Env var names — mirrored from package main (update_source.go etc.). DO NOT
// rename: they are the public env contract documented in CLAUDE.md.
const (
	envUpdateSource  = "BROWSER_MCP_UPDATE_SOURCE"  // gitee | http
	envUpdateIdleMin = "BROWSER_MCP_UPDATE_IDLE_MIN" // idle threshold minutes
	envUpdateChannel = "BROWSER_MCP_UPDATE_CHANNEL" // telemetry channel (stable/sit/...); empty=null
	envDataDir       = "BROWSER_MCP_DATA_DIR"       // data directory override
)

// BrowserMCPConfig is the central bridge configuration. Schema v2 (5 fields).
// Loaded from <dataDir>/browser-mcp.json with env overrides.
//
// update_base is NOT a field here: as of hardcode-update-base-url, the http
// base URL is hardcoded to profile.UpdateBase (ldflag-baked) — config/env
// cannot override it. Old config files with a `update_base` key are silently
// ignored by the json decoder (unknown field), so no migration is needed.
type BrowserMCPConfig struct {
	UpdateSource  string `json:"update_source"`  // gitee | http
	IdleMin       int    `json:"idle_min"`       // idle update threshold minutes; <=0 disables
	DataDir       string `json:"data_dir"`       // empty = ~/.browser-mcp (resolved by callers)
	AutoStart     bool   `json:"autostart"`      // GUI auto-start (no env override; config-only)
	UpdateChannel string `json:"update_channel"` // telemetry channel for install POST; empty = null
}

// defaultConfig returns the default config (no file, no env).
// update_source hardcoded to "http" — the tool-hub address is fixed now, users
// must not need to configure it. The http base URL itself is profile.UpdateBase
// (ldflag-baked, NOT a config field — see hardcode-update-base-url change).
// (unified-gui-update-channel)
func defaultConfig() BrowserMCPConfig {
	return BrowserMCPConfig{
		UpdateSource: "http",
		IdleMin:      2,
		DataDir:      "",
		AutoStart:    true,
	}
}

// resolveDataDir resolves the data directory from env-or-default. This is used
// to locate the config file ITSELF — a chicken-and-egg: config.DataDir may
// point events/runlog paths elsewhere, but the config file stays at the
// env-or-default dataDir. (Documented in the design doc: a config-file data_dir
// field affects events/runlog paths but does NOT relocate the config file.)
func resolveDataDir() (string, error) {
	if p := os.Getenv(envDataDir); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	return filepath.Join(home, profile.DataDirName), nil
}

// ConfigPath returns the path to the central config file
// (<dataDir>/browser-mcp.json). The dataDir here is env-or-default, NOT
// config.DataDir (see resolveDataDir chicken-and-egg note).
func ConfigPath() string {
	dir, err := resolveDataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "browser-mcp.json")
}

// fileCache caches the parsed config file to avoid re-reading disk on every
// Load() call. The cache is keyed by ConfigPath() so env-driven dataDir changes
// (tests, isolation) invalidate it automatically. Reload() forces a re-read.
type fileCache struct {
	mu  sync.RWMutex
	path string                   // ConfigPath the file was read from (cache key)
	raw map[string]json.RawMessage // field presence map (which keys were in the file)
	cfg BrowserMCPConfig           // parsed values (zero for absent fields)
}

var cache = &fileCache{}

// loadFile returns the cached (or freshly read) file config: a presence map +
// parsed struct. Missing file = empty map + zero struct (Load applies defaults).
func loadFile() (map[string]json.RawMessage, BrowserMCPConfig) {
	path := ConfigPath()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.raw != nil && cache.path == path {
		return cache.raw, cache.cfg
	}
	// Re-read: file may have been created/modified/deleted since last read.
	raw := map[string]json.RawMessage{}
	cfg := BrowserMCPConfig{}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			// Parse into both the presence map + the typed struct. If the file
			// is malformed, raw/cfg stay zero (Load falls back to defaults).
			if json.Unmarshal(data, &raw) == nil {
				_ = json.Unmarshal(data, &cfg) // best-effort; presence is in raw
			}
		}
	}
	cache.path = path
	cache.raw = raw
	cache.cfg = cfg
	return raw, cfg
}

// invalidateCache forces the next Load() to re-read the file. Called by Save +
// Reload.
func invalidateCache() {
	cache.mu.Lock()
	cache.raw = nil
	cache.path = ""
	cache.cfg = BrowserMCPConfig{}
	cache.mu.Unlock()
}

// Load returns the effective merged config: defaults → file (present fields) →
// non-empty env. The file read is cached (keyed by ConfigPath); env is applied
// fresh on every call so runtime env changes (and test t.Setenv) are reflected
// immediately. Returns a pointer to a fresh copy each call.
func Load() *BrowserMCPConfig {
	raw, fileCfg := loadFile()
	merged := defaultConfig()
	// Overlay file fields (only those present in the file).
	if _, ok := raw["update_source"]; ok {
		merged.UpdateSource = fileCfg.UpdateSource
	}
	if _, ok := raw["idle_min"]; ok {
		merged.IdleMin = fileCfg.IdleMin
	}
	if _, ok := raw["data_dir"]; ok {
		merged.DataDir = fileCfg.DataDir
	}
	if _, ok := raw["autostart"]; ok {
		merged.AutoStart = fileCfg.AutoStart
	}
	if _, ok := raw["update_channel"]; ok {
		merged.UpdateChannel = fileCfg.UpdateChannel
	}
	// Overlay env (non-empty wins). IdleMin parse error → keep current (env
	// silently ignored, matching the legacy strconv-fallback behavior).
	if v := os.Getenv(envUpdateSource); v != "" {
		merged.UpdateSource = v
	}
	if v := os.Getenv(envUpdateIdleMin); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			merged.IdleMin = n
		}
	}
	if v := os.Getenv(envUpdateChannel); v != "" {
		merged.UpdateChannel = v
	}
	if v := os.Getenv(envDataDir); v != "" {
		merged.DataDir = v
	}
	return &merged
}

// Save marshals cfg to JSON and writes it to ConfigPath() (0600). Invalidates
// the read cache so the next Load() sees the new values.
func Save(cfg *BrowserMCPConfig) error {
	path := ConfigPath()
	if path == "" {
		return fmt.Errorf("resolve config path: data dir unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	invalidateCache()
	return nil
}

// Reload re-reads the config file and swaps the cache. Returns the list of
// changed field names (by effective value) + needsRestart=true if DataDir
// changed (events/runlog path migration is a restart-required operation).
func Reload() (reloaded []string, needsRestart bool) {
	old := Load()
	invalidateCache()
	fresh := Load()
	if old.UpdateSource != fresh.UpdateSource {
		reloaded = append(reloaded, "update_source")
	}
	if old.IdleMin != fresh.IdleMin {
		reloaded = append(reloaded, "idle_min")
	}
	if old.DataDir != fresh.DataDir {
		reloaded = append(reloaded, "data_dir")
		needsRestart = true
	}
	if old.AutoStart != fresh.AutoStart {
		reloaded = append(reloaded, "autostart")
	}
	if old.UpdateChannel != fresh.UpdateChannel {
		reloaded = append(reloaded, "update_channel")
	}
	return reloaded, needsRestart
}

// EffectiveWithSources returns each field's effective value + the layer it came
// from ("env" | "config" | "default"). Used by GET /api/config so the GUI can
// label env-overridden fields as read-only.
func EffectiveWithSources() map[string]any {
	raw, fileCfg := loadFile()
	d := defaultConfig()
	result := map[string]any{}

	result["update_source"] = fieldSource(
		os.Getenv(envUpdateSource),
		fileCfg.UpdateSource, raw, "update_source", d.UpdateSource,
	)
	result["idle_min"] = intFieldSource(
		os.Getenv(envUpdateIdleMin),
		fileCfg.IdleMin, raw, "idle_min", d.IdleMin,
	)
	result["data_dir"] = fieldSource(
		os.Getenv(envDataDir),
		fileCfg.DataDir, raw, "data_dir", d.DataDir,
	)
	result["autostart"] = boolFieldSource(
		fileCfg.AutoStart, raw, "autostart", d.AutoStart,
	)
	result["update_channel"] = fieldSource(
		os.Getenv(envUpdateChannel),
		fileCfg.UpdateChannel, raw, "update_channel", d.UpdateChannel,
	)
	return result
}

// fieldSource resolves a string field's value + from layer.
func fieldSource(envVal, fileVal string, raw map[string]json.RawMessage, key, def string) map[string]any {
	if envVal != "" {
		return map[string]any{"value": envVal, "from": "env"}
	}
	if _, ok := raw[key]; ok {
		return map[string]any{"value": fileVal, "from": "config"}
	}
	return map[string]any{"value": def, "from": "default"}
}

// intFieldSource resolves an int field's value + from layer. Env parse error →
// fall through to config/default (matching Load's behavior).
func intFieldSource(envVal string, fileVal int, raw map[string]json.RawMessage, key string, def int) map[string]any {
	if envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil {
			return map[string]any{"value": n, "from": "env"}
		}
	}
	if _, ok := raw[key]; ok {
		return map[string]any{"value": fileVal, "from": "config"}
	}
	return map[string]any{"value": def, "from": "default"}
}

// boolFieldSource resolves a bool field's value + from layer (no env for
// autostart — config-only).
func boolFieldSource(fileVal bool, raw map[string]json.RawMessage, key string, def bool) map[string]any {
	if _, ok := raw[key]; ok {
		return map[string]any{"value": fileVal, "from": "config"}
	}
	return map[string]any{"value": def, "from": "default"}
}
