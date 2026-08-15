package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/browser-mcp/bridge/logging"
	mcpbridge "github.com/browser-mcp/bridge/mcp"
)

// eventSink is the process-wide structured event sink (F6 C-foundation). It
// appends one JSONL line per MCP tool call / CDP command to
// <dataDir>/events/events.jsonl and fans the same Event out to in-memory
// subscribers (non-blocking). The sink is OPTIONAL infrastructure: init is
// non-fatal (a read-only data dir just disables the file), emit is a no-op when
// eventSink is nil, and a slow/empty subscriber never blocks emit
// (`select { case c <- e: default: }`). GUI does NOT consume in v1 — only disk
// + subscribe exist (future C-features: task history / live / replay).
//
// The Event type lives in the mcp package (the producer); the sink (consumer)
// lives here in package main next to bridgeDataDir(). main wires
// mcp.EmitHook -> eventSink.emit after init (see main.go).
var eventSink *sink

// sink appends structured events to a JSONL file and fans them out to subs.
type sink struct {
	mu   sync.Mutex
	rot  *logging.DayRotatingWriter
	subs []chan mcpbridge.Event
}

// initEventSink opens (creating if needed) <dataDir>/logs/<YYYYMMDD>/events.jsonl
// for append (cross-day rotation handled by DayRotatingWriter) and installs
// the process-wide eventSink. It is non-fatal: on any error it logs and leaves
// eventSink nil (emit becomes a no-op), so a broken events dir NEVER breaks
// MCP/CDP flow. Honors the CLAUDE.md data-dir contract (BROWSER_MCP_DATA_DIR
// override via bridgeDataDir); NEVER uses os.TempDir().
func initEventSink() {
	dir, err := bridgeDataDir()
	if err != nil {
		log.Printf("event-sink: resolve data dir: %v (sink disabled)", err)
		return
	}
	logsDir := filepath.Join(dir, "logs")
	// Parent logs/ must exist so the writer's first rotate can MkdirAll the
	// dated subdir under it; MkdirAll the parent here (best-effort).
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		log.Printf("event-sink: mkdir %s: %v (sink disabled)", logsDir, err)
		return
	}
	eventSink = &sink{rot: logging.NewDayRotatingWriter(logsDir, "events.jsonl")}
	log.Printf("event-sink: writing %s/<YYYYMMDD>/events.jsonl", logsDir)
}

// emit writes one JSONL line to disk (best-effort; write errors are logged once
// and swallowed — never panic) and fans the Event out to every subscriber
// non-blockingly (a slow/empty sub is dropped, not blocked on).
func (s *sink) emit(e mcpbridge.Event) {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rot != nil {
		b, err := json.Marshal(e)
		if err == nil {
			if _, werr := s.rot.Write(append(b, '\n')); werr != nil {
				// Log once-ish; don't spam. File writes are best-effort.
				log.Printf("event-sink: write error: %v", werr)
			}
		}
	}
	for _, c := range s.subs {
		select {
		case c <- e:
		default:
			// subscriber buffer full — drop this event for this sub rather
			// than blocking the MCP/CDP hot path.
		}
	}
}

// subscribe returns a buffered (64) channel that receives every emitted Event.
// Receivers must be non-blocking consumers; emit drops events for full subs.
// Intended for future C-features (task history / live / replay) — no HTTP
// endpoint is exposed in v1.
func (s *sink) subscribe() chan mcpbridge.Event {
	c := make(chan mcpbridge.Event, 64)
	s.mu.Lock()
	s.subs = append(s.subs, c)
	s.mu.Unlock()
	return c
}

// close releases the JSONL file handle. After close, eventSink is nil so emit
// becomes a no-op. Used by tests to release the file lock before TempDir
// cleanup; production bridge does not close the sink (it lives for the process).
func (s *sink) close() {
	s.mu.Lock()
	if s.rot != nil {
		_ = s.rot.Close()
		s.rot = nil
	}
	s.subs = nil
	s.mu.Unlock()
}
