// Package logging provides bridge-side log infrastructure (daily-rotating
// writer). Mirrors the data-dir contract (CLAUDE.md "环境契约"): logs live under
// <dataDir>/logs/<YYYYMMDD>/, never %TEMP%.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DayRotatingWriter writes to <dir>/<YYYYMMDD>/<name>, reopening on cross-day.
// It is the shared sink for bridge.log and events.jsonl so both rotate into
// the same dated subdir and users can copy/zip one directory per day.
//
// Concurrency: a sync.Mutex guards every Write (log.Printf is concurrent).
// Cross-day rotation: each Write checks now().Format("20060102") against the
// current date; on mismatch it closes the old file, MkdirAll's the new dated
// subdir, and opens the new file — no goroutine, no timer, zero cross-day
// latency.
//
// Failure mode: if rotate fails (e.g. permissions), it keeps the old file
// (if any) and falls back to stderr — never drops a log line, never blocks
// the MCP/CDP hot path. Production never calls Close (OS reclaims the fd);
// Close is for test cleanup.
type DayRotatingWriter struct {
	mu   sync.Mutex
	dir  string
	name string
	now  func() time.Time // injectable for tests; nil → time.Now at first Write
	cur  string
	f    *os.File
}

// NewDayRotatingWriter returns a writer that rotates <dir>/<date>/<name>.
// now defaults to time.Now; tests may override the returned struct's now field
// (same package) to drive cross-day scenarios deterministically.
func NewDayRotatingWriter(dir, name string) *DayRotatingWriter {
	return &DayRotatingWriter{dir: dir, name: name}
}

func (w *DayRotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now == nil {
		w.now = time.Now
	}
	today := w.now().Format("20060102")
	if today != w.cur || w.f == nil {
		if err := w.rotate(today); err != nil {
			fmt.Fprintf(os.Stderr, "dayrot: rotate failed: %v\n", err)
			if w.f != nil {
				return w.f.Write(p)
			}
			return os.Stderr.Write(p)
		}
	}
	return w.f.Write(p)
}

func (w *DayRotatingWriter) rotate(today string) error {
	sub := filepath.Join(w.dir, today)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(sub, w.name),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if w.f != nil {
		_ = w.f.Close()
	}
	w.f = f
	w.cur = today
	return nil
}

// Close releases the current file. Production does not call this (the writer
// lives for the process); tests use it to release the lock before TempDir
// cleanup.
func (w *DayRotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		return err
	}
	return nil
}
