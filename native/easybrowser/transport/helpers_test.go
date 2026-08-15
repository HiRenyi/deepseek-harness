package transport

import (
	"os"
	"path/filepath"
	"testing"
)

// P1.1 cross-platform: default data dir is ~/.browser-mcp (UserHomeDir),
// NOT %LOCALAPPDATA%\browser-mcp (UserCacheDir). BROWSER_MCP_DATA_DIR still wins.
func TestDataDir_DefaultIsUserHomeDotBrowserMcp(t *testing.T) {
	t.Setenv(dataDirEnv, "") // clear override -> exercise default branch

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	got, err := dataDir()
	if err != nil {
		t.Fatalf("dataDir: %v", err)
	}
	want := filepath.Join(home, ".browser-mcp")
	if got != want {
		t.Fatalf("dataDir=%q, want %q (UserHomeDir/.browser-mcp, not UserCacheDir/browser-mcp)", got, want)
	}
}

// BROWSER_MCP_DATA_DIR override must still win over the default.
func TestDataDir_OverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)

	got, err := dataDir()
	if err != nil {
		t.Fatalf("dataDir: %v", err)
	}
	if got != dir {
		t.Fatalf("dataDir=%q, want override %q", got, dir)
	}
}
