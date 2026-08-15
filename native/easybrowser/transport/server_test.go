package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- FallbackServer tests ---

// mockServer is a controllable Server implementation for testing.
type mockServer struct {
	addrInfo    AddrInfo
	startErr    error
	onMessageFn func([]byte)
	closed      bool
}

func (m *mockServer) Start(_ context.Context) error { return m.startErr }
func (m *mockServer) OnMessage(fn func([]byte))     { m.onMessageFn = fn }
func (m *mockServer) Send(msg []byte) error         { return nil }
func (m *mockServer) Close() error                  { m.closed = true; return nil }
func (m *mockServer) HasActiveConnection() bool     { return true }
func (m *mockServer) AddrInfo() AddrInfo            { return m.addrInfo }

func TestFallbackServerPrimarySucceeds(t *testing.T) {
	primary := &mockServer{addrInfo: AddrInfo{Network: "pipe", Address: `\\.\pipe\test`}}
	fs := NewFallbackServer([]ServerFactory{
		func() Server { return primary },
		func() Server { return &mockServer{addrInfo: AddrInfo{Network: "tcp", Address: "127.0.0.1:0"}} },
	})

	if err := fs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	info := fs.AddrInfo()
	if info.Network != "pipe" {
		t.Errorf("AddrInfo.Network = %q, want %q", info.Network, "pipe")
	}
}

func TestFallbackServerPrimaryFailsFallbackSucceeds(t *testing.T) {
	primary := &mockServer{
		addrInfo: AddrInfo{Network: "pipe", Address: `\\.\pipe\test`},
		startErr: fmt.Errorf("pipe creation failed: access denied"),
	}
	fallback := &mockServer{addrInfo: AddrInfo{Network: "tcp", Address: "127.0.0.1:54321"}}

	fs := NewFallbackServer([]ServerFactory{
		func() Server { return primary },
		func() Server { return fallback },
	})

	if err := fs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	info := fs.AddrInfo()
	if info.Network != "tcp" {
		t.Errorf("AddrInfo.Network = %q, want %q (fallback)", info.Network, "tcp")
	}
}

func TestFallbackServerAllFail(t *testing.T) {
	fs := NewFallbackServer([]ServerFactory{
		func() Server { return &mockServer{addrInfo: AddrInfo{Network: "pipe"}, startErr: fmt.Errorf("pipe failed")} },
		func() Server { return &mockServer{addrInfo: AddrInfo{Network: "tcp"}, startErr: fmt.Errorf("tcp failed")} },
	})

	err := fs.Start(context.Background())
	if err == nil {
		t.Fatal("expected error when all transports fail")
	}
	if !strings.Contains(err.Error(), "all transports failed") {
		t.Errorf("error = %q, want to contain 'all transports failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "tcp failed") {
		t.Errorf("error = %q, want to contain last error 'tcp failed'", err.Error())
	}
}

func TestFallbackServerDelegatesMethods(t *testing.T) {
	active := &mockServer{addrInfo: AddrInfo{Network: "pipe", Address: `\\.\pipe\test`}}
	fs := NewFallbackServer([]ServerFactory{
		func() Server { return active },
	})

	if err := fs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// OnMessage delegation
	fs.OnMessage(func(data []byte) {})
	if active.onMessageFn == nil {
		t.Error("OnMessage not delegated to active server")
	}

	// HasActiveConnection delegation
	if !fs.HasActiveConnection() {
		t.Error("HasActiveConnection should delegate to active server")
	}

	// Send delegation (no error from mock)
	if err := fs.Send([]byte(`{"test":1}`)); err != nil {
		t.Errorf("Send: %v", err)
	}

	// Close delegation
	if err := fs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !active.closed {
		t.Error("Close not delegated to active server")
	}
}

func TestFallbackServerAddrInfoBeforeStart(t *testing.T) {
	fs := NewFallbackServer([]ServerFactory{
		func() Server { return &mockServer{addrInfo: AddrInfo{Network: "pipe"}} },
	})
	info := fs.AddrInfo()
	if info.Network != "" || info.Address != "" {
		t.Errorf("AddrInfo before Start should be zero-value, got %+v", info)
	}
}

// --- TCPServer fixes tests ---

func TestTCPServerAddrInfoReturnsListenerAddr(t *testing.T) {
	srv := NewTCPServer("127.0.0.1:0")
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	info := srv.AddrInfo()
	if info.Network != "tcp" {
		t.Errorf("Network = %q, want %q", info.Network, "tcp")
	}
	// Must not be "127.0.0.1:0" — should be the actual allocated port
	if info.Address == "127.0.0.1:0" {
		t.Errorf("Address = %q, should be actual listener address", info.Address)
	}
	if !strings.HasPrefix(info.Address, "127.0.0.1:") {
		t.Errorf("Address = %q, want 127.0.0.1:XXXXX", info.Address)
	}
}

func TestTCPServerNoOrphanTokenFileOnListenFail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("BROWSER_MCP_TOKEN_DIR", tmp)

	// Bind to a port that is already taken
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot create blocker listener: %v", err)
	}
	defer blocker.Close()
	blockerAddr := blocker.Addr().String()

	srv := NewTCPServer(blockerAddr)
	err = srv.Start(context.Background())
	if err == nil {
		t.Fatal("expected error when binding to occupied port")
	}

	// Token file should NOT exist
	tokenPath := filepath.Join(tmp, "browser-mcp-token")
	if _, statErr := os.Stat(tokenPath); !os.IsNotExist(statErr) {
		t.Errorf("token file should not exist after Listen failure, but found at %s", tokenPath)
	}
}

// --- Environment variable routing tests ---

func TestNewServerTransportEnvTcp(t *testing.T) {
	t.Setenv("BROWSER_MCP_TRANSPORT", "tcp")
	srv := NewServer()
	if srv == nil {
		t.Fatal("NewServer() returned nil")
	}
	// Just verify it implements the interface
	var _ Server = srv
}

func TestNewServerTransportEnvAuto(t *testing.T) {
	t.Setenv("BROWSER_MCP_TRANSPORT", "auto")
	srv := NewServer()
	if srv == nil {
		t.Fatal("NewServer() returned nil")
	}
	// Should be a FallbackServer
	if _, ok := srv.(*FallbackServer); !ok {
		t.Errorf("expected *FallbackServer, got %T", srv)
	}
}

func TestNewServerTransportEnvUnset(t *testing.T) {
	// Ensure BROWSER_MCP_TRANSPORT is not set
	os.Unsetenv("BROWSER_MCP_TRANSPORT")
	srv := NewServer()
	if srv == nil {
		t.Fatal("NewServer() returned nil")
	}
	// Should be a FallbackServer when unset
	if _, ok := srv.(*FallbackServer); !ok {
		t.Errorf("expected *FallbackServer, got %T", srv)
	}
}

// --- Existing tests (preserved) ---

// TestServerWritesTokenFileSingleLine covers the TCP server's token file:
// after Start the token file must exist at a predictable path and contain
// exactly one line (the token).
func TestServerWritesTokenFileSingleLine(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("BROWSER_MCP_TOKEN_DIR", tmp)

	srv := NewTCPServer("127.0.0.1:0")
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	expectedPath := filepath.Join(tmp, "browser-mcp-token")
	if srv.TokenFile() != expectedPath {
		t.Fatalf("tokenFile = %q, want %q", srv.TokenFile(), expectedPath)
	}
	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	body := strings.TrimRight(string(data), "\r\n")
	if body != srv.Token() {
		t.Fatalf("token file body = %q, want exactly the token %q", body, srv.Token())
	}
	if strings.Count(body, "\n") != 0 {
		t.Fatalf("token file must be a single line, got %q", string(data))
	}
}

// TestHandshakeNoAckRoundtrip covers that the server must NOT write an ack
// line after a valid token — the client treats an open connection as success.
func TestHandshakeNoAckRoundtrip(t *testing.T) {
	srv := NewTCPServer("127.0.0.1:0")
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	addr := srv.listener.Addr().String()
	dial := func(token string) (readFirst string, err error) {
		conn, derr := dialCtx(addr)
		if derr != nil {
			return "", derr
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(token + "\n")); err != nil {
			return "", err
		}
		buf := make([]byte, 64)
		conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, rerr := conn.Read(buf)
		if rerr == nil {
			return strings.TrimRight(string(buf[:n]), "\r\n"), nil
		}
		return "", rerr
	}

	if first, err := dial("wrong-token"); err == nil {
		t.Fatalf("wrong token: expected error/timeout, got %q", first)
	}

	if first, err := dial(srv.Token()); err == nil {
		t.Fatalf("correct token: expected NO data from server (ack removed), but read %q", first)
	} else if first == "ok" {
		t.Fatalf("correct token: server sent ack %q — Bug 2 regressed", first)
	}
}

// TestAuthenticateHelper is a focused unit test for authenticate().
func TestAuthenticateHelper(t *testing.T) {
	srv := &TCPServer{}
	srv.token = "abc123"

	// Valid token
	c1, s1 := netPair(t)
	go func() { c1.Write([]byte("abc123\n")) }()
	if !srv.authenticate(s1) {
		t.Error("authenticate() rejected a valid token")
	}

	// Invalid token
	c2, s2 := netPair(t)
	go func() { c2.Write([]byte("nope\n")) }()
	if srv.authenticate(s2) {
		t.Error("authenticate() accepted an invalid token")
	}
}

// TestAddrInfoJSONRoundtrip tests AddrInfo JSON serialization.
func TestAddrInfoJSONRoundtrip(t *testing.T) {
	cases := []AddrInfo{
		{Network: "pipe", Address: `\\.\pipe\browser-mcp-550e8400-e29b-41d4-a716-446655440000`},
		{Network: "unix", Address: "/tmp/browser-mcp-550e8400.sock"},
		{Network: "tcp", Address: "127.0.0.1:59222"},
	}
	for _, want := range cases {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got AddrInfo
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != want {
			t.Errorf("roundtrip: got %+v, want %+v", got, want)
		}
	}
}

// TestNewServerReturnsPlatformDefault verifies NewServer() returns a
// valid Server implementation.
func TestNewServerReturnsPlatformDefault(t *testing.T) {
	// Ensure no env override
	os.Unsetenv("BROWSER_MCP_TRANSPORT")

	srv := NewServer()
	if srv == nil {
		t.Fatal("NewServer() returned nil")
	}
	// With default settings, should be a FallbackServer
	var _ Server = srv
}

// dialCtx dials addr with a short timeout.
func dialCtx(addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.Dial("tcp", addr)
}

// netPair returns a connected pair of net.Conns (in-memory pipe).
func netPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	return a, b
}
