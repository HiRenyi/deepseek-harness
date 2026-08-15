//go:build !windows

package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

// UnixClient connects to the bridge process via Unix Domain Socket.
// No token authentication is needed — filesystem permissions (0600)
// provide access control.
type UnixClient struct {
	socketPath string
	conn       net.Conn
	scanner    *bufio.Scanner
}

// NewUnixClient creates a Unix Socket client for the given socket address.
func NewUnixClient(addrInfo AddrInfo) *UnixClient {
	return &UnixClient{socketPath: addrInfo.Address}
}

// Connect dials the Unix Socket and prepares the receive scanner.
func (c *UnixClient) Connect(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("unix connect to %s: %w", c.socketPath, err)
	}
	c.conn = conn
	c.scanner = bufio.NewScanner(conn)
	c.scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	// No token handshake — filesystem permissions provide access control
	return nil
}

// SendMessage sends a message to the bridge by appending a newline delimiter.
func (c *UnixClient) SendMessage(msg []byte) error {
	line := append(msg, '\n')
	if _, err := c.conn.Write(line); err != nil {
		return fmt.Errorf("unix send: %w", err)
	}
	return nil
}

// RecvMessage reads the next newline-delimited message from the bridge.
func (c *UnixClient) RecvMessage() ([]byte, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("unix recv: %w", err)
		}
		return nil, io.EOF
	}
	return c.scanner.Bytes(), nil
}

// Close closes the Unix Socket connection.
func (c *UnixClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Compile-time check: UnixClient implements Client
var _ Client = (*UnixClient)(nil)

// Unused import guard
var _ = time.Duration(0)
