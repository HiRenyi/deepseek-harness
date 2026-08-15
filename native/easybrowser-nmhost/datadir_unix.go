//go:build !windows

package main

import (
	"fmt"

	"github.com/browser-mcp/nm-host/transport"
)

// resolveBridgeAddr resolves how nm-host connects to bridge on Mac/Linux.
// Uses a deterministic Unix socket path derived from dataDir — mirrors
// bridge/transport/server_unixsock.go deterministicUnixSocketPath so both
// sides independently compute the same path without addr.json discovery.
func resolveBridgeAddr() (transport.AddrInfo, error) {
	dir, err := bridgeDataDir()
	if err != nil {
		return transport.AddrInfo{}, fmt.Errorf("dataDir unresolvable: %w", err)
	}
	return transport.AddrInfo{
		Network: "unix",
		Address: transport.DeterministicUnixSocketName(dir),
	}, nil
}
