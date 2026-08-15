package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// failingWriter always returns an error on Write, simulating the invalid
// os.Stderr handle under -H=windowsgui (bridge run_log.go has the same shape).
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("invalid handle") }

// TestTolerantMultiWriter_KeepsFileSinkWhenStderrFails is the windowsgui-log
// contract: io.MultiWriter stops on the FIRST writer error, which under
// --boot/windowsgui (os.Stderr = invalid handle) would kill the FILE sink.
// tolerantMultiWriter ignores per-writer errors so the file keeps receiving.
func TestTolerantMultiWriter_KeepsFileSinkWhenStderrFails(t *testing.T) {
	var buf strings.Builder
	// failingWriter first (like os.Stderr), then the real sink (file).
	w := newTolerantMultiWriter(failingWriter{}, &buf)
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned err %v; tolerant writer must never surface per-writer errors", err)
	}
	if buf.String() != "hello\n" {
		t.Fatalf("file sink got %q, want %q (stderr failure must not stop the file)", buf.String(), "hello\n")
	}
}

// TestTolerantMultiWriter_WritesToAllSinks confirms the happy path (console
// attached, stderr valid) still fans out to every writer.
func TestTolerantMultiWriter_WritesToAllSinks(t *testing.T) {
	var a, b strings.Builder
	w := newTolerantMultiWriter(&a, &b)
	msg := []byte("ping\n")
	if n, err := w.Write(msg); n != len(msg) || err != nil {
		t.Fatalf("Write n=%d err=%v, want n=%d nil", n, err, len(msg))
	}
	if a.String() != "ping\n" || b.String() != "ping\n" {
		t.Fatalf("fan-out failed: a=%q b=%q", a.String(), b.String())
	}
}

// compile-time: tolerantMultiWriter implements io.Writer.
var _ io.Writer = (*tolerantMultiWriter)(nil)
