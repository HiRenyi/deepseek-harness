//go:build windows

package transport

import (
	"bufio"
	"context"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// TestPipeServerRoundTrip drives a REAL PipeServer end-to-end: a real client
// (winio pipe dialer, mirroring nm-host's PipeClient) connects to the address
// the server advertised via AddrInfo(), and messages flow both ways.
//
// This closes the biggest coverage gap from named-pipe-transport: the Pipe
// transport (the whole point of the refactor) was previously tested only via
// TCP and mock FallbackServer — never a real pipe round-trip.
func TestPipeServerRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(dataDirEnv, tmp) // isolate deterministic pipe name from any running bridge

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewPipeServer()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("PipeServer.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	info := srv.AddrInfo()
	if info.Network != "pipe" {
		t.Fatalf("AddrInfo.Network = %q, want pipe", info.Network)
	}
	if info.Address == "" {
		t.Fatal("AddrInfo.Address empty")
	}

	// Register the inbound handler before connecting.
	received := make(chan []byte, 4)
	srv.OnMessage(func(data []byte) { received <- data })

	// Real pipe client (same mechanism as nm-host PipeClient.Connect).
	conn, err := winio.DialPipeContext(ctx, info.Address)
	if err != nil {
		t.Fatalf("DialPipe: %v", err)
	}
	defer conn.Close()

	// Wait for the server's acceptLoop to register the active connection.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !srv.HasActiveConnection() {
		time.Sleep(10 * time.Millisecond)
	}
	if !srv.HasActiveConnection() {
		t.Fatal("server never registered the pipe connection")
	}

	// Client -> Server.
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	select {
	case msg := <-received:
		if string(msg) != "ping" {
			t.Errorf("server received %q, want %q", msg, "ping")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to receive client message")
	}

	// Server -> Client.
	if err := srv.Send([]byte("pong")); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	br := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if line != "pong\n" {
		t.Errorf("client received %q, want %q", line, "pong\n")
	}
}

// TestNewServerAutoStartsOnPipe verifies the default (auto/unset) factory
// actually starts successfully on Windows and lands on a pipe transport,
// not just that it returns a FallbackServer type.
func TestNewServerAutoStartsOnPipe(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(dataDirEnv, tmp) // isolate deterministic pipe name from any running bridge
	os.Unsetenv("BROWSER_MCP_TRANSPORT") // default = auto with fallback

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewServer()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("NewServer().Start: %v", err)
	}
	defer srv.Close()

	info := srv.AddrInfo()
	// On Windows the primary transport is pipe; fallback would be tcp only on failure.
	if info.Network != "pipe" {
		t.Errorf("default transport Network = %q, want pipe (pipe should succeed on Windows)", info.Network)
	}
}
