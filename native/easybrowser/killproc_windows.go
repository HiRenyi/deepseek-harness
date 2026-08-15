//go:build windows

package main

// killproc_windows.go — 跨平台 kill 运行中进程的 Windows 实现。
//
// 用于 D1 nm-host swap 前终止占用 nm-host.exe 的进程：Chrome 经 Native
// Messaging 拉起 nm-host 时持有文件句柄，sharing mode 不允许 rename，导致
// swapCoAssetBinary 的 os.Rename(nm-host.exe, nm-host.exe.old) 报 "Access is
// denied"。nm-host 是 Chrome 按需拉起的短命进程，kill 后 Chrome 下次 NM 消息
// 会按 NM manifest 的 path（D1 保证不变）重拉新版。与 bridge 自更新同趟——
// bridge 也要重启，nm-host 短暂断连可接受。
//
// 仅按 exePath 全路径匹配，绝不误杀 bridge（bridge 路径 ≠ nm-host 路径）。
// best-effort：找不到匹配进程（没在跑）不报错；快照/Open 失败静默跳过。

import (
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killProcessAt 终止所有"完整镜像路径 == exePath"的进程（best-effort）。
// 找不到匹配进程不算错误（nm-host 没在跑）。命中后短暂等 ~200ms 让 OS 释放
// 镜像锁再返回，调用方随即 rename。返回值始终为 nil——kill 失败不阻断 swap
// 流程（rename 仍会尝试，由上层 retry 兜底）。
func killProcessAt(exePath string) error {
	target := normalizeExePath(exePath)
	if target == "" {
		return nil // 无法规范化（理论不会发生）→ 不阻断
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil // best-effort：拿不到快照就不 kill
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	// COMMON BUG: Size 必须置为 unsafe.Sizeof(entry)，否则 Process32First 以
	// ERROR_BAD_LENGTH 拒绝（静默返回 0 条）。见 install_windows.go 同款注释。
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil
	}
	killed := 0
	for {
		pid := entry.ProcessID
		if img := processImagePath(pid); img != "" && pathsMatch(img, target) {
			// 不杀自己：bridge 自更新进程的镜像路径不会等于 nm-host 路径，
			// 但防御性地跳过当前 pid。
			if pid == windows.GetCurrentProcessId() {
				if err := windows.Process32Next(snap, &entry); err != nil {
					break
				}
				continue
			}
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
				if err := windows.TerminateProcess(h, 1); err == nil {
					killed++
				}
				_ = windows.CloseHandle(h)
			}
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	if killed > 0 {
		// 终止后镜像锁不会瞬间释放——给 OS 一个窗口再让调用方 rename。
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// processImagePath 返回 pid 的完整 Win32 镜像路径（带盘符，如
// C:\Users\...\nm-host.exe），经 QueryFullProcessImageName 取得。查询失败
// （进程已退出或权限不足）返回 ""。
func processImagePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil || h == 0 {
		return ""
	}
	defer windows.CloseHandle(h)
	var buf [windows.MAX_PATH]uint16
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// normalizeExePath 返回 exePath 的 Abs+EvalSymlinks 规范形（Windows 不分大小写，
// 但此处保留原样由 pathsMatch 的 EqualFold 处理）。EvalSymlinks 失败则回退 Abs。
// 返回 "" 表示连 Abs 都失败（理论不会发生）。
func normalizeExePath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r
	}
	return a
}

// pathsMatch 报告运行中进程的镜像路径 img 是否等于目标 nm-host 路径 target。
// 两侧都尽量 EvalSymlinks 规范化（进程在跑时文件存在，EvalSymlinks 应成功），
// 规范化失败则退化为 Clean 后的 EqualFold 比较。
func pathsMatch(img, target string) bool {
	if img == "" || target == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(img); err == nil {
		return strings.EqualFold(r, target)
	}
	return strings.EqualFold(filepath.Clean(img), filepath.Clean(target))
}
