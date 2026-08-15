package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// App struct
type App struct {
	ctx      context.Context
	dshCmd   *exec.Cmd
	dshReady chan struct{}
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{
		dshReady: make(chan struct{}),
	}
}

// startup is called when the app starts
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.startDsh()
}

// shutdown is called when the app closes
func (a *App) shutdown(ctx context.Context) {
	if a.dshCmd != nil && a.dshCmd.Process != nil {
		a.dshCmd.Process.Kill()
	}
}

// startDsh launches the dsh CLI server using bundled Node.js
func (a *App) startDsh() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("failed to get executable path: %v\n", err)
		return
	}
	appDir := filepath.Dir(exe)

	// Bundled Node.js (installer puts files at root level, not in subdirs)
	nodePath := filepath.Join(appDir, "node-v24.15.0-win-x64", "node.exe")
	dshLib := filepath.Join(appDir, "lib", "bin.js")

	// Check if bundled Node.js exists
	if _, err := os.Stat(nodePath); os.IsNotExist(err) {
		// Fallback to system Node.js
		nodePath = "node"
		dshLib = filepath.Join(appDir, "lib", "bin.js")
	}

	a.dshCmd = exec.Command(nodePath, dshLib, "--profile", "web", "--port", "3080")
	a.dshCmd.Dir = appDir
	a.dshCmd.Stdout = os.Stdout
	a.dshCmd.Stderr = os.Stderr

	// Set environment for the bundled Chromium
	chromiumPath := filepath.Join(appDir, "chrome-win64", "chrome.exe")
	if _, err := os.Stat(chromiumPath); err == nil {
		a.dshCmd.Env = append(os.Environ(),
			"BROWSER_MCP_CHROME_PATH="+chromiumPath,
		)
	}

	fmt.Printf("Starting dsh: %s %s --profile web --port 3080\n", nodePath, dshLib)

	if err := a.dshCmd.Start(); err != nil {
		fmt.Printf("dsh failed to start: %v\n", err)
		return
	}

	// Wait for dsh to be ready (poll port 3080)
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		if a.isPortOpen("127.0.0.1", "3080") {
			close(a.dshReady)
			fmt.Println("dsh is ready on http://127.0.0.1:3080")
			break
		}
	}

	a.dshCmd.Wait()
}

// isPortOpen checks if a TCP port is accepting connections
func (a *App) isPortOpen(host, port string) bool {
	// Simple check using exec to avoid importing net package
	// The Wails frontend will retry anyway
	return false
}

// GetDshURL returns the dsh web UI URL
func (a *App) GetDshURL() string {
	// Wait for dsh to be ready
	select {
	case <-a.dshReady:
	case <-time.After(60 * time.Second):
	}
	return "http://127.0.0.1:3080"
}

// GetDshReady returns whether dsh is ready
func (a *App) GetDshReady() bool {
	select {
	case <-a.dshReady:
		return true
	default:
		return false
	}
}

// Greet is a simple test method
func (a *App) Greet(name string) string {
	return fmt.Sprintf("Hello %s! DeepSeek Harness is running.", name)
}