//go:build windows

package main

import (
	"fmt"

	"github.com/browser-mcp/nm-host/transport"
)

// resolveBridgeAddr resolves how nm-host connects to bridge on Windows.
// Uses a deterministic Named Pipe name derived from dataDir.
func resolveBridgeAddr() (transport.AddrInfo, error) {
	dir, err := bridgeDataDir()
	if err != nil {
		return transport.AddrInfo{}, fmt.Errorf("dataDir unresolvable: %w", err)
	}
	return transport.AddrInfo{
		Network: "pipe",
		Address: transport.DeterministicPipeName(dir),
	}, nil
}
