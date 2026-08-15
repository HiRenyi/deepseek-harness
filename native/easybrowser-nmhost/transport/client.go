package transport

import (
	"context"
)

// Client is a platform-agnostic transport client for internal communication
// between nm-host and bridge.
type Client interface {
	// Connect dials the bridge and prepares the receive scanner.
	Connect(ctx context.Context) error

	// SendMessage sends a message to the bridge.
	SendMessage(msg []byte) error

	// RecvMessage reads the next message from the bridge.
	RecvMessage() ([]byte, error)

	// Close closes the connection.
	Close() error
}

// AddrInfo describes the actual transport address (mirrors bridge/transport.AddrInfo).
type AddrInfo struct {
	Network string `json:"network"` // "pipe", "unix", "tcp"
	Address string `json:"address"` // pipe name, socket path, or host:port
}
