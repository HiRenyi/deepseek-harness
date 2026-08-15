//go:build windows

package main

import (
	"testing"

	"github.com/browser-mcp/bridge/profile"
)

// TestSelfHealAutostart_ReRegistersIfMissing: with the HKCU Run entry absent,
// selfHealAutostart re-registers it pointing at the current test exe with
// --boot. Idempotent + cleaned up in t.Cleanup so no residue.
func TestSelfHealAutostart_ReRegistersIfMissing(t *testing.T) {
	t.Cleanup(func() { _ = deleteBridgeAutostartEntry() })

	// Start from a known-clean state.
	if err := deleteBridgeAutostartEntry(); err != nil {
		t.Fatalf("initial delete: %v", err)
	}
	if exists, _ := bridgeAutostartEntryExists(); exists {
		t.Fatalf("entry still present after delete")
	}

	// Self-heal → entry must be re-registered.
	selfHealAutostart()
	exists, err := bridgeAutostartEntryExists()
	if err != nil {
		t.Fatalf("exists check after self-heal: %v", err)
	}
	if !exists {
		t.Fatalf("entry NOT re-registered by selfHealAutostart")
	}
}

// TestSelfHealAutostart_NoOpIfPresent: with the entry already present (e.g.
// a sentinel value written by a prior install), selfHealAutostart must NOT
// overwrite it (idempotent no-op — don't fight a registration another path
// just made).
func TestSelfHealAutostart_NoOpIfPresent(t *testing.T) {
	t.Cleanup(func() { _ = deleteBridgeAutostartEntry() })

	// Pre-register with a sentinel value (simulates install.Run's registration).
	sentinel := `"C:\sentinel\bridge-prod.exe" --boot`
	if err := setBridgeAutostartEntry(sentinel); err != nil {
		t.Fatalf("pre-set sentinel: %v", err)
	}

	// selfHealAutostart must be a no-op (entry exists).
	selfHealAutostart()

	// Re-read + assert unchanged.
	got, err := getBridgeAutostartEntry()
	if err != nil {
		t.Fatalf("read entry after self-heal: %v", err)
	}
	if got != sentinel {
		t.Fatalf("selfHealAutostart overwrote existing entry: got %q want %q (must be no-op)", got, sentinel)
	}
}

// TestBridgeAutostartEntryName_MatchesDesktopContract guards that bridge's
// self-heal writes the SAME entry name desktop/autostart registers (both
// derive from AutostartKey + "Bridge"). If they diverge, self-heal would
// write a duplicate entry instead of healing the install's entry.
func TestBridgeAutostartEntryName_MatchesDesktopContract(t *testing.T) {
	got := bridgeAutostartEntryName()
	want := profile.AutostartKey + "Bridge"
	if got != want {
		t.Fatalf("entry name = %q, want %q", got, want)
	}
	// Must NOT equal the legacy "BrowserMCP-Bridge" (with hyphen) that
	// RemoveLegacyBridgeAutostart deletes.
	if got == "BrowserMCP-Bridge" {
		t.Fatalf("entry name collides with legacy BrowserMCP-Bridge")
	}
}
