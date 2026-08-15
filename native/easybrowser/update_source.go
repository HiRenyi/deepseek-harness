package main

// update_source.go — P5.1 self-update distribution source abstraction.
//
// bridge self-updates by fetching a version manifest (version.json) from a
// distribution source and, when a newer version exists, downloading the
// platform's installer and triggering the BrowserMCPUpdater scheduled task
// (installer-rerun, see install-win.ps1 P2.2).
//
// Two backends share the UpdateSource interface so the transition from the
// public gitee release repo (interim, anon-readable) to an internal nginx
// file server (target, once the user provisions one) is a config flip — no
// code change:
//
//	BROWSER_MCP_UPDATE_SOURCE = gitee | http   (default gitee)
//
// The http base URL is HARDCODED to profile.UpdateBase (ldflag-baked per
// profile); it is NOT configurable via env or config file as of the
// hardcode-update-base-url change.
//
// CRX distribution is NOT served here — CRX stays on bridge localhost
// (/crx/update.xml + /crx/extension.crx, P6) regardless of this source.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/profile"
)

// VersionManifest is the bridge self-update manifest published at
// <source>/version.json. 支持 v1 schema（browser-mcp-v1，拆 gui/binaries/extension
// 三件套独立 asset）与旧 schema（仅顶层 assets.<platform> GUI 包）双读。
//
// v1 schema（推荐，bridge-decouple-self-update §3.3）：
//
//	{
//	  "schema": "browser-mcp-v1",
//	  "version": "0.3.0",
//	  "released_at": "2026-06-29T20:00:00+08:00",
//	  "min_host_version": "0.2.0",
//	  "assets": {
//	    "gui": {
//	      "windows_amd64": {"url": "BrowserMCP-windows-amd64.exe", "sha256": "..."},
//	      "darwin_arm64":  {"url": "...", "sha256": "..."}
//	    },
//	    "binaries": {
//	      "windows_amd64": {
//	        "bridge":  {"url": "bridge.exe", "sha256": "..."},
//	        "nmhost":  {"url": "nm-host.exe", "sha256": "..."}
//	      },
//	      "darwin_arm64": { "bridge": {...}, "nmhost": {...} }
//	    },
//	    "extension": {"url": "extension.zip", "sha256": "...", "version": "0.3.0"},
//	    "windows_amd64": {"url": "BrowserMCP-windows-amd64.exe", "sha256": "..."}
//	  }
//	}
//
// 旧 schema（过渡期保留一个版本周期，顶层 assets.<platform> = GUI 包）：
//
//	{
//	  "version": "0.2.2",
//	  "released_at": "...",
//	  "assets": {
//	    "windows_amd64": {"url": "...", "sha256": "..."}
//	  }
//	}
//
// 双读窗口：v1 manifest 同时保留顶层 assets.<platform>（旧 GUI 字段），旧 GUI 仍能读；
// 新代码读 assets.gui / assets.binaries / assets.extension。schema 字段缺省=旧。
// 相对 url 相对 manifest 基址拼接（httpSource.resolveURL）。
type VersionManifest struct {
	Schema         string                  `json:"schema,omitempty"`          // "browser-mcp-v1" 或缺省=旧
	Version        string                  `json:"version"`                   // 三件套统一版本号
	ReleasedAt     string                  `json:"released_at,omitempty"`     // ISO8601 发布时间
	MinHostVersion string                  `json:"min_host_version,omitempty"` // 最低宿主版本（过旧不强行更新）
	// Assets 保留为旧顶层平台 asset 的 map（双读窗口）：v1 manifest 中只有平台 key（gui/binaries/extension
	// 由自定义 UnmarshalJSON 拆到下面三个字段）；旧 schema 即此 map 本身。这样 m.Assets[assetKey()]
	// 旧逻辑不破坏。
	Assets map[string]VersionAsset `json:"assets"`
	// v1 拆分 asset（由自定义 UnmarshalJSON 从 assets 对象里拆出，不直接参与 json 序列化）。
	AssetsGUI        map[string]VersionAsset `json:"-"` // assets.gui.<platform> —— GUI 包
	AssetsBinaries   map[string]PlatformBinaries `json:"-"` // assets.binaries.<platform> —— bridge + nm-host
	AssetsExtension  ExtensionAsset           `json:"-"` // assets.extension —— 扩展 zip
}

// VersionAsset is one asset descriptor（url + sha256）。
type VersionAsset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

// PlatformBinaries is one platform's bridge + nm-host binary descriptors (v1).
type PlatformBinaries struct {
	Bridge VersionAsset `json:"bridge,omitempty"`  // bridge.exe
	NmHost VersionAsset `json:"nmhost,omitempty"`  // nm-host.exe
}

// ExtensionAsset is the Chrome extension zip descriptor (v1)。
type ExtensionAsset struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256,omitempty"`
	Version string `json:"version,omitempty"` // 扩展独立版本号（一般与 manifest version 同源）
}

// IsV1 reports whether the manifest declares the browser-mcp-v1 schema. 缺省=旧。
// asset 解析方法以 v1 字段是否填充为准（更鲁棒），此方法仅作 schema 标记判定。
func (m *VersionManifest) IsV1() bool { return m.Schema == schemaV1 }

// schema 常量。
const schemaV1 = "browser-mcp-v1"

// UnmarshalJSON 拆 assets 对象：命名 key（gui/binaries/extension）落到 v1 字段，
// 其余平台 key（windows_amd64 等）落到旧 Assets map（双读窗口）。这样一份 v1 manifest
// 同时满足新代码（读 gui/binaries/extension）与旧代码（读顶层平台 asset）。
func (m *VersionManifest) UnmarshalJSON(data []byte) error {
	type plain VersionManifest // 别名类型绕过递归 UnmarshalJSON
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	m.Schema = p.Schema
	m.Version = p.Version
	m.ReleasedAt = p.ReleasedAt
	m.MinHostVersion = p.MinHostVersion
	m.Assets = p.Assets
	if m.Assets == nil {
		m.Assets = map[string]VersionAsset{}
	}
	// 重新解析 assets 原文，把命名 key 拆到 v1 字段。
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err == nil {
		_ = raw // 不直接用顶层 raw
	}
	// 从 assets 子对象再拆一次。
	// （plain 解析已把 assets 当 map[string]VersionAsset 解，gui/binaries/extension 值会被
	// 当成空 VersionAsset 放进 m.Assets —— 下面清理 + 重解析。）
	// 用临时结构取 assets 原文。
	type assetsRaw struct {
		Assets json.RawMessage `json:"assets"`
	}
	var ar assetsRaw
	if err := json.Unmarshal(data, &ar); err != nil {
		return err
	}
	if len(ar.Assets) > 0 {
		assetMap := map[string]json.RawMessage{}
		if err := json.Unmarshal(ar.Assets, &assetMap); err != nil {
			return fmt.Errorf("parse assets: %w", err)
		}
		// 清掉被 plain 当成平台 key 误塞进 m.Assets 的命名 key。
		for _, name := range []string{"gui", "binaries", "extension"} {
			delete(m.Assets, name)
		}
		for k, v := range assetMap {
			switch k {
			case "gui":
				m.AssetsGUI = make(map[string]VersionAsset)
				if err := json.Unmarshal(v, &m.AssetsGUI); err != nil {
					return fmt.Errorf("parse assets.gui: %w", err)
				}
			case "binaries":
				m.AssetsBinaries = make(map[string]PlatformBinaries)
				if err := json.Unmarshal(v, &m.AssetsBinaries); err != nil {
					return fmt.Errorf("parse assets.binaries: %w", err)
				}
			case "extension":
				if err := json.Unmarshal(v, &m.AssetsExtension); err != nil {
					return fmt.Errorf("parse assets.extension: %w", err)
				}
			default:
				// 平台 key（旧顶层 GUI asset，双读窗口）。plain 已塞 VersionAsset，但若旧 schema
				// 值形如 {url,sha256} 会被正确解析；这里覆盖一次确保一致。
				var va VersionAsset
				if err := json.Unmarshal(v, &va); err != nil {
					return fmt.Errorf("parse assets.%s: %w", k, err)
				}
				m.Assets[k] = va
			}
		}
	}
	return nil
}

// UpdateSource abstracts where bridge fetches its self-update manifest +
// installer. gitee (public repo raw/release) and http (internal nginx) are the
// two implementations.
type UpdateSource interface {
	// FetchVersionManifest retrieves and parses version.json.
	FetchVersionManifest() (*VersionManifest, error)
	// DownloadInstaller downloads the installer for the current GOOS/GOARCH
	// to dest (a full file path). Caller is responsible for staging dir.
	DownloadInstaller(m *VersionManifest, dest string) error
	// AssetURL returns the resolved absolute download URL + sha256 for the
	// current GOOS/GOARCH GUI package. Exposed so /api/update-check can hand the
	// GUI the URL to download itself (the GUI is the update unit — bridge only
	// fetches the manifest). Relative asset URLs are resolved against Base().
	// v1 schema 读 assets.gui.<platform>；旧 schema 回退顶层 assets.<platform>。
	AssetURL(m *VersionManifest) (url, sha256 string, err error)
	// BridgeAssetURL returns the resolved bridge binary URL + sha256 for the
	// current platform（v1 assets.binaries.<platform>.bridge）。旧 schema 无独立
	// bridge asset → err。bridge 自更新（B3/B4）用此方法。
	BridgeAssetURL(m *VersionManifest) (url, sha256 string, err error)
	// NmHostAssetURL returns the resolved nm-host binary URL + sha256（v1
	// assets.binaries.<platform>.nmhost）。旧 schema → err。
	NmHostAssetURL(m *VersionManifest) (url, sha256 string, err error)
	// ExtensionAssetURL returns the resolved extension zip URL + sha256（v1
	// assets.extension）。旧 schema → err。
	ExtensionAssetURL(m *VersionManifest) (url, sha256 string, err error)
	// Name is "gitee" or "http" (for /api/status surfacing + logs).
	Name() string
	// Base is the configured base URL (for /api/status dist_url_configured).
	Base() string
}

// assetKey is the manifest key for the current platform (GOOS_GOARCH).
func assetKey() string {
	return fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
}

// httpClient is shared by both backends. gitee raw redirects to
// raw.giteeusercontent.com with a signed token; the default client follows
// redirects, which is what we want.
var updateHTTPClient = &http.Client{Timeout: 60 * time.Second}

// --- giteeSource ---

// giteeSource reads version.json from the public gitee release repo via the
// raw endpoint (anon-readable, redirects to raw.giteeusercontent.com) and
// downloads installers from release-download URLs stored absolute in the
// manifest.
type giteeSource struct{ base string }

func (s *giteeSource) Name() string { return "gitee" }
func (s *giteeSource) Base() string { return s.base }

func (s *giteeSource) FetchVersionManifest() (*VersionManifest, error) {
	// raw URL: <base>/raw/main/version.json — gitee serves the latest main copy.
	url := strings.TrimRight(s.base, "/") + "/raw/main/version.json"
	return fetchManifest(url)
}

func (s *giteeSource) DownloadInstaller(m *VersionManifest, dest string) error {
	url, sha, err := s.AssetURL(m)
	if err != nil {
		return err
	}
	return downloadFile(url, dest, sha)
}

// AssetURL returns the gitee release-download URL (absolute) + sha256 for the
// current platform's GUI package. v1 读 assets.gui.<platform>，回退顶层
// assets.<platform>。 (W2.3a/F4 — exposed for /api/update-check.)
func (s *giteeSource) AssetURL(m *VersionManifest) (string, string, error) {
	a, err := guiAsset(m)
	if err != nil {
		return "", "", err
	}
	// gitee release-download URLs are absolute; use as-is.
	return a.URL, a.SHA256, nil
}

// BridgeAssetURL returns the gitee bridge binary URL + sha256（v1）。
func (s *giteeSource) BridgeAssetURL(m *VersionManifest) (string, string, error) {
	a, err := bridgeAsset(m)
	if err != nil {
		return "", "", err
	}
	return a.URL, a.SHA256, nil
}

// NmHostAssetURL returns the gitee nm-host binary URL + sha256（v1）。
func (s *giteeSource) NmHostAssetURL(m *VersionManifest) (string, string, error) {
	a, err := nmhostAsset(m)
	if err != nil {
		return "", "", err
	}
	return a.URL, a.SHA256, nil
}

// ExtensionAssetURL returns the gitee extension zip URL + sha256（v1）。
func (s *giteeSource) ExtensionAssetURL(m *VersionManifest) (string, string, error) {
	a, err := extensionAsset(m)
	if err != nil {
		return "", "", err
	}
	return a.URL, a.SHA256, nil
}

// --- httpSource ---

// httpSource reads version.json + installer from an internal nginx-style HTTP
// file server (anon-readable on the intranet). Asset URLs in the manifest may
// be relative (e.g. "browser-mcp-0.2.0-windows-amd64.exe") and are resolved
// against base.
type httpSource struct{ base string }

func (s *httpSource) Name() string { return "http" }
func (s *httpSource) Base() string { return s.base }

func (s *httpSource) FetchVersionManifest() (*VersionManifest, error) {
	url := strings.TrimRight(s.base, "/") + "/version.json"
	return fetchManifest(url)
}

func (s *httpSource) DownloadInstaller(m *VersionManifest, dest string) error {
	url, sha, err := s.AssetURL(m)
	if err != nil {
		return err
	}
	return downloadFile(url, dest, sha)
}

// AssetURL resolves the current-platform GUI package URL against base (relative
// → absolute) + returns its sha256. v1 读 assets.gui.<platform>，回退顶层
// assets.<platform>。 (W2.3a/F4 — exposed for /api/update-check.)
func (s *httpSource) AssetURL(m *VersionManifest) (string, string, error) {
	a, err := guiAsset(m)
	if err != nil {
		return "", "", err
	}
	return s.resolveURL(a.URL), a.SHA256, nil
}

// BridgeAssetURL resolves the bridge binary URL against base（v1）。
func (s *httpSource) BridgeAssetURL(m *VersionManifest) (string, string, error) {
	a, err := bridgeAsset(m)
	if err != nil {
		return "", "", err
	}
	return s.resolveURL(a.URL), a.SHA256, nil
}

// NmHostAssetURL resolves the nm-host binary URL against base（v1）。
func (s *httpSource) NmHostAssetURL(m *VersionManifest) (string, string, error) {
	a, err := nmhostAsset(m)
	if err != nil {
		return "", "", err
	}
	return s.resolveURL(a.URL), a.SHA256, nil
}

// ExtensionAssetURL resolves the extension zip URL against base（v1）。
func (s *httpSource) ExtensionAssetURL(m *VersionManifest) (string, string, error) {
	a, err := extensionAsset(m)
	if err != nil {
		return "", "", err
	}
	return s.resolveURL(a.URL), a.SHA256, nil
}

// resolveURL makes a relative asset URL absolute against base; absolute URLs
// (http/https) pass through.
func (s *httpSource) resolveURL(assetURL string) string {
	if strings.HasPrefix(assetURL, "http://") || strings.HasPrefix(assetURL, "https://") {
		return assetURL
	}
	return strings.TrimRight(s.base, "/") + "/" + strings.TrimLeft(assetURL, "/")
}

// --- shared manifest asset extractors（纯 manifest 解析，不含 URL resolve） ---
//
// 这些函数只从 manifest 取原始 url+sha256（相对/绝对未定），由各 source 的 *AssetURL
// 方法包一层 URL resolve（gitee 透传绝对、http 相对 base 拼接）。asset 是否可用以 v1
// 字段是否填充为准（不硬绑 schema 字符串，过渡期更鲁棒）。

// guiAsset 取本平台 GUI 包 asset：v1 优先 assets.gui.<platform>，回退旧顶层
// assets.<platform>（双读窗口）。两者都缺 → err。
// guiPlatformKey maps (goos, goarch) to the channel's platform key convention
// as published by tool-hub (publish-flow.md): win-amd64 / darwin-intel /
// darwin-arm64 / linux-amd64 / linux-arm64. Note darwin-amd64 → "darwin-intel"
// (channel uses intel, not amd64, for the mac x64 slot). win-amd64 is the only
// slot exercised on the current channel; the rest are defined for
// forward-compat with the mac/linux platform-extension change.
//
// This is DIFFERENT from assetKey() (which returns "windows_amd64" — GOOS_GOARCH
// underscore, full names). The tool-hub channel publishes under the abbreviated
// dashed form; guiAsset tries guiPlatformKey first (channel) then assetKey
// (legacy/test fixtures) so both manifest shapes resolve.
func guiPlatformKey(goos, goarch string) string {
	switch {
	case goos == "windows" && goarch == "amd64":
		return "win-amd64"
	case goos == "darwin" && goarch == "amd64":
		return "darwin-intel"
	case goos == "darwin" && goarch == "arm64":
		return "darwin-arm64"
	case goos == "linux" && goarch == "amd64":
		return "linux-amd64"
	case goos == "linux" && goarch == "arm64":
		return "linux-arm64"
	}
	return fmt.Sprintf("%s-%s", goos, goarch)
}

// guiAsset 取本平台 GUI 包 asset。v1 优先读 assets.gui.<channel-platform-key>
// （win-amd64 等通道约定），回退 assets.gui.<assetKey()>（windows_amd64，旧测试
// fixture / legacy manifest），再回退顶层 assets.<assetKey()>（旧 schema 双读窗口）。
func guiAsset(m *VersionManifest) (VersionAsset, error) {
	chanKey := guiPlatformKey(runtime.GOOS, runtime.GOARCH)
	if a, ok := m.AssetsGUI[chanKey]; ok && a.URL != "" {
		return a, nil
	}
	key := assetKey()
	if a, ok := m.AssetsGUI[key]; ok && a.URL != "" {
		return a, nil
	}
	if a, ok := m.Assets[key]; ok && a.URL != "" {
		return a, nil
	}
	return VersionAsset{}, fmt.Errorf("no gui asset for %s (%s) in version.json", chanKey, key)
}

// bridgeAsset 取本平台 bridge 二进制 asset（v1 assets.binaries.<platform>.bridge）。
// 旧 schema 无独立 bridge asset → err。
func bridgeAsset(m *VersionManifest) (VersionAsset, error) {
	key := assetKey()
	bin, ok := m.AssetsBinaries[key]
	if !ok || bin.Bridge.URL == "" {
		return VersionAsset{}, fmt.Errorf("no bridge asset for %s in version.json (v1 binaries required)", key)
	}
	return bin.Bridge, nil
}

// nmhostAsset 取本平台 nm-host 二进制 asset（v1 assets.binaries.<platform>.nmhost）。
func nmhostAsset(m *VersionManifest) (VersionAsset, error) {
	key := assetKey()
	bin, ok := m.AssetsBinaries[key]
	if !ok || bin.NmHost.URL == "" {
		return VersionAsset{}, fmt.Errorf("no nmhost asset for %s in version.json (v1 binaries required)", key)
	}
	return bin.NmHost, nil
}

// extensionAsset 取扩展 zip asset（v1 assets.extension）。旧 schema → err。
func extensionAsset(m *VersionManifest) (ExtensionAsset, error) {
	if m.AssetsExtension.URL == "" {
		return ExtensionAsset{}, fmt.Errorf("no extension asset in version.json (v1 required)")
	}
	return m.AssetsExtension, nil
}

// --- shared helpers ---

// setTelemetryHeaders attaches the version header to an update-channel HTTP
// request: X-Client-Version (current bridge version). 不带 UUID/client_id——
// UUID 遥测曾触发 Chrome/Windows 拦截机制，已移除；内网按来源 IP 区分用户。
// fetchManifest（version-check）+ downloadFile（asset 下载）共用。
func setTelemetryHeaders(req *http.Request) {
	req.Header.Set("X-Client-Version", currentVersion())
}

func fetchManifest(url string) (*VersionManifest, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request %s: %w", url, err)
	}
	setTelemetryHeaders(req)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m VersionManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w (body=%q)", err, truncate(string(body), 200))
	}
	if m.Version == "" {
		return nil, fmt.Errorf("manifest has empty version (body=%q)", truncate(string(body), 200))
	}
	return &m, nil
}

// downloadFile GETs url to dest, optionally verifying sha256. Temp file is
// written alongside dest and renamed on success so a partial download never
// masquerades as a complete installer.
func downloadFile(url, dest, wantSHA256 string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir staging: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request %s: %w", url, err)
	}
	setTelemetryHeaders(req)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if wantSHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, wantSHA256) {
			os.Remove(tmp)
			return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA256)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename to %s: %w", dest, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- factory ---

const (
	defaultGiteeBase = "https://gitee.com/renvip/browser-mcp-release"
	envUpdateSource  = "BROWSER_MCP_UPDATE_SOURCE" // gitee | http
	// update_base is HARDCODED to profile.UpdateBase (ldflag-baked) as of the
	// hardcode-update-base-url change. The former env overrides
	// (BROWSER_MCP_UPDATE_BASE, legacy BROWSER_MCP_DIST_URL) and the config-file
	// update_base field are all removed — runtime has no override path. This
	// eliminates the stale-config-overrides-baked-URL drift that broke auto-update
	// for pre-/tool-hub/ users (see memory update-base-config-file-override-trap).
)

var (
	updateSrcMu sync.Mutex
	updateSrc   UpdateSource
)

// updateSource returns the configured UpdateSource, lazily initialized.
func updateSource() UpdateSource {
	updateSrcMu.Lock()
	defer updateSrcMu.Unlock()
	if updateSrc == nil {
		updateSrc = newUpdateSource()
	}
	return updateSrc
}

// resetUpdateSource clears the cached UpdateSource so the next updateSource()
// call rebuilds it from the current config. Called by /api/config/reload when
// update_source changed (base URL is hardcoded profile.UpdateBase, not in config).
func resetUpdateSource() {
	updateSrcMu.Lock()
	updateSrc = nil
	updateSrcMu.Unlock()
}

// invalidateManifestCache clears the cached version manifest so the next fetch
// hits the (possibly new) distribution source. Called by /api/config/reload
// when the UpdateSource is rebuilt.
func invalidateManifestCache() {
	manifestCacheMu.Lock()
	manifestCache = nil
	manifestCacheTime = time.Time{}
	manifestCacheMu.Unlock()
}

// newUpdateSource builds an UpdateSource from the central config. Only
// `update_source` (the backend selector: gitee | http) is read from config/env
// — the base URL is HARDCODED per backend:
//
//   - http → profile.UpdateBase (ldflag-baked per profile; see profile.go)
//   - gitee → defaultGiteeBase
//
// As of the hardcode-update-base-url change, runtime has NO override path for
// the base URL: the config-file `update_base` field was removed from the
// schema (json decoder silently ignores it on old configs — no migration
// needed), and the env overrides BROWSER_MCP_UPDATE_BASE / BROWSER_MCP_DIST_URL
// were deleted. This eliminates the stale-config-overrides-baked-URL drift
// that broke auto-update for pre-/tool-hub/ users.
//
// update_source itself stays config/env-configurable (it selects the backend
// type, not the URL — not a path-drift vector).
func newUpdateSource() UpdateSource {
	eff := config.EffectiveWithSources()
	src, _ := fieldStr(eff, "update_source")

	switch strings.ToLower(src) {
	case "http":
		return &httpSource{base: profile.UpdateBase}
	case "gitee":
		return &giteeSource{base: defaultGiteeBase}
	default:
		return &giteeSource{base: defaultGiteeBase}
	}
}

// fieldStr extracts a string field's value + from-layer from EffectiveWithSources
// output. Returns ("", "default") on type mismatch (defensive — never panic).
func fieldStr(eff map[string]any, key string) (string, string) {
	m, ok := eff[key].(map[string]any)
	if !ok {
		return "", "default"
	}
	v, _ := m["value"].(string)
	f, _ := m["from"].(string)
	return v, f
}

// stagingDir is the directory under the data dir where installer downloads land
// before the updater task picks them up. ~/.browser-mcp/staging (matches the
// dataDir contract — never %TEMP%, see CLAUDE.md env contract).
func stagingDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("UserHomeDir: %w", err)
	}
	return filepath.Join(home, profile.DataDirName, "staging"), nil
}

// manifestCache memoizes the fetched version manifest so the high-frequency
// /api/status poll doesn't hammer the distribution source on every request.
// /api/update-check (the explicit "check now" button) bypasses the cache via
// FetchVersionManifest directly. TTL 30s: the idle updater ticks every 30s and
// relies on this cache to discover a freshly published version within ~30s
// (end-to-end "publish → bridge sees it" latency), without hitting the source
// on every /api/status poll.
var (
	manifestCacheMu   sync.Mutex
	manifestCache     *VersionManifest
	manifestCacheTime time.Time
	manifestCacheTTL  = 30 * time.Second
)

// cachedManifest returns the cached manifest if fresher than manifestCacheTTL,
// otherwise refetches. Errors are returned (caller decides whether to swallow
// — /api/status swallows to stay green, updateCheck surfaces).
func cachedManifest() (*VersionManifest, error) {
	manifestCacheMu.Lock()
	defer manifestCacheMu.Unlock()
	if manifestCache != nil && time.Since(manifestCacheTime) < manifestCacheTTL {
		return manifestCache, nil
	}
	m, err := updateSource().FetchVersionManifest()
	if err != nil {
		return nil, err
	}
	manifestCache = m
	manifestCacheTime = time.Now()
	return m, nil
}

// isNewerVersion reports whether latest is a higher version than current using
// a dotted-numeric comparison (semver-ish, no pre-release logic — bridge
// versions are simple MAJOR.MINOR.PATCH). Non-numeric segments fall back to
// string compare so a malformed manifest never falsely reports "no update".
// Equal versions → false (no update). Empty latest → false.
func isNewerVersion(latest, current string) bool {
	if latest == "" || latest == current {
		return false
	}
	// Normalize a leading "v" prefix (semver convention) so "0.2.2" compares
	// correctly against a git-describe current like "v0.2.1-30-gabcd-dirty".
	// Without this, segment 0 is "0" vs "v0" → lexicographic "0"<"v0" → false
	// negative (the v-prefixed current always "wins"). (W2.3a/F4 fix.)
	latest = strings.TrimPrefix(latest, "v")
	current = strings.TrimPrefix(current, "v")
	la := strings.Split(latest, ".")
	cu := strings.Split(current, ".")
	n := len(la)
	if len(cu) > n {
		n = len(cu)
	}
	for i := 0; i < n; i++ {
		var l, c string
		if i < len(la) {
			l = la[i]
		}
		if i < len(cu) {
			c = cu[i]
		}
		// Strip any non-numeric suffix (e.g. "0-rc1" → "0") for numeric compare.
		ln := numPrefix(l)
		cn := numPrefix(c)
		if ln != cn {
			return ln > cn
		}
		if l != c {
			return l > c // numeric-equal but suffix differs → lexicographic tiebreak
		}
	}
	return false // all segments equal
}

// numPrefix returns the leading numeric prefix of s as an int (0 if none).
func numPrefix(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 {
		return 0
	}
	v := 0
	for i := 0; i < n; i++ {
		v = v*10 + int(s[i]-'0')
	}
	return v
}
