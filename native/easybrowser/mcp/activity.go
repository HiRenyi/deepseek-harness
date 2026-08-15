package mcp

// activity.go — A4 (bridge-decouple-self-update): 轻量活跃度计数器，供 bridge
// /health 暴露 in_use（"是否在用"），让宿主（GUI/KairosAIStudio）判断更新时机。
//
// 三路信号：
//   - activeSessions  : 当前注册的 MCP/SSE 会话数（连接打开 +1，断开 -1）。
//   - activeToolCalls : 当前正在执行的工具调用数（进入 handler +1，返回 -1）。
//   - nm 连接状态     : 由 package main 的 nmAlive() 提供（见 /health 组装处）。
//
// 之所以不直接反射第三方 mcp-go 内部的 sessions sync.Map，是因为它未导出且语义
// 不稳定；这里用两个原子计数器 + MCPServer 的 OnRegister/OnUnregister 钩子驱动，
// 足够轻量且自洽。GET /sse 心跳不计入"在用"（对齐 update_idle 的口径：连接但无
// 工具调用视为空闲），只有真实的会话注册 + 工具调用 + nm 心跳才算 in_use。

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// 包级活跃度计数器。原子操作，无需锁。
var (
	activeSessions  atomic.Int64
	activeToolCalls atomic.Int64
)

// ActiveSessions 返回当前活跃 MCP/SSE 会话数（已注册未注销）。
func ActiveSessions() int64 { return activeSessions.Load() }

// ActiveToolCalls 返回当前正在执行的工具调用数（in-flight）。
func ActiveToolCalls() int64 { return activeToolCalls.Load() }

// incToolCall / decToolCall 由 wrapToolHandler（event_hook.go）在每次工具调用
// 前后调用，驱动 in-flight 计数。
func incToolCall() { activeToolCalls.Add(1) }
func decToolCall() { activeToolCalls.Add(-1) }

// SessionNotifier abstracts the JSON-RPC sender used to notify the extension
// of session lifecycle events. *jsonrpc.Router satisfies it; tests pass a fake.
type SessionNotifier interface {
	SendRequest(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error)
}

// sessionHooks 构造一组会话生命周期钩子，驱动 activeSessions 计数。调用方通过
// mcpserver.WithHooks(sessionHooks(router)) 在 NewMCPServer 时注入。SSE 会话由
// SSEServer.handleSSE 调用 server.RegisterSession/UnregisterSession 触发钩子；
// streamable-http 在 stateful 模式下同样触发。stateless 模式不触发——此时连接
// 计数为 0，但 in_use 仍可由 activeToolCalls + nmAlive 兜底，不影响"在用"判定。
//
// session-tab-isolation: OnRegisterSession 额外经 notifier 发 session.start 通知
// 扩展释放上一会话的 attachedTabId（releaseActiveTab 轻清保组），使每 session
// 独立 tab，消除多会话 navigate hijack。best-effort：2s 超时，err 跳过，不让
// session 注册失败（nm 未连/router err 是旁路，非注册前置）。
func sessionHooks(notifier SessionNotifier) *mcpserver.Hooks {
	h := &mcpserver.Hooks{}
	h.AddOnRegisterSession(func(ctx context.Context, _ mcpserver.ClientSession) {
		activeSessions.Add(1)
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := notifier.SendRequest(sctx, "session.start", nil); err != nil {
			log.Printf("session.start notify skipped: %v", err)
		}
	})
	h.AddOnUnregisterSession(func(ctx context.Context, _ mcpserver.ClientSession) {
		// 防御性：异常路径下不让计数器变负（best-effort，不致命）。
		if activeSessions.Load() > 0 {
			activeSessions.Add(-1)
		}
	})
	return h
}
