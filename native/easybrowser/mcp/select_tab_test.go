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

// TestBrowserSelectTabForwardsSelectTab verifies the handler forwards a
// {"tabId":N} JSON-RPC request with method "tabs.select_tab" to the extension,
// and returns a success result mentioning "selected".
func TestBrowserSelectTabForwardsSelectTab(t *testing.T) {
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
	handler := BrowserSelectTabHandler(router)

	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{
		Arguments: map[string]any{"tabId": 42},
	}}

	done := make(chan *mcptypes.CallToolResult, 1)
	go func() {
		result, err := handler(context.Background(), req)
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
	var fwd jsonrpc.Request
	if err := json.Unmarshal(sent, &fwd); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	mu.Unlock()

	if fwd.Method != "tabs.select_tab" {
		t.Fatalf("expected method tabs.select_tab, got %s", fwd.Method)
	}

	var params struct {
		TabID int `json:"tabId"`
	}
	if err := json.Unmarshal(fwd.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.TabID != 42 {
		t.Fatalf("expected params.tabId=42, got %d", params.TabID)
	}

	// deliver a success response with the parsed ID
	respData, _ := json.Marshal(jsonrpc.Response{
		JSONRPC: "2.0",
		ID:      fwd.ID,
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
	// textual content should mention "selected"
	text := fmt.Sprintf("%v", result.Content)
	if !strings.Contains(strings.ToLower(text), "selected") {
		t.Fatalf("result text does not mention selected: %s", text)
	}
}

// TestBrowserSelectTabToolSchema verifies the static tool definition.
func TestBrowserSelectTabToolSchema(t *testing.T) {
	tool := BrowserSelectTabTool()
	if tool.Name != "browser_select_tab" {
		t.Fatalf("expected name browser_select_tab, got %s", tool.Name)
	}
	required := tool.InputSchema.Required
	foundTabID := false
	for _, r := range required {
		if r == "tabId" {
			foundTabID = true
		}
	}
	if !foundTabID {
		t.Fatalf("expected Required to contain tabId, got %v", required)
	}
	desc := strings.ToLower(tool.Description)
	if !strings.Contains(desc, "no focus") &&
		!strings.Contains(desc, "does not focus") &&
		!strings.Contains(desc, "without") {
		t.Fatalf("description should advertise the no-focus contract: %s", tool.Description)
	}
}
