package transport

import (
	"context"
)

// Server is a platform-agnostic transport server for internal communication
// between bridge and nm-host. Implementations include Named Pipe (Windows),
// Unix Socket (Linux/Mac), and TCP (fallback).
type Server interface {
	// Start begins listening for connections. It blocks until the listener is ready.
	Start(ctx context.Context) error

	// OnMessage registers a callback for incoming JSON messages.
	OnMessage(fn func([]byte))

	// Send writes a message to the active connection.
	Send(msg []byte) error

	// Close shuts down the listener and active connection.
	Close() error

	// HasActiveConnection returns true if a client is currently connected.
	HasActiveConnection() bool

	// AddrInfo returns the actual address the server is listening on.
	AddrInfo() AddrInfo
}

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

// AddrInfo describes the actual transport address.
type AddrInfo struct {
	Network string `json:"network"` // "pipe", "unix", "tcp"
	Address string `json:"address"` // pipe name, socket path, or host:port
}
