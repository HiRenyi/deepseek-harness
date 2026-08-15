package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/jsonrpc"
	mcpbridge "github.com/browser-mcp/bridge/mcp"
	"github.com/browser-mcp/bridge/profile"
	"github.com/browser-mcp/bridge/transport"
)

// nmLastSeen tracks when we last heard from nm-host (any inbound message,
// including heartbeats). /health uses it to report the REAL link state rather
// than just "a conn object exists" (which lied when nm-host silently exited).
// (transport-resilience: honest connection state.)
var nmLastSeenUnixNano int64

const heartbeatMethod = "__heartbeat__"
const nmAliveTimeout = 15 * time.Second

func touchNMLastSeen()      { atomic.StoreInt64(&nmLastSeenUnixNano, time.Now().UnixNano()) }
func nmAlive() bool {
	v := atomic.LoadInt64(&nmLastSeenUnixNano)
	if v == 0 {
		return false // never heard from nm-host
	}
	return time.Since(time.Unix(0, v)) < nmAliveTimeout
}
func isHeartbeat(data []byte) bool {
	// cheap substring check before full JSON parse
	return bytes.Contains(data, []byte(`"`+heartbeatMethod+`"`))
}

// buildHealthPayload 组装 /health 的 JSON payload（A4：暴露 version + in_use）。
// 抽成函数以便单测验证 in_use/version 等字段存在且语义正确，无需起整个 HTTP 服务。
//
// in_use 判定（"是否在用"）= 以下任一为真：
//   - nm-host 在线（nmAlive）—— 扩展正通过 NM 驱动浏览器；
//   - 有活跃 MCP/SSE 会话（activeSessions>0）—— 客户端连着；
//   - 有正在执行的工具调用（activeToolCalls>0）—— 有 in-flight 请求。
//
// 三路信号任一为真即视为"在用"，宿主据此决定更新时机（在用→推迟，空闲→可更新）。
func buildHealthPayload(mcpPort int, addrInfo transportAddrInfoLite, perms *config.PermissionStore) map[string]any {
	sessions := mcpbridge.ActiveSessions()
	toolCalls := mcpbridge.ActiveToolCalls()
	nm := nmAlive()
	result := map[string]any{
		"status":             "ok",
		"version":            version,
		"transports":         []string{"sse", "http"},
		"port":               mcpPort,
		"nm_connected":       nm,
		"in_use":             nm || sessions > 0 || toolCalls > 0,
		"active_sessions":    sessions,
		"active_tool_calls":  toolCalls,
		"internal_transport": map[string]string{"type": addrInfo.Network, "address": addrInfo.Address},
	}
	if perms != nil {
		result["cdp_permissions"] = perms.Get()
	} else {
		result["cdp_permissions"] = config.DefaultPermissions()
	}
	return result
}

// Build metadata injected via -ldflags. Defaults let the binary run
// unconfigured (e.g. `go run`, dev builds); production builds override them.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
	startedAt = time.Now()
)

func main() {
	mcpAddr := flag.String("mcp-addr", "127.0.0.1:"+profile.Port, "MCP server listen address")
	timeout := flag.Duration("timeout", 30*time.Second, "CDP request timeout")
	showVersion := flag.Bool("version", false, "print version info and exit")
	postSelfUpdate := flag.Bool("post-self-update", false, "internal: self-update swap just completed — clean .old then resume normal startup")
	// --boot is set by the HKCU Run autostart entry (bridge-boot-autostart):
	// signals "launched at boot without a parent terminal" → self-hide the
	// console window (see hide_console_windows.go). Terminal launches never
	// pass --boot, so the user's console is never hidden.
	boot := flag.Bool("boot", false, "internal: launched at boot — hide console window")
	flag.Parse()

	// Hide the console window FIRST (before anything that might emit to it)
	// to minimize the boot flash. Only when --boot; --version from a terminal
	// leaves the user's console visible. Then self-heal the autostart entry
	// (harden-bridge-autostart): re-register HKCU Run\<AutostartKey>Bridge if
	// it was deleted/stripped/never written, so next boot still autostarts.
	if *boot {
		hideConsoleWindow()
		selfHealAutostart()
	}

	// Version short-circuit: MUST run before AcquireLock so a version query
	// never occupies the bridge lock or binds the MCP port.
	if *showVersion {
		fmt.Printf("bridge version=%s commit=%s build=%s started=%s pid=%d\n",
			version, commit, buildTime, startedAt.Format(time.RFC3339), os.Getpid())
		return
	}

	// install 上报:首次跑某版本(首装或自更新落新版本)时发一次 install 事件,
	// 普通重启跳过(标记文件去重)。DAU/MAU 不靠这个——靠 idle fetch version.json
	// 的 version_check 事件(服务端按 IP 去重)。install URL 从 base 反推带 mount
	// 前缀,不 strip 到 host root(否则 /tool-hub/ 子路径挂载会 404)。best-effort,
	// 5s 超时,失败静默,绝不阻塞启动。见 telemetry_report.go。
	go reportInstallIfNewVersion()

	// B7: --post-self-update is set by runSelfUpdate's re-exec of the freshly
	// swapped binary. Best-effort clean the leftover .old (the previous binary,
	// renamed out of the way by swapBinary). 此刻旧进程可能还活着（re-exec 后
	// ~400ms 才 exit）、.old 镜像锁着 → 这次删除可能失败，无妨——bind 成功后
	// 会起 cleanupOldBinaryWithRetry goroutine 重试兜底（见下方 listenWithRetry
	// 后）。NOT a short-circuit — falls through to acquire the lock + serve, so
	// the new bridge takes over from the old one seamlessly.
	if *postSelfUpdate {
		CleanupOldBinary()
		log.Printf("bridge: --post-self-update, best-effort .old cleanup, resuming startup")
	}

	// Create context that cancels on shutdown signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Redirect logs to ~/.browser-mcp/logs/<YYYYMMDD>/bridge.log (F2.5 RunLog reads it).
	setupRunLog()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 0. Occupancy lock: ensure at most one live bridge per data dir.
	// (Contract — see CLAUDE.md "环境契约": single bridge instance per data dir.)
	lockRelease, err := transport.AcquireLock()
	if err != nil {
		log.Fatalf("bridge: %v", err)
	}
	defer lockRelease()

	// B7: record the running bridge's build-time version so currentVersion()
	// (the self-update comparison truth-source, B6) converges: after a
	// self-update the re-execd new bridge writes its build-time var, which
	// matches the manifest version → the idle loop stops re-triggering. Also
	// makes /api/status show the live version. MUST run AFTER AcquireLock: a
	// bridge that loses the lock race (a stale/duplicate start, a dev build
	// while an installed bridge serves) log.Fatals at AcquireLock and must NOT
	// pollute bridge_version with its version — that would desync the file
	// ahead of the actually-serving binary and suppress the idle updater
	// (isNewerVersion sees "already on recorded" → false). Best-effort; a
	// failure never blocks startup.
	if d, err := bridgeDataDir(); err == nil {
		_ = writeBridgeVersion(d, version)
	}

	// 1. Start platform-default transport (Named Pipe on Windows, Unix Socket on Linux/Mac)
	srv := transport.NewServer()
	if err := srv.Start(ctx); err != nil {
		log.Fatalf("transport start failed: %v", err)
	}
	addrInfo := srv.AddrInfo()
	log.Printf("transport: using %s at %s", addrInfo.Network, addrInfo.Address)

	// 2. Create JSON-RPC router with transport sender
	router := jsonrpc.NewRouter(srv.Send, *timeout)

	// 4. Wire transport messages to router. Any inbound message proves nm-host
	// is alive — refresh lastSeen. Heartbeats are keepalives (not JSON-RPC
	// responses), so skip the router for them.
	srv.OnMessage(func(data []byte) {
		touchNMLastSeen()
		if isHeartbeat(data) {
			return
		}
		router.HandleResponse(data)
	})

	// 5. Load CDP permission config
	perms, err := config.NewPermissionStore()
	if err != nil {
		log.Printf("warning: failed to load permission config, using defaults: %v", err)
		perms = nil
	}

	// 5.5 F6 C-foundation: structured event sink — events.jsonl + subscribe chan.
	// Non-fatal: on init failure eventSink stays nil and emit is a no-op, so a
	// broken events dir NEVER breaks MCP/CDP flow. Wired BEFORE the MCP server
	// is created so every tool/CDP call is observed from the first request.
	initEventSink()
	mcpbridge.EmitHook = func(e mcpbridge.Event) {
		if eventSink != nil {
			eventSink.emit(e)
		}
	}

	// 6. Register config.set_cdp_permission JSON-RPC handler
	router.RegisterMethod("config.set_cdp_permission", func(params json.RawMessage) (any, error) {
		var p struct {
			Key   string `json:"key"`
			Value bool   `json:"value"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if perms == nil {
			return nil, fmt.Errorf("permission store not available")
		}
		if err := perms.Set(p.Key, p.Value); err != nil {
			return nil, err
		}
		return map[string]bool{"success": true}, nil
	})

	// 7. Create MCP server with permission checks
	mcpSrv := mcpbridge.NewMCPServer(router, perms)

	// 8. Create health check handler with CORS for extension access
	mcpPort := extractPort(*mcpAddr)
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(buildHealthPayload(mcpPort, transportAddrInfoLite{Network: addrInfo.Network, Address: addrInfo.Address}, perms))
	}

	// 9. Start dual-transport MCP server on shared port
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)

	// P3: dashboard + status + update routes
	addrInfoLite := transportAddrInfoLite{Network: addrInfo.Network, Address: addrInfo.Address}
	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/api/status", apiStatusHandler(mcpPort, addrInfoLite))
	mux.HandleFunc("/api/update-check", updateCheckHandler)
	mux.HandleFunc("/api/config", configHandler)             // F2.6: GET effective config + sources
	mux.HandleFunc("/api/config/reload", configReloadHandler) // F2.6: POST hot-reload config
	mux.HandleFunc("/update/install", updateInstallHandler)
	mux.HandleFunc("/api/detect-clients", detectClientsHandler)   // P4.1
	mux.HandleFunc("/api/configure", configureHandler)            // P4.2
	mux.HandleFunc("/api/config-view", configViewHandler)         // P4.3 view client config (W2.3a)
	mux.HandleFunc("/api/config-rollback", configRollbackHandler) // P4.3 restore .browser-mcp.bak (W2.3a)
	mux.HandleFunc("/api/extension-info", extensionInfoHandler)   // ext self-reports version → dashboard

	sseServer := mcpbridge.NewSSEServer(mcpSrv,
		mcpbridge.WithSSECORS(mcpbridge.WithCORSAllowedOrigins("*")),
	)
	mux.Handle("/sse", sseServer)
	mux.Handle("/message", trackMCPActivity(sseServer)) // P5.5: timestamp MCP POSTs

	streamableServer := mcpbridge.NewStreamableHTTPServer(mcpSrv,
		mcpbridge.WithStreamableHTTPCORS(mcpbridge.WithCORSAllowedOrigins("*")),
	)
	mux.Handle("/mcp", trackMCPActivity(streamableServer)) // P5.5: timestamp MCP POSTs

	// REST API layer — thin HTTP adapter over the same tool registry as MCP.
	// Enables script-based concurrent access (Python asyncio / curl parallel).
	restHandler := NewRESTHandler(router, perms)
	mux.Handle("/api/tool/", restHandler)
	mux.HandleFunc("/api/tools", restHandler.ListTools)
	mux.HandleFunc("/api/batch", restHandler.Batch)

	go func() {
		log.Printf("mcp: dual transport server starting on %s (routes: /, /sse, /message, /mcp, /health, /api/status, /api/update-check, /api/config, /api/config/reload, /update/install)", *mcpAddr)
		// --post-self-update 模式：旧进程刚 re-exec 完新 bridge，但旧进程要
		// ~400ms 后才 exit 释放 58080。期间新 bridge bind 会 addr-in-use 失败。
		// 用重试循环等旧进程退出：25 次 × 200ms = 5s 窗口，足够覆盖 400ms 延迟。
		// 非 post-self-update（冷启动）传 0 → 立即 fatal（被别家占用就该 fatal）。
		retries := 0
		if *postSelfUpdate {
			retries = 25
		}
		ln, err := listenWithRetry(*mcpAddr, retries, 200*time.Millisecond)
		if err != nil {
			// Check if it's a port binding error and provide enhanced diagnostics
			if isAddrInUseError(err) {
				occupant, pid := findPortOccupantByAddr(*mcpAddr)
				if occupant != "" {
					if isBridgeProcess(occupant) {
						log.Fatalf("MCP port %s 被旧 bridge 进程 (PID: %d) 占用。请先终止: taskkill /PID %d /F",
							*mcpAddr, pid, pid)
					}
					log.Fatalf("MCP port %s 被 %s (PID: %d) 占用。解决方案：1. 关闭该程序  2. 使用 --mcp-addr 指定其他端口",
						*mcpAddr, occupant, pid)
				}
				log.Fatalf("MCP port %s 被占用。请使用 --mcp-addr 指定其他端口", *mcpAddr)
			}
			log.Fatalf("MCP server start failed: %v", err)
		}
		// bind 成功 = 旧进程已 exit 释放端口。此时 .old 镜像锁也已释放（旧进程
		// exit 同时释放端口 + 文件锁）。起后台 goroutine 重试清 .old 兜底启动初
		// 那次 best-effort 的失败（仅 --post-self-update 路径需要）。
		if *postSelfUpdate {
			go cleanupOldBinaryWithRetry()
		}
		if err := http.Serve(ln, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("MCP server serve failed: %v", err)
		}
	}()

	log.Printf("bridge: ready (transport=%s://%s, mcp=%s)", addrInfo.Network, addrInfo.Address, *mcpAddr)

	// P5.5: arm the idle silent auto-updater (no-op if
	// BROWSER_MCP_UPDATE_IDLE_MIN<=0). Only checks for a newer version when
	// idle; on a hit, triggers a bridge self-update (B1 rename-swap + re-exec).
	startIdleUpdater()

	// Wait for shutdown signal
	select {
	case sig := <-sigCh:
		log.Printf("bridge: received signal %s, shutting down...", sig)
	case <-ctx.Done():
		log.Printf("bridge: context cancelled, shutting down...")
	}

	// Cleanup
	cancel()
	if err := srv.Close(); err != nil {
		log.Printf("transport close error: %v", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := sseServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("SSE server shutdown error: %v", err)
	}
	log.Println("bridge: stopped")
}

// extractPort extracts the port number from an address string like "127.0.0.1:58080".
func extractPort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultPort()
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return defaultPort()
	}
	return port
}

// defaultPort parses profile.Port (a string, since ldflag -X only supports
// strings) to int, falling back to 58080 if unparseable. Used as the fallback
// for malformed --mcp-addr values and port-occupant diagnostics.
func defaultPort() int {
	p, err := strconv.Atoi(profile.Port)
	if err != nil || p <= 0 {
		return 58080
	}
	return p
}

// isAddrInUseError checks if the error indicates the address is already in use.
// 跨平台：Linux/macOS 报 "address already in use"；Windows 报 "Only one usage
// of each socket address ... is normally permitted"。统一取最内层错误信息
// lowercase 匹配三种子串，避免 OpError/SyscallError 包装路径漏判 Windows
// 的 addr-in-use（漏判会导致 listenWithRetry 不重试 + 端口诊断 fallback 丢失）。
func isAddrInUseError(err error) bool {
	msg := err.Error()
	if opErr, ok := err.(*net.OpError); ok {
		if opErr.Err != nil {
			msg = opErr.Err.Error()
		}
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "in use") ||
		strings.Contains(lower, "address already") ||
		strings.Contains(lower, "only one usage of each socket address")
}

// listenWithRetry 在 addr 上 listen，addr-in-use 时按 interval 重试最多
// retries 次。retries=0 → 单次尝试（失败即返回，原冷启动行为：被别家占用就该
// fatal）。retries>0 用于 --post-self-update 路径：旧进程 re-exec 后 ~400ms
// 才 exit 释放 58080，期间新 bridge 的 net.Listen 会 addr-in-use 失败，重试
// 等旧进程退出后即可成功。非 addr-in-use 错误（如权限不足、坏地址）立即返回
// 不重试——这类错误重试无意义。返回 listener（bind 成功）或错误。
//
// 拆 net.Listen + http.Serve 两步（而非 http.ListenAndServe）是为了让 bind
// 成功的时机确定：listener 拿到 = 端口已绑定 = 旧进程已 exit，此后即可起
// cleanupOldBinaryWithRetry goroutine 清 .old（.old 锁随旧进程 exit 释放）。
func listenWithRetry(addr string, retries int, interval time.Duration) (net.Listener, error) {
	for attempt := 0; ; attempt++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		// 非 addr-in-use 错误立即返回——重试无意义（坏地址/权限等）。
		if !isAddrInUseError(err) {
			return nil, err
		}
		if attempt >= retries {
			return nil, err
		}
		time.Sleep(interval)
	}
}

// findPortOccupantByAddr extracts the port from an address string and finds the occupant.
func findPortOccupantByAddr(addr string) (string, int) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = profile.Port
	}
	return findPortOccupant(port)
}

// findPortOccupant attempts to find the process occupying a TCP port.
// Returns (processName, pid). Returns ("", 0) if unable to determine.
func findPortOccupant(port string) (string, int) {
	if runtime.GOOS == "windows" {
		return findPortOccupantWindows(port)
	}
	return findPortOccupantUnix(port)
}

func findPortOccupantWindows(port string) (string, int) {
	// Use netstat -ano to find the PID
	cmd := exec.Command("netstat", "-ano")
	output, err := cmd.Output()
	if err != nil {
		return "", 0
	}

	for _, line := range strings.Split(string(output), "\n") {
		// Look for LISTENING on the port
		if !strings.Contains(line, "LISTENING") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// Check if the local address ends with :port
		localAddr := fields[1]
		if !strings.HasSuffix(localAddr, ":"+port) {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		// Get process name
		name := getProcessNameWindows(pid)
		return name, pid
	}
	return "", 0
}

func findPortOccupantUnix(port string) (string, int) {
	// Try lsof first
	cmd := exec.Command("lsof", "-i", ":"+port, "-t", "-sTCP:LISTEN")
	output, err := cmd.Output()
	if err == nil {
		pidStr := strings.TrimSpace(string(output))
		pid, err := strconv.Atoi(pidStr)
		if err == nil {
			name := getProcessNameUnix(pid)
			return name, pid
		}
	}

	// Fallback to ss
	cmd = exec.Command("ss", "-tlnp")
	output, err = cmd.Output()
	if err != nil {
		return "", 0
	}

	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, ":"+port) {
			continue
		}
		// Extract PID from ss output like "users:(("bridge",pid=1234,...))"
		idx := strings.Index(line, "pid=")
		if idx == -1 {
			continue
		}
		pidStr := line[idx+4:]
		if commaIdx := strings.Index(pidStr, ","); commaIdx != -1 {
			pidStr = pidStr[:commaIdx]
		}
		if parenIdx := strings.Index(pidStr, ")"); parenIdx != -1 {
			pidStr = pidStr[:parenIdx]
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		name := getProcessNameUnix(pid)
		return name, pid
	}
	return "", 0
}

func getProcessNameWindows(pid int) string {
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Parse CSV: "bridge.exe","1234","Console","1","5,120 K"
	line := strings.TrimSpace(string(output))
	if line == "" {
		return ""
	}
	parts := strings.Split(line, ",")
	if len(parts) < 1 {
		return ""
	}
	name := strings.Trim(parts[0], `"`)
	return name
}

func getProcessNameUnix(pid int) string {
	// Read /proc/[pid]/comm on Linux
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err == nil {
		return strings.TrimSpace(string(data))
	}
	// Fallback: ps
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// isBridgeProcess checks if a process name matches the bridge binary for THIS
// profile (bridge.exe or bridge-prod.exe — profile.BinSuffix). A test build's
// port-occupant diagnostic must NOT recognize a prod bridge as "self" (and
// vice versa), so the two profiles' diagnostics stay independent.
func isBridgeProcess(name string) bool {
	lower := strings.ToLower(name)
	base := "bridge" + profile.BinSuffix
	return lower == base || lower == base+".exe"
}
