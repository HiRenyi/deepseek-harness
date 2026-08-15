package main

// selfupdate.go — B1/B2: bridge 真正自更新（rename-swap + re-exec）。
//
// 复刻 desktop/update/update.go 的 Apply 套路，目标改为 bridge 二进制自身。
// bridge 与 desktop 是独立 Go module（bridge/go.mod + desktop/go.mod，repo 根无
// go.mod），抽共享包需 root module + replace，本批 pragmatic 各持一份 moveOrCopy/
// CleanupOldBinary/swap，注释标注同源，后续可抽 root shared module。
//
// 流程（RFC §3.2.1）：
//  1. os.Executable + EvalSymlinks 取当前 bridge exe 路径
//  2. updateSource.FetchVersionManifest → BridgeAssetURL 取本平台 bridge asset（url+sha256）
//  3. 版本比较：isNewerVersion 不新则返回 {ok:false}
//  4. 下载到 <dataDir>/staging/bridge-new（downloadFile: .part + sha256 + 原子 rename）
//  5. sha256 不符 → downloadFile 已丢弃暂存 + 返回 err；旧 bridge 继续运行（不 swap）
//  6. rename 自己 → bridge.old（释放 Windows 镜像锁）
//  7. moveOrCopy bridge-new → 原位（跨卷兜底）
//  8. re-exec 新 bridge（detached，--post-self-update）
//  9. 旧进程退出（re-exec 后）；下次启动 CleanupOldBinary 清 .old
//
// 数据目录绝不用 os.TempDir（CLAUDE.md 环境契约——%TEMP% 曾被系统清理中断更新）。

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// exitFunc 退出当前进程。包级变量以便测试注入可观测的 fake（避免真退出
// 测试进程）。生产路径 = os.Exit。
var exitFunc = os.Exit

// reExecExitDelay 是 re-exec 成功后旧进程延迟退出的等待时间。延迟是为了
// 让 /update/install 的 HTTP 响应先 flush 出去（handler 返回 result →
// 编码到 ResponseWriter → flush → 400ms 后旧进程 exit），再释放 58080
// 端口 + .old 镜像锁，让新 bridge 能 bind + 清 .old。包级变量以便测试缩短。
var reExecExitDelay = 400 * time.Millisecond

// scheduleReExecExit 在 re-exec 成功后调度旧进程延迟退出。默认起 goroutine
// sleep reExecExitDelay 后 os.Exit(0)。抽成包级变量以便测试注入同步/no-op
// 版本，避免生产 goroutine 跨测试残留（随机杀死测试进程或误触发后续测试
// 的 fake exitFunc）。
var scheduleReExecExit = func() {
	go func() {
		time.Sleep(reExecExitDelay)
		exitFunc(0)
	}()
}

// selfUpdateResult is the structured result of a bridge self-update attempt.
// Returned by runSelfUpdate + surfaced verbatim by POST /update/install (B4)
// and logged by the idle updater (B5).
type selfUpdateResult struct {
	OK         bool   `json:"ok"`                   // true = swap succeeded (re-exec may still fail)
	Action     string `json:"action,omitempty"`     // self-swapped | binary-updated-restart-manual
	OldVersion string `json:"old_version,omitempty"`
	NewVersion string `json:"new_version,omitempty"`
	Restarting bool   `json:"restarting,omitempty"` // true = new bridge re-execd
	Error      string `json:"error,omitempty"`      // present when OK=false, or re-exec warning
}

// coAssetHandler is the D-group hook: download + swap a co-asset (nm-host,
// extension) in the same self-update ride. D1 (nm-host) and D2 (extension) will
// append handlers to coAssetHandlers; B-group leaves the slice empty so the
// bridge-only swap runs unchanged. A handler error aborts the self-update
// BEFORE the bridge swap (so a failed co-asset never leaves bridge
// half-swapped). Not implemented in B-group — reserved hook per RFC §4.4/§4.5.
type coAssetHandler func(m *VersionManifest, stagingDir string) error

// coAssetHandlers is the registry of same-ride co-asset handlers. Empty in
// B-group; D1/D2 append at init time in a later batch.
var coAssetHandlers []coAssetHandler

// reExecFunc re-launches the freshly swapped bridge binary in
// --post-self-update mode (detached). The real impl spawns the new process;
// tests inject a no-op to avoid actually re-exec'ing the test binary.
type reExecFunc func(selfPath, newVersion string) error

// defaultReExec spawns the new bridge detached in --post-self-update mode. The
// new process cleans .old (CleanupOldBinary) + continues normal startup
// (writes bridge_version from its build-time var). The old process exits
// separately after runSelfUpdate returns. Detached so a bridge exit / ctrl-C
// does not cascade to the new process.
func defaultReExec(selfPath, newVersion string) error {
	cmd := exec.Command(selfPath, "--post-self-update")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		// Swap already succeeded; the new binary is on disk but we couldn't
		// launch it. Surface the error so the caller can restart manually.
		return fmt.Errorf("re-exec new bridge: %w (binary updated; restart manually)", err)
	}
	return nil
}

// guiUpgradeSpawn launches the freshly downloaded GUI binary in --upgrade mode
// (detached). The GUI embeds bridge + nm-host + extension; in --upgrade mode
// it re-extracts the three-piece (install.RunUpgrade) over the OLD bridge
// (which has exited by then, releasing the image lock) + spawns the new
// bridge. This is the fused-GUI update path (doc/update-channel-fused-gui-design.md §4.1).
// Package var so tests inject an observable fake (no real process spawn).
var guiUpgradeSpawn = func(guiExe string) error {
	cmd := exec.Command(guiExe, "--upgrade")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn gui --upgrade: %w", err)
	}
	return nil
}

// guiExeSuffix returns the platform executable suffix (".exe" on windows, "" else).
// Used to name the staged GUI binary so it's directly launchable after download.
func guiExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// runSelfUpdateViaGUI is the fused-GUI new path: download the GUI asset to
// staging, spawn "gui-new --upgrade" detached, then OLD bridge exits. The
// --upgrade process re-extracts the three-piece + spawns the new bridge. OLD
// bridge does NOT self-swap/re-exec — extracting the three-piece is the GUI's
// job (it embeds them; bridge doesn't). On download/spawn failure the running
// bridge is untouched (keeps serving). staging/gui-new is cleaned by the
// --upgrade process on success; on spawn failure we remove it here so the next
// attempt re-downloads.
func runSelfUpdateViaGUI(m *VersionManifest, guiURL, guiSHA, dataDir, oldVer string) selfUpdateResult {
	stagingDir := filepath.Join(dataDir, "staging")
	// Keep the asset's original filename ("Browser MCP Setup.exe") — do NOT
	// rename to gui-new.exe. The downloaded file IS the GUI installer; naming
	// it plainly avoids confusion + matches what the user sees on disk.
	guiNew := filepath.Join(stagingDir, "Browser MCP Setup"+guiExeSuffix())
	if err := downloadFile(guiURL, guiNew, guiSHA); err != nil {
		return selfUpdateResult{OK: false, Error: "download gui asset: " + err.Error()}
	}
	if err := guiUpgradeSpawn(guiNew); err != nil {
		_ = os.Remove(guiNew) // spawn failed: clean staging so next attempt re-downloads
		return selfUpdateResult{OK: false, Error: err.Error()}
	}
	// Record the new version best-effort so currentVersion() converges even
	// before the --upgrade process finishes writing installed_version. The
	// --upgrade process will also write it; same value, no harm.
	_ = writeBridgeVersion(dataDir, m.Version)
	// OLD bridge exits (delayed so any HTTP response flushes). The --upgrade
	// process takes over: extractThreePiece → spawn new bridge → clean staging.
	scheduleReExecExit()
	return selfUpdateResult{
		OK:         true,
		Action:     "gui-upgrade-spawned",
		OldVersion: oldVer,
		NewVersion: m.Version,
		Restarting: true,
	}
}

// runSelfUpdate is the public entry: resolve self path + data dir, fetch
// manifest, download bridge asset, swap, re-exec. Called by /update/install
// (B4) and the idle updater (B5). Returns a structured result.
func runSelfUpdate() selfUpdateResult {
	self, err := os.Executable()
	if err != nil {
		return selfUpdateResult{OK: false, Error: "resolve self exe: " + err.Error()}
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return selfUpdateResult{OK: false, Error: "eval self symlinks: " + err.Error()}
	}
	dataDir, err := bridgeDataDir()
	if err != nil {
		return selfUpdateResult{OK: false, Error: "resolve data dir: " + err.Error()}
	}
	return runSelfUpdateWith(self, dataDir, defaultReExec, coAssetHandlers)
}

// runSelfUpdateWith is the testable core: explicit selfPath + dataDir +
// injectable re-exec + co-asset handlers. The selfPath is the resolved current
// bridge exe; dataDir is ~/.browser-mcp (or BROWSER_MCP_DATA_DIR override).
//
// On any pre-swap error (manifest fetch, version compare, asset resolve,
// download, sha mismatch, co-asset) the running binary is UNTOUCHED — the
// caller can surface the error + bridge keeps serving. On swap success the new
// binary is on disk; a re-exec failure is surfaced as
// {ok:true, action:"binary-updated-restart-manual"} (not a hard error — the
// disk binary is new, just needs a manual restart).
func runSelfUpdateWith(selfPath, dataDir string, reExec reExecFunc, extra []coAssetHandler) selfUpdateResult {
	oldVer := currentVersion()
	src := updateSource()
	m, err := src.FetchVersionManifest()
	if err != nil {
		return selfUpdateResult{OK: false, Error: "fetch manifest: " + err.Error()}
	}
	if !isNewerVersion(m.Version, oldVer) {
		return selfUpdateResult{OK: false, Error: "已是最新版本 (" + oldVer + ")"}
	}
	// Dual-shape routing (doc/update-channel-fused-gui-design.md §5.2): if the
	// manifest has a GUI asset for this platform → new fused-GUI path (download
	// GUI + spawn `gui --upgrade` + OLD bridge exits). Else → legacy binaries.*
	// co-asset path (kept for rollback safety, doc §5.4). The channel is
	// currently GUI-only, so the gui path is the live one; legacy is fallback.
	if guiURL, guiSHA, guiErr := src.AssetURL(m); guiErr == nil {
		return runSelfUpdateViaGUI(m, guiURL, guiSHA, dataDir, oldVer)
	}
	url, sha, err := src.BridgeAssetURL(m)
	if err != nil {
		return selfUpdateResult{OK: false, Error: "resolve bridge asset: " + err.Error()}
	}

	// 4. Download to <dataDir>/staging/bridge-new. downloadFile writes a .part
	// temp, verifies sha256, atomically renames to dest on success. On sha
	// mismatch it removes the temp + returns an error — no partial file reaches
	// newBin, so the swap step never runs a corrupt binary.
	stagingDir := filepath.Join(dataDir, "staging")
	newBin := filepath.Join(stagingDir, "bridge-new")
	if err := downloadFile(url, newBin, sha); err != nil {
		return selfUpdateResult{OK: false, Error: "download bridge asset: " + err.Error()}
	}
	// If anything below fails after download, clean up the staged new binary
	// so the next attempt re-downloads fresh (best-effort; the swap consumes
	// newBin via rename on success so this is a no-op there).
	defer func() { _ = os.Remove(newBin) }()

	// D-group hooks (D1 nm-host, D2 extension) go here: download + swap
	// co-assets in the same ride, BEFORE the bridge swap. Empty in B-group →
	// no-op. A handler error aborts so a failed co-asset never leaves bridge
	// half-swapped. (RFC §4.4 nm-host 同趟 swap, §4.5 extension zip 解压.)
	for _, h := range extra {
		if err := h(m, stagingDir); err != nil {
			return selfUpdateResult{OK: false, Error: "co-asset: " + err.Error()}
		}
	}

	// 6-7. Swap: rename self→.old (release Windows image lock), moveOrCopy
	// new→self (cross-volume tolerant), rollback on failure.
	if err := swapBinary(selfPath, newBin); err != nil {
		return selfUpdateResult{OK: false, Error: "swap: " + err.Error()}
	}

	// Swap succeeded: the new binary is on disk. Record the new version so
	// currentVersion() converges (idle loop won't re-trigger /api/status shows
	// new version) even if re-exec fails + the old process keeps running
	// transiently. Best-effort — a write failure never aborts the swap.
	_ = writeBridgeVersion(dataDir, m.Version)

	// 8. Re-exec the new bridge in --post-self-update mode (detached). The new
	// process cleans .old + continues normal startup (writes bridge_version
	// from its build-time var, overwriting this). The old process returns +
	// exits separately after runSelfUpdate returns.
	if err := reExec(selfPath, m.Version); err != nil {
		// re-exec 失败：磁盘已是新二进制但没能拉起新进程。旧进程继续 serve，
		// 等用户手动重启——因此这里 NOT exit（让旧进程保持服务可用性）。
		return selfUpdateResult{
			OK:         true,
			Action:     "binary-updated-restart-manual",
			OldVersion: oldVer,
			NewVersion: m.Version,
			Restarting: false,
			Error:      err.Error(),
		}
	}
	// re-exec 成功：旧进程必须退出，否则它会继续占着 58080 端口 + .old 镜像
	// 锁，导致新 bridge bind 失败（log.Fatalf 退出）+ .old 删不掉。bridge 是
	// 长驻 HTTP server，handler 返回后旧进程不会自然退出——这里显式延迟 exit
	// 闭合 re-exec 重启循环。400ms 延迟让 HTTP 响应先 flush 出去；之后旧进程
	// exit 释放端口 + .old 锁，新 bridge 的 listenWithRetry 即可 bind 成功、
	// cleanupOldBinaryWithRetry 即可删 .old。
	scheduleReExecExit()
	return selfUpdateResult{
		OK:         true,
		Action:     "self-swapped",
		OldVersion: oldVer,
		NewVersion: m.Version,
		Restarting: true,
	}
}

// swapBinary renames self→"<self>.old", moves tmp into self's place
// (cross-volume tolerant via moveOrCopy), rolls back on failure. Cross-platform
// single impl: os.Rename (MoveFileEx MOVEFILE_REPLACE_EXISTING on Windows)
// releases the running exe's image lock on rename — the same trick
// desktop/update/swap_*.go uses (the three per-OS files are identical, so one
// cross-platform impl suffices here). On Windows the .old stays locked until
// the OLD process fully exits; best-effort os.Remove may fail — that's fine,
// CleanupOldBinary on the next launch retries.
func swapBinary(self, tmp string) error {
	old := self + ".old"
	_ = os.Remove(old) // best-effort; ignore "still locked" / ENOENT

	if err := os.Rename(self, old); err != nil {
		return err
	}
	if err := moveOrCopy(tmp, self); err != nil {
		// Rollback: put the old binary back so bridge can still run.
		_ = os.Rename(old, self)
		return err
	}
	// New binary in place. .old may still be locked (old process alive);
	// best-effort delete, no rollback path.
	_ = os.Remove(old)
	return nil
}

// moveOrCopy moves tmp to dst. os.Rename is atomic + fast on the same volume
// but FAILS across volumes (Windows ERROR_NOT_SAME_DEVICE, Linux EXDEV). The
// self-update staging dir (~/.browser-mcp/staging) may be on a different
// volume than the running bridge exe (data dir on C:, exe on D:), so the
// swap's tmp→self step must tolerate cross-volume: try Rename first; on
// failure, copy tmp to a temp file in dst's directory (same volume as dst)
// then same-volume atomic Rename onto dst. dst is assumed free (the running
// binary was renamed to .old first), so the final Rename is a clean create.
//
// 与 desktop/update/update.go:moveOrCopy 同源，独立 module 暂各持一份。
func moveOrCopy(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err == nil {
		return nil // same-volume — atomic, done
	}
	// Cross-volume (or other rename error) → copy into dst's dir, then rename.
	in, err := os.Open(tmp)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".bridge-new-*")
	if err != nil {
		return fmt.Errorf("create temp in dst dir: %w", err)
	}
	tmpName := out.Name()
	defer func() { _ = os.Remove(tmpName) }() // clean up on any failure path
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil { // exec bit (harmless on Windows)
		return fmt.Errorf("chmod: %w", err)
	}
	// Same-volume rename onto dst (dst is free — running binary moved to .old).
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temp→dst: %w", err)
	}
	return nil
}

// CleanupOldBinary removes "<self>.old" if present. Called from main.go startup
// (--post-self-update path) to clean the leftover from a previous self-update
// (the .old file is the previous binary, renamed out of the way by swapBinary;
// it couldn't be deleted while the old process was still exiting). Best-effort
// — a failure (e.g. still locked) is silent; next launch retries.
//
// 与 desktop/update/update.go:CleanupOldBinary 同源。
func CleanupOldBinary() {
	_ = cleanupOldBinaryErr()
}

// cleanupOldBinaryErr removes "<self>.old" if present, returning the remove
// error (nil if .old absent or successfully removed). 返回 error 让重试循环
// 能判断 .old 锁是否已释放：.old 仍在 + Remove 失败 = 旧进程还没 exit（镜像
// 锁未释放）；成功 = 旧进程已 exit，可停止重试。
func cleanupOldBinaryErr() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	old := self + ".old"
	if _, err := os.Stat(old); err != nil {
		return nil // .old 不存在——无需清理
	}
	return os.Remove(old)
}

// cleanupOldBinaryWithRetry 在 bind 成功后起 goroutine 重试清理 .old。.old 的
// 镜像锁释放依赖旧进程 exit（旧进程在 re-exec 后 ~400ms exit），启动初那次
// best-effort CleanupOldBinary 可能因锁未释放而失败，这里用 6s 窗口（30×200ms）
// 兜底：旧进程 exit 后锁释放，下一次重试即能删除。
func cleanupOldBinaryWithRetry() {
	cleanupOldBinaryRetry(30, 200*time.Millisecond)
}

// cleanupOldBinaryRetry 是可参数化的重试核心（便于单测）。最多 maxAttempts 次，
// 每次间隔 interval；成功（cleanupOldBinaryErr 返回 nil）即提前返回。
func cleanupOldBinaryRetry(maxAttempts int, interval time.Duration) {
	for i := 0; i < maxAttempts; i++ {
		if err := cleanupOldBinaryErr(); err == nil {
			return
		}
		time.Sleep(interval)
	}
}

// writeBridgeVersion writes v to <dataDir>/bridge_version (best-effort). The
// version truth-source file (B6): currentVersion() prefers it over
// installed_version + the build-time `version` var. Written on startup
// (= build-time version var, B7) + after a successful self-update (= manifest
// version, B1) so the idle loop + /api/status converge on the new version
// immediately, even before the re-execd bridge takes over.
func writeBridgeVersion(dataDir, v string) error {
	if v == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, "bridge_version"), []byte(v), 0o644)
}
