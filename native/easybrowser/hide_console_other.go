//go:build !windows

package main

// hide_console_other.go — non-Windows no-op for hideConsoleWindow.
// Console-window hiding is a Windows-only concern (the "闪黑框" flash from a
// console-subsystem binary launched at boot). macOS/Linux bridge launched
// from LaunchAgents/xdg-autostart doesn't allocate a visible terminal window.
func hideConsoleWindow() {}
