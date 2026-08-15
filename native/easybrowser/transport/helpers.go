package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/browser-mcp/bridge/profile"
)

const (
	dataDirEnv   = "BROWSER_MCP_DATA_DIR"  // absolute path override for the data directory
	tokenDirEnv  = "BROWSER_MCP_TOKEN_DIR" // absolute path override for the TCP token dir
	lockName     = "bridge.lock"           // occupancy-protection lock file name
	tokenName    = "browser-mcp-token"     // TCP+token fallback auth file name
)

// dataDir returns the persistent, per-user data directory used by browser-mcp
// for occupancy lock, TCP token, and logs.
//
// IMPORTANT (contract, do not drift): this MUST be a persistent user-owned
// directory, NEVER a temp directory (os.TempDir gets garbage-collected, which
// orphaned nm-host — see transport-resilience change). Default: ~/.browser-mcp
// (os.UserHomeDir(), per-OS: Windows C:\Users\<u>\.browser-mcp; Mac /Users/<u>;
// Linux /home/<u>). Override with BROWSER_MCP_DATA_DIR (tests/isolation).
//
// Cross-platform pivot (cross-platform-distribution P1.1): previously
// os.UserCacheDir()/browser-mcp (Windows: %LOCALAPPDATA%\browser-mcp); moved to
// UserHomeDir so all OSes share one consistent path.
func dataDir() (string, error) {
	if p := os.Getenv(dataDirEnv); p != "" {
		return p, nil
	}
	base, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	return filepath.Join(base, profile.DataDirName), nil
}

// lockFilePath returns the occupancy-lock file path inside the data dir.
func lockFilePath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, lockName), nil
}

// tokenFilePath returns the path to the TCP+token fallback auth file.
//
// CONTRACT (do not drift — same rule as lock): this MUST live in the
// persistent, per-user data dir, NEVER os.TempDir(). When transport-resilience
// moved addr.json/lock into dataDir(), the TCP token was missed and stayed in
// os.TempDir(); once the system cleaned it, nm-host's TCP fallback could no longer
// authenticate and "Native host has exited" storms recurred. BROWSER_MCP_TOKEN_DIR
// (absolute path) overrides for tests/isolation. Returns "" only if the user
// cache dir is unresolvable (last-resort temp), matching the data-dir policy.
func tokenFilePath() string {
	if p := os.Getenv(tokenDirEnv); p != "" {
		return filepath.Join(p, tokenName)
	}
	dir, err := dataDir()
	if err != nil {
		// last resort only; mirror the data-dir fallback policy.
		return filepath.Join(os.TempDir(), tokenName)
	}
	return filepath.Join(dir, tokenName)
}

// generateUUID generates a new UUID v4 string.
func generateUUID() string {
	return uuid.New().String()
}

// deterministicPipeName returns a stable Windows Named Pipe name derived from
// the data directory, so bridge and nm-host independently compute the SAME name
// without exchanging it via addr.json. Different BROWSER_MCP_DATA_DIR ->
// different name -> instance isolation. Aligned with Codex's deterministic
// pipe-name approach (codex-browser-use-{uuid}); we use sha256(dataDir)[:8]
// instead of a session UUID (no multi-session concurrency on single user/machine).
//
// Callers MUST pass the already-resolved absolute dataDir string (the return
// value of dataDir()), not an env var name. Both sides use the same dataDir()
// logic so inputs match -> hashes match.
func deterministicPipeName(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return `\\.\pipe\browser-mcp-` + hex.EncodeToString(h[:4])
}

// deterministicUnixSocketPath returns a stable Unix socket path derived from
// the data directory. MIRRORS nm-host/transport.DeterministicUnixSocketName —
// both sides MUST produce identical paths from identical dataDir so nm-host
// connects to bridge's socket without addr.json discovery. Mac/Linux only.
func deterministicUnixSocketPath(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return filepath.Join(dir, "browser-mcp-"+hex.EncodeToString(h[:4])+".sock")
}
