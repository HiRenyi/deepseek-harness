package transport

import (
	"context"
	"fmt"
	"log"
	"sync"
)

// ServerFactory creates a Server instance.
// Used by FallbackServer to try multiple transports in order.
type ServerFactory func() Server

// FallbackServer tries multiple Server factories in priority order.
// On Start(), it attempts each factory; the first successful one
// becomes the active server. If all fail, it returns the last error.
//
// Typical usage:
//   - Windows: [NewPipeServer, NewTCPServer("127.0.0.1:0")]
//   - Linux/Mac: [NewUnixServer, NewTCPServer("127.0.0.1:0")]
type FallbackServer struct {
	factories []ServerFactory
	active    Server
	mu        sync.RWMutex
}

// NewFallbackServer creates a server that tries each factory in order,
// falling back to the next on failure.
func NewFallbackServer(factories []ServerFactory) *FallbackServer {
	return &FallbackServer{
		factories: factories,
	}
}

// Start tries each transport factory in order. The first one that
// successfully starts becomes the active server. If all fail,
// returns an aggregated error.
func (fs *FallbackServer) Start(ctx context.Context) error {
	var lastErr error
	for i, factory := range fs.factories {
		srv := factory()
		if err := srv.Start(ctx); err != nil {
			transportName := "unknown"
			// AddrInfo() may panic before Start succeeds on some implementations,
			// so we try to get it safely.
			func() {
				defer func() { recover() }()
				transportName = srv.AddrInfo().Network
			}()
			if i < len(fs.factories)-1 {
				log.Printf("WARNING: transport %s failed (%v), falling back to next transport", transportName, err)
			}
			lastErr = err
			continue
		}

		fs.mu.Lock()
		fs.active = srv
		fs.mu.Unlock()

		info := srv.AddrInfo()
		if i > 0 {
			log.Printf("WARNING: fell back to %s transport at %s (primary transport failed)", info.Network, info.Address)
		}
		return nil
	}

	return fmt.Errorf("all transports failed, last error: %w", lastErr)
}

// OnMessage registers a callback for incoming JSON messages.
func (fs *FallbackServer) OnMessage(fn func([]byte)) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.active != nil {
		fs.active.OnMessage(fn)
	}
}

// Send writes a JSON message to the active connection.
func (fs *FallbackServer) Send(msg []byte) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.active == nil {
		return fmt.Errorf("no active server")
	}
	return fs.active.Send(msg)
}

// Close shuts down the active server.
func (fs *FallbackServer) Close() error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.active == nil {
		return nil
	}
	return fs.active.Close()
}

// HasActiveConnection returns true if a client is currently connected.
func (fs *FallbackServer) HasActiveConnection() bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.active == nil {
		return false
	}
	return fs.active.HasActiveConnection()
}

// AddrInfo returns the address info of the active server.
// Must be called after Start() succeeds.
func (fs *FallbackServer) AddrInfo() AddrInfo {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.active == nil {
		return AddrInfo{}
	}
	return fs.active.AddrInfo()
}
