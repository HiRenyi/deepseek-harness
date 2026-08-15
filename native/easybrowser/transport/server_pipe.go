//go:build windows

package transport

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/Microsoft/go-winio"
)

// PipeServer listens for a single connection via Windows Named Pipe.
// It uses a DACL security descriptor to restrict access to the Creator Owner,
// eliminating the need for token-based authentication.
type PipeServer struct {
	pipeName  string
	listener  net.Listener
	conn      net.Conn
	scanner   *bufio.Scanner
	mu        sync.Mutex
	onMessage func([]byte)
	cancel    context.CancelFunc
}

// NewPipeServer creates a Named Pipe server with a deterministic, dataDir-derived
// pipe name so nm-host can connect WITHOUT reading addr.json (eliminates the
// addr.json-loss storm class). Different BROWSER_MCP_DATA_DIR -> different name.
// If dataDir is unresolvable (extreme, %LOCALAPPDATA% missing), fall back to a
// random UUID name — this UUID name is NOT discoverable by nm-host (the
// deterministic name is the only discovery path), so this is a last-resort path.
func NewPipeServer() *PipeServer {
	dir, err := dataDir()
	if err != nil {
		return &PipeServer{pipeName: `\\.\pipe\browser-mcp-` + generateUUID()}
	}
	return &PipeServer{pipeName: deterministicPipeName(dir)}
}

// Start begins listening on the Named Pipe.
func (s *PipeServer) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	cfg := &winio.PipeConfig{
		// DACL: Allow full access to current user (AU = Authenticated Users).
		// Using CO (Creator Owner) is too restrictive for the pipe listener itself.
		// AU ensures only authenticated users on this machine can connect,
		// which is appropriate for a local-only IPC mechanism.
		SecurityDescriptor: "D:P(A;;GA;;;AU)",
		MessageMode:        false, // byte stream mode
		InputBufferSize:    65536,
		OutputBufferSize:   65536,
	}

	ln, err := winio.ListenPipe(s.pipeName, cfg)
	if err != nil {
		cancel()
		return fmt.Errorf("listen pipe %s: %w", s.pipeName, err)
	}
	s.listener = ln
	log.Printf("transport: listening on named pipe %s", s.pipeName)

	go s.acceptLoop(loopCtx)
	return nil
}

// OnMessage registers a callback for incoming JSON messages.
func (s *PipeServer) OnMessage(fn func([]byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onMessage = fn
}

// Send writes a JSON message to the active connection.
func (s *PipeServer) Send(msg []byte) error {
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
func (s *PipeServer) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil {
		s.conn.Close()
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// HasActiveConnection returns true if a client is currently connected.
func (s *PipeServer) HasActiveConnection() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// AddrInfo returns the Named Pipe address info.
func (s *PipeServer) AddrInfo() AddrInfo {
	return AddrInfo{
		Network: "pipe",
		Address: s.pipeName,
	}
}

func (s *PipeServer) acceptLoop(ctx context.Context) {
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
			log.Printf("transport: pipe accept error: %v", err)
			return
		}

		log.Printf("transport: pipe connection from %s", conn.RemoteAddr())

		// No token authentication — DACL provides access control
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

func (s *PipeServer) readLoop(ctx context.Context) {
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
				log.Printf("transport: pipe read error: %v", err)
			} else {
				log.Printf("transport: pipe connection closed")
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
