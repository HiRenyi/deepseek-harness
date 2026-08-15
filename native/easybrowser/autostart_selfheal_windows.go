//go:build windows

package main

// autostart_selfheal_windows.go — bridge --boot self-heal of its own HKCU Run
// autostart entry (harden-bridge-autostart, 2026-07-08).
//
// WHY: the entry written by install.Run (desktop/install → autostart.
// EnableBridge) can be deleted by security software / GPO strip / a failed
// best-effort registration, leaving the bridge non-autostarting at next boot
// with no mechanism to re-register. bridge is an independent Go module and
// cannot import desktop/autostart, so this is a minimal in-bridge re-impl of
// the same EnableBridge/IsBridgeEnabled logic (~40 lines, Windows only) using
// the registry subpackage of golang.org/x/sys (already a dependency).
//
// GATE: only runs when launched with --boot (the autostart / nm-host launch
// path). A terminal `bridge --version` or a dev `bridge --mcp-addr ...` run
// does NOT pass --boot, so a dev bridge never writes the user's registry.
//
// The entry name is profile.AutostartKey + "Bridge" — IDENTICAL to desktop/
// autostart.bridgeRunValueName (both derive from the same profiles/*.env
// AUTOSTART_KEY, baked into each module's profile package via ldflag). So
// bridge's self-heal writes the SAME entry install registered, never a
// duplicate. Idempotent: if the entry exists, selfHealAutostart is a no-op.

import (
	"fmt"
	"os"

	"github.com/browser-mcp/bridge/profile"
	"golang.org/x/sys/windows/registry"
)

const selfHealRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// selfHealBootArg is the arg written into the autostart entry's value so the
// booted bridge self-hides its console (mirrors desktop/autostart.bootFlag =
// "--boot"; bridge is a separate module and can't import that const).
const selfHealBootArg = "--boot"

// bridgeAutostartEntryName is the HKCU Run value name for this profile's
// bridge autostart entry (= desktop/autostart.bridgeRunValueName).
func bridgeAutostartEntryName() string {
	return profile.AutostartKey + "Bridge"
}

// bridgeAutostartEntryExists reports whether the HKCU Run entry is present
// (presence only; no path validation).
func bridgeAutostartEntryExists() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, selfHealRunKey, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetStringValue(bridgeAutostartEntryName())
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// setBridgeAutostartEntry writes HKCU Run\<entry> = data. Idempotent overwrite.
func setBridgeAutostartEntry(data string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, selfHealRunKey,
		registry.SET_VALUE|registry.CREATE_SUB_KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(bridgeAutostartEntryName(), data)
}

// deleteBridgeAutostartEntry removes the HKCU Run entry; idempotent (missing =
// no-op).
func deleteBridgeAutostartEntry() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, selfHealRunKey, registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(bridgeAutostartEntryName()); err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	return nil
}

// getBridgeAutostartEntry reads the HKCU Run entry's value. Returns "" + nil
// if absent (test/read-back helper; not used by selfHealAutostart itself).
func getBridgeAutostartEntry() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, selfHealRunKey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(bridgeAutostartEntryName())
	return v, err
}

// selfHealAutostart ensures the bridge's HKCU Run autostart entry exists; if
// missing, re-registers it pointing at the current exe with --boot. Best-
// effort: errors are logged via println, never fatal — the bridge still
// starts. No-op if the entry is already present (idempotent).
func selfHealAutostart() {
	exists, err := bridgeAutostartEntryExists()
	if err != nil {
		println("[autostart] self-heal exists check:", err.Error())
		// fall through (try to write anyway)
	}
	if exists {
		return // entry present — no-op
	}
	exe, err := os.Executable()
	if err != nil {
		println("[autostart] self-heal exe path:", err.Error())
		return
	}
	data := `"` + exe + `" ` + selfHealBootArg
	if err := setBridgeAutostartEntry(data); err != nil {
		println("[autostart] self-heal set:", fmt.Sprintf("%s: %s", bridgeAutostartEntryName(), err.Error()))
		return
	}
	println("[autostart] self-heal re-registered:", bridgeAutostartEntryName(), "->", data)
}
