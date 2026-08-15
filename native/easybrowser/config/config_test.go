package config

// config_test.go — F2.6 central config layer tests.
//
// Covers the three-layer precedence (env > config > default), missing-file
// defaults, Save/Reload round-trip, and the data_dir-change needsRestart flag.
//
// Isolation note: BROWSER_MCP_DATA_DIR is both the isolation knob AND the
// DataDir env override. Tests that need to check DataDir from the file (not env)
// use setTempHome (override USERPROFILE/HOME, clear envDataDir) so the default
// dataDir path is a temp dir without an env override.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// isolateDataDir points BROWSER_MCP_DATA_DIR at a fresh temp dir + invalidates
// the read cache. Use when the test checks non-DataDir fields (DataDir will be
// the temp dir from env — that's expected, env wins).
func isolateDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envDataDir, dir)
	invalidateCache()
	return dir
}

// setTempHome overrides the user home dir env var (USERPROFILE on Windows, HOME
// elsewhere) to a temp dir + clears BROWSER_MCP_DATA_DIR. Use when a test needs
// to check DataDir from the file without an env override (ConfigPath resolves to
// <tempHome>/.browser-mcp/browser-mcp.json).
func setTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	t.Setenv(envDataDir, "")
	invalidateCache()
	return home
}

// writeFile writes a JSON body to ConfigPath() + invalidates the read cache.
func writeFile(t *testing.T, body string) {
	t.Helper()
	path := ConfigPath()
	if path == "" {
		t.Fatal("ConfigPath empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	invalidateCache()
}

// writeFileNoInvalidate writes the file WITHOUT invalidating the cache. Use for
// Reload tests where the cache must still hold the old state when Reload runs
// (simulates an external GUI write — different process, bridge cache stale).
func writeFileNoInvalidate(t *testing.T, body string) {
	t.Helper()
	path := ConfigPath()
	if path == "" {
		t.Fatal("ConfigPath empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestLoad_MissingFile_Defaults: no config file → all defaults (DataDir = env
// isolation dir since env wins for DataDir).
func TestLoad_MissingFile_Defaults(t *testing.T) {
	dir := isolateDataDir(t)
	cfg := Load()
	if cfg.UpdateSource != "http" {
		t.Errorf("UpdateSource=%q want http (hardcoded default)", cfg.UpdateSource)
	}
	if cfg.IdleMin != 2 {
		t.Errorf("IdleMin=%d want 2", cfg.IdleMin)
	}
	if cfg.DataDir != dir {
		t.Errorf("DataDir=%q want %q (env isolation)", cfg.DataDir, dir)
	}
	if !cfg.AutoStart {
		t.Errorf("AutoStart=%v want true", cfg.AutoStart)
	}
}

// TestLoad_ConfigOverridesDefault: file values beat defaults (non-DataDir
// fields; DataDir is env-overridden by the isolation knob).
func TestLoad_ConfigOverridesDefault(t *testing.T) {
	isolateDataDir(t)
	writeFile(t, `{"update_source":"http","update_base":"http://dist.intranet/bm","idle_min":5,"autostart":false}`)
	cfg := Load()
	if cfg.UpdateSource != "http" {
		t.Errorf("UpdateSource=%q want http", cfg.UpdateSource)
	}
	if cfg.IdleMin != 5 {
		t.Errorf("IdleMin=%d want 5", cfg.IdleMin)
	}
	if cfg.AutoStart {
		t.Errorf("AutoStart=%v want false (file)", cfg.AutoStart)
	}
}

// TestLoad_EnvWinsOverConfig: env beats file values (env is the top layer).
// DataDir is not changed here — the isolation env already wins for it.
// Note: update_base in the file/env is silently ignored — the field was
// removed from the schema (hardcode-update-base-url change); http base URL is
// always profile.UpdateBase (ldflag-baked), not a config field.
func TestLoad_EnvWinsOverConfig(t *testing.T) {
	isolateDataDir(t)
	writeFile(t, `{"update_source":"http","update_base":"http://file/bm","idle_min":5,"autostart":false}`)
	t.Setenv(envUpdateSource, "gitee")
	t.Setenv(envUpdateIdleMin, "10")
	cfg := Load()
	if cfg.UpdateSource != "gitee" {
		t.Errorf("UpdateSource=%q want gitee (env)", cfg.UpdateSource)
	}
	if cfg.IdleMin != 10 {
		t.Errorf("IdleMin=%d want 10 (env)", cfg.IdleMin)
	}
	// AutoStart has no env — stays from file.
	if cfg.AutoStart {
		t.Errorf("AutoStart=%v want false (file, no env override)", cfg.AutoStart)
	}
}

// TestLoad_DataDirEnvWinsOverFile: env BROWSER_MCP_DATA_DIR wins over a file
// data_dir field. The file lives at <envDir>/browser-mcp.json (the env locates
// the file AND overrides the data_dir field inside it).
func TestLoad_DataDirEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envDataDir, dir)
	invalidateCache()
	writeFile(t, `{"data_dir":"/from-file"}`)
	cfg := Load()
	if cfg.DataDir != dir {
		t.Fatalf("DataDir=%q want %q (env wins over file)", cfg.DataDir, dir)
	}
}

// TestLoad_DataDirConfigOverridesDefault: with no env, a file data_dir field
// overrides the default (""). Uses setTempHome so ConfigPath resolves to a temp
// home (no BROWSER_MCP_DATA_DIR env).
func TestLoad_DataDirConfigOverridesDefault(t *testing.T) {
	setTempHome(t)
	writeFile(t, `{"data_dir":"/from-file"}`)
	cfg := Load()
	if cfg.DataDir != "/from-file" {
		t.Fatalf("DataDir=%q want /from-file (config overrides default)", cfg.DataDir)
	}
}

// TestLoad_DataDirDefaultEmpty: with no env and no file, DataDir="" (resolved
// to ~/.browser-mcp by the caller, not the config layer).
func TestLoad_DataDirDefaultEmpty(t *testing.T) {
	setTempHome(t)
	cfg := Load()
	if cfg.DataDir != "" {
		t.Fatalf("DataDir=%q want empty (default)", cfg.DataDir)
	}
}

// TestLoad_EnvGarbageFallsBack: a non-integer env for idle_min is silently
// ignored (falls back to config/default), matching legacy strconv behavior.
func TestLoad_EnvGarbageFallsBack(t *testing.T) {
	isolateDataDir(t)
	writeFile(t, `{"idle_min":7}`)
	t.Setenv(envUpdateIdleMin, "garbage")
	cfg := Load()
	if cfg.IdleMin != 7 {
		t.Errorf("IdleMin=%d want 7 (file, garbage env ignored)", cfg.IdleMin)
	}
}

// TestLoad_PartialFile: a file with only some fields set leaves the rest at
// default (not zero).
func TestLoad_PartialFile(t *testing.T) {
	isolateDataDir(t)
	writeFile(t, `{"idle_min":0}`) // idle_min=0 is a deliberate "disable"
	cfg := Load()
	if cfg.IdleMin != 0 {
		t.Errorf("IdleMin=%d want 0 (file, disabled)", cfg.IdleMin)
	}
	if cfg.UpdateSource != "http" {
		t.Errorf("UpdateSource=%q want http (hardcoded default, not in file)", cfg.UpdateSource)
	}
	if !cfg.AutoStart {
		t.Errorf("AutoStart=%v want true (default, not in file)", cfg.AutoStart)
	}
}

// TestSave_RoundTrip: Save writes the file + next Load returns the saved values
// (non-DataDir fields; DataDir is env-overridden by the isolation knob).
func TestSave_RoundTrip(t *testing.T) {
	isolateDataDir(t)
	want := &BrowserMCPConfig{
		UpdateSource:  "http",
		IdleMin:       42,
		AutoStart:     false,
		UpdateChannel: "sit",
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := Load()
	if got.UpdateSource != want.UpdateSource ||
		got.IdleMin != want.IdleMin || got.AutoStart != want.AutoStart ||
		got.UpdateChannel != want.UpdateChannel {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", *got, *want)
	}
}

// TestLoad_UpdateChannel_Env: env BROWSER_MCP_UPDATE_CHANNEL overrides file +
// default; empty env falls through to file/default.
func TestLoad_UpdateChannel_Env(t *testing.T) {
	isolateDataDir(t)
	if err := Save(&BrowserMCPConfig{UpdateChannel: "sit"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := Load().UpdateChannel; got != "sit" {
		t.Fatalf("file value: got %q want sit", got)
	}
	t.Setenv(envUpdateChannel, "stable")
	if got := Load().UpdateChannel; got != "stable" {
		t.Fatalf("env override: got %q want stable", got)
	}
	t.Setenv(envUpdateChannel, "")
	if got := Load().UpdateChannel; got != "sit" {
		t.Fatalf("empty env should fall through to file: got %q want sit", got)
	}
}

// TestSave_PersistsDataDirField: Save writes data_dir to the file even though
// Load().DataDir is env-overridden (verify by reading the raw file).
func TestSave_PersistsDataDirField(t *testing.T) {
	isolateDataDir(t)
	want := &BrowserMCPConfig{DataDir: "/persisted"}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := loadFile()
	if _, ok := raw["data_dir"]; !ok {
		t.Fatal("data_dir not in saved file")
	}
}

// TestReload_DetectsChanges: Reload re-reads the file + reports changed fields.
// Uses writeFileNoInvalidate so the cache holds the old state when Reload runs.
func TestReload_DetectsChanges(t *testing.T) {
	isolateDataDir(t)
	writeFile(t, `{"update_source":"gitee","idle_min":30}`)
	old := Load()
	if old.UpdateSource != "gitee" {
		t.Fatalf("initial Load UpdateSource=%q want gitee", old.UpdateSource)
	}
	// External write (GUI) — does NOT invalidate the bridge's cache.
	writeFileNoInvalidate(t, `{"update_source":"http","idle_min":5,"autostart":false}`)
	reloaded, needsRestart := Reload()
	fresh := Load()

	wantFields := map[string]bool{"update_source": false, "idle_min": false, "autostart": false}
	for _, f := range reloaded {
		if _, ok := wantFields[f]; ok {
			wantFields[f] = true
		}
	}
	for f, seen := range wantFields {
		if !seen {
			t.Errorf("Reload did not report changed field %q (got %v)", f, reloaded)
		}
	}
	if needsRestart {
		t.Errorf("needsRestart=true want false (data_dir unchanged)")
	}
	if fresh.UpdateSource != "http" || fresh.IdleMin != 5 || fresh.AutoStart {
		t.Fatalf("fresh Load after Reload mismatch: %+v", *fresh)
	}
}

// TestReload_DataDirChangeNeedsRestart: changing data_dir sets needsRestart.
// Uses setTempHome (no env DataDir) so the file's data_dir is the effective value.
func TestReload_DataDirChangeNeedsRestart(t *testing.T) {
	setTempHome(t)
	writeFile(t, `{"data_dir":"/old"}`)
	_ = Load()
	writeFileNoInvalidate(t, `{"data_dir":"/new"}`)
	reloaded, needsRestart := Reload()
	found := false
	for _, f := range reloaded {
		if f == "data_dir" {
			found = true
		}
	}
	if !found {
		t.Fatalf("data_dir not in reloaded fields: %v", reloaded)
	}
	if !needsRestart {
		t.Fatalf("needsRestart=false want true (data_dir changed)")
	}
}

// TestReload_NoChanges: Reload with an unchanged file reports nothing changed.
func TestReload_NoChanges(t *testing.T) {
	isolateDataDir(t)
	writeFile(t, `{"update_source":"http","idle_min":5}`)
	_ = Load()
	writeFileNoInvalidate(t, `{"update_source":"http","idle_min":5}`)
	reloaded, needsRestart := Reload()
	if len(reloaded) != 0 {
		t.Errorf("reloaded=%v want empty", reloaded)
	}
	if needsRestart {
		t.Errorf("needsRestart=true want false")
	}
}

// TestEffectiveWithSources_Layers: confirms the `from` label per field.
// update_base is NOT in the effective map (field removed from schema per
// hardcode-update-base-url; http base is always profile.UpdateBase).
func TestEffectiveWithSources_Layers(t *testing.T) {
	dir := isolateDataDir(t)
	writeFile(t, `{"update_source":"http","idle_min":7,"autostart":false}`)

	eff := EffectiveWithSources()

	// update_source: from config (file has it, no env).
	if v := eff["update_source"].(map[string]any); v["from"] != "config" || v["value"] != "http" {
		t.Errorf("update_source=%+v want config/http", v)
	}
	// update_base must be ABSENT from the effective map (field removed).
	if _, ok := eff["update_base"]; ok {
		t.Errorf("update_base should not be in EffectiveWithSources (field removed)")
	}
	// idle_min: from config.
	if v := eff["idle_min"].(map[string]any); v["from"] != "config" || v["value"].(int) != 7 {
		t.Errorf("idle_min=%+v want config/7", v)
	}
	// data_dir: from env (isolation env).
	if v := eff["data_dir"].(map[string]any); v["from"] != "env" || v["value"] != dir {
		t.Errorf("data_dir=%+v want env/%q", v, dir)
	}
	// autostart: from config (no env for autostart).
	if v := eff["autostart"].(map[string]any); v["from"] != "config" || v["value"].(bool) != false {
		t.Errorf("autostart=%+v want config/false", v)
	}
}

// TestEffectiveWithSources_AllDefault: no file, no env (beyond isolation) → all
// non-DataDir fields from default. DataDir is from env (isolation).
func TestEffectiveWithSources_AllDefault(t *testing.T) {
	dir := isolateDataDir(t)
	eff := EffectiveWithSources()
	for _, key := range []string{"update_source", "idle_min", "autostart"} {
		v, ok := eff[key].(map[string]any)
		if !ok {
			t.Fatalf("%s not a map: %T", key, eff[key])
		}
		if v["from"] != "default" {
			t.Errorf("%s from=%v want default", key, v["from"])
		}
	}
	// DataDir is from env (isolation knob).
	if v := eff["data_dir"].(map[string]any); v["from"] != "env" || v["value"] != dir {
		t.Errorf("data_dir=%+v want env/%q", v, dir)
	}
}

// TestConfigPath_HonorsEnvDataDir: ConfigPath tracks BROWSER_MCP_DATA_DIR.
func TestConfigPath_HonorsEnvDataDir(t *testing.T) {
	dir := isolateDataDir(t)
	path := ConfigPath()
	want := filepath.Join(dir, "browser-mcp.json")
	if path != want {
		t.Fatalf("ConfigPath=%q want %q", path, want)
	}
}

// TestLoad_CacheKeyedByPath: switching BROWSER_MCP_DATA_DIR invalidates the
// cache (keyed by ConfigPath) so the file at the new path is re-read.
func TestLoad_CacheKeyedByPath(t *testing.T) {
	dir1 := t.TempDir()
	t.Setenv(envDataDir, dir1)
	invalidateCache()
	cfg1 := Load()
	if cfg1.IdleMin != 2 {
		t.Fatalf("dir1 IdleMin=%d want 2 (default, no file)", cfg1.IdleMin)
	}

	// Write a file in dir1 + confirm Load sees it.
	writeFile(t, `{"idle_min":99}`)
	if cfg := Load(); cfg.IdleMin != 99 {
		t.Fatalf("after write IdleMin=%d want 99", cfg.IdleMin)
	}

	dir2 := t.TempDir()
	t.Setenv(envDataDir, dir2)
	cfg2 := Load()
	if cfg2.IdleMin != 2 {
		t.Fatalf("dir2 IdleMin=%d want 2 (different path invalidates cache)", cfg2.IdleMin)
	}
	// Switch back to dir1 — cache should re-read dir1's file.
	t.Setenv(envDataDir, dir1)
	cfg3 := Load()
	if cfg3.IdleMin != 99 {
		t.Fatalf("back to dir1 IdleMin=%d want 99 (cache re-read)", cfg3.IdleMin)
	}
}

// TestLoad_IdleMinZeroFromEnvDisabled: env=0 → disabled (<=0 semantics).
func TestLoad_IdleMinZeroFromEnvDisabled(t *testing.T) {
	isolateDataDir(t)
	t.Setenv(envUpdateIdleMin, "0")
	cfg := Load()
	if cfg.IdleMin != 0 {
		t.Fatalf("IdleMin=%d want 0 (env disabled)", cfg.IdleMin)
	}
}

// TestLoad_IdleMinNegativeFromEnv: env=-1 → disabled.
func TestLoad_IdleMinNegativeFromEnv(t *testing.T) {
	isolateDataDir(t)
	t.Setenv(envUpdateIdleMin, strconv.Itoa(-1))
	cfg := Load()
	if cfg.IdleMin != -1 {
		t.Fatalf("IdleMin=%d want -1", cfg.IdleMin)
	}
}
