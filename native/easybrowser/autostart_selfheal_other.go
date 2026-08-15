//go:build !windows

package main

// autostart_selfheal_other.go — bridge --boot self-heal of its own Unix
// autostart entry (unix-autostart-selfheal). Mirrors autostart_selfheal_
// windows.go's contract: install.Run (desktop/autostart.EnableBridge) writes
// the entry; if it's deleted (security software / GPO strip / failed best-
// effort registration), bridge --boot re-writes it. Closed loop via nm-host
// launchBridge (nm-host/main.go:187 spawns `bridge --boot` cross-platform)
// when the entry is gone and the user opens Chrome.
//
// GATE: only runs when launched with --boot (main.go:112, the same gate the
// Windows file uses). A terminal `bridge --version` or a dev `bridge --mcp-
// addr ...` run does NOT pass --boot, so a dev bridge never writes the
// user's autostart entry.
//
// bridge is an independent Go module and cannot import desktop/autostart, so
// this re-implements the .desktop/plist write (content MUST mirror desktop/
// autostart/autostart_{linux,darwin}.go EnableBridge — keep in sync). Same
// two-impl pattern as the Windows self-heal (autostart_selfheal_windows.go
// vs autostart_windows.go).
//
// IDEMPOTENT: if the entry exists (presence only, no path validation — same
// as Windows + desktop/autostart.IsEnabled), no-op. BEST-EFFORT: write errors
// logged via println, never fatal — bridge still starts.
//
// Name field uses profile.McpServerName (not GuiTitle): bridge/profile has no
// GuiTitle (desktop/profile has it, baked via transient profile_gen.go since
// ldflag -X can't carry CJK). Name is cosmetic (Exec is functional); self-
// heal only fires when the entry is MISSING, so a freshly-written ASCII-Name
// entry diverges only cosmetically from install's CJK-branded one.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/browser-mcp/bridge/profile"
)

// selfHealBootArg is the arg written into the autostart entry so the booted
// bridge self-hides its console (mirrors desktop/autostart.bootFlag = "--boot"
// and autostart_selfheal_windows.go's selfHealBootArg; bridge is a separate
// module and can't import that const). Defined here (!windows build) because
// the windows file's const is not visible in this build.
const selfHealBootArg = "--boot"

// selfHealAutostart ensures the bridge's Unix autostart entry exists; if
// missing, re-writes it pointing at the current exe with --boot. No-op if
// the entry is already present (idempotent, presence-only). Default
// (freebsd/openbsd/etc) = no-op — bridge has no autostart mechanism there.
func selfHealAutostart() {
	exe, err := os.Executable()
	if err != nil {
		println("[autostart] self-heal exe path:", err.Error())
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	switch runtime.GOOS {
	case "linux":
		selfHealLinuxDesktop(exe)
	case "darwin":
		selfHealDarwinPlist(exe)
	default:
		// freebsd/openbsd/etc: no autostart mechanism implemented; no-op.
	}
}

// selfHealLinuxDesktop re-writes ~/.config/autostart/<lower AutostartKey>bridge.desktop
// if missing. Content mirrors desktop/autostart/autostart_linux.go EnableBridge
// (Name source differs: McpServerName vs GuiTitle — see file header).
func selfHealLinuxDesktop(exe string) {
	home, err := os.UserHomeDir()
	if err != nil {
		println("[autostart] self-heal linux home:", err.Error())
		return
	}
	name := strings.ToLower(profile.AutostartKey) + "bridge.desktop"
	p := filepath.Join(home, ".config", "autostart", name)
	if _, err := os.Stat(p); err == nil {
		return // entry present — no-op (presence only, no content validation)
	} else if !os.IsNotExist(err) {
		println("[autostart] self-heal linux stat:", err.Error())
		return
	}
	content := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=%s Bridge\nExec=%s %s\nTerminal=false\nX-GNOME-Autostart-enabled=true\n",
		profile.McpServerName, exe, selfHealBootArg)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		println("[autostart] self-heal linux mkdir:", err.Error())
		return
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		println("[autostart] self-heal linux write:", err.Error())
		return
	}
	println("[autostart] self-heal re-registered:", p)
}

// selfHealDarwinPlist re-writes ~/Library/LaunchAgents/com.browsermcp.<AutostartKey>Bridge.plist
// if missing. Content mirrors desktop/autostart/autostart_darwin.go EnableBridge
// (RunAtLoad=true, not KeepAlive — bridge is long-lived but a crash doesn't
// auto-respawn in a loop; the user/GUI can re-launch).
func selfHealDarwinPlist(exe string) {
	home, err := os.UserHomeDir()
	if err != nil {
		println("[autostart] self-heal darwin home:", err.Error())
		return
	}
	label := "com.browsermcp." + profile.AutostartKey + "Bridge"
	p := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if _, err := os.Stat(p); err == nil {
		return // entry present — no-op
	} else if !os.IsNotExist(err) {
		println("[autostart] self-heal darwin stat:", err.Error())
		return
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
`, label, exe, selfHealBootArg)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		println("[autostart] self-heal darwin mkdir:", err.Error())
		return
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		println("[autostart] self-heal darwin write:", err.Error())
		return
	}
	println("[autostart] self-heal re-registered:", p)
}
