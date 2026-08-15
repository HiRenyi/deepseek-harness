package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// findFreePort returns a host:port string for a currently-free TCP port.
func findFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// bridgeModuleRoot resolves the bridge module root (parent of this transport
// package) from the test file location.
func bridgeModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// .../bridge/transport/health_smoke_test.go -> .../bridge
	return filepath.Dir(filepath.Dir(file))
}

// TestBridgeHealthSmoke runs a freshly-built bridge.exe as a subprocess
// (no Chrome/extension needed — transport+MCP server start standalone) and
// verifies /health exposes the live transport state.
//
// This is a true runtime smoke of the shipped binary, complementing the
// in-process pipe round-trip test.
func TestBridgeHealthSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("health smoke requires building+running bridge subprocess")
	}

	bridgeDir := bridgeModuleRoot(t)
	exeName := "bridge"
	if runtime.GOOS == "windows" {
		exeName = "bridge.exe"
	}
	tmpExe := filepath.Join(t.TempDir(), exeName)

	// Build a fresh binary to avoid testing a stale artifact.
	build := exec.Command("go", "build", "-o", tmpExe, ".")
	build.Dir = bridgeDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build bridge for smoke test: %v\n%s", err, out)
	}

	// Isolate the data directory so the spawned bridge never collides with
	// a real running bridge's lock in %LOCALAPPDATA%\browser-mcp\. Without
	// this, AcquireLock would correctly refuse (the real bridge is alive)
	// and the smoke would fail spuriously whenever a bridge happens to be
	// running during `go test`.
	dataDir := t.TempDir()
	mcpAddr := findFreePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, tmpExe, "--mcp-addr", mcpAddr)
	cmd.Env = append(os.Environ(),
		"BROWSER_MCP_DATA_DIR="+dataDir,
	)
	// bytes.Buffer never blocks on write (unlike io.Pipe), so the subprocess
	// can keep logging without filling a pipe buffer and stalling.
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bridge: %v", err)
	}
	// Ensure the subprocess is always reaped.
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}()

	// Poll /health until ready or timeout.
	url := "http://" + mcpAddr + "/health"
	var body []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(reqCtx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			reqCancel()
			if resp.StatusCode == 200 {
				break
			}
		} else {
			reqCancel()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(body) == 0 {
		t.Fatalf("bridge /health never responded within timeout. stderr:\n%s", stderr.String())
	}

	var health struct {
		Status            string            `json:"status"`
		Port              int               `json:"port"`
		NMConnected       bool              `json:"nm_connected"`
		InternalTransport map[string]string `json:"internal_transport"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("parse /health: %v\nbody: %s", err, body)
	}

	if health.Status != "ok" {
		t.Errorf("status = %q, want ok", health.Status)
	}
	tt := health.InternalTransport["type"]
	if tt != "pipe" && tt != "unix" && tt != "tcp" {
		t.Errorf("internal_transport.type = %q, want one of pipe/unix/tcp", tt)
	}
	if health.InternalTransport["address"] == "" {
		t.Error("internal_transport.address empty")
	}

	t.Logf("transport: %s://%s", tt, health.InternalTransport["address"])
}
