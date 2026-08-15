//go:build windows

package main

// hide_console_windows.go — no-op under the GUI subsystem.
//
// bridge is built with -ldflags -H=windowsgui (build.ps1 $bridgeLdflags), so
// the binary is a GUI-subsystem executable: Windows NEVER allocates a console
// for it, regardless of launcher (HKCU Run shell launch at boot, nm-host exec
// spawn, terminal, double-click). There is no console window to hide.
//
// History (why this file still exists as a no-op):
//  - bridge-boot-autostart (2026-07-08): HKCU Run\<AutostartKey>Bridge =
//    "bridge.exe" --boot. bridge was console-subsystem → shell boot launch
//    allocated a fresh console = black window. ShowWindow(GetConsoleWindow,
//    SW_HIDE) was the first attempt.
//  - On Win11 LTSC 2024, ShowWindow(SW_HIDE) only MINIMIZED the window to the
//    taskbar ("打开又最小化，跟病毒进程似的"), not hidden. v1.0.10 added
//    retry+diagnostic, still minimized.
//  - v1.0.11: switched to FreeConsole (detach → conhost disappears) — but that
//    still flashes the window before detach. Root-cause fix = build bridge as
//    GUI subsystem (-H=windowsgui) so no console is ever allocated. No console
//    ⇒ nothing to hide ⇒ this function is a no-op.
//
// main.go still calls hideConsoleWindow() under the --boot gate. It is kept as
// a no-op (not deleted) so the --boot path stays self-contained for any future
// console-subsystem build (dev `go run` / manual build without the ldflag) —
// those don't pass --boot in practice, but the call is harmless regardless.
func hideConsoleWindow() {}
