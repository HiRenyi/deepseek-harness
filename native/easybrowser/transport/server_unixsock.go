//go:build !windows

package transport

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
)

// UnixServer listens for a single connection via Unix Domain Socket.
// It uses filesystem permissions (0600) to restrict access to the owner,
// eliminating the need for token-based authentication.
type UnixServer struct {
	socketPath string
	listener   net.Listener
	conn       net.Conn
	scanner    *bufio.Scanner
	mu         sync.Mutex
	onMessage  func([]byte)
	cancel     context.CancelFunc
}

// NewUnixServer creates a Unix Socket server with a deterministic, dataDir-derived
// socket path. Both bridge and nm-host independently compute the same path from
// the same dataDir so nm-host can connect without addr.json discovery.
func NewUnixServer() *UnixServer {
	dir, err := dataDir()
	if err != nil {
		// dataDir failure is extreme (user home missing); fall back to a UUID path
		// so bridge still starts, but nm-host won't be able to connect deterministically.
		uuid := generateUUID()
		return &UnixServer{socketPath: "/tmp/browser-mcp-" + uuid + ".sock"}
	}
	return &UnixServer{
		socketPath: deterministicUnixSocketPath(dir),
	}
}

// Start begins listening on the Unix Socket.
func (s *UnixServer) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Clean up stale socket file from previous run
	os.Remove(s.socketPath)

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		cancel()
		return fmt.Errorf("listen unix %s: %w", s.socketPath, err)
	}
	s.listener = ln

	// Restrict to owner only
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		cancel()
		ln.Close()
		os.Remove(s.socketPath)
		return fmt.Errorf("chmod socket: %w", err)
	}
	log.Printf("transport: listening on unix socket %s", s.socketPath)

	go s.acceptLoop(loopCtx)
	return nil
}

// OnMessage registers a callback for incoming JSON messages.
func (s *UnixServer) OnMessage(fn func([]byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onMessage = fn
}

// Send writes a JSON message to the active connection.
func (s *UnixServer) Send(msg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("no active connection")
	}
	data := append(msg, '\n')
	_, err := s.conn.Write(data)
	return err
}

// Close shuts down the listener and active connection.
func (s *UnixServer) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil {
		s.conn.Close()
	}
	if s.listener != nil {
		err := s.listener.Close()
		os.Remove(s.socketPath)
		return err
	}
	os.Remove(s.socketPath)
	return nil
}

// HasActiveConnection returns true if a client is currently connected.
func (s *UnixServer) HasActiveConnection() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// AddrInfo returns the Unix Socket address info.
func (s *UnixServer) AddrInfo() AddrInfo {
	return AddrInfo{
		Network: "unix",
		Address: s.socketPath,
	}
}

func (s *UnixServer) acceptLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("transport: unix accept error: %v", err)
			return
		}

		log.Printf("transport: unix connection from %s", conn.RemoteAddr())

		// No token authentication — filesystem permissions provide access control
		s.mu.Lock()
		if s.conn != nil {
			s.conn.Close()
		}
		s.conn = conn
		s.scanner = bufio.NewScanner(conn)
		s.scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		s.mu.Unlock()

		go s.readLoop(ctx)
	}
}

func (s *UnixServer) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		s.mu.Lock()
		scanner := s.scanner
		s.mu.Unlock()

		if scanner == nil {
			return
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				log.Printf("transport: unix read error: %v", err)
			} else {
				log.Printf("transport: unix connection closed")
			}
			s.mu.Lock()
			if s.conn != nil {
				s.conn.Close()
				s.conn = nil
				s.scanner = nil
			}
			s.mu.Unlock()
			return
		}

		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}

		msg := make([]byte, len(data))
		copy(msg, data)

		s.mu.Lock()
		fn := s.onMessage
		s.mu.Unlock()

		if fn != nil {
			fn(msg)
		}
	}
}
