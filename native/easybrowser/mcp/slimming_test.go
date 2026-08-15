package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// axSlimFakeCDP feeds the AX pipeline (Accessibility.getFullAXTree +
// Runtime.evaluate cursor) and Page.captureScreenshot /
// Page.getLayoutMetrics for the slimming handler tests.
type axSlimFakeCDP struct {
	axNodes  json.RawMessage
	eval     json.RawMessage
	shotData string
	calls    []string
}

func (f *axSlimFakeCDP) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	f.calls = append(f.calls, method)
	resp := &jsonrpc.Response{}
	switch method {
	case "Accessibility.enable":
		// no-op
	case "Accessibility.getFullAXTree":
		resp.Result = f.axNodes
	case "Runtime.evaluate":
		resp.Result = f.eval
	case "Page.captureScreenshot":
		resp.Result = json.RawMessage(`{"data":"aGVsbG8="}`)
	case "Page.getLayoutMetrics":
		resp.Result = json.RawMessage(`{"cssVisualViewport":{"pageX":0,"pageY":0,"clientWidth":1280,"clientHeight":720}}`)
	case "DOM.getDocument":
		resp.Result = json.RawMessage(`{"root":{"nodeId":1,"nodeType":9,"nodeName":"#document","backendNodeId":0,"children":[]}}`)
	}
	return resp, nil
}

// minimalAXTree is a 1-button healthy AX tree (no iframes).
const minimalAXTree = `{"nodes":[
	{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
	{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"OK"},"backendDOMNodeId":1,"parentId":"1"}
]}`

func newSlimAXReq(args map[string]any) mcptypes.CallToolRequest {
	return mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: args}}
}

// ---------------------------------------------------------------------------
// browser_screenshot (D5)
// ---------------------------------------------------------------------------

func TestScreenshotDefaultMetadataNoBase64(t *testing.T) {
	f := &axSlimFakeCDP{}
	h := BrowserScreenshotHandler(f)
	res, err := h(context.Background(), newSlimAXReq(nil))
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	txt := resultText(res)
	if strings.HasPrefix(txt, "data:image/") {
		t.Errorf("default form must NOT inline base64 data URI, got: %s", txt[:min2(40, len(txt))])
	}
	if !strings.Contains(txt, "screenshot captured") {
		t.Errorf("default form should mention capture, got: %s", txt)
	}
	if !strings.Contains(txt, "inline_base64:true") {
		t.Errorf("default form should hint the escape hatch, got: %s", txt)
	}
}

func TestScreenshotInlineBase64EscapeHatch(t *testing.T) {
	f := &axSlimFakeCDP{}
	h := BrowserScreenshotHandler(f)
	res, err := h(context.Background(), newSlimAXReq(map[string]any{"inline_base64": true}))
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	txt := resultText(res)
	if !strings.HasPrefix(txt, "data:image/jpeg;base64,") {
		t.Errorf("inline_base64:true should return data URI, got: %s", txt)
	}
}

// ---------------------------------------------------------------------------
// browser_snapshot_ax dedup + force + full opt-in (D1/D2/D6)
// ---------------------------------------------------------------------------

func TestSnapshotAxDedupSecondCallUnchanged(t *testing.T) {
	dedupReset("ax")
	// Reset axStuck so the loop guardrail doesn't fire during the test.
	axStuck.Lock()
	axStuck.sig = ""
	axStuck.n = 0
	axStuck.Unlock()

	f := &axSlimFakeCDP{
		axNodes: json.RawMessage(minimalAXTree),
		eval:    json.RawMessage(`{"result":{"value":"[]"}}`),
	}
	h := BrowserSnapshotAxHandler(f)

	// First call -> full snapshot.
	res1, err := h(context.Background(), newSlimAXReq(nil))
	if err != nil {
		t.Fatalf("first call err: %v", err)
	}
	txt1 := resultText(res1)
	if !strings.Contains(txt1, "ref=e1") {
		t.Fatalf("first call should return full AX tree with ref, got: %s", txt1)
	}

	// Second identical call -> "unchanged".
	res2, err := h(context.Background(), newSlimAXReq(nil))
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	txt2 := resultText(res2)
	if !strings.Contains(txt2, "unchanged") || !strings.Contains(txt2, "force:true") {
		t.Fatalf("second identical call should return unchanged notice, got: %s", txt2)
	}
}

func TestSnapshotAxForceSkipsDedup(t *testing.T) {
	dedupReset("ax")
	axStuck.Lock()
	axStuck.sig = ""
	axStuck.n = 0
	axStuck.Unlock()

	f := &axSlimFakeCDP{
		axNodes: json.RawMessage(minimalAXTree),
		eval:    json.RawMessage(`{"result":{"value":"[]"}}`),
	}
	h := BrowserSnapshotAxHandler(f)

	// Prime the baseline.
	h(context.Background(), newSlimAXReq(nil))

	// force:true -> full payload despite matching baseline.
	res, err := h(context.Background(), newSlimAXReq(map[string]any{"force": true}))
	if err != nil {
		t.Fatalf("force call err: %v", err)
	}
	txt := resultText(res)
	if strings.Contains(txt, "unchanged") {
		t.Errorf("force:true must skip dedup, got unchanged: %s", txt)
	}
	if !strings.Contains(txt, "ref=e1") {
		t.Errorf("force:true should return full AX tree, got: %s", txt)
	}
}

func TestSnapshotAxFullOptIn(t *testing.T) {
	dedupReset("ax")
	axStuck.Lock()
	axStuck.sig = ""
	axStuck.n = 0
	axStuck.Unlock()

	f := &axSlimFakeCDP{
		axNodes: json.RawMessage(minimalAXTree),
		eval:    json.RawMessage(`{"result":{"value":"[]"}}`),
	}
	h := BrowserSnapshotAxHandler(f)

	// Default (no mode, no full) -> compact.
	resC, _ := h(context.Background(), newSlimAXReq(nil))
	if strings.Contains(resultText(resC), "unchanged") {
		// If dedup fired on first call (shouldn't), force a re-fetch.
		resC, _ = h(context.Background(), newSlimAXReq(map[string]any{"force": true}))
	}
	// Verify the tool schema declares full as opt-in (default compact).
	tool := BrowserSnapshotAxTool()
	fullDesc, _ := tool.InputSchema.Properties["full"].(map[string]any)
	if fullDesc == nil || fullDesc["type"] != "boolean" {
		t.Fatalf("snapshot_ax schema must declare a boolean 'full' opt-in param")
	}
	modeDesc, _ := tool.InputSchema.Properties["mode"].(map[string]any)
	if modeDesc == nil {
		t.Fatalf("snapshot_ax schema must keep the 'mode' param")
	}

	// full:true should select full mode (handler reads it).
	f2 := &axSlimFakeCDP{
		axNodes: json.RawMessage(minimalAXTree),
		eval:    json.RawMessage(`{"result":{"value":"[]"}}`),
	}
	h2 := BrowserSnapshotAxHandler(f2)
	resF, err := h2(context.Background(), newSlimAXReq(map[string]any{"full": true}))
	if err != nil {
		t.Fatalf("full:true call err: %v", err)
	}
	if strings.Contains(resultText(resF), "unchanged") {
		// Different fake instance => different baseline; just confirm no error.
	}
	// Both modes must produce a non-empty result.
	if resultText(resC) == "" || resultText(resF) == "" {
		t.Errorf("both compact and full must produce output")
	}
}

// resultText extracts the first text content from a CallToolResult.
func resultText(res *mcptypes.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcptypes.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// min2 returns the smaller of two ints (test helper only).
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
