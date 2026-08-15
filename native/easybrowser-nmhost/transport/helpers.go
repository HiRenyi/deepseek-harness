package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/browser-mcp/nm-host/profile"
)

const (
	dataDirEnv  = "BROWSER_MCP_DATA_DIR"  // absolute path override for the data directory
	tokenDirEnv = "BROWSER_MCP_TOKEN_DIR" // absolute path override for the TCP token dir
	tokenName   = "browser-mcp-token"     // TCP+token fallback auth file name (must match bridge side)
)

// dataDir mirrors bridge/transport/helpers.go: the persistent per-user data
// directory. MUST be identical to the bridge side so both processes resolve the
// same deterministic pipe name. NEVER os.TempDir() (gets cleaned → nm-host orphaned).
//
// Default: ~/<profile.DataDirName> (os.UserHomeDir(), per-OS consistent). The
// basename is profile-baked so test (.browser-mcp) and prod (.browser-mcp-prod)
// nm-hosts resolve separate dirs and thus separate pipe hashes. Override with
// BROWSER_MCP_DATA_DIR (wins outright, for isolation/testing).
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

// tokenFilePath mirrors bridge/transport/helpers.go tokenFilePath: the path to
// the TCP+token fallback auth file. MUST resolve identically to the bridge
// side so the reader finds the file the writer created.
//
// CONTRACT (do not drift): persistent data dir, NEVER os.TempDir() by default.
// When transport-resilience moved addr.json into dataDir(), the TCP token was
// missed and stayed in os.TempDir() on the bridge write side; nm-host read it
// from temp too. Once the system cleaned temp, the TCP fallback could not
// authenticate and "Native host has exited" storms recurred.
// BROWSER_MCP_TOKEN_DIR (absolute dir) overrides for tests/isolation. Returns
// "" only via temp last resort when the user cache dir is unresolvable.
func tokenFilePath() string {
	if p := os.Getenv(tokenDirEnv); p != "" {
		return filepath.Join(p, tokenName)
	}
	if dir, err := dataDir(); err == nil {
		return filepath.Join(dir, tokenName)
	}
	return filepath.Join(os.TempDir(), tokenName)
}

// DeterministicPipeName returns a stable Windows Named Pipe name derived from
// the data directory. MIRRORS bridge/transport/helpers.go deterministicPipeName
// (逐字节相同) — both sides MUST produce identical names from identical dataDir
// so nm-host connects to bridge's pipe without addr.json discovery.
//
// Exported because nm-host/main (datadir.go in package main) calls it across
// the package boundary. Bridge side is unexported (same-package use only).
func DeterministicPipeName(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return `\\.\pipe\browser-mcp-` + hex.EncodeToString(h[:4])
}

// DeterministicUnixSocketName returns a stable Unix socket path derived from
// the data directory. MIRRORS bridge/transport/server_unixsock.go
// deterministicUnixSocketPath — both sides MUST produce identical paths from
// identical dataDir so nm-host connects to bridge's socket without addr.json.
// Used on Mac/Linux only; Windows uses DeterministicPipeName.
func DeterministicUnixSocketName(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return filepath.Join(dir, "browser-mcp-"+hex.EncodeToString(h[:4])+".sock")
}
