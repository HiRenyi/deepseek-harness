package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/browser-mcp/bridge/profile"
)

// noopReExec is a reExecFunc that pretends the new bridge launched successfully
// without actually spawning a process. Used by runSelfUpdateWith tests so they
// never re-exec the test binary.
func noopReExec(selfPath, newVersion string) error { return nil }

// failReExec always returns an error, exercising the "swap succeeded but
// re-exec failed → binary-updated-restart-manual" branch.
func failReExec(selfPath, newVersion string) error {
	return errReExecFailed
}

var errReExecFailed = &reExecError{"simulated re-exec failure"}

type reExecError struct{ msg string }

func (e *reExecError) Error() string { return e.msg }

// setupSelfUpdateSource stands up an httptest distribution source serving a v1
// version.json + a bridge asset at /bridge-new, and isolates the data dir +
// update-source env so the test never touches a real ~/.browser-mcp or gitee.
// Writes bridge_version=currentVer so currentVersion() returns a controlled
// value. Returns the dataDir + the served bridge bytes (so the caller can
// assert the swapped-in content).
func setupSelfUpdateSource(t *testing.T, manifestVersion, currentVer string, bridgeBytes []byte, bridgeSHA string) (dataDir, manifestSHA string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dataDir)
	t.Setenv("BROWSER_MCP_UPDATE_SOURCE", "http")
	// bridge_version drives currentVersion(); write it before anything reads it.
	if currentVer != "" {
		if err := os.WriteFile(filepath.Join(dataDir, "bridge_version"), []byte(currentVer), 0o644); err != nil {
			t.Fatalf("write bridge_version: %v", err)
		}
	}
	// manifest sha: if caller passes "" compute the real one; else use the
	// caller's (lets a mismatch test serve a wrong sha).
	realSHA := bridgeSHA
	if realSHA == "" {
		sum := sha256.Sum256(bridgeBytes)
		realSHA = hex.EncodeToString(sum[:])
	}
	key := assetKey()
	manifest := `{
	  "schema": "browser-mcp-v1",
	  "version": "` + manifestVersion + `",
	  "assets": {
	    "binaries": { "` + key + `": {
	      "bridge": {"url": "bridge-new", "sha256": "` + realSHA + `"}
	    }}
	  }
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version.json":
			io.WriteString(w, manifest)
		case "/bridge-new":
			w.Write(bridgeBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	// Point the http update source at the local test server. As of
	// hardcode-update-base-url, env BROWSER_MCP_UPDATE_BASE is gone — set
	// profile.UpdateBase directly (same mechanism as the ldflag bake, test-only).
	oldBase := profile.UpdateBase
	profile.UpdateBase = srv.URL
	t.Cleanup(func() { profile.UpdateBase = oldBase })
	// Rebuild the cached UpdateSource singleton so it picks up the new base.
	resetUpdateSource()
	invalidateManifestCache()
	return dataDir, realSHA
}

// --- swapBinary unit tests (B2) ---

// TestSwapBinary_Success: a normal same-volume swap renames the new binary
// into self's place + the .old leftover is best-effort removed (no image lock
// in a test, so the remove succeeds). self ends up with the NEW content.
func TestSwapBinary_Success(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "bridge.exe")
	newBin := filepath.Join(dir, "bridge-new")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newBin, []byte("NEW-CONTENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapBinary(self, newBin); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self after swap: %v", err)
	}
	if string(got) != "NEW-CONTENT" {
		t.Fatalf("self content=%q want %q", string(got), "NEW-CONTENT")
	}
	// newBin was consumed by the rename.
	if _, err := os.Stat(newBin); !os.IsNotExist(err) {
		t.Fatalf("newBin should be gone after moveOrCopy, got %v", err)
	}
	// .old best-effort removed (no lock in test).
	if _, err := os.Stat(self + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old should be gone after successful swap, got %v", err)
	}
}

// TestSwapBinary_RollbackOnMoveFailure: when moveOrCopy fails (tmp missing),
// swapBinary rolls back — self is restored to its original content + still
// exists. This is the safety contract: a failed swap never leaves bridge
// without a working binary.
func TestSwapBinary_RollbackOnMoveFailure(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	// nonexistent tmp → moveOrCopy's os.Open fails → rollback.
	missingTmp := filepath.Join(dir, "does-not-exist")
	err := swapBinary(self, missingTmp)
	if err == nil {
		t.Fatal("swapBinary with missing tmp should error")
	}
	// self restored.
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self after rollback: %v", err)
	}
	if string(got) != "OLD" {
		t.Fatalf("self content=%q want %q (rollback)", string(got), "OLD")
	}
	// .old gone (rolled back to self).
	if _, err := os.Stat(self + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old should be gone after rollback, got %v", err)
	}
}

// --- moveOrCopy cross-volume fallback (B2) ---

// TestMoveOrCopy_SameVolume: os.Rename succeeds on the same volume → atomic.
func TestMoveOrCopy_SameVolume(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveOrCopy(src, dst); err != nil {
		t.Fatalf("moveOrCopy: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "X" {
		t.Fatalf("dst=%q want X", string(got))
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src should be gone after same-volume rename")
	}
}

// --- runSelfUpdateWith flow tests (B1) ---

// TestRunSelfUpdateWith_NotNewer: manifest version == current → no update,
// returns {ok:false} with the "already latest" message. Nothing is downloaded
// or swapped.
func TestRunSelfUpdateWith_NotNewer(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.3.0", []byte("NEW"), "")

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if res.OK {
		t.Fatalf("not-newer should return OK=false, got %+v", res)
	}
	if !strings.Contains(res.Error, "最新") {
		t.Fatalf("error=%q should mention 最新", res.Error)
	}
	// self untouched.
	got, _ := os.ReadFile(self)
	if string(got) != "OLD" {
		t.Fatalf("self should be untouched when not newer, got %q", string(got))
	}
}

// TestRunSelfUpdateWith_SHA256Mismatch: bad sha → downloadFile returns a sha
// mismatch error before any swap; the running binary is untouched + no staging
// file left at bridge-new.
func TestRunSelfUpdateWith_SHA256Mismatch(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Serve "WRONG" bytes but advertise a sha that doesn't match them.
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.2.0", []byte("WRONG-CONTENT"), "deadbeef")

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if res.OK {
		t.Fatalf("sha mismatch should return OK=false, got %+v", res)
	}
	if !strings.Contains(res.Error, "sha256") && !strings.Contains(strings.ToLower(res.Error), "mismatch") {
		t.Fatalf("error=%q should mention sha256/mismatch", res.Error)
	}
	// self untouched.
	got, _ := os.ReadFile(self)
	if string(got) != "OLD" {
		t.Fatalf("self should be untouched on sha mismatch, got %q", string(got))
	}
	// no staging leftover at bridge-new (downloadFile removes .part; the
	// renamed dest is cleaned by runSelfUpdateWith's defer).
	if _, err := os.Stat(filepath.Join(dataDir, "staging", "bridge-new")); !os.IsNotExist(err) {
		t.Fatalf("bridge-new staging file should be gone after mismatch, got %v", err)
	}
}

// TestRunSelfUpdateWith_Success: newer version + correct sha → swap succeeds,
// self now holds the NEW binary content, bridge_version is written to the
// manifest version, re-exec (noop) succeeds → {ok:true, action:self-swapped,
// restarting:true}.
func TestRunSelfUpdateWith_Success(t *testing.T) {
	// re-exec 成功分支会 scheduleReExecExit()（默认起 goroutine 延迟 os.Exit）。
	// 注入 no-op 防止生产 goroutine 跨测试残留（随机杀测试进程 / 误触发后续
	// 测试的 fake exitFunc）。本测试不验证延迟 exit 行为（见 ReExecSuccessSchedulesDelayedExit）。
	origSchedule := scheduleReExecExit
	scheduleReExecExit = func() {}
	t.Cleanup(func() { scheduleReExecExit = origSchedule })

	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := []byte("NEW-BRIDGE-BINARY")
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.2.0", newBytes, "")

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if !res.OK {
		t.Fatalf("success: got OK=false %+v", res)
	}
	if res.Action != "self-swapped" {
		t.Fatalf("action=%q want self-swapped", res.Action)
	}
	if res.OldVersion != "0.2.0" || res.NewVersion != "0.3.0" {
		t.Fatalf("versions old=%q new=%q", res.OldVersion, res.NewVersion)
	}
	if !res.Restarting {
		t.Fatalf("restarting should be true on re-exec success")
	}
	// self now holds the new binary.
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self after swap: %v", err)
	}
	if string(got) != string(newBytes) {
		t.Fatalf("self content mismatch after swap: got %q want %q", string(got), string(newBytes))
	}
	// bridge_version written to manifest version (convergence).
	bv, err := os.ReadFile(filepath.Join(dataDir, "bridge_version"))
	if err != nil {
		t.Fatalf("read bridge_version: %v", err)
	}
	if strings.TrimSpace(string(bv)) != "0.3.0" {
		t.Fatalf("bridge_version=%q want 0.3.0", string(bv))
	}
}

// TestRunSelfUpdateWith_ReExecFails: swap succeeded but re-exec returned an
// error → {ok:true, action:binary-updated-restart-manual, restarting:false}
// + the error is surfaced. The disk binary IS the new one (swap happened).
func TestRunSelfUpdateWith_ReExecFails(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := []byte("NEW-BRIDGE-BINARY")
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.2.0", newBytes, "")

	res := runSelfUpdateWith(self, dataDir, failReExec, nil)
	if !res.OK {
		t.Fatalf("re-exec-fail should still be OK=true (swap succeeded), got %+v", res)
	}
	if res.Action != "binary-updated-restart-manual" {
		t.Fatalf("action=%q want binary-updated-restart-manual", res.Action)
	}
	if res.Restarting {
		t.Fatalf("restarting should be false when re-exec failed")
	}
	if !strings.Contains(res.Error, "re-exec") {
		t.Fatalf("error=%q should mention re-exec", res.Error)
	}
	// disk binary is the new one.
	got, _ := os.ReadFile(self)
	if string(got) != string(newBytes) {
		t.Fatalf("self should hold NEW after swap despite re-exec fail, got %q", string(got))
	}
}

// TestRunSelfUpdateWith_CoAssetHookError: a registered co-asset handler that
// returns an error aborts BEFORE the bridge swap — self is untouched. This
// pins the D-group hook contract (a failed co-asset never half-swaps bridge).
func TestRunSelfUpdateWith_CoAssetHookError(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := []byte("NEW-BRIDGE-BINARY")
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.2.0", newBytes, "")

	failHook := func(m *VersionManifest, stagingDir string) error {
		return errCoAssetBoom
	}
	res := runSelfUpdateWith(self, dataDir, noopReExec, []coAssetHandler{failHook})
	if res.OK {
		t.Fatalf("co-asset error should return OK=false, got %+v", res)
	}
	if !strings.Contains(res.Error, "co-asset") {
		t.Fatalf("error=%q should mention co-asset", res.Error)
	}
	// self untouched (swap never ran).
	got, _ := os.ReadFile(self)
	if string(got) != "OLD" {
		t.Fatalf("self should be untouched when co-asset aborts, got %q", string(got))
	}
}

var errCoAssetBoom = &coAssetError{"simulated D-group co-asset failure"}

type coAssetError struct{ msg string }

func (e *coAssetError) Error() string { return e.msg }

// --- currentVersion precedence (B6) ---

// TestCurrentVersion_Precedence pins the B6 truth-source order:
// bridge_version > installed_version > build-time `version` var.
func TestCurrentVersion_Precedence(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dataDir)

	// 1. bridge_version wins over installed_version + version var.
	mustWrite(t, filepath.Join(dataDir, "bridge_version"), "1.2.3")
	mustWrite(t, filepath.Join(dataDir, "installed_version"), "9.9.9")
	if got := currentVersion(); got != "1.2.3" {
		t.Fatalf("bridge_version precedence: got %q want 1.2.3", got)
	}

	// 2. no bridge_version → installed_version (GUI-package-update compat).
	os.Remove(filepath.Join(dataDir, "bridge_version"))
	if got := currentVersion(); got != "9.9.9" {
		t.Fatalf("installed_version fallback: got %q want 9.9.9", got)
	}

	// 3. neither → build-time version var (dev in test builds).
	os.Remove(filepath.Join(dataDir, "installed_version"))
	if got := currentVersion(); got != version {
		t.Fatalf("version var fallback: got %q want %q", got, version)
	}
}

// --- re-exec 延迟退出闭环（Fix 1）---

// TestRunSelfUpdateWith_ReExecSuccessSchedulesDelayedExit: re-exec 成功分支
// 必须调度延迟 os.Exit（否则旧长驻进程不退出，新 bridge bind 失败 + .old 锁
// 不释放）。注入可观测 exitFunc fake + 缩短 reExecExitDelay，断言 scheduleReExecExit
// 被调用后 ~delay 内 exitFunc 被触发、code=0。测试进程不会被真退出（fake 只记录）。
func TestRunSelfUpdateWith_ReExecSuccessSchedulesDelayedExit(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.2.0", []byte("NEW-BRIDGE-BINARY"), "")

	// 缩短延迟让测试快（默认 400ms）；恢复默认 scheduleReExecExit（起 goroutine）。
	origDelay := reExecExitDelay
	reExecExitDelay = 30 * time.Millisecond
	var exitCalled atomic.Bool
	var exitCode int32
	origExit := exitFunc
	exitFunc = func(code int) {
		atomic.StoreInt32(&exitCode, int32(code))
		exitCalled.Store(true)
	}
	t.Cleanup(func() {
		reExecExitDelay = origDelay
		exitFunc = origExit
	})

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if !res.OK || !res.Restarting {
		t.Fatalf("re-exec success should return ok+restarting, got %+v", res)
	}

	// 轮询等待延迟 exit 被调度（delay=30ms，给 1s 余量）。
	deadline := time.Now().Add(1 * time.Second)
	for !exitCalled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !exitCalled.Load() {
		t.Fatal("re-exec 成功后应调度延迟 os.Exit，但 exitFunc 未被调用")
	}
	if got := atomic.LoadInt32(&exitCode); got != 0 {
		t.Fatalf("exit code=%d want 0", got)
	}
}

// TestRunSelfUpdateWith_ReExecFailNoExit: re-exec 失败分支必须 NOT 调度 exit——
// 旧进程继续 serve，等用户手动重启。注入可观测 scheduleReExecExit fake，断言
// 它未被调用。
func TestRunSelfUpdateWith_ReExecFailNoExit(t *testing.T) {
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir, _ := setupSelfUpdateSource(t, "0.3.0", "0.2.0", []byte("NEW-BRIDGE-BINARY"), "")

	var scheduleCalled atomic.Bool
	origSchedule := scheduleReExecExit
	scheduleReExecExit = func() { scheduleCalled.Store(true) }
	t.Cleanup(func() { scheduleReExecExit = origSchedule })

	res := runSelfUpdateWith(self, dataDir, failReExec, nil)
	if res.Restarting {
		t.Fatalf("re-exec fail should return restarting=false, got %+v", res)
	}
	if scheduleCalled.Load() {
		t.Fatal("re-exec 失败时不应调度 exit（旧进程须继续 serve）")
	}
}

// --- cleanupOldBinary 重试（Fix 3）---

// TestCleanupOldBinaryRetry_DeletesWhenUnlocked: .old 文件可删时（无镜像锁），
// 重试循环第一次即删除成功。验证 --post-self-update bind 成功后 goroutine
// 兜底清理能真正清掉 .old。
func TestCleanupOldBinaryRetry_DeletesWhenUnlocked(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	old := self + ".old"
	if err := os.WriteFile(old, []byte("OLD-BIN"), 0o644); err != nil {
		t.Fatalf("write .old: %v", err)
	}
	defer os.Remove(old) // 兜底（万一重试未删）

	cleanupOldBinaryRetry(5, 10*time.Millisecond)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf(".old 应被重试循环删除，stat got err=%v", err)
	}
}

// TestCleanupOldBinaryErr_NoOldFile: 无 .old 时 cleanupOldBinaryErr 返回 nil
// （启动初 best-effort 路径的正常情况）。
func TestCleanupOldBinaryErr_NoOldFile(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	old := self + ".old"
	os.Remove(old) // 确保不存在
	if err := cleanupOldBinaryErr(); err != nil {
		t.Fatalf("无 .old 时应返回 nil，got %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWriteBridgeVersion_RoundTrip: write + read back; empty v is a no-op
// (the file is not created/emptied).
func TestWriteBridgeVersion_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := writeBridgeVersion(dir, "0.4.1"); err != nil {
		t.Fatalf("writeBridgeVersion: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "bridge_version"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "0.4.1" {
		t.Fatalf("bridge_version=%q want 0.4.1", string(got))
	}
	// empty v → no write (don't clobber an existing file with nothing).
	if err := writeBridgeVersion(dir, ""); err != nil {
		t.Fatalf("empty v writeBridgeVersion: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "bridge_version"))
	if strings.TrimSpace(string(got)) != "0.4.1" {
		t.Fatalf("empty v should not clobber, got %q", string(got))
	}
}

// --- dual-shape routing (unified-gui-update-channel) ---

// setupSelfUpdateSourceGUI serves a GUI-only manifest (assets.gui.<chanKey>,
// no binaries.*) + a GUI binary at /gui-new. Mirrors setupSelfUpdateSource
// but for the fused-GUI new path. Returns dataDir + the served GUI bytes.
func setupSelfUpdateSourceGUI(t *testing.T, manifestVersion, currentVer string, guiBytes []byte) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("BROWSER_MCP_DATA_DIR", dataDir)
	t.Setenv("BROWSER_MCP_UPDATE_SOURCE", "http")
	if currentVer != "" {
		if err := os.WriteFile(filepath.Join(dataDir, "bridge_version"), []byte(currentVer), 0o644); err != nil {
			t.Fatalf("write bridge_version: %v", err)
		}
	}
	sum := sha256.Sum256(guiBytes)
	guiSHA := hex.EncodeToString(sum[:])
	chanKey := guiPlatformKey(runtime.GOOS, runtime.GOARCH)
	manifest := `{
	  "schema": "browser-mcp-v1",
	  "version": "` + manifestVersion + `",
	  "assets": {
	    "gui": { "` + chanKey + `": {"url": "gui-new", "sha256": "` + guiSHA + `"} }
	  }
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version.json":
			io.WriteString(w, manifest)
		case "/gui-new":
			w.Write(guiBytes)
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

// TestRunSelfUpdateWith_GUIPath verifies the dual-shape routing: a GUI-only
// manifest (assets.gui present, no binaries.*) routes to runSelfUpdateViaGUI —
// the GUI is downloaded to staging, guiUpgradeSpawn is invoked with the staged
// path, and the result reports gui-upgrade-spawned + restarting. The OLD
// bridge binary is NOT swapped (the GUI's --upgrade will re-extract it).
func TestRunSelfUpdateWith_GUIPath(t *testing.T) {
	// Prevent the production delayed-exit goroutine from killing the test process.
	origSchedule := scheduleReExecExit
	scheduleReExecExit = func() {}
	t.Cleanup(func() { scheduleReExecExit = origSchedule })

	// Capture the guiExe path passed to guiUpgradeSpawn; do not really spawn.
	var spawnedPath string
	origSpawn := guiUpgradeSpawn
	guiUpgradeSpawn = func(guiExe string) error {
		spawnedPath = guiExe
		return nil
	}
	t.Cleanup(func() { guiUpgradeSpawn = origSpawn })

	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	if err := os.WriteFile(self, []byte("OLD-BRIDGE"), 0o755); err != nil {
		t.Fatal(err)
	}
	guiBytes := []byte("FAKE-FUSED-GUI-BINARY")
	dataDir := setupSelfUpdateSourceGUI(t, "0.3.1", "0.3.0", guiBytes)

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if !res.OK {
		t.Fatalf("gui path: OK=false %+v", res)
	}
	if res.Action != "gui-upgrade-spawned" {
		t.Fatalf("action=%q want gui-upgrade-spawned", res.Action)
	}
	if res.OldVersion != "0.3.0" || res.NewVersion != "0.3.1" {
		t.Fatalf("versions old=%q new=%q", res.OldVersion, res.NewVersion)
	}
	if !res.Restarting {
		t.Fatal("restarting should be true (gui --upgrade will spawn new bridge)")
	}
	// guiUpgradeSpawn was called with the staged gui-new path.
	if spawnedPath == "" {
		t.Fatal("guiUpgradeSpawn not called")
	}
	if filepath.Base(spawnedPath) != "Browser MCP Setup"+guiExeSuffix() {
		t.Fatalf("spawnedPath base=%q want 'Browser MCP Setup%s' (keep asset name, no gui-new rename)", filepath.Base(spawnedPath), guiExeSuffix())
	}
	// The staged GUI binary content matches what was served.
	got, err := os.ReadFile(spawnedPath)
	if err != nil {
		t.Fatalf("read staged gui: %v", err)
	}
	if string(got) != string(guiBytes) {
		t.Fatalf("staged gui content mismatch: got %q want %q", string(got), string(guiBytes))
	}
	// OLD bridge binary is untouched (the --upgrade process will overwrite it;
	// runSelfUpdateViaGUI does not self-swap).
	oldSelf, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	if string(oldSelf) != "OLD-BRIDGE" {
		t.Fatalf("OLD bridge binary should be untouched on gui path, got %q", string(oldSelf))
	}
	// bridge_version written to new version (convergence best-effort).
	bv, _ := os.ReadFile(filepath.Join(dataDir, "bridge_version"))
	if strings.TrimSpace(string(bv)) != "0.3.1" {
		t.Fatalf("bridge_version=%q want 0.3.1", string(bv))
	}
}

// TestRunSelfUpdateWith_GUIPath_SpawnFails: guiUpgradeSpawn returns an error →
// result OK=false, OLD bridge keeps serving, staging gui-new is cleaned up.
func TestRunSelfUpdateWith_GUIPath_SpawnFails(t *testing.T) {
	origSchedule := scheduleReExecExit
	scheduleReExecExit = func() {}
	t.Cleanup(func() { scheduleReExecExit = origSchedule })

	origSpawn := guiUpgradeSpawn
	guiUpgradeSpawn = func(guiExe string) error {
		return fmt.Errorf("simulated spawn failure")
	}
	t.Cleanup(func() { guiUpgradeSpawn = origSpawn })

	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	_ = os.WriteFile(self, []byte("OLD"), 0o755)
	dataDir := setupSelfUpdateSourceGUI(t, "0.3.1", "0.3.0", []byte("GUI"))

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if res.OK {
		t.Fatalf("spawn fail: expected OK=false, got %+v", res)
	}
	if res.Action != "" {
		t.Fatalf("action should be empty on failure, got %q", res.Action)
	}
	// staging GUI cleaned up after spawn failure.
	staging := filepath.Join(dataDir, "staging", "Browser MCP Setup"+guiExeSuffix())
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging GUI should be removed on spawn failure, err=%v", err)
	}
}

// TestRunSelfUpdateWith_LegacyPathFallback confirms a binaries-only manifest
// (no gui asset) still routes to the legacy co-asset path (self-swapped), not
// the gui path. Regression for dual-shape: gui path must not silently swallow
// a legacy manifest.
func TestRunSelfUpdateWith_LegacyPathFallback(t *testing.T) {
	origSchedule := scheduleReExecExit
	scheduleReExecExit = func() {}
	t.Cleanup(func() { scheduleReExecExit = origSchedule })

	// gui path must NOT be taken.
	origSpawn := guiUpgradeSpawn
	guiUpgradeSpawn = func(guiExe string) error {
		t.Fatalf("gui path should not run for binaries-only manifest (spawned %s)", guiExe)
		return nil
	}
	t.Cleanup(func() { guiUpgradeSpawn = origSpawn })

	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge.exe")
	_ = os.WriteFile(self, []byte("OLD"), 0o755)
	newBytes := []byte("NEW-BRIDGE")
	// setupSelfUpdateSource serves binaries-only (no gui) → legacy path.
	dataDir, _ := setupSelfUpdateSource(t, "0.3.1", "0.3.0", newBytes, "")

	res := runSelfUpdateWith(self, dataDir, noopReExec, nil)
	if !res.OK || res.Action != "self-swapped" {
		t.Fatalf("expected legacy self-swapped, got %+v", res)
	}
}
