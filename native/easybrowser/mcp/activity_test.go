package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcpserver "github.com/mark3labs/mcp-go/server"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// fakeSessionNotifier records SendRequest calls for assertion.
type fakeSessionNotifier struct {
	calls     []string
	returnErr error
}

func (f *fakeSessionNotifier) SendRequest(_ context.Context, method string, _ json.RawMessage) (*jsonrpc.Response, error) {
	f.calls = append(f.calls, method)
	return &jsonrpc.Response{}, f.returnErr
}

var errFakeNotifier = &fakeNotifierError{}

type fakeNotifierError struct{}

func (e *fakeNotifierError) Error() string { return "fake notifier unavailable" }

// TestActiveToolCalls_WrapHandlerIncDec 验证 wrapToolHandler 在工具调用期间把
// activeToolCalls 抬到 1、返回后归零（A4 in_use 的 in-flight 信号）。
func TestActiveToolCalls_WrapHandlerIncDec(t *testing.T) {
	// 起点应为 0（其他测试可能动过计数器，但 wrapToolHandler 是配对的，归零）。
	started := make(chan struct{})
	done := make(chan struct{})
	var inner sync.WaitGroup
	inner.Add(1)

	h := wrapToolHandler("test_tool", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		// 进入 handler 期间 in-flight 计数必须 > 0。
		if ActiveToolCalls() <= 0 {
			t.Errorf("inside handler: activeToolCalls=%d, want >0", ActiveToolCalls())
		}
		close(started)
		inner.Wait() // 阻塞直到主测程读到中间态
		return &mcptypes.CallToolResult{}, nil
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h(context.Background(), mcptypes.CallToolRequest{})
		close(done)
	}()

	<-started
	if got := ActiveToolCalls(); got != 1 {
		t.Fatalf("during in-flight call: activeToolCalls=%d, want 1", got)
	}
	inner.Done()
	<-done
	if got := ActiveToolCalls(); got != 0 {
		t.Fatalf("after call returns: activeToolCalls=%d, want 0", got)
	}
}

// TestSessionHooks_RegisterUnregister 验证会话钩子正确驱动 activeSessions。
// 用真实 MCPServer.RegisterSession/UnregisterSession（不依赖 HTTP）触发钩子。
func TestSessionHooks_RegisterUnregister(t *testing.T) {
	// 记录起点，测试后恢复（计数器是包级共享的）。
	base := ActiveSessions()
	notifier := &fakeSessionNotifier{}
	srv := mcpserver.NewMCPServer("test", "0", mcpserver.WithHooks(sessionHooks(notifier)))

	sess := mcpserver.NewInProcessSession("s1", nil)
	ctx := context.Background()
	if err := srv.RegisterSession(ctx, sess); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if got := ActiveSessions() - base; got != 1 {
		t.Fatalf("after register: activeSessions delta=%d, want 1", got)
	}
	srv.UnregisterSession(ctx, "s1")
	if got := ActiveSessions() - base; got != 0 {
		t.Fatalf("after unregister: activeSessions delta=%d, want 0", got)
	}
}

// TestSessionHooks_RegisterSendsSessionStart 验证 OnRegisterSession 发 session.start。
func TestSessionHooks_RegisterSendsSessionStart(t *testing.T) {
	notifier := &fakeSessionNotifier{}
	srv := mcpserver.NewMCPServer("test", "0", mcpserver.WithHooks(sessionHooks(notifier)))

	sess := mcpserver.NewInProcessSession("s2", nil)
	if err := srv.RegisterSession(context.Background(), sess); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if len(notifier.calls) != 1 || notifier.calls[0] != "session.start" {
		t.Fatalf("want one session.start call, got %v", notifier.calls)
	}
}

// TestSessionHooks_RegisterSkipsOnNotifierError 验证 best-effort：nm 未连/router err
// 时 session.start 跳过但不 panic、activeSessions 仍 +1。
func TestSessionHooks_RegisterSkipsOnNotifierError(t *testing.T) {
	base := ActiveSessions()
	notifier := &fakeSessionNotifier{returnErr: errFakeNotifier}
	srv := mcpserver.NewMCPServer("test", "0", mcpserver.WithHooks(sessionHooks(notifier)))

	sess := mcpserver.NewInProcessSession("s3", nil)
	if err := srv.RegisterSession(context.Background(), sess); err != nil {
		t.Fatalf("RegisterSession must not fail when notifier errors: %v", err)
	}
	if got := ActiveSessions() - base; got != 1 {
		t.Fatalf("activeSessions delta=%d, want 1 (registration unaffected)", got)
	}
}
