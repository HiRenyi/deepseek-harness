package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/browser-mcp/bridge/logging"
)

// tolerantMultiWriter writes p to every writer, IGNORING per-writer errors.
// It reports success (len(p), nil) as long as at least one writer accepted
// the bytes, and (len(p), nil) even if all writers errored — it never
// surfaces a write error to the caller.
//
// WHY NOT io.MultiWriter: io.MultiWriter stops on the FIRST writer that
// returns an error. Under --boot, hideConsoleWindow() (Windows) calls
// FreeConsole, which detaches this process from its console and renders
// os.Stderr an invalid handle. The first log.Printf would then fail the
// os.Stderr Write inside io.MultiWriter and the FILE sink (the one we
// actually care about — the GUI RunLog panel + support zip) would stop
// receiving lines from that point on. tolerantMultiWriter skips the dead
// stderr and keeps the file alive.
//
// On a normal terminal run (no --boot, console attached) os.Stderr is a
// valid handle and stderr still receives output, so nothing changes for
// `bridge` / `bridge --version` from a terminal.
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

// setupRunLog redirects the standard logger to write to BOTH stderr (so direct
// console runs still show output) AND a daily-rotating file under
// <bridgeDataDir()>/logs/<YYYYMMDD>/bridge.log (so the GUI's F2.5 RunLog panel
// — desktop/app.go RunLog() — can read it, and users can copy/zip one dated
// directory per day for support).
//
// Cross-day rotation is handled by logging.DayRotatingWriter (checks date on
// every Write, reopens into a new dated subdir at midnight — no goroutine).
// Path honors BROWSER_MCP_DATA_DIR; never os.TempDir (CLAUDE.md env contract).
// BROWSER_MCP_RUN_LOG override: when set, writes that exact file with NO
// rotation (legacy contract for ad-hoc redirection).
//
// Uses tolerantMultiWriter (not io.MultiWriter): under --boot the console is
// detached (FreeConsole) and os.Stderr becomes invalid; io.MultiWriter would
// stop on that first error and the run-log FILE would stop receiving lines.
// tolerantMultiWriter skips the dead stderr and keeps the file alive.
func setupRunLog() {
	if p := os.Getenv("BROWSER_MCP_RUN_LOG"); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			log.Printf("run-log: mkdir %s: %v (stderr-only)", filepath.Dir(p), err)
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Printf("run-log: open %s: %v (stderr-only)", p, err)
			return
		}
		log.SetOutput(newTolerantMultiWriter(os.Stderr, f))
		log.Printf("run-log: writing %s", p)
		return
	}
	dir, err := bridgeDataDir()
	if err != nil {
		log.Printf("run-log: bridgeDataDir: %v (stderr-only)", err)
		return
	}
	logsDir := filepath.Join(dir, "logs")
	w := logging.NewDayRotatingWriter(logsDir, "bridge.log")
	log.SetOutput(newTolerantMultiWriter(os.Stderr, w))
	log.Printf("run-log: writing %s/%s/bridge.log", logsDir, time.Now().Format("20060102"))
}
