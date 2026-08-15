package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/browser-mcp/nm-host/transport"
)

// addr.json missing + resolvable dataDir -> deterministic pipe (NOT tcp).
// This is the core storm-fix assertion.
func TestResolveBridgeAddr_AddrFileMissing_ReturnsDeterministicPipe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dir)

	a, err := resolveBridgeAddr()
	if err != nil {
		t.Fatalf("expected nil err (deterministic pipe is a valid address), got %v", err)
	}
	if a.Network != "pipe" {
		t.Fatalf("Network=%q, want \"pipe\" (must NOT degrade to tcp)", a.Network)
	}
	expected := transport.DeterministicPipeName(dir)
	if a.Address != expected {
		t.Fatalf("Address=%q, want deterministic %q", a.Address, expected)
	}
}

// stale TCP addr.json present -> nm-host MUST IGNORE it and still return the
// deterministic pipe (NOT the stale TCP). D2: addr.json discovery deleted.
// This is the regression guard for the stale-TCP reconnect-loop storm.
func TestResolveBridgeAddr_IgnoresStaleAddrFile_ReturnsDeterministicPipe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dir)
	addrPath := filepath.Join(dir, "addr.json")
	// write a stale TCP addr.json (the exact poison that caused the storm)
	if err := os.WriteFile(addrPath, []byte(`{"network":"tcp","address":"127.0.0.1:59108"}`+"\n"), 0600); err != nil {
		t.Fatalf("write stale addr.json %s: %v", addrPath, err)
	}

	a, err := resolveBridgeAddr()
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if a.Network != "pipe" {
		t.Fatalf("Network=%q, want \"pipe\" (MUST ignore stale addr.json tcp)", a.Network)
	}
	expected := transport.DeterministicPipeName(dir)
	if a.Address != expected {
		t.Fatalf("Address=%q, want deterministic %q (addr.json must be ignored)", a.Address, expected)
	}
}
