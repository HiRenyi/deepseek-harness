package main

// update_spawn.go — W1.8 / W3.1: bridge version-detect → spawn GUI --update-prompt.
//
// When the idle version-check loop (update_idle.go) detects a newer bridge
// version is available, instead of self-updating bridge (auto-install is
// disabled pending the Wails GUI), bridge spawns the Wails desktop GUI in
// --update-prompt mode so the user is shown an update prompt and can trigger
// the rename-swap self-update from the GUI (T7).
//
// The GUI binary lives at <dataDir>/bin/BrowserMCP[.exe]. T6 only *returns*
// that path string and attempts a detached spawn; whether the file actually
// exists is a runtime concern (T7 / install wizard place the GUI there). A
// failed spawn is logged + swallowed so bridge keeps running.
//
// Throttling: the idle loop ticks every 60s; to avoid spawning a fresh GUI
// process on every tick we gate spawns with a package-level lastSpawnedAt +
// minSpawnInterval (1h). This is a simple stand-in — a richer remind-gate
// (next_remind_at / ignored_versions) can layer on later.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/profile"
)

// minSpawnInterval is the minimum gap between two --update-prompt spawns for
// the same bridge process. Repeated idle ticks inside this window are swallowed
// (the GUI is already up / the user has already been prompted).
const minSpawnInterval = time.Hour

var (
	spawnMu        sync.Mutex
	lastSpawnedAt  time.Time
	lastSpawnedVer string
)

// bridgeDataDir resolves the persistent per-user data directory used by the
// bridge main package. Mirrors transport.dataDir()'s contract (CLAUDE.md env
// contract): default ~/.browser-mcp, override via BROWSER_MCP_DATA_DIR. Now
// delegates to the central config layer (config.Load().DataDir) so the
// browser-mcp.json data_dir field + env are both honored (env still wins via
// the config layer). When config.DataDir is empty (neither env nor file set
// it), falls back to ~/.browser-mcp — matching the original behavior.
//
// Note: the config FILE itself is located via config.resolveDataDir() (env or
// default), NOT via this function — a config-file data_dir field affects
// events/runlog paths but does NOT relocate the config file (chicken-and-egg,
// see config.ConfigPath docs).
func bridgeDataDir() (string, error) {
	if d := config.Load().DataDir; d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	return filepath.Join(home, profile.DataDirName), nil
}

// currentVersion is the version used for self-update comparison. Precedence
// (B6, RFC §3.2.4):
//  1. <dataDir>/bridge_version — bridge's own truth-source file, written by
//     bridge on startup (= build-time `version` var, B7) AND after a
//     successful self-update (= manifest version, B1). Preferred because it
//     converges after a self-update: the new bridge records the new version,
//     so version.json's same version is no longer "newer".
//  2. <dataDir>/installed_version — the GUI-recorded release version (written
//     by the GUI's --post-update after a GUI-package self-update). Kept for
//     GUI-package-update compat so a GUI-managed bridge still converges when
//     bridge_version is absent.
//  3. build-time `version` var (git-describe) — fallback for a fresh install
//     before any self-update / GUI install recorded a version.
//
// Why bridge_version is preferred over the build-time var: the `version` var
// is baked at build time + stays stale across GUI self-updates (the extracted
// bridge is the same build), so version.json "0.2.2" would ALWAYS compare
// newer than the running bridge's "v0.2.1-30-g..." → the update loop never
// converges. Reading the recorded version makes it converge.
func currentVersion() string {
	if dir, err := bridgeDataDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(dir, "bridge_version")); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
		// GUI 包整包更新兼容：bridge_version 缺失时回退 GUI 写的 installed_version。
		if b, err := os.ReadFile(filepath.Join(dir, "installed_version")); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return version
}

// defaultGUIPath returns the expected path to the Wails desktop GUI binary
// (<dataDir>/bin/BrowserMCP, with a .exe suffix on Windows). The file may not
// exist yet (T7 / install wizard place it); callers must handle spawn failure.
func defaultGUIPath() (string, error) {
	dir, err := bridgeDataDir()
	if err != nil {
		return "", err
	}
	name := "BrowserMCP"
	if runtime.GOOS == "windows" {
		name = "BrowserMCP.exe"
	}
	return filepath.Join(dir, "bin", name), nil
}

// spawnUpdatePrompt launches the GUI binary in --update-prompt mode with the
// detected new version, detached so bridge does NOT block on it. Returns nil
// on a successful Start (the GUI is on its own). Errors (missing path, Start
// failure) are returned so the caller can log + swallow.
//
// Throttled: returns nil (treated as "already spawned recently") when a spawn
// for the same version happened within minSpawnInterval.
func spawnUpdatePrompt(guiPath, newVersion string) error {
	if guiPath == "" {
		return errors.New("spawnUpdatePrompt: guiPath is empty")
	}
	if newVersion == "" {
		return errors.New("spawnUpdatePrompt: newVersion is empty")
	}

	spawnMu.Lock()
	if !lastSpawnedAt.IsZero() && time.Since(lastSpawnedAt) < minSpawnInterval && lastSpawnedVer == newVersion {
		spawnMu.Unlock()
		return nil // throttled — GUI already prompted for this version recently
	}
	spawnMu.Unlock()

	cmd := exec.Command(guiPath, "--update-prompt", "--new-version", newVersion)
	cmd.Env = os.Environ()
	detachSpawn(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start GUI %s --update-prompt: %w", guiPath, err)
	}

	spawnMu.Lock()
	lastSpawnedAt = time.Now()
	lastSpawnedVer = newVersion
	spawnMu.Unlock()
	return nil
}
