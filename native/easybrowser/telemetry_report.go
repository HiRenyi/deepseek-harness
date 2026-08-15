package main

// telemetry_report.go — install 上报(首次装 / 自更新落新版本 各一次)。
//
// DAU/MAU 不靠 install 上报:bridge idle fetch version.json 的动作本身就是
// DAU 信号——服务端 routes_channel.py 的 version.json 路由记 version_check 事件,
// 按 IP 去重(COALESCE(client_id, ip),client_id/X-Client-Id UUID 已移除,纯按 IP)。
// install 上报只用于"安装量"统计(installs_total),触发时机:
//   - 首次运行(<dataDir>/install_reported_version 标记不存在)
//   - 自更新装上新版本(标记记录的是旧版本号,≠ 当前版本)
//   - 普通重启(标记 == 当前版本)→ 不发,避免把"安装量"变成"启动量"
// 一个标记检查覆盖三种情形,无需 --post-self-update 特判。
//
// 标记文件存最近一次已上报的版本号。仅 HTTP 2xx 才写标记 → 失败下次启动重试。
// best-effort:失败 log 静默,不重试(同进程内)、不阻塞启动、不 panic。
//
// install URL 从 update-source base 反推:base = <prefix>/<slug>/<channel>
// (如 http://host/tool-hub/browser-mcp/stable),install 端点在
// <prefix>/api/telemetry/install(同 mount 前缀,去掉末尾 slug/channel 对)。
// 这样 tool-hub 挂在 /tool-hub/ 子路径下时 install POST 不会落根 404(旧的
// strip-to-host-root 在 2026-07-03 加 /tool-hub/ mount 后会落根 404 漏报)。
// gitee base → https://gitee.com/api/telemetry/install(无此端点,404 静默,可接受)。
//
// body = {client_version, channel?}。不带 client_id/IP——服务端用 nginx 的
// X-Real-IP 区分用户(内网一人一 IP)。channel 缺省则该字段省略(服务端 channel=null)。

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/profile"
)

// startupPingTimeout 是 install 上报的超时。包级变量以便测试缩短(默认 5s;
// 测试可降到 100ms 加速不可达用例)。
var startupPingTimeout = 5 * time.Second

// startupPingClient 是 install 上报的独立短超时 client,不共用 60s 的
// updateHTTPClient——上报不能拖慢启动。失败静默。
var startupPingClient = &http.Client{Timeout: startupPingTimeout}

// reportInstallIfNewVersion 在 bridge 首次跑某版本(首装或自更新落新版本)时
// 发一次 install 事件,然后写 <dataDir>/install_reported_version 标记,使普通
// 重启不膨胀安装计数。标记按版本:版本变化(含自更新落地新 build)各触发恰好
// 一次。从 main() 异步调用,绝不阻塞启动。
func reportInstallIfNewVersion() {
	// paranoia:上报绝不能因 panic 拖垮启动。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("telemetry: install report panic (swallowed): %v", r)
		}
	}()

	dataDir, err := bridgeDataDir()
	if err != nil {
		return // 无数据目录 → 无法跟踪标记,跳过
	}
	marker := filepath.Join(dataDir, "install_reported_version")
	if prev, err := os.ReadFile(marker); err == nil &&
		strings.TrimSpace(string(prev)) == version {
		return // 该版本已上报过,普通重启 → 跳过
	}
	if reportInstallPing() {
		// 仅 2xx 才写标记:失败下次启动重试。
		_ = os.WriteFile(marker, []byte(version), 0o600)
	}
}

// reportInstallPing POST 一个 install 事件(当前版本)到从 update-source base
// 反推出的 telemetry 端点。2xx 返回 true。body = {client_version, channel?};
// 无 client_id/IP。任何错误仅 log,不影响启动。
func reportInstallPing() bool {
	base := updateSource().Base()
	if base == "" {
		return false
	}
	endpoint := telemetryInstallURL(base)

	// channel: runtime env/config wins (BROWSER_MCP_UPDATE_CHANNEL override),
	// else the build-time-baked profile.Channel default (prod=stable, test=sit)
	// — lets stats group installs by channel so prod/sit on the same IP differ.
	ch := config.Load().UpdateChannel
	if ch == "" {
		ch = profile.Channel
	}
	payload := struct {
		ClientVersion string `json:"client_version"`
		Channel       string `json:"channel,omitempty"`
	}{
		ClientVersion: version,
		Channel:       ch,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("telemetry: marshal install payload: %v", err)
		return false
	}

	resp, err := startupPingClient.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("telemetry: install report %s failed (non-blocking): %v", endpoint, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("telemetry: install report %s returned HTTP %d (non-blocking)", endpoint, resp.StatusCode)
		return false
	}
	return true
}

// telemetryInstallURL 从 update-source base 反推 install 端点。
// base = <prefix>/<slug>/<channel>(如 http://host/tool-hub/browser-mcp/stable);
// install 端点 = <prefix>/api/telemetry/install(去掉末尾 slug/channel 对,保留
// mount 前缀,使子路径挂载的 tool-hub 不 404)。gitee base →
// https://gitee.com/api/telemetry/install(404 静默)。解析失败回退
// <base>/api/telemetry/install。
func telemetryInstallURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(base, "/") + "/api/telemetry/install"
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	// parts[0] == ""(前导斜杠)。去掉末尾 slug + channel(2 段)。
	if len(parts) > 2 {
		parts = parts[:len(parts)-2]
	} else {
		parts = []string{""}
	}
	u.Path = strings.Join(parts, "/") + "/api/telemetry/install"
	return u.String()
}
