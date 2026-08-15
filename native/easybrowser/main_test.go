package main

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	mcpbridge "github.com/browser-mcp/bridge/mcp"
)

// TestNMLiveness verifies /health's connection state is driven by real
// last-seen timestamps, not just "a conn object exists". This is the fix for
// /health lying when nm-host silently exited.
func TestNMLiveness(t *testing.T) {
	// reset to "never heard from nm-host"
	atomic.StoreInt64(&nmLastSeenUnixNano, 0)
	if nmAlive() {
		t.Fatal("nmAlive() should be false when no message ever received")
	}

	// a fresh message makes it alive
	touchNMLastSeen()
	if !nmAlive() {
		t.Fatal("nmAlive() should be true right after touchNMLastSeen")
	}

	// an old timestamp should read as not alive
	old := time.Now().Add(-2 * nmAliveTimeout).UnixNano()
	atomic.StoreInt64(&nmLastSeenUnixNano, old)
	if nmAlive() {
		t.Fatal("nmAlive() should be false when last seen is older than the timeout")
	}

	// heartbeat detection
	if !isHeartbeat([]byte(`{"jsonrpc":"2.0","method":"__heartbeat__"}`)) {
		t.Error("isHeartbeat should detect a heartbeat message")
	}
	if isHeartbeat([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) {
		t.Error("isHeartbeat should not flag a normal response")
	}
}

// --- listenWithRetry（Fix 2）---

// TestListenWithRetry_AddrInUseThenFree: 端口被占时重试，占用释放后成功 bind。
// 模拟 --post-self-update 场景：旧进程占 58080，400ms 后 exit 释放，新 bridge
// 重试期间拿到端口。
func TestListenWithRetry_AddrInUseThenFree(t *testing.T) {
	// 占一个随机端口。
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupant listen: %v", err)
	}
	addr := ln1.Addr().String()

	// 500ms 后释放（模拟旧进程 exit）。
	go func() {
		time.Sleep(500 * time.Millisecond)
		ln1.Close()
	}()

	// 重试：25 次 × 100ms = 2.5s 窗口，足够覆盖 500ms。
	ln2, err := listenWithRetry(addr, 25, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("重试后应 bind 成功，got %v", err)
	}
	defer ln2.Close()

	// 确认是新 listener（能 Accept）。
	if ln2.Addr().String() != addr {
		t.Fatalf("addr=%q want %q", ln2.Addr().String(), addr)
	}
}

// TestListenWithRetry_NoRetriesImmediateFail: retries=0 时 addr-in-use 立即返回
// 错误（冷启动行为——被别家占用就该 fatal，不重试）。
func TestListenWithRetry_NoRetriesImmediateFail(t *testing.T) {
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupant listen: %v", err)
	}
	defer ln1.Close()
	addr := ln1.Addr().String()

	start := time.Now()
	_, err = listenWithRetry(addr, 0, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("retries=0 + addr-in-use 应立即返回错误")
	}
	if !isAddrInUseError(err) {
		t.Fatalf("应是 addr-in-use 错误，got %v", err)
	}
	// 立即返回，不应 sleep（< 50ms）。
	if elapsed > 50*time.Millisecond {
		t.Fatalf("retries=0 应立即返回，took %v", elapsed)
	}
}

// TestListenWithRetry_NonAddrInUseNoRetry: 非 addr-in-use 错误（坏地址）即使
// retries>0 也不重试——立即返回。验证只有 addr-in-use 才重试。
func TestListenWithRetry_NonAddrInUseNoRetry(t *testing.T) {
	start := time.Now()
	_, err := listenWithRetry("not-a-valid-addr", 25, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("坏地址应返回错误")
	}
	// 非 addr-in-use → 不重试 → 立即返回（< 200ms，远小于 25×100ms=2.5s）。
	if elapsed > 200*time.Millisecond {
		t.Fatalf("非 addr-in-use 错误不应重试，took %v", elapsed)
	}
	if isAddrInUseError(err) {
		t.Fatalf("坏地址不应被判为 addr-in-use: %v", err)
	}
}

// TestBuildHealthPayload_InUse 验证 /health payload 含 version + in_use +
// active_sessions + active_tool_calls 字段，且 in_use 语义正确（A4）。
func TestBuildHealthPayload_InUse(t *testing.T) {
	// nm alive → in_use 必为 true（无论计数器状态）。
	touchNMLastSeen()
	p := buildHealthPayload(58080, transportAddrInfoLite{Network: "tcp", Address: "127.0.0.1:58080"}, nil)
	if p["version"] != version {
		t.Errorf("version=%v want %q", p["version"], version)
	}
	if p["nm_connected"] != true {
		t.Error("nm_connected should be true after touchNMLastSeen")
	}
	if p["in_use"] != true {
		t.Error("in_use must be true when nm alive")
	}
	for _, k := range []string{"active_sessions", "active_tool_calls"} {
		if _, ok := p[k]; !ok {
			t.Errorf("missing field %q in /health payload", k)
		}
	}

	// nm not alive → in_use 由活跃会话/工具调用决定（计数器读当前值，确定性比对）。
	atomic.StoreInt64(&nmLastSeenUnixNano, 0)
	p2 := buildHealthPayload(58080, transportAddrInfoLite{}, nil)
	sessions := mcpbridge.ActiveSessions()
	toolCalls := mcpbridge.ActiveToolCalls()
	wantInUse := sessions > 0 || toolCalls > 0
	if p2["in_use"] != wantInUse {
		t.Errorf("nm-dead in_use=%v want %v (sessions=%d toolCalls=%d)",
			p2["in_use"], wantInUse, sessions, toolCalls)
	}
}
