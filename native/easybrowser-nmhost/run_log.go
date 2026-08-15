package main

import "io"

// tolerantMultiWriter writes p to every writer, IGNORING per-writer errors.
// It reports (len(p), nil) as long as it wrote the bytes to at least one
// writer (and even if all errored — it never surfaces a write error).
//
// WHY NOT io.MultiWriter: io.MultiWriter stops on the FIRST writer error.
// Under -H=windowsgui (D1, nm-host-pipe-only-no-blackwindow) os.Stderr is an
// invalid handle when launched without a parent console (Chrome connectNative).
// The first log.Printf would fail the os.Stderr Write inside io.MultiWriter and
// the FILE sink (the one we care about for support/diagnostics) would stop
// receiving lines. tolerantMultiWriter skips the dead stderr and keeps the
// file alive. Mirror of bridge/run_log.go (separate module — own copy, no import).
type tolerantMultiWriter struct {
	writers []io.Writer
}

func (t *tolerantMultiWriter) Write(p []byte) (int, error) {
	for _, w := range t.writers {
		_, _ = w.Write(p) // best-effort per sink; never propagate
	}
	return len(p), nil
}

func newTolerantMultiWriter(writers ...io.Writer) io.Writer {
	return &tolerantMultiWriter{writers: writers}
}
