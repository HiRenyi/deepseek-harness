package logging

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDayRotatingWriter_SameDayAppends(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	w := &DayRotatingWriter{dir: dir, name: "bridge.log", now: func() time.Time { return fixed }}
	defer w.Close()
	w.Write([]byte("line1\n"))
	w.Write([]byte("line2\n"))
	b, err := os.ReadFile(filepath.Join(dir, "20260706", "bridge.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "line1\nline2\n") {
		t.Fatalf("same-day append failed: %q", b)
	}
}

func TestDayRotatingWriter_CrossDayRotates(t *testing.T) {
	dir := t.TempDir()
	t1 := time.Date(2026, 7, 6, 23, 59, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 7, 0, 1, 0, 0, time.UTC)
	w := &DayRotatingWriter{dir: dir, name: "bridge.log", now: func() time.Time { return t1 }}
	defer w.Close()
	w.Write([]byte("day1\n"))
	w.now = func() time.Time { return t2 }
	w.Write([]byte("day2\n"))
	b1, err := os.ReadFile(filepath.Join(dir, "20260706", "bridge.log"))
	if err != nil {
		t.Fatalf("read old: %v", err)
	}
	b2, err := os.ReadFile(filepath.Join(dir, "20260707", "bridge.log"))
	if err != nil {
		t.Fatalf("read new: %v", err)
	}
	if !strings.Contains(string(b1), "day1\n") || strings.Contains(string(b1), "day2") {
		t.Fatalf("old file corrupted: %q", b1)
	}
	if !strings.Contains(string(b2), "day2\n") || strings.Contains(string(b2), "day1") {
		t.Fatalf("new file wrong: %q", b2)
	}
}

func TestDayRotatingWriter_Concurrent(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	w := &DayRotatingWriter{dir: dir, name: "bridge.log", now: func() time.Time { return fixed }}
	defer w.Close()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.Write([]byte("x")) }()
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(dir, "20260706", "bridge.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) != 100 {
		t.Fatalf("lost bytes: got %d, want 100", len(b))
	}
}

func TestDayRotatingWriter_RotateFailureFallback(t *testing.T) {
	dir := t.TempDir()
	// Point dir at a path whose parent is a file → MkdirAll fails.
	filePath := filepath.Join(dir, "blocker.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := &DayRotatingWriter{dir: filepath.Join(filePath, "logs"), name: "bridge.log",
		now: func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) }}
	// rotate fails (MkdirAll under a file path); fallback to stderr — no panic.
	n, err := w.Write([]byte("x"))
	if err != nil || n != 1 {
		t.Fatalf("fallback write failed: n=%d err=%v", n, err)
	}
}

func TestNewDayRotatingWriter(t *testing.T) {
	w := NewDayRotatingWriter(t.TempDir(), "bridge.log")
	if w == nil || w.name != "bridge.log" {
		t.Fatalf("bad constructor: %+v", w)
	}
}
