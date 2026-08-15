package main

import (
	"strings"
	"testing"
	"time"
)

// TestSpawnUpdatePrompt_MissingPath: empty guiPath returns an error and does
// not attempt a spawn. (End-to-end spawn is covered by DV Task 13/15, not a
// unit test — we only assert the guard.)
func TestSpawnUpdatePrompt_MissingPath(t *testing.T) {
	err := spawnUpdatePrompt("", "0.2.1")
	if err == nil {
		t.Fatal("spawnUpdatePrompt(\"\") = nil, want error")
	}
	if !strings.Contains(err.Error(), "guiPath") {
		t.Fatalf("error %q does not mention guiPath", err)
	}
}

// TestSpawnUpdatePrompt_EmptyVersion: empty newVersion returns an error.
func TestSpawnUpdatePrompt_EmptyVersion(t *testing.T) {
	err := spawnUpdatePrompt("/some/path", "")
	if err == nil {
		t.Fatal("spawnUpdatePrompt(empty version) = nil, want error")
	}
	if !strings.Contains(err.Error(), "newVersion") {
		t.Fatalf("error %q does not mention newVersion", err)
	}
}

// TestSpawnUpdatePrompt_NonexistentPath: a path that does not exist should
// return an error from Start (we do NOT assert the GUI runs). This also guards
// that we don't panic on a missing binary.
func TestSpawnUpdatePrompt_NonexistentPath(t *testing.T) {
	// Reset throttle so this call isn't deduped by a prior test's state.
	spawnMu.Lock()
	lastSpawnedAt = time.Time{}
	lastSpawnedVer = ""
	spawnMu.Unlock()

	err := spawnUpdatePrompt("/definitely/does/not/exist/BrowserMCP", "9.9.9")
	if err == nil {
		t.Fatal("spawnUpdatePrompt(nonexistent path) = nil, want Start error")
	}
}

// TestDefaultGUIPath_HasBinSuffix: defaultGUIPath returns a path ending in
// bin/BrowserMCP[.exe] (platform-dependent, just assert the dir + base name).
func TestDefaultGUIPath_HasBinSuffix(t *testing.T) {
	p, err := defaultGUIPath()
	if err != nil {
		t.Fatalf("defaultGUIPath: %v", err)
	}
	if !strings.HasSuffix(strings.ReplaceAll(p, "\\", "/"), "/bin/BrowserMCP") && !strings.HasSuffix(p, "BrowserMCP.exe") {
		t.Fatalf("defaultGUIPath = %q, want suffix bin/BrowserMCP[.exe]", p)
	}
}
