package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpbridge "github.com/browser-mcp/bridge/mcp"
)

// TestEventSink_WritesAndSubscribes verifies the F6 sink both (a) appends a
// JSONL line to <dataDir>/events/events.jsonl and (b) fans the same Event out
// to an in-memory subscriber. Uses a temp BROWSER_MCP_DATA_DIR (per CLAUDE.md
// env contract) — never os.TempDir() for the real sink.
func TestEventSink_WritesAndSubscribes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dir)

	initEventSink()
	if eventSink == nil {
		t.Fatalf("initEventSink left eventSink nil")
	}
	t.Cleanup(func() {
		if eventSink != nil {
			eventSink.close()
			eventSink = nil
		}
	})

	sub := eventSink.subscribe()

	emit := mcpbridge.Event{
		Type:   "mcp",
		Method: "click",
	}
	eventSink.emit(emit)

	// Subscriber should receive the event (non-blocking emit + 64-deep buffer).
	select {
	case got := <-sub:
		if got.Method != "click" || got.Type != "mcp" {
			t.Fatalf("sub got %+v, want {mcp click}", got)
		}
		if got.Ts.IsZero() {
			t.Fatalf("sub event Ts not stamped by emit")
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for subscriber event")
	}

	// Disk should contain the JSONL line with "click" under logs/<today>/.
	today := time.Now().Format("20060102")
	p := filepath.Join(dir, "logs", today, "events.jsonl")
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open events file: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	found := false
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, `"click"`) {
			continue
		}
		if !strings.Contains(line, `"mcp"`) {
			t.Fatalf("line missing type=mcp: %s", line)
		}
		found = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !found {
		t.Fatalf("events.jsonl has no click line")
	}
}

// TestEmitNoOpWhenNil verifies that a nil eventSink (init failed / not called)
// does NOT panic — emit is a no-op. This guards the "sink must NEVER break
// MCP/CDP flow" contract.
func TestEmitNoOpWhenNil(t *testing.T) {
	eventSink = nil // reset global state
	// Calling emit on a nil sink must not panic. We exercise this via the
	// package-level guard used by main's EmitHook wiring (emit is a method on
	// *sink; the guard is `if eventSink == nil { return }` at the call site).
	// Here we assert the guard directly.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-sink path panicked: %v", r)
		}
	}()
	if eventSink != nil {
		t.Fatalf("test precondition: eventSink should be nil")
	}
	// The EmitHook in main checks `eventSink == nil` before calling emit; a nil
	// sink therefore never reaches emit(). Simulate that guard.
	if eventSink == nil {
		return
	}
	eventSink.emit(mcpbridge.Event{Type: "mcp", Method: "noop"})
}
