# Windows 发行版

本目录包含 DeepSeek Harness 的 Windows 桌面发行版组件。

## 目录结构

| 路径 | 说明 |
|---|---|
| `packages/windows/launcher/` | Cordis 插件：系统托盘、自启动、通知、sidecar 进程管理 |
| `packages/windows/easybrowser/` | Cordis 插件：easybrowser 桥接服务 |
| `apps/windows-desktop/` | Wails 桌面应用（Go + React） |
| `native/windows/` | 构建脚本和 NSIS 安装包 |

## 构建

```powershell
# 完整构建
native/windows/build.ps1

# 跳过桌面应用和安装包
native/windows/build.ps1 -SkipDesktop -SkipInstaller
```

## 与上游同步

```bash
# 拉取上游更新
git checkout main
git pull upstream main
git checkout release/windows
git merge main
```

## 依赖

- easybrowser: `vendor/easybrowser/`（git submodule）
- Wails CLI: `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- NSIS: https://nsis.sourceforge.io/