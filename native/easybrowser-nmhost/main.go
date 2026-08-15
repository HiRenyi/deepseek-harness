package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/browser-mcp/nm-host/native"
	"github.com/browser-mcp/nm-host/profile"
	"github.com/browser-mcp/nm-host/transport"
)

// Build metadata injected via -ldflags. Defaults let the binary run unconfigured.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

const (
	maxBridgeRetries = 3
	retryInterval    = 1 * time.Second
)

func main() {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-v" {
			fmt.Printf("nm-host version=%s commit=%s build=%s\n", version, commit, buildTime)
			return
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Set up debug log file for diagnosing NM connection issues
	// Chrome swallows stderr, so we write to a file instead.
	// Contract (see CLAUDE.md "环境契约"): log lives in the persistent data dir
	// under logs/<YYYYMMDD>/, NOT %TEMP% (which gets garbage-collected).
	// nm-host is short-lived (Chrome-spawned, seconds-minutes), so it computes
	// today's date once at startup — no cross-day rotation needed (that's
	// bridge's DayRotatingWriter's job for the long-lived process).
	logPath := os.Getenv("BROWSER_MCP_LOG")
	if logPath == "" {
		if dir, derr := bridgeDataDir(); derr == nil {
			today := time.Now().Format("20060102")
			logsSub := filepath.Join(dir, "logs", today)
			_ = os.MkdirAll(logsSub, 0o700)
			logPath = filepath.Join(logsSub, "nm-host.log")
		} else {
			// last-resort fallback only if user cache unresolvable
			logPath = filepath.Join(os.TempDir(), "browser-mcp-nm-host.log")
		}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		// tolerantMultiWriter (not io.MultiWriter): under -H=windowsgui (D1)
		// os.Stderr is an invalid handle when Chrome spawns nm-host without a
		// parent console; io.MultiWriter would stop on that first error and the
		// FILE sink would stop receiving. Tolerant writer skips dead stderr.
		log.SetOutput(newTolerantMultiWriter(os.Stderr, logFile))
		defer logFile.Close()
	}
	log.Printf("===== nm-host starting =====")

	// Resolve bridge transport address (deterministic pipe from dataDir).
	// dataDir unresolvable is the only error path (no TCP fallback — D2 removed it).
	addrInfo, addrErr := resolveBridgeAddr()
	if addrErr != nil {
		log.Printf("nm-host: bridge address unresolvable: %v", addrErr)
	}
	log.Printf("nm-host: bridge address is %s://%s", addrInfo.Network, addrInfo.Address)

	client := transport.NewClient(addrInfo)
	log.Printf("nm-host: connecting to bridge via %s ...", addrInfo.Network)
	err = client.Connect(ctx)
	if err != nil {
		log.Printf("nm-host: bridge not reachable, attempting auto-launch...")
		if launchErr := launchBridge(); launchErr != nil {
			log.Printf("nm-host: auto-launch warning: %v", launchErr)
		}
		for i := 0; i < maxBridgeRetries; i++ {
			time.Sleep(retryInterval)
			// addrInfo is the fixed deterministic pipe (D3: no re-read —
			// launchBridge spawned bridge on this same pipe, retry it directly).
			client = transport.NewClient(addrInfo)
			err = client.Connect(ctx)
			if err == nil {
				break
			}
			log.Printf("nm-host: retry %d/%d failed: %v", i+1, maxBridgeRetries, err)
		}
		if err != nil {
			log.Fatalf("nm-host: failed to connect to bridge: %v", err)
		}
	}
	defer client.Close()

	log.Printf("nm-host: connected to bridge via %s, entering relay loop", addrInfo.Network)

	// Bidirectional relay: NM stdin <-> Transport
	done := make(chan error, 2)

	// Chrome Extension -> Bridge
	go func() {
		for {
			msg, err := native.ReadMessage(os.Stdin)
			if err != nil {
				done <- fmt.Errorf("nm read: %w", err)
				return
			}
			if err := client.SendMessage(msg); err != nil {
				done <- fmt.Errorf("transport send: %w", err)
				return
			}
		}
	}()

	// Bridge -> Chrome Extension
	go func() {
		for {
			msg, err := client.RecvMessage()
			if err != nil {
				done <- fmt.Errorf("transport recv: %w", err)
				return
			}
			if err := native.WriteMessage(os.Stdout, msg); err != nil {
				done <- fmt.Errorf("nm write: %w", err)
				return
			}
		}
	}()

	// Heartbeat: tell bridge we're alive every few seconds so /health can
	// report the REAL nm-host link state (not just "a conn object exists").
	// (transport-resilience: honest connection state.)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		hb := []byte(`{"jsonrpc":"2.0","method":"__heartbeat__"}`)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := client.SendMessage(hb); err != nil {
					// best-effort: real failure surfaces via the recv loop
					log.Printf("nm-host: heartbeat send error: %v", err)
					return
				}
			}
		}
	}()

	select {
	case err := <-done:
		log.Printf("nm-host: relay stopped: %v", err)
	case <-ctx.Done():
		log.Printf("nm-host: shutting down")
	}
}

// launchBridge attempts to start the bridge binary located next to the
// current executable. On Windows it detaches the process so the bridge
// runs independently. The bridge filename uses profile.BinSuffix (test
// "bridge.exe", prod "bridge-prod.exe") — bridge-boot-autostart fix: the
// previous hardcoded "bridge.exe" could not find "bridge-prod.exe" on prod.
func launchBridge() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	bridgePath := filepath.Join(filepath.Dir(exePath), "bridge"+profile.BinSuffix)
	if _, err := os.Stat(bridgePath); os.IsNotExist(err) {
		bridgePath = filepath.Join(filepath.Dir(exePath), "bridge"+profile.BinSuffix+".exe")
		if _, err := os.Stat(bridgePath); os.IsNotExist(err) {
			return fmt.Errorf("bridge binary not found near %s (looked for bridge%s[.exe])", exePath, profile.BinSuffix)
		}
	}
	cmd := exec.Command(bridgePath, "--boot")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}
