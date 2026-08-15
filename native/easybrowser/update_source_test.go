package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/profile"
)

// TestAssetKey confirms the manifest key matches the running platform so the
// right installer is selected.
func TestAssetKey(t *testing.T) {
	want := runtime.GOOS + "_" + runtime.GOARCH
	if got := assetKey(); got != want {
		t.Fatalf("assetKey=%q want %q", got, want)
	}
}

// TestGuiPlatformKey confirms the channel-convention platform key mapping
// (tool-hub publishes under win-amd64 / darwin-intel / darwin-arm64 / linux-amd64
// / linux-arm64, NOT the assetKey() windows_amd64 form).
func TestGuiPlatformKey(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"windows", "amd64", "win-amd64"},
		{"darwin", "amd64", "darwin-intel"},
		{"darwin", "arm64", "darwin-arm64"},
		{"linux", "amd64", "linux-amd64"},
		{"linux", "arm64", "linux-arm64"},
		{"unknown", "weird", "unknown-weird"}, // fallback formula
	}
	for _, c := range cases {
		if got := guiPlatformKey(c.goos, c.goarch); got != c.want {
			t.Fatalf("guiPlatformKey(%s,%s)=%q want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// TestGuiAssetChannelKey confirms guiAsset resolves a manifest whose gui asset
// is published under the channel-convention key (win-amd64), which is what the
// real tool-hub channel serves. Regression for the platform-key mismatch that
// left the GUI update URL unresolved even when a gui asset existed.
func TestGuiAssetChannelKey(t *testing.T) {
	chanKey := guiPlatformKey(runtime.GOOS, runtime.GOARCH)
	raw := `{
	  "schema": "browser-mcp-v1",
	  "version": "0.3.0",
	  "assets": {
	    "gui": { "__KEY__": {"url": "Browser MCP Setup.exe", "sha256": "CHANNEL_SHA"} }
	  }
	}`
	raw = strings.ReplaceAll(raw, "__KEY__", chanKey)
	var m VersionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a, err := guiAsset(&m)
	if err != nil {
		t.Fatalf("guiAsset err=%v (chanKey=%s) — channel manifest must resolve", err, chanKey)
	}
	if a.SHA256 != "CHANNEL_SHA" {
		t.Fatalf("guiAsset sha=%q want CHANNEL_SHA (chanKey=%s)", a.SHA256, chanKey)
	}
}

// TestGuiAssetLegacyKey confirms guiAsset still resolves the legacy
// assetKey() form (windows_amd64) so old test fixtures / legacy manifests
// don't break (dual-read).
func TestGuiAssetLegacyKey(t *testing.T) {
	legacyKey := assetKey()
	raw := `{
	  "schema": "browser-mcp-v1",
	  "version": "0.3.0",
	  "assets": {
	    "gui": { "__KEY__": {"url": "gui.exe", "sha256": "LEGACY_SHA"} }
	  }
	}`
	raw = strings.ReplaceAll(raw, "__KEY__", legacyKey)
	var m VersionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a, err := guiAsset(&m)
	if err != nil {
		t.Fatalf("guiAsset err=%v (legacyKey=%s)", err, legacyKey)
	}
	if a.SHA256 != "LEGACY_SHA" {
		t.Fatalf("guiAsset sha=%q want LEGACY_SHA", a.SHA256)
	}
}

// TestParseVersionManifest round-trips the documented schema.
func TestParseVersionManifest(t *testing.T) {
	raw := `{"version":"0.2.0","released_at":"2026-06-25T20:00:00+08:00","assets":{"windows_amd64":{"url":"https://x/y.exe","sha256":"abc"}}}`
	var m VersionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Version != "0.2.0" {
		t.Fatalf("version=%q", m.Version)
	}
	a, ok := m.Assets["windows_amd64"]
	if !ok {
		t.Fatal("no windows_amd64 asset")
	}
	if a.URL != "https://x/y.exe" || a.SHA256 != "abc" {
		t.Fatalf("asset=%+v", a)
	}
}

// TestFetchManifest_OK serves a valid manifest over httptest and confirms it
// parses + validates the non-empty version rule.
func TestFetchManifest_OK(t *testing.T) {
	body := `{"version":"0.2.0","assets":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()
	m, err := fetchManifest(srv.URL)
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if m.Version != "0.2.0" {
		t.Fatalf("version=%q", m.Version)
	}
}

// TestFetchManifest_EmptyVersionRejected guards the "empty version = corrupt
// manifest" rule (gitee placeholder / a mispublished version.json must not be
// treated as a real version).
func TestFetchManifest_EmptyVersionRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"","assets":{}}`)
	}))
	defer srv.Close()
	if _, err := fetchManifest(srv.URL); err == nil {
		t.Fatal("expected error for empty version")
	}
}

// TestFetchManifest_Non200 surfaces the HTTP status in the error (gitee raw
// returns 302→login for a private branch; a 404 for a missing file).
func TestFetchManifest_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := fetchManifest(srv.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 in error, got %v", err)
	}
}

// TestHTTPSource_ResolveURL confirms relative asset URLs are rooted at base and
// absolute ones pass through (the contract that lets one manifest work for both
// gitee absolute release URLs and nginx relative filenames).
func TestHTTPSource_ResolveURL(t *testing.T) {
	s := &httpSource{base: "http://dist.intranet/browser-mcp/"}
	cases := map[string]string{
		"installer.exe":                              "http://dist.intranet/browser-mcp/installer.exe",
		"/browser-mcp-0.2.0-windows-amd64.exe":       "http://dist.intranet/browser-mcp/browser-mcp-0.2.0-windows-amd64.exe",
		"https://gitee.com/x/y/raw/main/installer.exe": "https://gitee.com/x/y/raw/main/installer.exe",
		"http://other.example/z.exe":                 "http://other.example/z.exe",
	}
	for in, want := range cases {
		if got := s.resolveURL(in); got != want {
			t.Errorf("resolveURL(%q)=%q want %q", in, got, want)
		}
	}
}

// TestDownloadFile_OK writes a known body through an httptest server, downloads
// it with a matching sha256, and confirms the dest file content + that the
// .part temp is gone (rename-on-success contract).
func TestDownloadFile_OK(t *testing.T) {
	want := []byte("hello installer body")
	sum := sha256.Sum256(want)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "installer.exe")
	if err := downloadFile(srv.URL, dest, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dest body mismatch")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part temp should be gone after rename, got %v", err)
	}
}

// TestTelemetryHeaders_FetchManifest asserts fetchManifest sends the
// X-Client-Version header (UUID X-Client-Id removed — no longer sent).
func TestTelemetryHeaders_FetchManifest(t *testing.T) {
	t.Setenv("BROWSER_MCP_DATA_DIR", t.TempDir())

	var gotID, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get("X-Client-Id")
		gotVer = r.Header.Get("X-Client-Version")
		// Minimal valid manifest so fetchManifest parses without error.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"0.0.1","assets":{}}`))
	}))
	defer srv.Close()

	if _, err := fetchManifest(srv.URL); err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if gotID != "" {
		t.Fatalf("X-Client-Id = %q, want empty (UUID removed)", gotID)
	}
	if gotVer != currentVersion() {
		t.Fatalf("X-Client-Version = %q, want %q (currentVersion())", gotVer, currentVersion())
	}
}

// TestTelemetryHeaders_DownloadFile asserts downloadFile sends the
// X-Client-Version header (UUID X-Client-Id removed — no longer sent).
func TestTelemetryHeaders_DownloadFile(t *testing.T) {
	t.Setenv("BROWSER_MCP_DATA_DIR", t.TempDir())

	var gotID, gotVer string
	body := []byte("telemetry-header-probe")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get("X-Client-Id")
		gotVer = r.Header.Get("X-Client-Version")
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "asset.bin")
	if err := downloadFile(srv.URL, dest, ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if gotID != "" {
		t.Fatalf("X-Client-Id = %q, want empty (UUID removed)", gotID)
	}
	if gotVer != currentVersion() {
		t.Fatalf("X-Client-Version = %q, want %q", gotVer, currentVersion())
	}
}

// TestDownloadFile_SHA256Mismatch confirms a bad checksum leaves no partial
// file at dest (so the updater never runs a corrupt installer).
func TestDownloadFile_SHA256Mismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not the expected content"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "installer.exe")
	err := downloadFile(srv.URL, dest, "deadbeef")
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest must not exist after mismatch, got %v", err)
	}
}

// TestNewUpdateSource_HardcodedBase pins the hardcode-update-base-url contract:
// the http base URL is ALWAYS profile.UpdateBase (ldflag-baked). The former
// runtime override paths — env BROWSER_MCP_UPDATE_BASE, legacy
// BROWSER_MCP_DIST_URL, and the config-file update_base field — are all
// ignored (the field was removed from the config schema; the env constants
// were deleted). Runtime has no override path.
func TestNewUpdateSource_HardcodedBase(t *testing.T) {
	// Isolate data dir so ~/.browser-mcp/browser-mcp.json doesn't leak in.
	t.Setenv("BROWSER_MCP_DATA_DIR", t.TempDir())
	// Pin profile.UpdateBase to a known value — same mechanism as the ldflag
	// bake, exercised at test time (not a runtime override path).
	wantBase := "http://test-hardcoded.intranet/bm"
	old := profile.UpdateBase
	profile.UpdateBase = wantBase
	t.Cleanup(func() { profile.UpdateBase = old })
	resetUpdateSource()

	// All three former override paths set to wrong values — must be IGNORED.
	t.Setenv("BROWSER_MCP_UPDATE_BASE", "http://env-override.invalid/bm")
	t.Setenv("BROWSER_MCP_DIST_URL", "http://legacy-override.invalid/bm")

	// http source → always profile.UpdateBase, never the env/legacy values.
	t.Setenv(envUpdateSource, "http")
	resetUpdateSource()
	s := newUpdateSource()
	if s.Name() != "http" || s.Base() != wantBase {
		t.Fatalf("http: name=%s base=%q want %q (env+distURL overrides must be ignored)", s.Name(), s.Base(), wantBase)
	}

	// Default (no explicit source) → defaultConfig().UpdateSource == "http" →
	// profile.UpdateBase (the test profile's baked base).
	t.Setenv(envUpdateSource, "")
	resetUpdateSource()
	if s := newUpdateSource(); s.Name() != "http" || s.Base() != wantBase {
		t.Fatalf("default: name=%s base=%q want http/%q", s.Name(), s.Base(), wantBase)
	}

	// gitee source → defaultGiteeBase (base var unused, update_base irrelevant).
	t.Setenv(envUpdateSource, "gitee")
	resetUpdateSource()
	if s := newUpdateSource(); s.Name() != "gitee" || s.Base() != defaultGiteeBase {
		t.Fatalf("gitee: name=%s base=%s want %s", s.Name(), s.Base(), defaultGiteeBase)
	}
}

// TestNewUpdateSource_ConfigFileUpdateBaseIgnored confirms a config file with
// a stale `update_base` key (pre-hardcode configs) is silently ignored — the
// json decoder drops the unknown field, and newUpdateSource still uses
// profile.UpdateBase for the http backend.
func TestNewUpdateSource_ConfigFileUpdateBaseIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dir)
	// Write a config file with a stale update_base (pre-/tool-hub/ URL).
	configPath := filepath.Join(dir, "browser-mcp.json")
	if err := os.WriteFile(configPath, []byte(`{"update_source":"http","update_base":"http://stake.invalid/browser-mcp/stable","idle_min":5}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wantBase := "http://test-hardcoded.intranet/bm"
	old := profile.UpdateBase
	profile.UpdateBase = wantBase
	t.Cleanup(func() { profile.UpdateBase = old })
	resetUpdateSource()

	s := newUpdateSource()
	if s.Name() != "http" || s.Base() != wantBase {
		t.Fatalf("stale config update_base must be ignored: name=%s base=%q want %q", s.Name(), s.Base(), wantBase)
	}
}

// TestGiteeSource_ManifestURL confirms the raw URL shape (latest main copy).
func TestGiteeSource_ManifestURL(t *testing.T) {
	s := &giteeSource{base: defaultGiteeBase}
	// Indirectly: serve at the expected raw path and fetch.
	want := `{"version":"9.9.9","assets":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/raw/main/version.json" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		io.WriteString(w, want)
	}))
	defer srv.Close()
	// Swap base to the test server to exercise the URL-build + fetch path.
	s.base = srv.URL
	m, err := s.FetchVersionManifest()
	if err != nil {
		t.Fatalf("FetchVersionManifest: %v", err)
	}
	if m.Version != "9.9.9" {
		t.Fatalf("version=%q", m.Version)
	}
}

// TestStagingDir_NotTemp asserts the staging dir lives under ~/.browser-mcp,
// never os.TempDir (CLAUDE.md env contract — %TEMP% got cleaned mid-update once).
func TestStagingDir_NotTemp(t *testing.T) {
	dir, err := stagingDir()
	if err != nil {
		t.Fatalf("stagingDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".browser-mcp", "staging")
	if dir != want {
		t.Fatalf("stagingDir=%q want %q", dir, want)
	}
}

// TestIsNewerVersion_VPrefixStrip: a manifest version without a "v" prefix
// ("0.2.2") must compare as newer than a git-describe current with a "v" prefix
// ("v0.2.1-30-gabcd-dirty"). Without the v-strip, segment 0 is "0" vs "v0" →
// lexicographic "0"<"v0" → false negative. (W2.3a/F4 fix.)
func TestIsNewerVersion_VPrefixStrip(t *testing.T) {
	cases := []struct{ latest, current string; want bool }{
		{"0.2.2", "v0.2.1-30-gabcd-dirty", true},   // v-prefix + git-describe suffix
		{"v0.2.2", "v0.2.1", true},                 // both v-prefixed, clean
		{"0.2.2", "0.2.1", true},                   // neither v-prefixed
		{"0.2.1", "v0.2.1-30-gabcd-dirty", false},  // same base, git-describe ahead → not newer
		{"0.2.0", "v0.2.1", false},                 // older
		{"", "v0.2.1", false},                      // empty latest
	}
	for _, c := range cases {
		if got := isNewerVersion(c.latest, c.current); got != c.want {
			t.Errorf("isNewerVersion(%q,%q)=%v want %v", c.latest, c.current, got, c.want)
		}
	}
}

// --- C2/C3: v1 schema（browser-mcp-v1）拆 asset + 双读兼容测试 ---

// v1ManifestFixture 构造一个 v1 schema manifest，平台 key 用运行平台（assetKey），
// 以便 Bridge/NmHost/Extension/GUI 各方法都能取到本平台 asset。
func v1ManifestFixture(t *testing.T) *VersionManifest {
	t.Helper()
	key := assetKey()
	raw := `{
	  "schema": "browser-mcp-v1",
	  "version": "0.3.0",
	  "released_at": "2026-06-29T20:00:00+08:00",
	  "min_host_version": "0.2.0",
	  "assets": {
	    "gui": { "__KEY__": {"url": "gui.exe", "sha256": "GUI_SHA"} },
	    "binaries": { "__KEY__": {
	      "bridge": {"url": "bridge.exe", "sha256": "BRIDGE_SHA"},
	      "nmhost": {"url": "nmhost.exe", "sha256": "NM_SHA"}
	    }},
	    "extension": {"url": "extension.zip", "sha256": "EXT_SHA", "version": "0.3.0"},
	    "__KEY__": {"url": "gui.exe", "sha256": "GUI_SHA"}
	  }
	}`
	raw = strings.ReplaceAll(raw, "__KEY__", key)
	var m VersionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal v1 fixture: %v", err)
	}
	return &m
}

// TestV1Schema_Parse 确认 v1 manifest 拆出 gui/binaries/extension + 保留旧顶层平台 asset。
func TestV1Schema_Parse(t *testing.T) {
	m := v1ManifestFixture(t)
	if !m.IsV1() {
		t.Fatalf("IsV1=false, schema=%q", m.Schema)
	}
	if m.Version != "0.3.0" {
		t.Fatalf("version=%q", m.Version)
	}
	if m.MinHostVersion != "0.2.0" {
		t.Fatalf("min_host_version=%q", m.MinHostVersion)
	}
	key := assetKey()
	// gui 拆到 AssetsGUI，不留在 Assets。
	if _, ok := m.Assets["gui"]; ok {
		t.Fatal(" Assets should not contain gui key (moved to AssetsGUI)")
	}
	ga, ok := m.AssetsGUI[key]
	if !ok || ga.URL != "gui.exe" || ga.SHA256 != "GUI_SHA" {
		t.Fatalf("AssetsGUI[%s]=%+v", key, ga)
	}
	// binaries 拆到 AssetsBinaries。
	bin, ok := m.AssetsBinaries[key]
	if !ok || bin.Bridge.URL != "bridge.exe" || bin.Bridge.SHA256 != "BRIDGE_SHA" {
		t.Fatalf("AssetsBinaries[%s]=%+v", key, bin)
	}
	if bin.NmHost.URL != "nmhost.exe" || bin.NmHost.SHA256 != "NM_SHA" {
		t.Fatalf("NmHost=%+v", bin.NmHost)
	}
	// extension 拆到 AssetsExtension。
	if m.AssetsExtension.URL != "extension.zip" || m.AssetsExtension.SHA256 != "EXT_SHA" || m.AssetsExtension.Version != "0.3.0" {
		t.Fatalf("AssetsExtension=%+v", m.AssetsExtension)
	}
	// 旧顶层平台 asset 仍在 m.Assets（双读窗口）。
	legacy, ok := m.Assets[key]
	if !ok || legacy.URL != "gui.exe" || legacy.SHA256 != "GUI_SHA" {
		t.Fatalf("legacy Assets[%s]=%+v", key, legacy)
	}
}

// TestV1Schema_AssetURLs 确认各 *AssetURL 方法取对平台 asset；httpSource 解析相对 URL，
// giteeSource 透传绝对。
func TestV1Schema_AssetURLs(t *testing.T) {
	m := v1ManifestFixture(t)

	t.Run("httpSource resolves relative against base", func(t *testing.T) {
		s := &httpSource{base: "http://dist.intranet/browser-mcp/"}
		url, sha, err := s.AssetURL(m)
		if err != nil || url != "http://dist.intranet/browser-mcp/gui.exe" || sha != "GUI_SHA" {
			t.Fatalf("AssetURL(gui): url=%q sha=%q err=%v", url, sha, err)
		}
		url, sha, err = s.BridgeAssetURL(m)
		if err != nil || url != "http://dist.intranet/browser-mcp/bridge.exe" || sha != "BRIDGE_SHA" {
			t.Fatalf("BridgeAssetURL: url=%q sha=%q err=%v", url, sha, err)
		}
		url, sha, err = s.NmHostAssetURL(m)
		if err != nil || url != "http://dist.intranet/browser-mcp/nmhost.exe" || sha != "NM_SHA" {
			t.Fatalf("NmHostAssetURL: url=%q sha=%q err=%v", url, sha, err)
		}
		url, sha, err = s.ExtensionAssetURL(m)
		if err != nil || url != "http://dist.intranet/browser-mcp/extension.zip" || sha != "EXT_SHA" {
			t.Fatalf("ExtensionAssetURL: url=%q sha=%q err=%v", url, sha, err)
		}
	})

	t.Run("giteeSource passes absolute URL through", func(t *testing.T) {
		s := &giteeSource{base: defaultGiteeBase}
		url, sha, err := s.AssetURL(m)
		if err != nil || url != "gui.exe" || sha != "GUI_SHA" {
			t.Fatalf("AssetURL(gui): url=%q sha=%q err=%v", url, sha, err)
		}
		url, sha, err = s.BridgeAssetURL(m)
		if err != nil || url != "bridge.exe" || sha != "BRIDGE_SHA" {
			t.Fatalf("BridgeAssetURL: url=%q sha=%q err=%v", url, sha, err)
		}
		url, sha, err = s.NmHostAssetURL(m)
		if err != nil || url != "nmhost.exe" || sha != "NM_SHA" {
			t.Fatalf("NmHostAssetURL: url=%q sha=%q err=%v", url, sha, err)
		}
		url, sha, err = s.ExtensionAssetURL(m)
		if err != nil || url != "extension.zip" || sha != "EXT_SHA" {
			t.Fatalf("ExtensionAssetURL: url=%q sha=%q err=%v", url, sha, err)
		}
	})
}

// TestOldSchema_Fallback 确认旧 schema（无 binaries/extension，仅顶层平台 asset）
// 双读：AssetURL(gui) 读顶层；Bridge/NmHost/Extension 返回 err（旧 manifest 无独立 asset）。
func TestOldSchema_Fallback(t *testing.T) {
	key := assetKey()
	raw := `{
	  "version": "0.2.2",
	  "released_at": "2026-06-27T10:53:00+08:00",
	  "assets": { "__KEY__": {"url": "BrowserMCP.exe", "sha256": "OLD_GUI_SHA"} }
	}`
	raw = strings.ReplaceAll(raw, "__KEY__", key)
	var m VersionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal old schema: %v", err)
	}
	if m.IsV1() {
		t.Fatalf("IsV1=true for old schema, schema=%q", m.Schema)
	}
	if m.Schema != "" {
		t.Fatalf("old schema Schema should be empty, got %q", m.Schema)
	}
	s := &httpSource{base: "http://dist.intranet/browser-mcp/"}
	// GUI 走旧顶层 assets.<platform> 回退。
	url, sha, err := s.AssetURL(&m)
	if err != nil || url != "http://dist.intranet/browser-mcp/BrowserMCP.exe" || sha != "OLD_GUI_SHA" {
		t.Fatalf("AssetURL(gui) old fallback: url=%q sha=%q err=%v", url, sha, err)
	}
	// 旧 manifest 无独立 bridge/nmhost/extension asset → err。
	for _, call := range []struct {
		name string
		fn   func(*VersionManifest) (string, string, error)
	}{
		{"BridgeAssetURL", s.BridgeAssetURL},
		{"NmHostAssetURL", s.NmHostAssetURL},
		{"ExtensionAssetURL", s.ExtensionAssetURL},
	} {
		if _, _, err := call.fn(&m); err == nil {
			t.Fatalf("%s: expected err for old schema (no v1 asset), got nil", call.name)
		}
	}
}

// TestSchemaField_Distinguishes 确认 schema 字段被解析并区分新旧。
func TestSchemaField_Distinguishes(t *testing.T) {
	v1 := `{"schema":"browser-mcp-v1","version":"0.3.0","assets":{"extension":{"url":"x.zip"}}}`
	var m1 VersionManifest
	if err := json.Unmarshal([]byte(v1), &m1); err != nil {
		t.Fatalf("unmarshal v1: %v", err)
	}
	if m1.Schema != "browser-mcp-v1" || !m1.IsV1() {
		t.Fatalf("v1: schema=%q IsV1=%v", m1.Schema, m1.IsV1())
	}
	old := `{"version":"0.2.2","assets":{"windows_amd64":{"url":"x.exe"}}}`
	var m2 VersionManifest
	if err := json.Unmarshal([]byte(old), &m2); err != nil {
		t.Fatalf("unmarshal old: %v", err)
	}
	if m2.Schema != "" || m2.IsV1() {
		t.Fatalf("old: schema=%q IsV1=%v", m2.Schema, m2.IsV1())
	}
}

// TestGUIAsset_FallbackToLegacy 确认 v1 manifest 缺 assets.gui.<platform> 时，AssetURL
// 回退顶层 assets.<platform>（双读窗口，过渡期旧 GUI 字段仍可用）。
func TestGUIAsset_FallbackToLegacy(t *testing.T) {
	key := assetKey()
	// v1 schema 但 gui 字段缺本平台条目，只有顶层旧 asset。
	raw := `{
	  "schema":"browser-mcp-v1",
	  "version":"0.3.0",
	  "assets": {
	    "gui": {"other_os_arch": {"url":"other.exe","sha256":"X"}},
	    "binaries": {"__KEY__": {"bridge":{"url":"b.exe","sha256":"BS"},"nmhost":{"url":"n.exe","sha256":"NS"}}},
	    "extension": {"url":"e.zip","sha256":"ES"},
	    "__KEY__": {"url": "legacy-gui.exe", "sha256": "LEGACY_SHA"}
	  }
	}`
	raw = strings.ReplaceAll(raw, "__KEY__", key)
	var m VersionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := &httpSource{base: "http://dist/"}
	url, sha, err := s.AssetURL(&m)
	if err != nil || url != "http://dist/legacy-gui.exe" || sha != "LEGACY_SHA" {
		t.Fatalf("AssetURL gui fallback to legacy: url=%q sha=%q err=%v", url, sha, err)
	}
	// bridge 仍走 v1 binaries（不应受 gui 缺失影响）。
	url, sha, err = s.BridgeAssetURL(&m)
	if err != nil || url != "http://dist/b.exe" || sha != "BS" {
		t.Fatalf("BridgeAssetURL: url=%q sha=%q err=%v", url, sha, err)
	}
}

