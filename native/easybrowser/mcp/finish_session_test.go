package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// TestBrowserFinishSessionForwardsSessionEnd verifies the handler forwards a
// nil-param JSON-RPC request with method "session.end" to the extension.
func TestBrowserFinishSessionForwardsSessionEnd(t *testing.T) {
	var mu sync.Mutex
	var sent []byte

	sender := func(data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent = make([]byte, len(data))
		copy(sent, data)
		return nil
	}

	router := jsonrpc.NewRouter(sender, 5*time.Second)
	handler := BrowserFinishSessionHandler(router)

	done := make(chan *mcptypes.CallToolResult, 1)
	go func() {
		result, err := handler(context.Background(), mcptypes.CallToolRequest{})
		if err != nil {
			t.Errorf("handler returned err: %v", err)
			done <- nil
			return
		}
		done <- result
	}()

	// wait for the request to be sent
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	var req jsonrpc.Request
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	mu.Unlock()

	if req.Method != "session.end" {
		t.Fatalf("expected method session.end, got %s", req.Method)
	}

	// deliver a success response with the parsed ID
	respData, _ := json.Marshal(jsonrpc.Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  json.RawMessage(`{}`),
	})
	router.HandleResponse(respData)

	result := <-done
	if result == nil {
		t.Fatal("got nil result")
	}
	if result.IsError {
		t.Fatalf("result unexpectedly in error: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("result has no content")
	}
	// textual content should mention "session"
	text := fmt.Sprintf("%v", result.Content)
	if !strings.Contains(strings.ToLower(text), "session") {
		t.Fatalf("result text does not mention session: %s", text)
	}
}

// TestBrowserFinishSessionToolSchema verifies the static tool definition.
func TestBrowserFinishSessionToolSchema(t *testing.T) {
	tool := BrowserFinishSessionTool()
	if tool.Name != "finish_session" {
		t.Fatalf("expected name finish_session, got %s", tool.Name)
	}
	if len(tool.InputSchema.Required) != 0 {
		t.Fatalf("expected no required params, got %v", tool.InputSchema.Required)
	}
	if !strings.Contains(strings.ToLower(tool.Description), "task is complete") {
		t.Fatalf("description should mention 'task is complete': %s", tool.Description)
	}
}
