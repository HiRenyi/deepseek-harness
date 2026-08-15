package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// TCPClient connects to the bridge process over TCP and communicates
// using newline-delimited JSON-RPC messages. This is the fallback
// implementation; prefer Named Pipe (Windows) or Unix Socket (Linux/Mac).
type TCPClient struct {
	addr    string
	conn    net.Conn
	scanner *bufio.Scanner
}

// NewTCPClient creates a TCP client that will connect to the given address.
func NewTCPClient(addr string) *TCPClient {
	return &TCPClient{addr: addr}
}

// Connect dials the bridge address, reads the token file, sends the
// authentication token as the first line, and prepares the receive scanner.
func (c *TCPClient) Connect(ctx context.Context) error {
	// Read token file for TCP authentication
	token, err := readTokenFile()
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}

	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("tcp connect to %s: %w", c.addr, err)
	}
	c.conn = conn
	c.scanner = bufio.NewScanner(conn)
	c.scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	// Send token handshake as first line
	handshake := token + "\n"
	if _, err := conn.Write([]byte(handshake)); err != nil {
		conn.Close()
		return fmt.Errorf("token handshake write: %w", err)
	}
	return nil
}

// SendMessage sends a message to the bridge by appending a newline delimiter.
func (c *TCPClient) SendMessage(msg []byte) error {
	line := append(msg, '\n')
	if _, err := c.conn.Write(line); err != nil {
		return fmt.Errorf("tcp send: %w", err)
	}
	return nil
}

// RecvMessage reads the next newline-delimited message from the bridge.
func (c *TCPClient) RecvMessage() ([]byte, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("tcp recv: %w", err)
		}
		return nil, io.EOF
	}
	return c.scanner.Bytes(), nil
}

// Close closes the TCP connection.
func (c *TCPClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// readTokenFile reads the authentication token from the well-known path.
// This is only used for the TCP fallback transport.
//
// Path resolution must mirror bridge's writeTokenFile/tokenFilePath: the token
// lives in the persistent data dir (NEVER os.TempDir() by default — see
// tokenFilePath contract). BROWSER_MCP_TOKEN_FILE (full absolute path) still
// wins for tests; otherwise resolve via tokenFilePath() to match the bridge.
func readTokenFile() (string, error) {
	tokenPath := os.Getenv("BROWSER_MCP_TOKEN_FILE")
	if tokenPath == "" {
		tokenPath = tokenFilePath()
	}
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", tokenPath, err)
	}
	token := strings.TrimRight(string(data), "\r\n")
	return token, nil
}
