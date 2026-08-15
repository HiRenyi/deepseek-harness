package transport

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTokenFilePath_PersistentDir pins the contract fix: the TCP+token fallback
// file MUST resolve to the persistent data dir, NEVER os.TempDir() by default.
// transport-resilience moved addr.json/lock into dataDir() but missed the token;
// that orphaned nm-host's TCP fallback when temp got cleaned.
func TestTokenFilePath_PersistentDir(t *testing.T) {
	t.Setenv(dataDirEnv, "")
	t.Setenv(tokenDirEnv, "")

	got := tokenFilePath()

	// Must NOT be the raw temp dir default.
	if filepath.Dir(got) == os.TempDir() {
		t.Fatalf("tokenFilePath() resolved to temp dir %q (contract violation); got %s", os.TempDir(), got)
	}
	// Must end with the token file name.
	if filepath.Base(got) != tokenName {
		t.Fatalf("tokenFilePath() base = %q, want %q", filepath.Base(got), tokenName)
	}

	// BROWSER_MCP_TOKEN_DIR overrides the directory.
	override := t.TempDir()
	t.Setenv(tokenDirEnv, override)
	if got := tokenFilePath(); got != filepath.Join(override, tokenName) {
		t.Fatalf("BROWSER_MCP_TOKEN_DIR override: got %s, want %s", got, filepath.Join(override, tokenName))
	}
}

// TestWriteTokenFile_LandsInDataDir verifies the writer honors the data-dir
// contract end to end (dir created, file written, TokenFile() reflects it).
func TestWriteTokenFile_LandsInDataDir(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(dataDirEnv, dataDir)
	t.Setenv(tokenDirEnv, "")

	s := NewTCPServer("127.0.0.1:0")
	s.token = "test-token-value"

	if err := s.writeTokenFile(); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}

	wantPath := filepath.Join(dataDir, tokenName)
	if s.TokenFile() != wantPath {
		t.Fatalf("TokenFile() = %s, want %s", s.TokenFile(), wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read written token: %v", err)
	}
	if string(data) != "test-token-value\n" {
		t.Fatalf("token content = %q, want %q", string(data), "test-token-value\\n")
	}
}
