//go:build windows

package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// PipeClient connects to the bridge process via Windows Named Pipe.
// No token authentication is needed — the DACL on the pipe provides
// access control.
type PipeClient struct {
	pipeName string
	conn     net.Conn
	scanner  *bufio.Scanner
}

// NewPipeClient creates a Named Pipe client for the given pipe address.
func NewPipeClient(addrInfo AddrInfo) *PipeClient {
	return &PipeClient{pipeName: addrInfo.Address}
}

// Connect dials the Named Pipe and prepares the receive scanner.
func (c *PipeClient) Connect(ctx context.Context) error {
	// winio.DialPipe is a function, not a type
	conn, err := winio.DialPipe(c.pipeName, nil)
	if err != nil {
		return fmt.Errorf("pipe connect to %s: %w", c.pipeName, err)
	}
	c.conn = conn
	c.scanner = bufio.NewScanner(conn)
	c.scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	// No token handshake — pipe security is handled by DACL
	return nil
}

// SendMessage sends a message to the bridge by appending a newline delimiter.
func (c *PipeClient) SendMessage(msg []byte) error {
	line := append(msg, '\n')
	if _, err := c.conn.Write(line); err != nil {
		return fmt.Errorf("pipe send: %w", err)
	}
	return nil
}

// RecvMessage reads the next newline-delimited message from the bridge.
func (c *PipeClient) RecvMessage() ([]byte, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("pipe recv: %w", err)
		}
		return nil, io.EOF
	}
	return c.scanner.Bytes(), nil
}

// Close closes the Named Pipe connection.
func (c *PipeClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Compile-time check: PipeClient implements Client
var _ Client = (*PipeClient)(nil)

// Unused import guard for time (may be needed in future for dial timeout)
var _ = time.Duration(0)
