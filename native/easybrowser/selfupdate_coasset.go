package main

// selfupdate_coasset.go — D1/D2: nm-host + 扩展随 bridge 自更新同趟处理。
//
// 复用 B-group 的 coAssetHandler hook（selfupdate.go:coAssetHandlers）：在 bridge
// swap **之前**下载并替换 nm-host 二进制 + 解压扩展 zip。任一 handler 失败，
// runSelfUpdateWith 即中止 bridge swap（nm-host/扩展换不了就别换 bridge，保一致）。
//
// D1（§4.4 nm-host）：nm-host 与 bridge 同目录同名，swap 后 NM manifest 的 `path`
// 不变，无需重注册。nm-host 是 Chrome 按需拉起的短命进程，bridge 自更新时大概率
// 未运行，故镜像锁不是主要矛盾；但仍走 rename-.old 套路以兼容"恰好运行中"的边角。
//
// D2（§4.5 扩展）：解压到 <dataDir>/extension/（用户数据目录，绝不用 os.TempDir）。
// bridge 无写权限到用户 Chrome，无法 reload 扩展——本 handler 只解压+更新 on-disk
// manifest 版本；宿主（GUI/KairosAIStudio）引导用户 reload 是后续 change（本批不做 UI）。
//
// 数据目录绝不用 os.TempDir（CLAUDE.md 环境契约）。

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// init registers the D1/D2 co-asset handlers so the public runSelfUpdate() entry
// swaps nm-host + extension in the same ride as bridge. runSelfUpdateWith (the
// testable core) takes an explicit `extra` slice, so B-group tests that pass nil
// are unaffected — the registry only flows through runSelfUpdate().
func init() {
	coAssetHandlers = append(coAssetHandlers, nmHostCoAssetHandler, extCoAssetHandler)
}

// --- D1: nm-host 同趟 swap ---

// nmHostPathFunc resolves the on-disk nm-host binary path (与 bridge 同目录同名).
// Default derives from os.Executable; tests override to target a temp dir.
var nmHostPathFunc = defaultNmHostPath

// defaultNmHostPath returns <exe-dir>/nm-host(.exe). nm-host 与 bridge 同目录，
// NM manifest 的 `path` 指向此路径——同目录同名 swap 后 path 不变，无需重注册。
func defaultNmHostPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve self exe for nm-host path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("eval symlinks for nm-host path: %w", err)
	}
	name := "nm-host"
	if runtime.GOOS == "windows" {
		name = "nm-host.exe"
	}
	return filepath.Join(filepath.Dir(self), name), nil
}

// nmHostCoAssetHandler is the D1 hook: resolve nm-host path → download asset →
// sha256 校验 → swap. 失败返回 err，runSelfUpdateWith 中止 bridge swap。
func nmHostCoAssetHandler(m *VersionManifest, stagingDir string) error {
	nmPath, err := nmHostPathFunc()
	if err != nil {
		return fmt.Errorf("resolve nm-host path: %w", err)
	}
	return swapNmHostAsset(m, stagingDir, nmPath)
}

// swapNmHostAsset is the testable core: download nm-host to staging, sha256
// 校验，swap 到 nmPath。sha 不符 → downloadFile 丢弃暂存 + 返回 err（旧 nm-host
// 不动）。nmPath 不存在（全新安装尚未放 nm-host）时直接放入新二进制。
//
// 真机 DV 修复（D1）：Chrome 经 Native Messaging 拉起 nm-host 时持有
// nm-host.exe 句柄，sharing mode 不允许 rename，swapCoAssetBinary 的
// os.Rename(nm-host.exe, .old) 报 "Access is denied"。故 swap 前先
// killProcessAt(nmPath) 终止占用进程（best-effort，仅按全路径匹配 nm-host，
// 绝不杀 bridge），再 rename；rename 仍失败则 retry（每趟重新 kill 以防
// Chrome 已重拉 nm-host）。nm-host 是短命进程，kill 后 Chrome 下次 NM 消息会
// 按 NM manifest 的 path（D1 不变）重拉新版。
func swapNmHostAsset(m *VersionManifest, stagingDir, nmPath string) error {
	url, sha, err := updateSource().NmHostAssetURL(m)
	if err != nil {
		return fmt.Errorf("resolve nm-host asset: %w", err)
	}
	staged := filepath.Join(stagingDir, "nm-host-new")
	if err := downloadFile(url, staged, sha); err != nil {
		return fmt.Errorf("download nm-host: %w", err)
	}
	defer func() { _ = os.Remove(staged) }() // swap 成功时由 rename 消费；失败时清暂存
	if err := swapNmHostBinaryWithRetry(nmPath, staged); err != nil {
		return fmt.Errorf("swap nm-host: %w", err)
	}
	return nil
}

// killProcessAtFunc 终止所有"完整镜像路径 == exePath"的运行中进程（best-effort）。
// 默认 = killProcessAt（build-tagged 跨平台实现）；测试覆盖以断言调用顺序而不
// 触发脆弱的真杀进程。仅 nm-host swap 用，绝不用于 bridge（bridge 的 kill/exit
// 是 runSelfUpdate 主流程的事）。
var killProcessAtFunc = killProcessAt

// swapCoAssetBinaryFunc 是 swapCoAssetBinary 的可测试间接层，让测试能模拟
// "rename 第一次 Access denied、第二次成功"的重试路径。
var swapCoAssetBinaryFunc = swapCoAssetBinary

// nmHost swap 的重试参数：rename 失败（Chrome 持句柄）时最多再试 3 趟，每趟
// 间隔 200ms 让 OS 释放镜像锁 + 给 Chrome 重拉 nm-host 的窗口。
const (
	nmHostSwapRetries = 3
	nmHostSwapBackoff = 200 * time.Millisecond
)

// swapNmHostBinaryWithRetry 把 nm-host swap 到位，先 kill 占用进程、失败则 retry。
// 每趟（含首趟）rename 前都调 killProcessAtFunc(nmPath)：①首趟 kill 释放 Chrome
// 持有的句柄；②retry 趟 kill 防止 Chrome 在间隔期已按 NM 流量重拉旧版 nm-host
// 重新占位。kill 是 best-effort（找不到进程=没在跑=无需 kill，不报错）。
// swapCoAssetBinary 自身幂等可重试：rename dst→old 失败时 dst/.old 状态保持
// 干净，下次重试从头来过即可。
func swapNmHostBinaryWithRetry(nmPath, staged string) error {
	var lastErr error
	for attempt := 0; attempt <= nmHostSwapRetries; attempt++ {
		if attempt > 0 {
			// 间隔期 Chrome 可能已重拉 nm-host → 重新 kill 再 rename。
			_ = killProcessAtFunc(nmPath)
			time.Sleep(nmHostSwapBackoff)
		} else {
			// 首趟：rename 前先 kill 释放 Chrome 持有的句柄。
			_ = killProcessAtFunc(nmPath)
		}
		err := swapCoAssetBinaryFunc(nmPath, staged)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// swapCoAssetBinary swaps a co-asset binary (nm-host) into place. 与 swapBinary
// 的差异：① 容忍 dst 缺失（全新安装 nm-host 尚未就位）；② 成功后**保留** .old
// （nm-host 无 CleanupOldBinary 等价物——不像 bridge 下次启动会清 .old；保留 .old
// 既作 rollback artifact，下次 swap 开头的 os.Remove(old) 会清掉，至多一份）。
// 这满足 D1 验收".old 生成"且不堆积。
func swapCoAssetBinary(dst, tmp string) error {
	old := dst + ".old"
	_ = os.Remove(old) // 清上一次失败/成功遗留的 .old（至多一份）
	if _, err := os.Stat(dst); err == nil {
		// dst 存在 → rename 出来腾位（兼释放 Windows 镜像锁，若 nm-host 恰运行中）。
		if err := os.Rename(dst, old); err != nil {
			return fmt.Errorf("rename old nm-host: %w", err)
		}
		if err := moveOrCopy(tmp, dst); err != nil {
			_ = os.Rename(old, dst) // rollback：把旧 nm-host 放回
			return err
		}
		return nil // .old 保留
	}
	// dst 缺失 → 直接放入新二进制（无 .old 可生成）。
	return moveOrCopy(tmp, dst)
}

// --- D2: 扩展 zip 解压 ---

// extCoAssetHandler is the D2 hook: download extension.zip → sha256 → 解压到
// <dataDir>/extension/。失败返回 err 中止 bridge swap。bridge 无法 reload 用户
// Chrome 扩展——只解压+更新 on-disk manifest 版本；宿主引导 reload 后续做。
func extCoAssetHandler(m *VersionManifest, stagingDir string) error {
	// stagingDir = <dataDir>/staging，故 dataDir = 其父。
	dataDir := filepath.Dir(stagingDir)
	return extractExtensionAsset(m, stagingDir, dataDir)
}

// extractExtensionAsset is the testable core: download extension.zip → sha256 →
// 解压到 <dataDir>/extension/（原子 swap：先解压到 .extension-new 再 rename）。
// 成功后读新 manifest 版本供 /api/extension-info 暴露（不调 setExtVersion：loaded
// 扩展仍是旧版，conflating on-disk 与 loaded 版本会误导用户）。
func extractExtensionAsset(m *VersionManifest, stagingDir, dataDir string) error {
	url, sha, err := updateSource().ExtensionAssetURL(m)
	if err != nil {
		return fmt.Errorf("resolve extension asset: %w", err)
	}
	staged := filepath.Join(stagingDir, "extension.zip")
	if err := downloadFile(url, staged, sha); err != nil {
		return fmt.Errorf("download extension.zip: %w", err)
	}
	defer func() { _ = os.Remove(staged) }()

	extDir := filepath.Join(dataDir, "extension")
	tmpExt := filepath.Join(dataDir, ".extension-new")
	_ = os.RemoveAll(tmpExt)
	if err := unzipTo(staged, tmpExt); err != nil {
		_ = os.RemoveAll(tmpExt)
		return fmt.Errorf("unzip extension: %w", err)
	}
	if !manifestExists(tmpExt) {
		_ = os.RemoveAll(tmpExt)
		return fmt.Errorf("extracted extension missing manifest.json")
	}
	// 原子 swap：旧 extension → .extension-old，新 → extension。
	oldExt := filepath.Join(dataDir, ".extension-old")
	_ = os.RemoveAll(oldExt)
	if _, err := os.Stat(extDir); err == nil {
		if err := os.Rename(extDir, oldExt); err != nil {
			_ = os.RemoveAll(tmpExt)
			return fmt.Errorf("rename old extension: %w", err)
		}
	}
	if err := os.Rename(tmpExt, extDir); err != nil {
		if _, e := os.Stat(oldExt); e == nil { // rollback
			_ = os.Rename(oldExt, extDir)
		}
		_ = os.RemoveAll(tmpExt)
		return fmt.Errorf("rename new extension into place: %w", err)
	}
	_ = os.RemoveAll(oldExt) // 成功后清旧（同卷 rename 已就位，oldExt 是上一版）
	return nil
}

// unzipTo extracts src zip into dst (created if needed). 路径清洗防 Zip-Slip
// （拒绝绝对路径与 .. 逃逸条目）。跨平台：zip 内部用 / 分隔，转 os 路径分隔。
func unzipTo(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()
	cleanDst := filepath.Clean(dst)
	if err := os.MkdirAll(cleanDst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", cleanDst, err)
	}
	for _, f := range r.File {
		name := filepath.FromSlash(f.Name)
		if name == "" || filepath.IsAbs(name) {
			continue
		}
		target := filepath.Clean(filepath.Join(cleanDst, name))
		// Zip-Slip guard：target 必须在 cleanDst 之下。
		rel, err := filepath.Rel(cleanDst, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, f.Mode()); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

// extractZipFile copies one zip entry to target, preserving file mode.
func extractZipFile(f *zip.File, target string) error {
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	rc, err := f.Open()
	if err != nil {
		out.Close()
		return fmt.Errorf("open zip entry %s: %w", f.Name, err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		rc.Close()
		out.Close()
		return fmt.Errorf("extract %s: %w", target, err)
	}
	rc.Close()
	return out.Close()
}

// readExtensionManifestVersion reads the "version" field from dir/manifest.json.
// 供 D2 解压后 + GET /api/extension-info 暴露 on-disk 新版本。best-effort：缺
// 失/不可解析返回 ("",err)。
func readExtensionManifestVersion(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", fmt.Errorf("parse manifest.json: %w", err)
	}
	return strings.TrimSpace(v.Version), nil
}

// extensionOnDiskInfo returns the on-disk extension dir + version (the freshly
// extracted ride-along extension, or the load-unpacked dir if no ride-along
// happened). 供 GET /api/extension-info 暴露新版本/新路径（§4.5）。无扩展目录
// 返回 ("","")。
func extensionOnDiskInfo() (dir, version string) {
	dir = extensionDir()
	if dir == "" {
		return "", ""
	}
	v, _ := readExtensionManifestVersion(dir)
	return dir, v
}
