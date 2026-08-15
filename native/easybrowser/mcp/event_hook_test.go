package mcp

import (
	"context"
	"strings"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// captureEmit swaps EmitHook to capture the last emitted Event; returns a
// restore func. Tests in package mcp can set EmitHook directly.
func captureEmit() (*Event, func()) {
	saved := EmitHook
	var got Event
	EmitHook = func(e Event) { got = e }
	return &got, func() { EmitHook = saved }
}

func TestWrapToolHandler_IsErrorSummaryWithText(t *testing.T) {
	h := wrapToolHandler("browser_click", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return mcptypes.NewToolResultError(`unknown ref "e72" — this ref is not in the current snapshot. Re-run browser_snapshot_ax`), nil
	})
	last, restore := captureEmit()
	defer restore()
	if _, err := h(context.Background(), mcptypes.CallToolRequest{}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !strings.HasPrefix(last.ResultSummary, "err:") {
		t.Fatalf("want 'err:' prefix, got %q", last.ResultSummary)
	}
	if !strings.Contains(last.ResultSummary, `unknown ref "e72"`) {
		t.Fatalf("summary missing error text: %q", last.ResultSummary)
	}
	if r := len([]rune(last.ResultSummary)); r > 66 { // "err:" + 60 + "…" ≈ 65 runes
		t.Fatalf("summary too long (not truncated to ~60): %d %q", r, last.ResultSummary)
	}
}

func TestWrapToolHandler_IsErrorEmptyContentDegrades(t *testing.T) {
	h := wrapToolHandler("x", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return &mcptypes.CallToolResult{IsError: true}, nil // no Content
	})
	last, restore := captureEmit()
	defer restore()
	h(context.Background(), mcptypes.CallToolRequest{})
	if last.ResultSummary != "error" {
		t.Fatalf("want degrade to 'error', got %q", last.ResultSummary)
	}
}

func TestWrapToolHandler_OkUnchanged(t *testing.T) {
	h := wrapToolHandler("x", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return mcptypes.NewToolResultText("ok"), nil
	})
	last, restore := captureEmit()
	defer restore()
	h(context.Background(), mcptypes.CallToolRequest{})
	if last.ResultSummary != "ok" {
		t.Fatalf("want 'ok', got %q", last.ResultSummary)
	}
}

// contentText extracts the Text of the first TextContent in a CallToolResult.
// Returns "" if res is nil, has no Content, or Content[0] isn't a TextContent.
func contentText(res *mcptypes.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcptypes.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestWrapToolHandler_NoTabAttached_Replaced(t *testing.T) {
	h := wrapToolHandler("browser_snapshot_ax", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return mcptypes.NewToolResultError("AX degraded (getFullAXTree failed: getFullAXTree error: JSON-RPC error -32000: no tab attached; call tabs.create or tabs.adopt first) and DOM fallback: not applied — getLayoutMetrics error: no tab attached"), nil
	})
	last, restore := captureEmit()
	defer restore()
	res, _ := h(context.Background(), mcptypes.CallToolRequest{})
	if got := contentText(res); got != noTabAttachedFriendlyMsg {
		t.Fatalf("no-tab-attached error not replaced:\nwant %q\ngot  %q", noTabAttachedFriendlyMsg, got)
	}
	// event kept the original error text for diagnostics
	if !strings.Contains(last.ResultSummary, "no tab attached") {
		t.Fatalf("event ResultSummary should tag no tab attached: %q", last.ResultSummary)
	}
}

func TestWrapToolHandler_NotInControlGroup_Replaced(t *testing.T) {
	h := wrapToolHandler("browser_click", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return mcptypes.NewToolResultError("click: not applied — tab 123 not in MCP control group; call tabs.adopt to enroll it first"), nil
	})
	last, restore := captureEmit()
	defer restore()
	res, _ := h(context.Background(), mcptypes.CallToolRequest{})
	if got := contentText(res); got != noTabAttachedFriendlyMsg {
		t.Fatalf("not-in-control-group error not replaced:\nwant %q\ngot  %q", noTabAttachedFriendlyMsg, got)
	}
	if !strings.Contains(last.ResultSummary, "no tab attached") {
		t.Fatalf("event ResultSummary should tag no tab attached: %q", last.ResultSummary)
	}
}

func TestWrapToolHandler_NormalError_Unchanged(t *testing.T) {
	h := wrapToolHandler("browser_click", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return mcptypes.NewToolResultError(`unknown ref "e72" — this ref is not in the current snapshot`), nil
	})
	last, restore := captureEmit()
	defer restore()
	res, _ := h(context.Background(), mcptypes.CallToolRequest{})
	if got := contentText(res); !strings.Contains(got, `unknown ref "e72"`) {
		t.Fatalf("normal error should be unchanged, got %q", got)
	}
	if !strings.Contains(last.ResultSummary, `unknown ref "e72"`) {
		t.Fatalf("event should keep original: %q", last.ResultSummary)
	}
}

func TestWrapToolHandler_NoTabEventKeepsOriginal(t *testing.T) {
	// The event emitted (for events.jsonl) must keep the ORIGINAL error text
	// (with "no tab attached" for diagnosis), while the returned res.Content
	// is the friendly message. Diagnostics vs AI-facing separation.
	h := wrapToolHandler("browser_snapshot_ax", func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return mcptypes.NewToolResultError("getFullAXTree error: no tab attached; call tabs.create or tabs.adopt first"), nil
	})
	last, restore := captureEmit()
	defer restore()
	res, _ := h(context.Background(), mcptypes.CallToolRequest{})
	// returned to AI = friendly
	if contentText(res) != noTabAttachedFriendlyMsg {
		t.Fatalf("AI-facing should be friendly, got %q", contentText(res))
	}
	// event = original (truncated, but contains "no tab attached")
	if !strings.Contains(last.ResultSummary, "no tab attached") {
		t.Fatalf("event should keep original 'no tab attached' for diagnostics, got %q", last.ResultSummary)
	}
	if strings.Contains(last.ResultSummary, "尚未关联") {
		t.Fatalf("event must NOT contain the friendly text (diagnostics needs real cause), got %q", last.ResultSummary)
	}
}
