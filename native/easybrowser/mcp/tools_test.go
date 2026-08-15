package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/axtree"
	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// ---- browser_snapshot_ax handler wiring ----

// axTree5Buttons is a healthy AX fixture: 5 buttons -> above COVERAGE_MIN.
const axTree5Buttons = `{"nodes":[
	{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3","4","5","6"]},
	{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"Submit"},"backendDOMNodeId":1,"parentId":"1"},
	{"nodeId":"3","role":{"type":"role","value":"button"},"name":{"type":"string","value":"Cancel"},"backendDOMNodeId":2,"parentId":"1"},
	{"nodeId":"4","role":{"type":"role","value":"button"},"name":{"type":"string","value":"Save"},"backendDOMNodeId":3,"parentId":"1"},
	{"nodeId":"5","role":{"type":"role","value":"button"},"name":{"type":"string","value":"Delete"},"backendDOMNodeId":4,"parentId":"1"},
	{"nodeId":"6","role":{"type":"role","value":"button"},"name":{"type":"string","value":"Edit"},"backendDOMNodeId":5,"parentId":"1"}
]}`

func TestBrowserSnapshotAxHealthy(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Accessibility.getFullAXTree": json.RawMessage(axTree5Buttons),
			"Runtime.evaluate":             json.RawMessage(`{"result":{"value":"[]"}}`),
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserSnapshotAxHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	// Must surface at least one ref and not be degraded.
	text := toolResultText(res)
	if !strings.Contains(text, "ref=e") {
		t.Errorf("expected refs in healthy snapshot, got:\n%s", text)
	}
	if strings.HasPrefix(text, "[degraded=true") {
		t.Errorf("healthy tree should not degrade, got:\n%s", text)
	}
}

// TestBrowserSnapshotAxDegradesToDOM feeds a DEGENERATE AX tree (RootWebArea +
// generic only, zero assigned refs) and asserts the handler falls back to
// GetVisibleDOM and flags degraded=true. Under strategy D a simple page with
// few interactives is NOT degraded; only a zero-ref degenerate tree degrades.
func TestBrowserSnapshotAxDegradesToDOM(t *testing.T) {
	degenerate := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"generic"},"parentId":"1"}
	]}`
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Accessibility.getFullAXTree": json.RawMessage(degenerate),
			"Runtime.evaluate":             json.RawMessage(`{"result":{"value":"[]"}}`),
			// GetVisibleDOM CDP calls:
			"Page.getLayoutMetrics":        json.RawMessage(`{"cssLayoutViewport":{"clientWidth":100,"clientHeight":100}}`),
			"DOM.getDocument":              json.RawMessage(`{"root":{"nodeId":1,"children":[]}}`),
			"DOM.querySelectorAll":         json.RawMessage(`{"nodeIds":[]}`),
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserSnapshotAxHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	text := toolResultText(res)
	// The degrade decision was triggered: result contains the reason. (We assert
	// on the reason substring rather than the DOM payload because fully mocking
	// GetVisibleDOM's CDP sequence is out of scope — the degrade *decision* is
	// the behavior under test; GetVisibleDOM is covered elsewhere.)
	if !strings.Contains(text, "degenerate AX tree: 0 refs") {
		t.Errorf("degenerate (zero-ref) tree should trigger degrade, got:\n%s", text)
	}
}

// toolResultText extracts the first text content from a CallToolResult.
func toolResultText(res *mcptypes.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(mcptypes.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// ---- interaction tools accept optional ref (Task 15) ----

// TestBrowserClickViaRef asserts browser_click resolves a ref to a backendNodeId
// and dispatches ClickPoint with it (no nodeId required).
func TestBrowserClickViaRef(t *testing.T) {
	// Seed DefaultRefMap with a ref whose backendNodeId is alive (42).
	refID := axtree.DefaultRefMap.Assign("button", "ClickViaRef", -1, "")
	axtree.DefaultRefMap.SetBackend(refID, 42)
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			// ResolveRef fast path probes getBoxModel -> alive.
			"DOM.getBoxModel": json.RawMessage(`{"model":{"border":[0,0,2,0,2,2,0,2]}}`),
			// ClickPoint uses getContentQuads then dispatchMouseEvent.
			"DOM.getContentQuads":      json.RawMessage(`{"quads":[[0,0,2,0,2,2,0,2]]}`),
			"Input.dispatchMouseEvent": json.RawMessage(`{}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{"ref": refID}}}
	res, err := BrowserClickHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("click via ref err: %v", err)
	}
	if res.IsError {
		t.Fatalf("click via ref failed: %+v", res.Content)
	}
	text := toolResultText(res)
	if !strings.Contains(text, "node_id=42") {
		t.Errorf("expected click on resolved backendNodeId 42, got: %s", text)
	}
}

// TestBrowserClickAtHandlerAppendsActualHit verifies the raw-coordinate click
// path appends the elementFromPoint-resolved actual_hit so the agent can
// self-check whether the click passed through a visual overlay (design D2).
func TestBrowserClickAtHandlerAppendsActualHit(t *testing.T) {
	// HitTestAt uses evalJS → Runtime.evaluate; returns the hit element desc.
	// ClickAt uses Input.dispatchMouseEvent (fake returns empty resp).
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Runtime.evaluate": json.RawMessage(`{"result":{"type":"string","value":"div.modal-backdrop"}}`),
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"x": float64(5), "y": float64(5),
	}}}
	res, err := BrowserClickAtHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("handler returned tool error: %s", toolResultText(res))
	}
	txt := toolResultText(res)
	if !containsStr(txt, "actual_hit") || !containsStr(txt, "div.modal-backdrop") {
		t.Errorf("result=%q, want to contain 'actual_hit' and 'div.modal-backdrop'", txt)
	}
}

// TestBrowserClickAtHandlerHitTestFailureNonFatal verifies that an
// elementFromPoint probe failure does NOT turn a successful click into an
// error — hit-test is informational only. The test also guards against a
// vacuous pass: if a future refactor deletes the HitTestAt call, the
// Runtime.evaluate error is never triggered and the handler still succeeds,
// so we assert the probe was actually invoked and that actual_hit is absent
// (proving the probe ran, failed, and was suppressed).
func TestBrowserClickAtHandlerHitTestFailureNonFatal(t *testing.T) {
	f := &fakeCDP{
		errors: map[string]*jsonrpc.ResponseError{
			"Runtime.evaluate": {Code: -32000, Message: "cannot evaluate"},
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"x": float64(5), "y": float64(5),
	}}}
	res, err := BrowserClickAtHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("hit-test failure must be non-fatal, got tool error: %s", toolResultText(res))
	}
	probeInvoked := false
	for _, c := range f.calls {
		if c.method == "Runtime.evaluate" {
			probeInvoked = true
			break
		}
	}
	if !probeInvoked {
		t.Fatal("HitTestAt probe was not invoked — test is vacuous")
	}
	if txt := toolResultText(res); containsStr(txt, "actual_hit") {
		t.Errorf("probe failure should suppress actual_hit, got: %s", txt)
	}
}

// TestBrowserClickMissingRefAndNodeId asserts the tool errors when neither is given.
func TestBrowserClickMissingRefAndNodeId(t *testing.T) {
	f := &fakeCDP{}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserClickHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error result when no ref/nodeId given")
	}
}
