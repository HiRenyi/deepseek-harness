package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/profile"
)

// setupFullUpdateSource stands up an httptest distribution source serving a v1
// version.json + bridge + nm-host + extension assets, with BROWSER_MCP_DATA_DIR
// isolation. Lets the caller pass per-asset sha overrides ("" = compute real).
// Mirrors setupSelfUpdateSource but adds nmhost + extension assets for D1/D2.
func setupFullUpdateSource(t *testing.T, manifestVersion, currentVer string, bridgeBytes, nmhostBytes, extZipBytes []byte, bridgeSHA, nmSHA, extSHA string) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dataDir)
	t.Setenv("BROWSER_MCP_UPDATE_SOURCE", "http")
	if currentVer != "" {
		if err := os.WriteFile(filepath.Join(dataDir, "bridge_version"), []byte(currentVer), 0o644); err != nil {
			t.Fatalf("write bridge_version: %v", err)
		}
	}
	realSHA := func(b []byte, want string) string {
		if want != "" {
			return want
		}
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	bridgeSHA = realSHA(bridgeBytes, bridgeSHA)
	nmSHA = realSHA(nmhostBytes, nmSHA)
	extSHA = realSHA(extZipBytes, extSHA)
	key := assetKey()
	manifest := `{
	  "schema": "browser-mcp-v1",
	  "version": "` + manifestVersion + `",
	  "assets": {
	    "binaries": { "` + key + `": {
	      "bridge":  {"url": "bridge-new",   "sha256": "` + bridgeSHA + `"},
	      "nmhost":  {"url": "nm-host-new",  "sha256": "` + nmSHA + `"}
	    }},
	    "extension": {"url": "extension.zip", "sha256": "` + extSHA + `", "version": "` + manifestVersion + `"}
	  }
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version.json":
			io.WriteString(w, manifest)
		case "/bridge-new":
			w.Write(bridgeBytes)
		case "/nm-host-new":
			w.Write(nmhostBytes)
		case "/extension.zip":
			w.Write(extZipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	// hardcode-update-base-url: env BROWSER_MCP_UPDATE_BASE removed — set
	// profile.UpdateBase directly (test-only, same mechanism as ldflag bake).
	oldBase := profile.UpdateBase
	profile.UpdateBase = srv.URL
	t.Cleanup(func() { profile.UpdateBase = oldBase })
	resetUpdateSource()
	invalidateManifestCache()
	return dataDir
}

// makeExtensionZip builds a zip whose root contains manifest.json {version} +
// one extra file (content.js) so unzipTo's file + dir paths are both exercised.
func makeExtensionZip(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	add("manifest.json", `{"manifest_version":3,"name":"Browser MCP","version":"`+version+`"}`)
	add("content.js", `console.log("hi");`)
	// nested dir entry to exercise directory creation path
	add("icons/icon.png", "FAKE-PNG")
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// --- D1: nm-host swap handler ---

// TestSwapNmHostAsset_Success: nm-host asset downloads + sha 校验 + swap 到位，
// 旧 nm-host 被改名为 .old（D1 验收".old 生成"）。
func TestSwapNmHostAsset_Success(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	if err := os.WriteFile(nmPath, []byte("OLD-NMHOST"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NEW-NMHOST"), nil, "", "", "")

	orig := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = orig })

	m, err := updateSource().FetchVersionManifest()
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	staging := filepath.Join(dataDir, "staging")
	if err := nmHostCoAssetHandler(m, staging); err != nil {
		t.Fatalf("nmHostCoAssetHandler: %v", err)
	}
	got, err := os.ReadFile(nmPath)
	if err != nil {
		t.Fatalf("read nm-host after swap: %v", err)
	}
	if string(got) != "NEW-NMHOST" {
		t.Fatalf("nm-host content=%q want NEW-NMHOST", string(got))
	}
	// .old 生成（保留作 rollback artifact，下次 swap 开头清掉）。
	old, err := os.ReadFile(nmPath + ".old")
	if err != nil {
		t.Fatalf(".old should exist after swap: %v", err)
	}
	if string(old) != "OLD-NMHOST" {
		t.Fatalf(".old content=%q want OLD-NMHOST", string(old))
	}
	// staged 文件被消费。
	if _, err := os.Stat(filepath.Join(staging, "nm-host-new")); !os.IsNotExist(err) {
		t.Fatalf("staged nm-host-new should be gone, got %v", err)
	}
}

// TestSwapNmHostAsset_NoExistingDst: 全新安装 nm-host 尚未就位 → 直接放入新二进
// 制（无 .old 生成，不报错）。
func TestSwapNmHostAsset_NoExistingDst(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe") // 不存在
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("FRESH-NMHOST"), nil, "", "", "")

	orig := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = orig })

	m, _ := updateSource().FetchVersionManifest()
	if err := nmHostCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("nmHostCoAssetHandler on fresh install: %v", err)
	}
	got, err := os.ReadFile(nmPath)
	if err != nil {
		t.Fatalf("read nm-host: %v", err)
	}
	if string(got) != "FRESH-NMHOST" {
		t.Fatalf("nm-host=%q want FRESH-NMHOST", string(got))
	}
	if _, err := os.Stat(nmPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old should NOT exist when dst was absent, got %v", err)
	}
}

// TestSwapNmHostAsset_SHA256Mismatch: bad sha → downloadFile 报错，旧 nm-host 不动，
// 无 .old 生成。
func TestSwapNmHostAsset_SHA256Mismatch(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	if err := os.WriteFile(nmPath, []byte("OLD-NMHOST"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 传错 sha（"deadbeef..." 不会匹配 NEW-NMHOST 的真实 sha）。
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NEW-NMHOST"), nil, "", "deadbeefdeadbeef", "")

	orig := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = orig })

	m, _ := updateSource().FetchVersionManifest()
	err := nmHostCoAssetHandler(m, filepath.Join(dataDir, "staging"))
	if err == nil {
		t.Fatal("sha mismatch should return error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "sha256") && !strings.Contains(strings.ToLower(err.Error()), "mismatch") {
		t.Fatalf("error=%q should mention sha256/mismatch", err.Error())
	}
	// 旧 nm-host 不动。
	got, _ := os.ReadFile(nmPath)
	if string(got) != "OLD-NMHOST" {
		t.Fatalf("nm-host should be untouched on sha mismatch, got %q", string(got))
	}
	// 无 .old。
	if _, err := os.Stat(nmPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old should NOT exist on mismatch, got %v", err)
	}
}

// --- D1 真机 DV 修复：nm-host 运行中先 kill 再 rename + retry ---

// TestSwapNmHostAsset_KillsRunningBeforeRename: nm-host 进程占用 nmPath 时，
// swap 前必须先调 killProcessAt(nmPath) 释放 Chrome 持有的句柄（D1 真机 DV
// "Access is denied" 根因修复）。注入 fake killProcessAtFunc 断言被调用且
// 路径正确，swap 成功。
func TestSwapNmHostAsset_KillsRunningBeforeRename(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	if err := os.WriteFile(nmPath, []byte("OLD-NMHOST"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NEW-NMHOST"), nil, "", "", "")

	origPath := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = origPath })

	killCalls := 0
	var killPaths []string
	origKill := killProcessAtFunc
	killProcessAtFunc = func(p string) error {
		killCalls++
		killPaths = append(killPaths, p)
		return nil
	}
	t.Cleanup(func() { killProcessAtFunc = origKill })

	m, _ := updateSource().FetchVersionManifest()
	if err := nmHostCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("nmHostCoAssetHandler: %v", err)
	}
	if killCalls == 0 {
		t.Fatal("killProcessAt must be called before rename (Chrome holds nm-host handle)")
	}
	if killPaths[0] != nmPath {
		t.Fatalf("kill path=%q want %q", killPaths[0], nmPath)
	}
	got, _ := os.ReadFile(nmPath)
	if string(got) != "NEW-NMHOST" {
		t.Fatalf("nm-host=%q want NEW-NMHOST", string(got))
	}
}

// TestSwapNmHostAsset_NoKillNoRunning: killProcessAt 找不到运行中进程（返回 nil，
// 未命中任何 pid）→ 不报错，直接 swap 成功。覆盖"nm-host 没在跑"的正常路径。
func TestSwapNmHostAsset_NoKillNoRunning(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	if err := os.WriteFile(nmPath, []byte("OLD-NMHOST"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NEW-NMHOST"), nil, "", "", "")

	origPath := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = origPath })

	// fake kill：模拟"没找到运行中进程"——返回 nil，killCalls 计数但不真杀。
	killCalls := 0
	origKill := killProcessAtFunc
	killProcessAtFunc = func(string) error {
		killCalls++
		return nil
	}
	t.Cleanup(func() { killProcessAtFunc = origKill })

	m, _ := updateSource().FetchVersionManifest()
	if err := nmHostCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("handler should succeed when no nm-host running: %v", err)
	}
	if killCalls == 0 {
		t.Fatal("killProcessAt should still be invoked (best-effort) before swap")
	}
	got, _ := os.ReadFile(nmPath)
	if string(got) != "NEW-NMHOST" {
		t.Fatalf("nm-host=%q want NEW-NMHOST", string(got))
	}
}

// TestSwapNmHostAsset_RetryRenameOnAccessDenied: swapCoAssetBinary 第一次 rename
// 返回 "Access is denied"（模拟 nm-host 句柄未释放），handler 应 retry——
// 第二次成功。断言 swap 被重试且最终 swap 成功。
func TestSwapNmHostAsset_RetryRenameOnAccessDenied(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	if err := os.WriteFile(nmPath, []byte("OLD-NMHOST"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NEW-NMHOST"), nil, "", "", "")

	origPath := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = origPath })

	// fake kill：best-effort no-op（模拟 kill 后句柄将释放）。
	origKill := killProcessAtFunc
	killProcessAtFunc = func(string) error { return nil }
	t.Cleanup(func() { killProcessAtFunc = origKill })

	// fake swap：第一次失败（Access denied），第二次走真实 swap 成功。
	swapCalls := 0
	realSwap := swapCoAssetBinaryFunc
	swapCoAssetBinaryFunc = func(dst, tmp string) error {
		swapCalls++
		if swapCalls == 1 {
			return fmt.Errorf("rename old nm-host: rename %s %s: Access is denied.", dst, dst+".old")
		}
		return realSwap(dst, tmp)
	}
	t.Cleanup(func() { swapCoAssetBinaryFunc = realSwap })

	m, _ := updateSource().FetchVersionManifest()
	if err := nmHostCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("handler should retry-succeed after Access denied: %v", err)
	}
	if swapCalls < 2 {
		t.Fatalf("swap should be retried after Access denied, calls=%d", swapCalls)
	}
	got, _ := os.ReadFile(nmPath)
	if string(got) != "NEW-NMHOST" {
		t.Fatalf("nm-host=%q want NEW-NMHOST after retry", string(got))
	}
}

// TestSwapNmHostAsset_RetryExhaustedAborts: swapCoAssetBinary 每次都失败 →
// retry 耗尽后返回最后一次错误（不无限重试）。
func TestSwapNmHostAsset_RetryExhaustedAborts(t *testing.T) {
	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	if err := os.WriteFile(nmPath, []byte("OLD-NMHOST"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NEW-NMHOST"), nil, "", "", "")

	origPath := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = origPath })

	origKill := killProcessAtFunc
	killProcessAtFunc = func(string) error { return nil }
	t.Cleanup(func() { killProcessAtFunc = origKill })

	swapCalls := 0
	origSwap := swapCoAssetBinaryFunc
	swapCoAssetBinaryFunc = func(dst, tmp string) error {
		swapCalls++
		return fmt.Errorf("rename old nm-host: Access is denied.")
	}
	t.Cleanup(func() { swapCoAssetBinaryFunc = origSwap })

	m, _ := updateSource().FetchVersionManifest()
	err := nmHostCoAssetHandler(m, filepath.Join(dataDir, "staging"))
	if err == nil {
		t.Fatal("handler should fail after retries exhausted")
	}
	// 首趟 + nmHostSwapRetries(3) = 4 次尝试。
	if swapCalls != nmHostSwapRetries+1 {
		t.Fatalf("swap attempts=%d want %d", swapCalls, nmHostSwapRetries+1)
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("error=%q should contain Access is denied", err.Error())
	}
	// 旧 nm-host 不动。
	got, _ := os.ReadFile(nmPath)
	if string(got) != "OLD-NMHOST" {
		t.Fatalf("nm-host should be untouched after exhausted retry, got %q", string(got))
	}
}

// --- D2: extension zip extract handler ---

// TestExtractExtensionAsset_Success: extension.zip 下载 + sha + 解压到
// <dataDir>/extension/，manifest.json 就位，extensionInfo 能读新版本。
func TestExtractExtensionAsset_Success(t *testing.T) {
	zipBytes := makeExtensionZip(t, "1.2.3")
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NMHOST"), zipBytes, "", "", "")

	m, _ := updateSource().FetchVersionManifest()
	if err := extCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("extCoAssetHandler: %v", err)
	}
	extDir := filepath.Join(dataDir, "extension")
	// manifest.json 就位。
	if !manifestExists(extDir) {
		t.Fatalf("manifest.json missing in %s", extDir)
	}
	// extensionInfo 能读新版本。
	dir, ver := extensionOnDiskInfo()
	if dir == "" || ver != "1.2.3" {
		t.Fatalf("extensionOnDiskInfo: dir=%q ver=%q want 1.2.3", dir, ver)
	}
	// 嵌套文件解压成功（Zip-Slip guard 不误杀正常条目）。
	if _, err := os.Stat(filepath.Join(extDir, "icons", "icon.png")); err != nil {
		t.Fatalf("nested icon.png should be extracted: %v", err)
	}
	// extensionDir() 现在指向 ride-along 落点。
	if got := extensionDir(); !strings.EqualFold(got, extDir) {
		t.Fatalf("extensionDir()=%q want %q", got, extDir)
	}
	// staged zip 被清。
	if _, err := os.Stat(filepath.Join(dataDir, "staging", "extension.zip")); !os.IsNotExist(err) {
		t.Fatalf("staged extension.zip should be gone, got %v", err)
	}
}

// TestExtractExtensionAsset_SHA256Mismatch: bad sha → 不解压，原 extension 目录
// 不变（这里原本无 extension，故仍无）。
func TestExtractExtensionAsset_SHA256Mismatch(t *testing.T) {
	zipBytes := makeExtensionZip(t, "9.9.9")
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NMHOST"), zipBytes, "", "", "badsha")

	m, _ := updateSource().FetchVersionManifest()
	err := extCoAssetHandler(m, filepath.Join(dataDir, "staging"))
	if err == nil {
		t.Fatal("sha mismatch should return error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "sha256") && !strings.Contains(strings.ToLower(err.Error()), "mismatch") {
		t.Fatalf("error=%q should mention sha256/mismatch", err.Error())
	}
	// 解压未发生：无 extension 目录，无 .extension-new 残留。
	if _, err := os.Stat(filepath.Join(dataDir, "extension")); !os.IsNotExist(err) {
		t.Fatalf("extension dir should NOT exist on mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".extension-new")); !os.IsNotExist(err) {
		t.Fatalf(".extension-new should NOT linger on mismatch, got %v", err)
	}
}

// TestExtractExtensionAsset_ReplacesExisting: 已有旧 extension → 解压新版本后
// 旧目录被替换（.extension-old 清理），新版本可读。
func TestExtractExtensionAsset_ReplacesExisting(t *testing.T) {
	zipBytes := makeExtensionZip(t, "2.0.0")
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NMHOST"), zipBytes, "", "", "")

	extDir := filepath.Join(dataDir, "extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "manifest.json"), []byte(`{"version":"0.0.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	m, _ := updateSource().FetchVersionManifest()
	if err := extCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("extCoAssetHandler: %v", err)
	}
	_, ver := extensionOnDiskInfo()
	if ver != "2.0.0" {
		t.Fatalf("after replace, version=%q want 2.0.0", ver)
	}
	// 旧目录的 0.0.1 manifest 不应残留（被 rename 走后 RemoveAll）。
	b, err := os.ReadFile(filepath.Join(extDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read new manifest: %v", err)
	}
	if strings.Contains(string(b), "0.0.1") {
		t.Fatalf("old manifest content leaked into new dir: %q", string(b))
	}
	// .extension-old 不残留（成功后清掉）。
	if _, err := os.Stat(filepath.Join(dataDir, ".extension-old")); !os.IsNotExist(err) {
		t.Fatalf(".extension-old should be cleaned after success, got %v", err)
	}
}

// --- runSelfUpdateWith 集成：co-asset 失败中止 bridge swap ---

// TestRunSelfUpdateWith_NmHostFailureAbortsBridgeSwap: nm-host handler 失败 →
// runSelfUpdateWith 中止，bridge 二进制不动（D1 验收"handler 失败中止 swap"）。
func TestRunSelfUpdateWith_NmHostFailureAbortsBridgeSwap(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD-BRIDGE"), 0o755); err != nil {
		t.Fatal(err)
	}
	// nm-host asset 给错 sha → handler 必失败。
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("NEW-BRIDGE"), []byte("NEW-NMHOST"), nil, "", "deadbeefdeadbeef", "")

	nmDir := t.TempDir()
	nmPath := filepath.Join(nmDir, "nm-host.exe")
	orig := nmHostPathFunc
	nmHostPathFunc = func() (string, error) { return nmPath, nil }
	t.Cleanup(func() { nmHostPathFunc = orig })

	res := runSelfUpdateWith(self, dataDir, noopReExec, []coAssetHandler{nmHostCoAssetHandler})
	if res.OK {
		t.Fatalf("nm-host failure should abort: OK=true %+v", res)
	}
	if !strings.Contains(res.Error, "co-asset") {
		t.Fatalf("error=%q should mention co-asset", res.Error)
	}
	// bridge 不动。
	got, _ := os.ReadFile(self)
	if string(got) != "OLD-BRIDGE" {
		t.Fatalf("bridge should be untouched when nm-host handler aborts, got %q", string(got))
	}
}

// TestRunSelfUpdateWith_ExtFailureAbortsBridgeSwap: 扩展 handler 失败 → 中止
// bridge swap（D2 验收"handler 失败中止 swap"）。
func TestRunSelfUpdateWith_ExtFailureAbortsBridgeSwap(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD-BRIDGE"), 0o755); err != nil {
		t.Fatal(err)
	}
	zipBytes := makeExtensionZip(t, "1.2.3")
	// extension asset 给错 sha → handler 必失败。
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("NEW-BRIDGE"), []byte("NMHOST"), zipBytes, "", "", "badsha")

	res := runSelfUpdateWith(self, dataDir, noopReExec, []coAssetHandler{extCoAssetHandler})
	if res.OK {
		t.Fatalf("ext failure should abort: OK=true %+v", res)
	}
	if !strings.Contains(res.Error, "co-asset") {
		t.Fatalf("error=%q should mention co-asset", res.Error)
	}
	got, _ := os.ReadFile(self)
	if string(got) != "OLD-BRIDGE" {
		t.Fatalf("bridge should be untouched when ext handler aborts, got %q", string(got))
	}
}

// --- extensionInfoHandler GET (§4.5) ---

// TestExtensionInfoHandler_GET_ReflectsOnDiskVersion: 解压后 GET /api/extension-info
// 暴露 on_disk_version + extension_dir。
func TestExtensionInfoHandler_GET_ReflectsOnDiskVersion(t *testing.T) {
	zipBytes := makeExtensionZip(t, "3.1.4")
	dataDir := setupFullUpdateSource(t, "0.3.0", "0.2.0",
		[]byte("BRIDGE"), []byte("NMHOST"), zipBytes, "", "", "")

	m, _ := updateSource().FetchVersionManifest()
	if err := extCoAssetHandler(m, filepath.Join(dataDir, "staging")); err != nil {
		t.Fatalf("extCoAssetHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/extension-info", nil)
	rec := httptest.NewRecorder()
	extensionInfoHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"on_disk_version":"3.1.4"`) {
		t.Fatalf("body=%q should contain on_disk_version 3.1.4", body)
	}
	if !strings.Contains(body, `"extension_dir"`) {
		t.Fatalf("body=%q should contain extension_dir", body)
	}
	// loaded_version 此时为空（无 Chrome 上报）。
	if !strings.Contains(body, `"loaded_version":""`) {
		t.Fatalf("body=%q should have empty loaded_version", body)
	}
}
