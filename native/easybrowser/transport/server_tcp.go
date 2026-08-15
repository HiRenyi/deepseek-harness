package transport

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
)

// TCPServer listens for a single Chrome Extension connection via TCP,
// authenticates with a random token, and dispatches newline-delimited JSON messages.
// This is the fallback implementation; prefer Named Pipe (Windows) or Unix Socket
// (Linux/Mac) which avoid port conflicts entirely.
type TCPServer struct {
	addr      string
	token     string
	tokenFile string
	listener  net.Listener
	conn      net.Conn
	scanner   *bufio.Scanner
	mu        sync.Mutex
	onMessage func([]byte)
	cancel    context.CancelFunc // stops acceptLoop on Close()
}

// NewTCPServer creates a TCP transport server bound to addr.
// If addr is empty, defaults to 127.0.0.1:0 (OS-assigned port).
func NewTCPServer(addr string) *TCPServer {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	return &TCPServer{
		addr: addr,
	}
}

// Start begins listening and accepting connections.
func (s *TCPServer) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Generate random token for TCP authentication
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		cancel()
		return fmt.Errorf("generate token: %w", err)
	}
	s.token = hex.EncodeToString(tokenBytes)

	// Start listening first (before writing any files)
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		cancel()
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	s.listener = ln
	log.Printf("transport: listening on %s (actual: %s)", s.addr, ln.Addr().String())

	// Write token file only after Listen succeeds
	if err := s.writeTokenFile(); err != nil {
		cancel()
		ln.Close()
		return fmt.Errorf("write token file: %w", err)
	}
	log.Printf("transport: auth token written to %s", s.tokenFile)

	go s.acceptLoop(loopCtx)
	return nil
}

// OnMessage registers a callback for incoming JSON messages.
func (s *TCPServer) OnMessage(fn func([]byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onMessage = fn
}

// Send writes a JSON message to the active connection.
func (s *TCPServer) Send(msg []byte) error {
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
func (s *TCPServer) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tokenFile != "" {
		os.Remove(s.tokenFile)
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// HasActiveConnection returns true if a client is currently connected.
func (s *TCPServer) HasActiveConnection() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// AddrInfo returns the TCP address info.
// Returns the actual listener address (e.g. "127.0.0.1:54321") rather
// than the configured address which may be "127.0.0.1:0".
func (s *TCPServer) AddrInfo() AddrInfo {
	addr := s.addr
	if s.listener != nil {
		addr = s.listener.Addr().String()
	}
	return AddrInfo{
		Network: "tcp",
		Address: addr,
	}
}

// Token returns the current auth token (TCP-specific, not part of Server interface).
func (s *TCPServer) Token() string {
	return s.token
}

// TokenFile returns the path to the token file (TCP-specific).
func (s *TCPServer) TokenFile() string {
	return s.tokenFile
}

func (s *TCPServer) writeTokenFile() error {
	path := tokenFilePath()
	if dir, err := dataDir(); err == nil {
		_ = os.MkdirAll(dir, 0700)
	}
	s.tokenFile = path
	return os.WriteFile(path, []byte(s.token+"\n"), 0600)
}

func (s *TCPServer) acceptLoop(ctx context.Context) {
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
			log.Printf("transport: accept error: %v", err)
			return
		}

		// Handshake: expect token as first line
		if !s.authenticate(conn) {
			log.Printf("transport: authentication failed from %s", conn.RemoteAddr())
			conn.Close()
			continue
		}

		log.Printf("transport: authenticated connection from %s", conn.RemoteAddr())

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

func (s *TCPServer) authenticate(conn net.Conn) bool {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	line = line[:len(line)-1]
	return line == s.token
}

func (s *TCPServer) readLoop(ctx context.Context) {
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
				log.Printf("transport: read error: %v", err)
			} else {
				log.Printf("transport: connection closed")
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
