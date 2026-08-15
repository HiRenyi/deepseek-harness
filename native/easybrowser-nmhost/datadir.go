package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/browser-mcp/nm-host/profile"
)

// bridgeDataDir mirrors bridge/transport/helpers.go dataDir(): the persistent
// per-user data directory. Contract (see CLAUDE.md "环境契约"): NEVER use
// os.TempDir() for discovery/lock/log — it gets garbage-collected.
//
// Default: ~/<profile.DataDirName> (os.UserHomeDir(), per-OS consistent). The
// basename is profile-baked so test (.browser-mcp) and prod (.browser-mcp-prod)
// nm-hosts resolve separate dirs and stay paired with their matching bridge
// (pipe hash + lock + token all key off this). Override with
// BROWSER_MCP_DATA_DIR (wins outright).
//
// Cross-platform pivot (P1.1): was os.UserCacheDir()/browser-mcp
// (Windows: %LOCALAPPDATA%\browser-mcp); moved to UserHomeDir for cross-OS parity.
func bridgeDataDir() (string, error) {
	if p := os.Getenv("BROWSER_MCP_DATA_DIR"); p != "" {
		return p, nil
	}
	base, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	return filepath.Join(base, profile.DataDirName), nil
}

