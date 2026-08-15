package main

import (
	"os"
	"path/filepath"
)

// extensionDir resolves the UNPACKED extension directory (containing
// manifest.json) that the user loads via chrome://extensions → "Load unpacked"
// (the only viable install path on non-managed personal Chrome — see
// doc/extension-distribution-analysis.md). Lookup order:
//  1. BROWSER_MCP_EXTENSION_DIR env
//  2. <dataDir>/extension (D2 ride-along extract landing dir — ~/.browser-mcp/extension;
//     after a self-update ride-along this holds the freshly extracted extension)
//  3. <exe-dir>/extension (installed layout: installer drops extension/ next to bridge)
//  4. <exe-dir>/../extension (dev layout)
// Returns "" if no manifest.json-bearing dir is found.
func extensionDir() string {
	if p := os.Getenv("BROWSER_MCP_EXTENSION_DIR"); p != "" && manifestExists(p) {
		return p
	}
	// D2 ride-along 解压落点：<dataDir>/extension。bridgeDataDir 走 config 层
	// （env 优先），与 staging/bridge_version 同一数据目录契约。
	if dd, err := bridgeDataDir(); err == nil {
		p := filepath.Join(dd, "extension")
		if manifestExists(p) {
			return p
		}
	}
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(exeDir, "extension"),
			filepath.Join(exeDir, "..", "extension"),
		} {
			if abs, err := filepath.Abs(c); err == nil && manifestExists(abs) {
				return abs
			}
		}
	}
	return ""
}

// manifestExists reports whether dir/manifest.json exists (the marker that a
// dir is loadable as an unpacked extension).
func manifestExists(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "manifest.json"))
	return err == nil && !fi.IsDir()
}
