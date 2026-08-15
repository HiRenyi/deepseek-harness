//go:build !windows

package main

// killproc_other.go — 跨平台 kill 运行中进程的 Mac/Linux 实现。
//
// Unix 系 rename 一个正在运行的二进制通常不会 "Access is denied"（内核
// 允许 unlink + 替换，进程继续跑旧 inode），故 nm-host swap 在非 Windows
// 上无镜像锁矛盾。本实现仅在 Linux（有 /proc）时按 /proc/*/exe 符号链接
// 匹配 exePath 并发 SIGTERM；Mac 无 /proc → ReadDir 失败 → best-effort
// no-op，rename 直接由上层 swapCoAssetBinary 完成。
//
// 与 killproc_windows.go 同签名 killProcessAt(exePath) error。仅按全路径匹配，
// 不误杀 bridge；找不到进程不算错误。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func killProcessAt(exePath string) error {
	target, err := filepath.Abs(exePath)
	if err != nil {
		return nil
	}
	if r, err := filepath.EvalSymlinks(target); err == nil {
		target = r
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil // 非 Linux（如 Mac 无 /proc）→ best-effort no-op
	}
	selfPid := os.Getpid()
	killed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == selfPid {
			continue
		}
		link, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		if !strings.EqualFold(link, target) {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			if err := p.Signal(syscall.SIGTERM); err == nil {
				killed++
			}
		}
	}
	if killed > 0 {
		time.Sleep(200 * time.Millisecond) // 让句柄释放
	}
	return nil
}
