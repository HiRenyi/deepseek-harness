package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// Start dsh server as a sidecar process
	go a.startDsh()
}

// shutdown is called when the app closes
func (a *App) shutdown(ctx context.Context) {
	if a.dshCmd != nil && a.dshCmd.Process != nil {
		a.dshCmd.Process.Kill()
	}
}

// startDsh launches the dsh CLI server
func (a *App) startDsh() {
	// Find the dsh executable relative to the app binary
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("failed to get executable path: %v\n", err)
		return
	}
	appDir := filepath.Dir(exe)

	// dsh is bundled alongside the desktop app
	dshPath := filepath.Join(appDir, "dsh.exe")

	// Check if dsh exists, otherwise try PATH
	if _, err := os.Stat(dshPath); os.IsNotExist(err) {
		dshPath = "dsh"
	}

	a.dshCmd = exec.Command(dshPath, "web", "--port", "8080")
	a.dshCmd.Stdout = os.Stdout
	a.dshCmd.Stderr = os.Stderr

	if err := a.dshCmd.Run(); err != nil {
		fmt.Printf("dsh exited: %v\n", err)
	}
}

// GetDshURL returns the dsh web UI URL
func (a *App) GetDshURL() string {
	return "http://127.0.0.1:8080"
}

// Greet is a simple test method
func (a *App) Greet(name string) string {
	return fmt.Sprintf("Hello %s! DeepSeek Harness is running.", name)
}