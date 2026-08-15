package main

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/browser-mcp/bridge/config"
)

// distURL returns the configured distribution source base URL (for /api/status
// surfacing). Delegates to the UpdateSource abstraction: http → hardcoded
// profile.UpdateBase (ldflag-baked), gitee → defaultGiteeBase. As of
// hardcode-update-base-url, env/config cannot override the http base.
func distURL() string {
	return updateSource().Base()
}

// extVersion is the extension version the extension reports via POST
// /api/extension-info (chrome.runtime.getManifest().version). The dashboard
// shows it so users see the loaded extension's version. Stale-cleared after
// 10 min so a removed/unloaded extension doesn't keep showing a version.
var (
	extInfoMu  sync.Mutex
	extVersion string
	extSeenAt  time.Time
)

// setExtVersion stores the version reported by the extension + stamps the time.
func setExtVersion(v string) {
	if v == "" {
		return
	}
	extInfoMu.Lock()
	defer extInfoMu.Unlock()
	extVersion = v
	extSeenAt = time.Now()
}

// currentExtVersion returns the extension version if reported in the last 10
// min, else "" (treated as "extension not reporting / not loaded").
func currentExtVersion() string {
	extInfoMu.Lock()
	defer extInfoMu.Unlock()
	if extVersion == "" || time.Since(extSeenAt) > 10*time.Minute {
		return ""
	}
	return extVersion
}

// extensionInfoHandler receives the extension's self-reported version (POST
// /api/extension-info {version, id}) AND serves the on-disk extension info
// (GET). The extension calls POST on startup + NM-connect + periodically so the
// dashboard can display the loaded version without the extension version being
// baked into the bridge. GET (§4.5) exposes the on-disk ride-along extension
// version + path so a host (GUI/KairosAIStudio) can detect a staged extension
// update + prompt the user to reload-unpacked.
func extensionInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method == http.MethodGet {
		dir, onDisk := extensionOnDiskInfo()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"extension_dir":     dir,
			"on_disk_version":   onDisk,           // 解压后的新版本（D2 ride-along）
			"loaded_version":    currentExtVersion(), // Chrome 实际加载的版本（POST 上报）
		})
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "POST required"})
		return
	}
	var req struct {
		Version string `json:"version"`
		ID      string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad JSON: " + err.Error()})
		return
	}
	setExtVersion(req.Version)
	w.WriteHeader(http.StatusNoContent)
}

type dashboardStatus struct {
	BridgeVersion string `json:"bridge_version"`
	Commit        string `json:"commit"`
	BuildTime     string `json:"build_time"`
	GoVer         string `json:"go_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Port          int    `json:"port"`
	NMConnected   bool   `json:"nm_connected"`
	InternalTrans string `json:"internal_transport"`
	TransAddr     string `json:"transport_address"`
	ExtVersion    string `json:"extension_version"`
	DistURL       string `json:"dist_url_configured"`  // empty = not set
	LatestVersion string `json:"latest_version"`      // from nginx version.json (P5)
	UpdateAvailable bool `json:"update_available"`
	ExtensionDir  string `json:"extension_dir"`        // unpacked ext dir for Load unpacked (P6.3 path)
}

// apiStatusHandler returns the dashboard status JSON (P3.2). UI polls this for
// green/red self-check + version comparison + CRX restart-prompt decision.
func apiStatusHandler(mcpPort int, addrInfo transportAddrInfoLite) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		st := dashboardStatus{
			BridgeVersion: version,
			Commit:        commit,
			BuildTime:     buildTime,
			GoVer:         runtime.Version(),
			OS:            runtime.GOOS,
			Arch:          runtime.GOARCH,
			Port:          mcpPort,
			NMConnected:   nmAlive(),
			InternalTrans: addrInfo.Network,
			TransAddr:     addrInfo.Address,
			ExtVersion:    currentExtVersion(),
			DistURL:       distURL(),
			ExtensionDir:  extensionDir(),
		}
		// P5: best-effort fetch version.json (cached) and compare vs bridge
		// version. Swallow errors — /api/status must stay green even if the
		// distribution source is unreachable (e.g. offline). The explicit
		// /api/update-check button surfaces fetch errors.
		if m, err := cachedManifest(); err == nil && m != nil {
			st.LatestVersion = m.Version
			st.UpdateAvailable = isNewerVersion(m.Version, currentVersion())
		}
		json.NewEncoder(w).Encode(st)
	}
}

// transportAddrInfoLite is a small copy of transport.AddrInfo to avoid an
// import cycle (main package can see transport.AddrInfo directly; we use a
// lite struct so dashboard.go stays decoupled from transport internals).
type transportAddrInfoLite struct {
	Network string
	Address string
}

// dashboardHandler: the web dashboard page has been removed (no longer needed).
// "/" redirects to /health so the root still returns useful JSON status instead
// of a 404. /api/* and /health remain. (GUI/popup no longer link here.)
func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/health", http.StatusSeeOther)
}


// updateInstallHandler is the manual trigger (dashboard "安装更新" button) +
// the cherry-studio/host API path. POST-only (non-POST → 405). Delegates to
// runSelfUpdate (B1/B3 real rename-swap + re-exec) + returns its structured
// selfUpdateResult verbatim:
//
//	{"ok":true,"action":"self-swapped","old_version":"…","new_version":"…","restarting":true}
//
// Synchronous for now; TODO(B4): ?async=true → goroutine + /api/update-status
// polling + SSE progress (countingWriter) — left as a follow-up to keep the
// first cut simple. The synchronous call returns once the swap + re-exec
// Start() has completed (the new bridge keeps booting detached).
func updateInstallHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "POST required"})
		return
	}
	json.NewEncoder(w).Encode(runSelfUpdate())
}

// updateCheckHandler is the P5 version-check endpoint (explicit "check now").
// Fetches version.json fresh (bypasses the /api/status cache), compares vs the
// running bridge version, returns latest + update_available.
func updateCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	src := updateSource()
	m, err := src.FetchVersionManifest()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":              false,
			"error":           "拉取 version.json 失败: " + err.Error(),
			"source":          src.Name(),
			"base":            src.Base(),
			"current_version": currentVersion(),
		})
		return
	}
	assetURL, assetSHA, assetErr := src.AssetURL(m) // resolved absolute URL for current platform
	resp := map[string]any{
		"ok":               true,
		"source":           src.Name(),
		"base":             src.Base(),
		"current_version":  currentVersion(),
		"latest_version":   m.Version,
		"update_available": isNewerVersion(m.Version, currentVersion()),
		// Hand the GUI the download URL + sha so it can ApplyUpdate itself (GUI is
		// the update unit — bridge only fetches the manifest). `url` is the field
		// the F2.4 panel reads; `sha256` lets the GUI verify the download.
		"url":    assetURL,
		"sha256": assetSHA,
	}
	if assetErr != nil {
		// No asset for this platform — still report update_available, but signal
		// the GUI there's no downloadable installer (it shows "更新未就绪").
		resp["url"] = ""
		resp["asset_error"] = assetErr.Error()
	}
	json.NewEncoder(w).Encode(resp)
}

// configHandler (GET /api/config) returns the effective merged config with each
// field's source layer ("env" | "config" | "default"). The GUI uses this to
// populate the settings form + label env-overridden fields as read-only.
func configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(config.EffectiveWithSources())
}

// configReloadHandler (POST /api/config/reload) re-reads browser-mcp.json,
// swaps the config cache, and rebuilds the UpdateSource if update_source
// changed. Returns the changed field names + needs_restart (true when data_dir
// changed — events/runlog path migration requires a bridge restart). The base
// URL is hardcoded (profile.UpdateBase) and not in the config schema, so only
// update_source changes trigger a rebuild.
func configReloadHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "POST required"})
		return
	}
	reloaded, needsRestart := config.Reload()
	// Rebuild UpdateSource if the source selector changed so the next
	// update-check uses the new backend (base URL is always profile.UpdateBase).
	for _, f := range reloaded {
		if f == "update_source" {
			resetUpdateSource()
			invalidateManifestCache()
			break
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"reloaded":       reloaded,
		"needs_restart":  needsRestart,
		"restart_reason": "data_dir",
	})
}
