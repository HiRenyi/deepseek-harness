package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/browser-mcp/bridge/axtree"
	"github.com/browser-mcp/bridge/cdp"
	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/jsonrpc"
	"github.com/browser-mcp/bridge/snapshot"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ---------------------------------------------------------------------------
// Tool: browser_click
// ---------------------------------------------------------------------------

// BrowserClickTool returns the MCP tool definition for browser_click.
func BrowserClickTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_click",
		Description: "Click on an element identified by node_id (BackendNodeID) or ref (from browser_snapshot_ax). When both ref and nodeId are given, ref wins. Before dispatching, performs a hit-target occlusion check (agent-browser BLOCKER_AT_JS): if the target is visually covered by an overlay/modal, returns an error instead of passing through to the lower layer — dismiss the covering element first, then retry.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{
					"type":        "integer",
					"description": "node_id from browser_snapshot",
				},
				"ref": map[string]any{
					"type":        "string",
					"description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given",
				},
			},
			Required: []string{},
		},
	}
}

// BrowserClickHandler clicks the element identified by ref or nodeId.
//
// Beyond Codex's bare clickPoint (no verify), the bridge probes the click's
// effect (navigation/toggle/none) and runs a fallback chain (JSClick → coord
// retry → screenshot) when an interactive element shows no effect — real-machine
// Drift-A: React synthetic handlers fire on JS click but not always on CDP
// dispatchMouseEvent, and a silent no-op was reported as success. The honest
// report lets the AI distinguish a real click from a silent no-op.
func BrowserClickHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cdpClient, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		eff, err := ClickWithEffect(ctx, cdpClient, nodeID)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("click: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(formatClickEffect(eff)), nil
	}
}

// formatClickEffect renders an honest click report. effect=none with exhausted
// fallbacks does NOT single-report success (DA3): the screenshot is attached so
// the AI can judge. The url before→after and fallback chain make the outcome
// auditable.
func formatClickEffect(eff *ClickEffect) string {
	urlPart := "url unchanged"
	if eff.URLBefore != eff.URLAfter && eff.URLAfter != "" {
		urlPart = fmt.Sprintf("url: %s → %s", eff.URLBefore, eff.URLAfter)
	} else if eff.URLBefore != "" {
		urlPart = fmt.Sprintf("url: %s (unchanged)", eff.URLBefore)
	}
	fb := "none"
	if len(eff.Fallbacks) > 0 {
		fb = strings.Join(eff.Fallbacks, " → ")
	}
	msg := fmt.Sprintf("clicked node_id=%d (%s; effect: %s; fallbacks: %s)",
		eff.NodeID, urlPart, eff.Effect, fb)
	if eff.Screenshot != "" {
		// Attach screenshot as a separate image content block when no effect
		// was observed after all fallbacks (DA3 honest reporting).
		msg += "\n[attached: screenshot of post-click state]"
	}
	return msg
}

// ---------------------------------------------------------------------------
// Tool: browser_fill
// ---------------------------------------------------------------------------

// BrowserFillTool returns the MCP tool definition for browser_fill.
func BrowserFillTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_fill",
		Description: "Fill an input element (identified by node_id or ref) with a value. Focuses, clears, types the value char-by-char, then blurs to trigger onChange. Use this for form fields; use browser_type for special/combo keys. When both ref and nodeId are given, ref wins. For canvas-rendered spreadsheets (Tencent Docs/Feishu sheets), fill the formula-bar content box (the combobox/textbox adjacent to the address box showing the selected cell's value) — not the in-cell editor (contenteditable, not in snapshot) — the bar syncs to the selected cell.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{"type": "integer", "description": "node_id from browser_snapshot"},
				"ref":    map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
				"value":  map[string]any{"type": "string", "description": "value to fill"},
			},
			Required: []string{"value"},
		},
	}
}

func BrowserFillHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cdpClient, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		value, err := req.RequireString("value")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := RobustFill(ctx, cdpClient, nodeID, value); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("fill: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("filled node_id=%d with %d chars", nodeID, len(value))), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_type
// ---------------------------------------------------------------------------

// BrowserTypeTool returns the MCP tool definition for browser_type.
func BrowserTypeTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_type",
		Description: "Dispatch a sequence of named/special keys (e.g. [\"Enter\"], [\"Control\",\"a\"], [\"Tab\"]). For typing regular text into a field, prefer browser_fill.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"keys": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "named keys to press in order",
				},
			},
			Required: []string{"keys"},
		},
	}
}

func BrowserTypeHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		keys, err := req.RequireStringSlice("keys")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := TypeKeys(ctx, cdpClient, keys); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("type: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("typed %d keys: %v", len(keys), keys)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_navigate
// ---------------------------------------------------------------------------

// BrowserNavigateTool returns the MCP tool definition for browser_navigate.
func BrowserNavigateTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_navigate",
		Description: "Navigate the active tab to a URL. Does not wait for full load — call browser_snapshot after to confirm. PROHIBITED: do NOT guess a 'new document' URL (e.g. https://docs.qq.com/sheet/new, /doc/new, or any /new-style creation URL with no referrer/origin). Such URLs are unauthorized direct access and will fail or be rejected by the site. To create a new document (空白文档/表格/etc.), you MUST click the page UI element (e.g. the template card) step by step; after the click opens a new tab, use browser_list_tabs + browser_switch_tab to follow it. Only pass a known, complete URL you obtained from the page itself.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"url": map[string]any{"type": "string", "description": "URL to navigate to"},
			},
			Required: []string{"url"},
		},
	}
}

func BrowserNavigateHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		urlParam, err := req.RequireString("url")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		// D1 / A item: detect chrome:// / chrome-extension:// / about: active tab.
		// CDP Page.navigate is rejected on these schemes ("Cannot access a
		// chrome:// URL"); rather than let the rejection trigger the model's
		// new_tab fallback loop, return an explicit guidance so the model
		// picks browser_new_tab deliberately. We probe the active tab URL via
		// Runtime.evaluate (window.location.href); if the probe itself fails we
		// proceed to Navigate (which will surface its own error).
		if activeURL, err := GetURL(ctx, cdpClient); err == nil {
			if isRestrictedNavScheme(activeURL) {
				return mcptypes.NewToolResultError(fmt.Sprintf(
					"active tab is on a chrome:// URL which CDP cannot navigate; call browser_new_tab with %s instead",
					urlParam,
				)), nil
			}
		}
		res, err := Navigate(ctx, cdpClient, urlParam)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("navigate: not applied — %v", err)), nil
		}
		// HTTP status probed via PerformanceNavigationTiming (Drift-C: 404 used
		// to be reported as loaded=true). -1 = unknown; surface as http_status=unknown.
		if res.HTTPStatus < 0 {
			return mcptypes.NewToolResultText(fmt.Sprintf("navigated to %s (frameId=%s, loaded=%v, http_status=unknown)", urlParam, res.FrameID, res.Loaded)), nil
		}
		if res.HTTPStatus >= 400 {
			return mcptypes.NewToolResultText(fmt.Sprintf("navigated to %s (frameId=%s, loaded=%v, http_status=%d) — likely error page", urlParam, res.FrameID, res.Loaded, res.HTTPStatus)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("navigated to %s (frameId=%s, loaded=%v, http_status=%d)", urlParam, res.FrameID, res.Loaded, res.HTTPStatus)), nil
	}
}

// isRestrictedNavScheme reports whether the active tab URL scheme is one CDP
// Page.navigate cannot touch (chrome:, chrome-extension:, about:). On these
// the navigate handler returns a guidance error instead of firing a CDP call
// that is guaranteed to be rejected.
func isRestrictedNavScheme(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch u.Scheme {
	case "chrome", "chrome-extension", "about":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Tool: browser_screenshot
// ---------------------------------------------------------------------------

func BrowserScreenshotTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_screenshot",
		Description: "Capture a JPEG screenshot of the current page. By default returns lightweight metadata (width, height, captured_at) without inlining the base64 image data, which would otherwise dominate the model context. Pass inline_base64:true to get the raw data:image/jpeg;base64,... payload (e.g. when you need to visually inspect the page to judge a click's effect).",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"inline_base64": map[string]any{
					"type":        "boolean",
					"description": "When true, return the JPEG as a data:image/jpeg;base64,... string (legacy behavior). Default false — return only metadata so base64 does not enter the model context.",
				},
			},
		},
	}
}

func BrowserScreenshotHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		data, err := Screenshot(ctx, cdpClient)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("screenshot: not applied — %v", err)), nil
		}
		// D5: default metadata form — no inline base64. The model learns a
		// screenshot was captured and its dimensions; KairosAIStudio / the
		// integrator can render on demand. inline_base64:true restores the
		// legacy data-URI payload for cases where the model must see the image.
		if req.GetBool("inline_base64", false) {
			return mcptypes.NewToolResultText(fmt.Sprintf("data:image/jpeg;base64,%s", data)), nil
		}
		width, height := screenshotDimensions(ctx, cdpClient)
		return mcptypes.NewToolResultText(fmt.Sprintf(
			"screenshot captured (width=%d, height=%d, captured_at=%s); call with inline_base64:true to get the base64 image data",
			width, height, time.Now().UTC().Format(time.RFC3339),
		)), nil
	}
}

// screenshotDimensions returns the current viewport width/height via
// Page.getLayoutMetrics, for the default metadata form. Returns 0,0 when the
// metrics are unavailable (the screenshot itself still succeeded).
func screenshotDimensions(ctx context.Context, c CDPClient) (int, int) {
	resp, err := c.Call(ctx, "Page.getLayoutMetrics", mustMarshal(struct{}{}))
	if err != nil || resp == nil || resp.Error != nil {
		return 0, 0
	}
	var layout struct {
		CSSVisualViewport *struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssVisualViewport"`
	}
	if err := json.Unmarshal(resp.Result, &layout); err != nil || layout.CSSVisualViewport == nil {
		return 0, 0
	}
	return int(layout.CSSVisualViewport.ClientWidth), int(layout.CSSVisualViewport.ClientHeight)
}

// ---------------------------------------------------------------------------
// Tool: browser_scroll
// ---------------------------------------------------------------------------

func BrowserScrollTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_scroll",
		Description: "Scroll the page by scrollX/scrollY pixels. Optionally scroll a specific element (by node_id or ref) into view first. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"scrollX": map[string]any{"type": "integer", "description": "horizontal scroll delta (default 0)"},
				"scrollY": map[string]any{"type": "integer", "description": "vertical scroll delta (default 0)"},
				"nodeId":  map[string]any{"type": "integer", "description": "optional node_id to scroll into view first"},
				"ref":     map[string]any{"type": "string", "description": "optional ref (e.g. e1) from browser_snapshot_ax to scroll into view; preferred over nodeId when both given"},
			},
		},
	}
}

func BrowserScrollHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		scrollX, _ := req.RequireInt("scrollX")
		scrollY, _ := req.RequireInt("scrollY")
		var nodeID *int
		if hasRefOrNode(req) {
			v, err := resolveNodeIDFromRef(ctx, cdpClient, req)
			if err != nil {
				return mcptypes.NewToolResultError(err.Error()), nil
			}
			nodeID = &v
		}
		if err := Scroll(ctx, cdpClient, scrollX, scrollY, nodeID); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("scroll: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("scrolled (x=%d, y=%d)", scrollX, scrollY)), nil
	}
}

// hasRefOrNode reports whether the request carries a ref or nodeId param.
func hasRefOrNode(req mcptypes.CallToolRequest) bool {
	_, hasRef := optionalString(req, "ref")
	_, hasNode := optionalInt(req, "nodeId")
	return hasRef || hasNode
}

// ---------------------------------------------------------------------------
// Tool: browser_select_option
// ---------------------------------------------------------------------------

func BrowserSelectOptionTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_select_option",
		Description: "Select an option in a <select> dropdown by value. Element identified by node_id or ref; when both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{"type": "integer", "description": "node_id of the <select> element"},
				"ref":    map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
				"value":  map[string]any{"type": "string", "description": "option value to select"},
			},
			Required: []string{"value"},
		},
	}
}

func BrowserSelectOptionHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cdpClient, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		value, err := req.RequireString("value")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := SelectOption(ctx, cdpClient, nodeID, value); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("select: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("selected value=%s on node_id=%d", value, nodeID)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_get_text
// ---------------------------------------------------------------------------

func BrowserGetTextTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_get_text",
		Description: "Read the innerText of an element identified by node_id or ref. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{"type": "integer", "description": "node_id from browser_snapshot"},
				"ref":    map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
			},
			Required: []string{},
		},
	}
}

func BrowserGetTextHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cdpClient, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		text, err := GetText(ctx, cdpClient, nodeID)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("get_text: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(text), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_get_attribute
// ---------------------------------------------------------------------------

func BrowserGetAttributeTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_get_attribute",
		Description: "Read all attributes of an element as {name: value} pairs. Element identified by node_id or ref; when both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{"type": "integer", "description": "node_id from browser_snapshot"},
				"ref":    map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
			},
			Required: []string{},
		},
	}
}

func BrowserGetAttributeHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cdpClient, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		attrs, err := GetAttributes(ctx, cdpClient, nodeID)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("get_attribute: not applied — %v", err)), nil
		}
		b, _ := json.Marshal(attrs)
		return mcptypes.NewToolResultText(string(b)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_wait_for_element
// ---------------------------------------------------------------------------

func BrowserWaitForElementTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_wait_for_element",
		Description: "Wait for an element matching a CSS selector to appear (or disappear). Polls every 300ms.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"selector":  map[string]any{"type": "string", "description": "CSS selector to query"},
				"timeoutMs": map[string]any{"type": "integer", "description": "max wait in ms (default 10000)"},
				"state":     map[string]any{"type": "string", "description": "'visible' (wait for present) or 'hidden' (wait for absent). Default 'visible'."},
			},
			Required: []string{"selector"},
		},
	}
}

func BrowserWaitForElementHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		selector, err := req.RequireString("selector")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		timeoutMs, _ := req.RequireInt("timeoutMs")
		if timeoutMs == 0 {
			timeoutMs = 10000
		}
		state, _ := req.RequireString("state")
		wantVisible := state != "hidden"
		found, err := WaitForElement(ctx, cdpClient, selector, time.Duration(timeoutMs)*time.Millisecond, wantVisible)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("wait: not applied — %v", err)), nil
		}
		if found {
			return mcptypes.NewToolResultText(fmt.Sprintf("element %q reached state %s", selector, ternary(wantVisible, "visible", "hidden"))), nil
		}
		return mcptypes.NewToolResultError(fmt.Sprintf("timeout waiting for element %q", selector)), nil
	}
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Tool: browser_go_back / browser_go_forward / browser_reload / browser_set_checked
// ---------------------------------------------------------------------------

func BrowserGoBackTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_go_back",
		Description: "Navigate to the previous page in browser history.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}},
	}
}

func BrowserGoBackHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		if err := GoBack(ctx, cdpClient); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("go_back: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText("navigated back"), nil
	}
}

func BrowserGoForwardTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_go_forward",
		Description: "Navigate to the next page in browser history.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}},
	}
}

func BrowserGoForwardHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		if err := GoForward(ctx, cdpClient); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("go_forward: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText("navigated forward"), nil
	}
}

func BrowserReloadTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_reload",
		Description: "Reload the current page.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}},
	}
}

func BrowserReloadHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		if err := Reload(ctx, cdpClient); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("reload: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText("page reloaded"), nil
	}
}

func BrowserSetCheckedTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_set_checked",
		Description: "Check or uncheck a checkbox/radio element identified by node_id or ref. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId":  map[string]any{"type": "integer", "description": "node_id from browser_snapshot"},
				"ref":     map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
				"checked": map[string]any{"type": "boolean", "description": "true to check, false to uncheck"},
			},
			Required: []string{"checked"},
		},
	}
}

func BrowserSetCheckedHandler(cdpClient CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cdpClient, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		checkedStr, err := req.RequireString("checked")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		checked := checkedStr == "true"
		if err := SetChecked(ctx, cdpClient, nodeID, checked); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("set_checked: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("set node_id=%d checked=%v", nodeID, checked)), nil
	}
}

// ---------------------------------------------------------------------------
// Tools Batch 1: query / mouse / keyboard / wait / clipboard
// ---------------------------------------------------------------------------

func BrowserIsVisibleTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_is_visible", Description: "Check if an element matching a CSS selector is visible.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"selector": map[string]any{"type": "string", "description": "CSS selector"}}, Required: []string{"selector"}}}
}
func BrowserIsVisibleHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		sel, err := req.RequireString("selector")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		v, err := IsVisible(ctx, cd, sel)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("visible=%v", v)), nil
	}
}

func BrowserIsEnabledTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_is_enabled", Description: "Check if an element is enabled (not disabled).",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"nodeId": map[string]any{"type": "integer", "description": "node_id"}}, Required: []string{"nodeId"}}}
}
func BrowserIsEnabledHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nid, err := req.RequireInt("nodeId")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		v, err := IsEnabled(ctx, cd, nid)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("enabled=%v", v)), nil
	}
}

func BrowserCountTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_count", Description: "Count elements matching a CSS selector.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"selector": map[string]any{"type": "string", "description": "CSS selector"}}, Required: []string{"selector"}}}
}
func BrowserCountHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		sel, err := req.RequireString("selector")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		n, err := Count(ctx, cd, sel)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("count=%d", n)), nil
	}
}

func BrowserDoubleClickTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_double_click", Description: "Double-click an element identified by node_id or ref. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"nodeId": map[string]any{"type": "integer", "description": "node_id"}, "ref": map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"}}, Required: []string{}}}
}
func BrowserDoubleClickHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nid, err := resolveNodeIDFromRef(ctx, cd, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := DoubleClick(ctx, cd, nid); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("double-clicked node_id=%d", nid)), nil
	}
}

func BrowserHoverTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_hover", Description: "Hover (move mouse to) an element without clicking. Element identified by node_id or ref; when both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"nodeId": map[string]any{"type": "integer", "description": "node_id"}, "ref": map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"}}, Required: []string{}}}
}
func BrowserHoverHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nid, err := resolveNodeIDFromRef(ctx, cd, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := Hover(ctx, cd, nid); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("hovered node_id=%d", nid)), nil
	}
}

func BrowserPressKeyTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_press_key", Description: "Press a named key (e.g. Enter, Escape) on the currently focused element.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"key": map[string]any{"type": "string", "description": "key name (Enter, Escape, Tab, etc.)"}}, Required: []string{"key"}}}
}
func BrowserPressKeyHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		key, err := req.RequireString("key")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := PressKey(ctx, cd, key); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("pressed %s", key)), nil
	}
}

func BrowserWaitForURLTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_wait_for_url", Description: "Wait for the page URL to contain a substring.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"url": map[string]any{"type": "string", "description": "URL substring to match"}, "timeoutMs": map[string]any{"type": "integer", "description": "timeout in ms (default 10000)"}}, Required: []string{"url"}}}
}
func BrowserWaitForURLHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		urlSub, err := req.RequireString("url")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		ms, _ := req.RequireInt("timeoutMs")
		if ms == 0 {
			ms = 10000
		}
		ok, err := WaitForURL(ctx, cd, urlSub, time.Duration(ms)*time.Millisecond)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if ok {
			return mcptypes.NewToolResultText(fmt.Sprintf("URL matched %q", urlSub)), nil
		}
		return mcptypes.NewToolResultError("timeout waiting for URL"), nil
	}
}

func BrowserWaitForTimeoutTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_wait_for_timeout", Description: "Sleep/wait for a fixed duration.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"ms": map[string]any{"type": "integer", "description": "milliseconds to wait"}}, Required: []string{"ms"}}}
}
func BrowserWaitForTimeoutHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		ms, err := req.RequireInt("ms")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := WaitForTimeout(ctx, time.Duration(ms)*time.Millisecond); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("waited %dms", ms)), nil
	}
}

func BrowserClipboardTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_clipboard", Description: "Read or write the system clipboard.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"action": map[string]any{"type": "string", "description": "'read' or 'write'"}, "text": map[string]any{"type": "string", "description": "text to write (required for write)"}}, Required: []string{"action"}}}
}
func BrowserClipboardHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		action, err := req.RequireString("action")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if action == "write" {
			text, err := req.RequireString("text")
			if err != nil {
				return mcptypes.NewToolResultError(err.Error()), nil
			}
			if err := ClipboardWriteText(ctx, cd, text); err != nil {
				return mcptypes.NewToolResultError(err.Error()), nil
			}
			return mcptypes.NewToolResultText("clipboard written"), nil
		}
		text, err := ClipboardReadText(ctx, cd)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(text), nil
	}
}

func BrowserConsoleLogsTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_console_logs", Description: "Read recent browser console log messages.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{"limit": map[string]any{"type": "integer", "description": "max messages to return (default 50)"}}}}
}
func BrowserConsoleLogsHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		limit, _ := req.RequireInt("limit")
		logs, err := ConsoleLogs(ctx, cd, limit)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(logs), nil
	}
}

// ---------------------------------------------------------------------------
// Tools Batch 2: tab info / drag / file upload / download
// ---------------------------------------------------------------------------

func BrowserGetURLTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_get_url", Description: "Get the current page URL.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}}}
}
func BrowserGetURLHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		url, err := GetURL(ctx, cd)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(url), nil
	}
}

func BrowserGetTitleTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_get_title", Description: "Get the current page title.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}}}
}
func BrowserGetTitleHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		title, err := GetTitle(ctx, cd)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(title), nil
	}
}

func BrowserNewTabTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_new_tab", Description: "Open a new browser tab with the given URL. 若已有同 origin tab，优先 browser_switch_tab + browser_navigate 复用，避免开多个同站点 tab（handler 会做同 origin 软去重提示）。PROHIBITED: do NOT pass a guessed 'new document' creation URL (e.g. https://docs.qq.com/sheet/new or any /new-style URL with no referrer/origin) — that is unauthorized direct access and the site rejects it. To create a new document, click the page UI element (e.g. the template card); the opened tab will be grouped automatically and you can follow it with browser_list_tabs + browser_switch_tab. Only pass a known, complete URL obtained from the page itself.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"url":   map[string]any{"type": "string", "description": "URL to open"},
			"force": map[string]any{"type": "boolean", "description": "设 true 强制新开，绕过同 origin 复用提示。默认 false：命中同 origin tab 时返回复用提示而非新建。", "default": false},
		}, Required: []string{"url"}}}
}
func BrowserNewTabHandler(cd CDPClient, router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		urlParam, err := req.RequireString("url")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		force := req.GetBool("force", false)
		// D2 / C item (C2 soft dedup): before creating a new tab, query the
		// extension's managed tab list and compare origins. If a same-origin
		// managed tab already exists, do NOT create a new tab — return a soft
		// (isError=false) hint guiding the model to browser_switch_tab +
		// browser_navigate to reuse. The model retains agency: pass force=true
		// (or just call again — but the handler is stateless, so force is the
		// explicit escape hatch) to skip dedup and force-open.
		if !force {
			if hint, ok := findSameOriginManagedTab(ctx, router, urlParam); ok {
				return mcptypes.NewToolResultText(hint), nil
			}
		}
		// Route through extension's tabs.create handler (not window.open) so the
		// extension's tab-grouping logic fires (chrome.tabs.group + tabGroups.update).
		params, _ := json.Marshal(map[string]any{"url": urlParam})
		resp, err := router.SendRequest(ctx, "tabs.create", params)
		if err != nil {
			// Fallback: if JSON-RPC to extension fails, use CDP window.open
			if fallbackErr := NewTab(ctx, cd, urlParam); fallbackErr != nil {
				return mcptypes.NewToolResultError(fallbackErr.Error()), nil
			}
			return mcptypes.NewToolResultText(fmt.Sprintf("opened new tab: %s", urlParam)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.create error: %v", resp.Error)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("opened new tab: %s", urlParam)), nil
	}
}

// findSameOriginManagedTab queries tabs.list for managed tabs and returns a
// (hint, true) soft-dedup message when an existing tab shares the target
// URL's origin (scheme+host+port). Returns ("", false) when no same-origin tab
// exists, tabs.list is unavailable, or the target URL is unparseable (in which
// cases the handler falls through to the normal create path). The hint is a
// non-error ToolResult text per D2.
func findSameOriginManagedTab(ctx context.Context, router *jsonrpc.Router, targetURL string) (string, bool) {
	target, err := url.Parse(targetURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return "", false // unparseable / non-absolute → don't dedup
	}
	resp, err := router.SendRequest(ctx, "tabs.list", nil)
	if err != nil || resp == nil || resp.Error != nil {
		return "", false // tabs.list unavailable → don't block create
	}
	var tabs []struct {
		ID  int    `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(resp.Result, &tabs); err != nil {
		return "", false
	}
	for _, t := range tabs {
		existing, err := url.Parse(t.URL)
		if err != nil || existing.Scheme == "" || existing.Host == "" {
			continue
		}
		if sameOrigin(target, existing) {
			return fmt.Sprintf(
				"same-origin tab already open: tabId=%d, url=%s; prefer browser_switch_tab(%d) + browser_navigate to reuse, or call browser_new_tab again with force=true to force open",
				t.ID, t.URL, t.ID,
			), true
		}
	}
	return "", false
}

// sameOrigin reports whether two URLs share origin (scheme+host+port). Port is
// elided when it's the scheme default (http=80, https=443) so that
// https://example.com and https://example.com:443 match.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		normalizedPort(a) == normalizedPort(b)
}

func normalizedPort(u *url.URL) string {
	if u.Port() == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		if u.Port() == "80" {
			return ""
		}
	case "https":
		if u.Port() == "443" {
			return ""
		}
	}
	return u.Port()
}

func BrowserCloseTabTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_close_tab", Description: "Close the current tab (only works if opened by script).",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}}}
}
func BrowserCloseTabHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		if err := CloseTab(ctx, cd); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText("tab closed"), nil
	}
}

// BrowserListTabsTool — enumerate all browser tabs (Drift-H).
//
// Real-machine motivation: clicking a card that opens a new tab via
// window.open / target=_blank (Tencent Docs "空白文档" template) leaves the
// bridge attached to the OLD tab. Without listing tabs the AI cannot discover
// the newly opened editor tab, misreads "click had no effect", and falls back to
// guessing a /new URL (case docs-qq-new-sheet.md §卡点). This tool lets the AI
// detect and then browser_switch_tab to the new tab. Routed through the
// extension's tabs.list handler (chrome.tabs.query) — same channel as
// tabs.create, so the extension's tab-grouping view of the world is what's
// returned.
func BrowserListTabsTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_list_tabs",
		Description: "List all open browser tabs (id, index, url, title, active, status, enrolled). Each tab is marked [controlled] (enrolled in AI control group) or [unmanaged—adopt first] (not enrolled). Use after an action that may have opened a new tab (e.g. clicking a 'blank document' template card that window.opens an editor) to discover the new tab. If the new tab is marked [controlled] (auto-grouped), you may switch/select directly. If marked [unmanaged—adopt first], call browser_adopt_tab first, then switch/select/close. Do NOT guess a /new URL and navigate — always click the page UI and verify via this tool.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{}}}
}
func BrowserListTabsHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		resp, err := router.SendRequest(ctx, "tabs.list", nil)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.list: not applied — %v", err)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.list error: %v", resp.Error)), nil
		}
		// Pretty-print the tab list compactly (one line per tab) so the AI can
		// scan it without re-parsing a giant JSON blob.
		var tabs []struct {
			ID       int    `json:"id"`
			Index    int    `json:"index"`
			WindowID int    `json:"windowId"`
			URL      string `json:"url"`
			Title    string `json:"title"`
			Active   bool   `json:"active"`
			Status   string `json:"status"`
			Enrolled bool   `json:"enrolled"`
		}
		if err := json.Unmarshal(resp.Result, &tabs); err != nil {
			// Fall back to raw JSON if the shape ever changes upstream.
			return mcptypes.NewToolResultText(string(resp.Result)), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d tab(s):\n", len(tabs))
		for _, t := range tabs {
			mark := " "
			if t.Active {
				mark = "*"
			}
			enroll := "[unmanaged—adopt first]"
			if t.Enrolled {
				enroll = "[controlled]"
			}
			fmt.Fprintf(&b, "%s [%d] id=%d url=%s title=%q status=%s %s\n", mark, t.Index, t.ID, t.URL, t.Title, t.Status, enroll)
		}
		return mcptypes.NewToolResultText(b.String()), nil
	}
}

// BrowserSwitchTabTool — activate (focus) AND select for CDP control a tab by
// id (Drift-H / D5, 2026-06-24). Pairs with browser_list_tabs to follow a
// window.open'd editor tab. Routed through the extension's tabs.switch handler
// (chrome.tabs.update active:true), which now also retargets the bridge's CDP
// debugger to the switched tab — snapshot after switching to confirm the URL
// matches expectations. Use browser_select_tab instead when you only need to
// control a background tab without stealing focus.
func BrowserSwitchTabTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_switch_tab",
		Description: "Activate (focus) AND select for CDP control a browser tab by id, as returned by browser_list_tabs. Brings the tab to the front and re-targets CDP operations to it. Use to follow a new tab opened by clicking a page element (e.g. a template card that window.opens an editor). If browser_list_tabs marks the tab [unmanaged—adopt first] (not enrolled, e.g. a window.open'd tab that escaped auto-grouping), call browser_adopt_tab first — switch_tab on an unmanaged tab errors with 'tab not associated'. Use browser_select_tab instead when you only need to control a background tab without stealing focus.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"tabId": map[string]any{"type": "integer", "description": "tab id to activate (from browser_list_tabs)"},
		}, Required: []string{"tabId"}}}
}
func BrowserSwitchTabHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		tabID, err := req.RequireInt("tabId")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		params, _ := json.Marshal(map[string]any{"tabId": tabID})
		resp, err := router.SendRequest(ctx, "tabs.switch", params)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.switch: not applied — %v", err)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.switch error: %v", resp.Error)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("switched to tab %d (call browser_snapshot to confirm the page)", tabID)), nil
	}
}

// BrowserSelectTabTool selects a tab as the CDP control target WITHOUT
// focusing it or stealing mouse focus — the Codex browser.user.claimTab
// equivalent. CDP works on background tabs, so the user can keep operating
// their current tab while the agent controls the selected one. Use
// browser_switch_tab when you want to both control AND bring a tab to front.
func BrowserSelectTabTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_select_tab",
		Description: "Select a browser tab by id as the target for subsequent CDP operations (snapshot, click, fill, etc.). Does NOT focus/activate the tab or steal mouse focus — CDP works on background tabs, so the user can keep operating their own tab while the agent controls the selected one. Use browser_list_tabs to get tab ids. If a tab is marked [unmanaged—adopt first] (not enrolled, e.g. a window.open'd tab that escaped auto-grouping), call browser_adopt_tab first — select_tab on an unmanaged tab errors with 'tab not associated'. Use browser_switch_tab instead when you want to both control and bring a tab to the front.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"tabId": map[string]any{"type": "integer", "description": "tab id to select as CDP target (from browser_list_tabs)"},
		}, Required: []string{"tabId"}}}
}
func BrowserSelectTabHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		tabID, err := req.RequireInt("tabId")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		params, _ := json.Marshal(map[string]any{"tabId": tabID})
		resp, err := router.SendRequest(ctx, "tabs.select_tab", params)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("select_tab: not applied — %v", err)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("select_tab error: %v", resp.Error)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("selected tab %d as CDP target (no focus change)", tabID)), nil
	}
}

// BrowserAdoptTabTool — enroll a user-opened tab into the MCP tab group and
// make it the CDP control target (control-safety-boundary: Tab Adoption
// Primitive). This is the SAFE path to "operate the page the user already has
// open": the agent never auto-grabs the active tab (the old active-tab fallback
// that hijacked user tabs is removed). Instead it lists tabs, picks the one the
// user wants controlled, and adopts it. Adoption is a privileged action — the
// caller's tool-approval gate (e.g. Kairos "Requesting user approval") is the
// consent mechanism; the user approves before the tab is enrolled.
func BrowserAdoptTabTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_adopt_tab",
		Description: "Enroll an existing user-opened browser tab into the AI control group and make it the CDP target. Use this when the user wants the agent to operate a page they already have open (e.g. an OA form) — do NOT rely on the agent auto-targeting the active tab (that path is disabled for safety). Obtain the tabId from browser_list_tabs. Adoption requires user approval (the tool-call approval gate); only adopt a tab the user has directed you to.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"tabId": map[string]any{"type": "integer", "description": "tab id to adopt into the AI control group (from browser_list_tabs)"},
		}, Required: []string{"tabId"}}}
}
func BrowserAdoptTabHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		tabID, err := req.RequireInt("tabId")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		params, _ := json.Marshal(map[string]any{"tabId": tabID})
		resp, err := router.SendRequest(ctx, "tabs.adopt", params)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.adopt: not applied — %v", err)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("tabs.adopt error: %v", resp.Error)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("adopted tab %d into AI control group (now the CDP target; call browser_snapshot to confirm)", tabID)), nil
	}
}

func BrowserDragTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_drag", Description: "Drag the mouse along a path of coordinate points.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"points": map[string]any{"type": "array", "description": "array of {x, y} coordinate objects", "items": map[string]any{"type": "object"}},
		}, Required: []string{"points"}}}
}
func BrowserDragHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		pointsRaw, err := req.RequireStringSlice("points")
		if err != nil {
			// try parsing as JSON array of objects
			return mcptypes.NewToolResultError("points must be an array of {x,y} objects"), nil
		}
		var pts []Point
		for _, raw := range pointsRaw {
			var p Point
			if e := json.Unmarshal([]byte(raw), &p); e == nil {
				pts = append(pts, p)
			}
		}
		if len(pts) == 0 {
			return mcptypes.NewToolResultError("no valid points"), nil
		}
		if err := Drag(ctx, cd, pts); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("dragged through %d points", len(pts))), nil
	}
}

func BrowserFileUploadTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_file_upload", Description: "Upload files to a file input element.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"nodeId": map[string]any{"type": "integer", "description": "node_id of the <input type=file> element"},
			"files":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "file paths to upload"},
		}, Required: []string{"nodeId", "files"}}}
}
func BrowserFileUploadHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nid, err := req.RequireInt("nodeId")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		files, err := req.RequireStringSlice("files")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := FileUpload(ctx, cd, nid, files); err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("uploaded %d files to node_id=%d", len(files), nid)), nil
	}
}

func BrowserDownloadMediaTool() mcptypes.Tool {
	return mcptypes.Tool{Name: "browser_download_media", Description: "Download media (image/video) from a URL as base64.",
		InputSchema: mcptypes.ToolInputSchema{Type: "object", Properties: map[string]any{
			"url": map[string]any{"type": "string", "description": "URL of the media to download"},
		}, Required: []string{"url"}}}
}
func BrowserDownloadMediaHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		url, err := req.RequireString("url")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		data, err := DownloadMedia(ctx, cd, url)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		return mcptypes.NewToolResultText(data), nil
	}
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: CUA Coordinate Tools (Tier 3)
// Aligned with Codex cua_click/cua_double_click/cua_move/cua_scroll/cua_drag/cua_type/cua_keypress
// ---------------------------------------------------------------------------

func BrowserClickAtTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_click_at",
		Description: "Click at viewport coordinates (x, y). For Canvas/WebGL pages where no DOM elements exist. Tier 3 of the Codex hybrid approach: DOM→dom_cua→cua degradation. NOTE: raw-coordinate click performs no occlusion check and may pass through a visual overlay (pointer-events:none layer) to hit an element beneath; prefer browser_click with a ref/node_id (which checks occlusion). The result includes actual_hit (the element elementFromPoint resolved) for self-checking.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"x":          map[string]any{"type": "number", "description": "X coordinate (viewport pixels from left)"},
				"y":          map[string]any{"type": "number", "description": "Y coordinate (viewport pixels from top)"},
				"clickCount": map[string]any{"type": "integer", "description": "Number of clicks (default 1, use 2 for double-click)"},
			},
			Required: []string{"x", "y"},
		},
	}
}
func BrowserClickAtHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		x, err := req.RequireFloat("x")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		y, err := req.RequireFloat("y")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		clickCount := req.GetInt("clickCount", 1)
		if err := ClickAt(ctx, cd, x, y, clickCount); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("click_at: not applied — %v", err)), nil
		}
		// Raw-coordinate clicks perform no occlusion check (no target to compare).
		// Append the actual hit element so the agent can self-check whether the
		// click passed through a visual overlay to an element beneath (design D2;
		// memory click-occlusion-source-agent-browser). Non-fatal: a probe failure
		// does not turn a successful click into an error.
		msg := fmt.Sprintf("clicked at (%.0f, %.0f) count=%d", x, y, clickCount)
		if hit, hitErr := HitTestAt(ctx, cd, x, y); hitErr == nil && hit != "" {
			msg += fmt.Sprintf(" | actual_hit=%s", hit)
		}
		return mcptypes.NewToolResultText(msg), nil
	}
}

func BrowserDoubleClickAtTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_double_click_at",
		Description: "Double-click at viewport coordinates (x, y). Tier 3 CUA tool for Canvas pages.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"x": map[string]any{"type": "number", "description": "X coordinate"},
				"y": map[string]any{"type": "number", "description": "Y coordinate"},
			},
			Required: []string{"x", "y"},
		},
	}
}
func BrowserDoubleClickAtHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		x, err := req.RequireFloat("x")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		y, err := req.RequireFloat("y")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := DoubleClickAt(ctx, cd, x, y); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("double_click_at: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("double-clicked at (%.0f, %.0f)", x, y)), nil
	}
}

func BrowserMoveMouseTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_move_mouse",
		Description: "Move mouse to viewport coordinates (x, y) without clicking. Useful for hover previews. Aligned with Codex cua_move.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"x": map[string]any{"type": "number", "description": "X coordinate"},
				"y": map[string]any{"type": "number", "description": "Y coordinate"},
			},
			Required: []string{"x", "y"},
		},
	}
}
func BrowserMoveMouseHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		x, err := req.RequireFloat("x")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		y, err := req.RequireFloat("y")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := MoveMouse(ctx, cd, x, y); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("move_mouse: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("moved mouse to (%.0f, %.0f)", x, y)), nil
	}
}

func BrowserScrollAtTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_scroll_at",
		Description: "Scroll at a specific viewport coordinate. Aligned with Codex cua_scroll: Input.dispatchMouseEvent type=mouseWheel.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"x":      map[string]any{"type": "number", "description": "X coordinate to scroll at"},
				"y":      map[string]any{"type": "number", "description": "Y coordinate to scroll at"},
				"deltaX": map[string]any{"type": "integer", "description": "Horizontal scroll delta (positive=right)"},
				"deltaY": map[string]any{"type": "integer", "description": "Vertical scroll delta (positive=down)"},
			},
			Required: []string{"x", "y"},
		},
	}
}
func BrowserScrollAtHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		x, err := req.RequireFloat("x")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		y, err := req.RequireFloat("y")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		deltaX := req.GetInt("deltaX", 0)
		deltaY := req.GetInt("deltaY", 0)
		if err := ScrollAt(ctx, cd, x, y, deltaX, deltaY); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("scroll_at: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("scrolled at (%.0f, %.0f) delta=(%d,%d)", x, y, deltaX, deltaY)), nil
	}
}

func BrowserDragTool2() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_drag_path",
		Description: "Drag along a path of viewport coordinates. mouseMoved→mousePressed→move along path→mouseReleased. Aligned with Codex cua_drag.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"path": map[string]any{"type": "array", "description": "Array of {x, y} coordinate objects defining the drag path", "items": map[string]any{"type": "object"}},
			},
			Required: []string{"path"},
		},
	}
}
func BrowserDragPathHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		pathRaw := req.GetArguments()["path"]
		if pathRaw == nil {
			return mcptypes.NewToolResultError("missing path"), nil
		}
		pathJSON, err := json.Marshal(pathRaw)
		if err != nil {
			return mcptypes.NewToolResultError("invalid path"), nil
		}
		var path []Point
		if err := json.Unmarshal(pathJSON, &path); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("parse path: %v", err)), nil
		}
		if err := DragPath(ctx, cd, path); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("drag_path: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("dragged through %d points", len(path))), nil
	}
}

func BrowserTypeAtTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_type_at",
		Description: "Type text at the current focus position (coordinate-based). Call browser_click_at first to focus. Aligned with Codex cua_type: Input.insertText preferred, per-char fallback.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"text": map[string]any{"type": "string", "description": "Text to type"},
			},
			Required: []string{"text"},
		},
	}
}
func BrowserTypeAtHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		text, err := req.RequireString("text")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := TypeAt(ctx, cd, text); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("type_at: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("typed %d chars", len(text))), nil
	}
}

func BrowserPressKeyComboTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_press_key_combo",
		Description: "Press a key combination (e.g. [\"Control\",\"a\"] for Ctrl+A, [\"Enter\"], [\"Escape\"]). Aligned with Codex cua_keypress / KeyboardDispatcher: holds modifiers, presses final key, releases modifiers in reverse.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Key combination array, e.g. [\"Control\",\"a\"] or [\"Enter\"]"},
			},
			Required: []string{"keys"},
		},
	}
}
func BrowserPressKeyComboHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		keys, err := req.RequireStringSlice("keys")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := PressKeyCombo(ctx, cd, keys); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("press_key_combo: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("pressed %v", keys)), nil
	}
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: dom_cua Tools (Tier 2)
// Aligned with Codex dom_cua_get_visible_dom / dom_cua_click
// ---------------------------------------------------------------------------

func BrowserSnapshotVisibleTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_snapshot_visible",
		Description: "Get a filtered DOM snapshot showing only interactable elements in the viewport, with node_id attributes for dom_cua tier interaction. Aligned with Codex dom_cua_get_visible_dom.",
		InputSchema: mcptypes.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}
}
func BrowserSnapshotVisibleHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		result, err := GetVisibleDOM(ctx, cd)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("snapshot_visible: not applied — %v", err)), nil
		}
		// D4: size cap (shared with the other snapshot tools).
		return mcptypes.NewToolResultText(truncateSnapshot(result)), nil
	}
}

// ---------------------------------------------------------------------------
// AXTree Perception Layer (Tier 1.5, primary path)
// Aligned with agent-browser snapshot.rs: Accessibility.getFullAXTree →
// ref=eN compact tree with auto-degrade to GetVisibleDOM on sparse coverage.
// ---------------------------------------------------------------------------

// BrowserSnapshotAxTool is the AXTree-based primary perception tool. It emits
// a compact accessibility tree annotated with stable ref IDs (e1, e2, ...) the
// LLM can pass back to interaction tools. On AX failure or sparse coverage it
// transparently degrades to the DOM (GetVisibleDOM) and flags degraded=true.
func BrowserSnapshotAxTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name: "browser_snapshot_ax",
		Description: "AXTree snapshot (primary perception path). Returns a compact accessibility tree annotated with stable ref IDs (e1, e2, ...) usable by browser_click/fill/etc. via the optional 'ref' param. Auto-degrades to DOM (GetVisibleDOM) with degraded=true when the AX tree is sparse; callers should prefer the ref-based interaction tools when refs are present.\n\n" +
			"When the AX tree contains an `Iframe` node, the response also includes a `[DOM supplement]` block listing the iframe-internal interactive nodes with `node_id` and `text=` — AX often cannot give refs for unnamed generic cards inside iframes (e.g. template cards), so act on those node_ids directly with browser_click_node / browser_js_click instead of re-snapshotting.\n\n" +
			"SWITCH to the DOM path when ANY of these hold — do NOT keep re-running browser_snapshot_ax or retrying clicks when the target is invisible to AX:\n" +
			"  • The output contains an `Iframe` node but the target element is known to be inside that iframe/popup (AX now drills same-origin iframes, but cross-origin iframes are not expandable — the iframe line will carry a trailing note when unexpanded).\n" +
			"  • You clicked a button that should open a popup/menu, but the popup's contents do not appear in the AX tree (AX did not see the newly-rendered subtree).\n" +
			"  • The target is a canvas-rendered cell (e.g. online spreadsheets): AX has no ref for individual cells, and the in-cell editor (contenteditable div) is NOT in the snapshot. To fill a cell, locate the formula-bar content box (the combobox/textbox adjacent to the address box, showing the current cell's value) and browser_fill it — its value syncs to the selected cell. If unreachable, switch to browser_snapshot_visible or browser_evaluate_js.\n" +
			"If the output lacks an interactable element needed for the current task (but did not auto-degrade), explicitly call browser_snapshot_visible (or browser_snapshot for the full DOM) to retry via the DOM path — the system does not auto-degrade on coverage judgments.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"mode": map[string]any{
					"type":        "string",
					"description": "Render mode: \"compact\" (default; interactive refs + value-bearing lines + ancestors) or \"full\" (entire AX tree — requires explicit opt-in to avoid bloating context).",
				},
				"full": map[string]any{
					"type":        "boolean",
					"description": "When true, render the full AX tree (alias for mode:\"full\"). Default false (compact). Full mode is an explicit opt-in: it emits non-interactive node noise (~163KB vs compact ~117KB).",
				},
				"force": map[string]any{
					"type":        "boolean",
					"description": "When true, skip the dedup 'unchanged' short-circuit and always return the full snapshot. Default false.",
				},
			},
		},
	}
}

// BrowserSnapshotAxHandler runs the AX pipeline and owns the DOM degrade
// decision (GetVisibleDOM lives in mcp, out of axtree's reach). When
// GetAXSnapshot returns a DegradedReason, it falls back to GetVisibleDOM and
// reports degraded=true so the LLM knows refs are unavailable.
func BrowserSnapshotAxHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		// D6: full mode is an explicit opt-in. mode:"full" OR full:true selects
		// the full AX tree; otherwise compact (default). This prevents the model
		// from accidentally emitting the ~163KB full tree when compact (~117KB)
		// suffices.
		mode := req.GetString("mode", "compact")
		if req.GetBool("full", false) {
			mode = "full"
		}
		force := req.GetBool("force", false)
		res, err := axtree.GetAXSnapshot(ctx, cd, axtree.DefaultRefMap, axtree.SnapshotOpts{Mode: mode})
		if err != nil {
			// AX itself errored -> degrade to DOM.
			return degradeToDOM(ctx, cd, fmt.Sprintf("ax error: %v", err))
		}
		if res.DegradedReason != "" {
			return degradeToDOM(ctx, cd, res.DegradedReason)
		}
		// DN (2026-06-23, broadened 2026-06-24): fire the switch-to-DOM hint
		// whenever the main-frame AX tree contains ANY Iframe node — not only
		// when drilling fails. Even when DL drills a same-origin iframe
		// successfully, iframe-internal interactive elements are often unnamed
		// generic divs (e.g. docs.qq.com template cards "空白表格") that the
		// AX whitelist filters out, so refs are unavailable for them.
		//
		// DQ1 (2026-06-24): proactive dual-emit — when the AX tree contains an
		// Iframe, ALSO fetch DOM.getDocument(pierce:true) and append the
		// iframe-internal interactive nodes (text=/node_id) to THIS response.
		// The LLM gets the DOM node_id for unnamed iframe cards without
		// deciding to switch perception paths — removes the "AX non-empty so
		// don't switch" failure mode that DN's note alone couldn't break.
		text := res.Text
		if res.IframesPresent > 0 {
			sup := iframeDOMSupplement(ctx, cd)
			if sup != "" {
				text += "\n[DOM supplement — iframe-internal interactive nodes (AX gives no ref for these; use the node_id below with browser_click_node / browser_js_click, do not retry via AX):]\n" + sup
			}
			text += fmt.Sprintf(
				"\n[note: %d iframe(s) present; AX may not expose iframe-internal elements (unnamed cards/divs) — if a DOM supplement is not shown above or the target is still missing, retry with browser_snapshot_visible (DOM, viewport-filtered, compact — Codex dom_cua_get_visible_dom aligned) first; only fall back to browser_snapshot (full DOM pierce) if the target is off-screen or you need the whole DOM]",
				res.IframesPresent,
			)
		}

		// D1/D2: dedup hash gating — compute the structural signature from the
		// snapshot text BEFORE the axStuck directive is prepended, so the
		// signature stays stable regardless of the directive (the directive
		// would otherwise change the text and break dedup matching). Reuses
		// the existing axSignature (url+node count+iframe count). force:true
		// skips dedup and always returns the full payload.
		sig := axSignature(text, res)
		if !force {
			if unchanged, msg := dedupUnchanged("ax", sig, res.InteractiveRefs); unchanged {
				return mcptypes.NewToolResultText(msg), nil
			}
		} else {
			dedupReset("ax")
		}

		// DQ2 (2026-06-24): count-based hard-switch guardrail. If consecutive
		// AX snapshots show no structural change (same url+node count+iframe
		// count), the LLM is re-snapshotting the same page expecting different
		// output — prepend a hard switch directive. B (dual-emit above) is the
		// primary fix for "can't see target"; A is the guardrail for "stuck
		// looping on the same non-progressing snapshot". Computed AFTER the
		// dedup signature so it cannot perturb the hash.
		if directive := axStuckDirective(sig); directive != "" {
			text = directive + "\n" + text
		}

		// D4: size cap (shared with the other snapshot tools).
		return mcptypes.NewToolResultText(truncateSnapshot(text)), nil
	}
}

// iframeDOMSupplement fetches DOM.getDocument(depth:-1,pierce:true) and returns
// a filtered listing of ONLY iframe-internal interactive nodes (via
// snapshot.FilterIframeDOM) — the AX perception gap. Returns "" on fetch/parse
// failure or when no iframe-internal interactives exist, so the AX tree is
// still returned intact (catch{} semantics). (DQ1, 2026-06-24.)
func iframeDOMSupplement(ctx context.Context, cd CDPClient) string {
	params := mustMarshal(struct {
		Depth  int  `json:"depth"`
		Pierce bool `json:"pierce"`
	}{-1, true})
	resp, err := cd.Call(ctx, "DOM.getDocument", params)
	if err != nil || resp == nil || resp.Error != nil {
		return ""
	}
	var doc cdp.DOMDocument
	if err := json.Unmarshal(resp.Result, &doc); err != nil {
		return ""
	}
	if doc.Root == nil {
		return ""
	}
	sup := snapshot.FilterIframeDOM(doc.Root)
	if len(sup.Lines) == 0 {
		return ""
	}
	return sup.String()
}

// axStuck tracks consecutive AX snapshots whose structural signature is
// unchanged — a proxy for "LLM is looping on the same page without progress".
// Single attached-tab deployment model (CLAUDE.md MCP concurrency limits),
// so a single counter suffices; multi-tab is an accepted approximation for
// this guardrail. (DQ2, 2026-06-24.)
var axStuck struct {
	sync.Mutex
	sig string
	n   int
}

// axStuckThreshold is the consecutive unchanged-snapshot count that trips the
// hard-switch directive. N=2: two unchanged AX snapshots in a row means the
// LLM is almost certainly re-snapshotting a stable page expecting new output.
const axStuckThreshold = 2

// axSignature builds a coarse structural fingerprint of an AX snapshot: the
// URL line (if present), the rendered node count, and the iframe count. Used
// only to detect "no structural change between snapshots" — not for equality
// of content.
func axSignature(text string, res axtree.AXResult) string {
	url := ""
	for _, ln := range strings.Split(text, "\n") {
		if i := strings.Index(ln, "url="); i >= 0 {
			url = strings.TrimSpace(ln[i:])
			break
		}
	}
	return fmt.Sprintf("url=%s|nodes=%d|iframes=%d", url, strings.Count(text, "\n"), res.IframesPresent)
}

// axStuckDirective updates the stuck counter against the given signature and
// returns a hard-switch directive when the threshold is exceeded, else "".
// A changed signature resets the counter to 0 (new page / popup opened).
func axStuckDirective(sig string) string {
	axStuck.Lock()
	defer axStuck.Unlock()
	if sig == axStuck.sig {
		axStuck.n++
	} else {
		axStuck.sig = sig
		axStuck.n = 0
	}
	if axStuck.n >= axStuckThreshold {
		return fmt.Sprintf(
			"⚠ %d consecutive AX snapshots with no structural change — you appear to be re-snapshotting the same page. Switch to browser_snapshot_visible (DOM path) NOW and act on a node_id; do NOT call browser_snapshot_ax again until the page has actually changed.",
			axStuck.n,
		)
	}
	return ""
}

// degradeToDOM falls back to GetVisibleDOM and surfaces degraded=true plus the
// reason, so the LLM knows ref-based interaction is unavailable this snapshot.
func degradeToDOM(ctx context.Context, cd CDPClient, reason string) (*mcptypes.CallToolResult, error) {
	dom, err := GetVisibleDOM(ctx, cd)
	if err != nil {
		return mcptypes.NewToolResultError(fmt.Sprintf("AX degraded (%s) and DOM fallback: not applied — %v", reason, err)), nil
	}
	body := fmt.Sprintf("[degraded=true: %s]\n%s", reason, dom)
	return mcptypes.NewToolResultText(body), nil
}

func BrowserClickNodeTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_click_node",
		Description: "Click an element by node_id or ref via coordinate resolution (Tier 2 dom_cua). Resolves the node→viewport (x,y) then dispatches mouse events. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId":     map[string]any{"type": "integer", "description": "node_id from browser_snapshot_visible"},
				"ref":        map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
				"clickCount": map[string]any{"type": "integer", "description": "Number of clicks (default 1)"},
			},
			Required: []string{},
		},
	}
}
func BrowserClickNodeHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cd, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		clickCount := req.GetInt("clickCount", 1)
		if err := ClickNode(ctx, cd, nodeID, clickCount); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("click_node: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("clicked node_id=%d count=%d", nodeID, clickCount)), nil
	}
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: Enhanced JS Evaluation + Raw CDP (Tier 4)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tool: browser_js_click — Tier 4 React Synthetic Event Fallback
// ---------------------------------------------------------------------------

func BrowserJSClickTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_js_click",
		Description: "Click an element by node_id or ref using JavaScript .click() (Tier 4 React fallback). CDP Input.dispatchMouseEvent cannot trigger React synthetic onClick handlers — this tool resolves the node to a DOM element and calls .click() directly, which DOES trigger React events. Use as last resort when L1 browser_click / L2 browser_click_node / L3 browser_click_at all fail on React-based apps. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{"type": "integer", "description": "node_id (BackendNodeID) from browser_snapshot or browser_snapshot_visible"},
				"ref":    map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
			},
			Required: []string{},
		},
	}
}
func BrowserJSClickHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cd, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		// Probe url before/after so the AI can tell a no-op .click() from a
		// real navigation (Drift-I: JSClick reported only "React .click()
		// dispatched" with no outcome, so the AI could not distinguish a
		// successful sheet-page navigation from a silent no-op on a React
		// app — it retried indefinitely. This mirrors ClickWithEffect's url
		// probe without adding a fallback chain, preserving the last-resort
		// semantics of browser_js_click.)
		urlBefore, _ := GetURL(ctx, cd)
		if err := JSClick(ctx, cd, nodeID); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("js_click: not applied — %v", err)), nil
		}
		_, _ = waitForLoad(ctx, cd, clickOpTimeout)
		urlAfter, _ := GetURL(ctx, cd)
		effect := "none"
		if urlAfter != "" && urlAfter != urlBefore {
			effect = "navigation"
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("js_click node_id=%d (effect: %s; url: %s)", nodeID, effect, urlAfter)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_js_fill — Tier 4 React Controlled Input Fallback
// ---------------------------------------------------------------------------

func BrowserJSFillTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_js_fill",
		Description: "Fill an input element by node_id or ref using JavaScript value setter + event dispatch (Tier 4 React fallback). CDP Input.insertText may not trigger React onChange handlers on controlled inputs — this tool sets .value directly and dispatches native input/change events, which DOES trigger React re-renders. Use as last resort when L1 browser_fill / L2 coordinate type / L3 type_at all fail on React-controlled inputs. When both ref and nodeId are given, ref wins.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"nodeId": map[string]any{"type": "integer", "description": "node_id (BackendNodeID) from browser_snapshot or browser_snapshot_visible"},
				"ref":    map[string]any{"type": "string", "description": "ref (e.g. e1) from browser_snapshot_ax; preferred over nodeId when both given"},
				"value":  map[string]any{"type": "string", "description": "Value to fill into the input element"},
			},
			Required: []string{"value"},
		},
	}
}
func BrowserJSFillHandler(cd CDPClient) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		nodeID, err := resolveNodeIDFromRef(ctx, cd, req)
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		value, err := req.RequireString("value")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		if err := JSFill(ctx, cd, nodeID, value); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("js_fill: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(fmt.Sprintf("js_fill node_id=%d value=%q (React value setter + events dispatched)", nodeID, value)), nil
	}
}

func BrowserEvaluateJSTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_evaluate_js",
		Description: "Evaluate JavaScript in the page and return the result. Aligned with Codex Runtime.evaluate with awaitPromise support. Returns JSON-serialized result. PROHIBITED: do NOT use this to guess `/new`-style document-creation URLs or `/api/v1/.../create` endpoints (e.g. `fetch('/api/v1/sheet/create')`, `location.href='.../sheet/new'`) — sites reject no-referrer direct access and you will get stuck. To create a document, click the page UI element step by step.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"expression":   map[string]any{"type": "string", "description": "JavaScript expression to evaluate"},
				"awaitPromise": map[string]any{"type": "boolean", "description": "If true, await the result if it's a Promise (default false)"},
			},
			Required: []string{"expression"},
		},
	}
}
func BrowserEvaluateJSHandler(cd CDPClient, perms *config.PermissionStore) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		// CDP security check: JS evaluation may be disabled
		if perms != nil && !perms.Get().AllowEvalJS {
			return mcptypes.NewToolResultError("JS evaluation is disabled. Enable '执行 JS' in extension popup."), nil
		}
		expr, err := req.RequireString("expression")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		// DR① (2026-06-24, syncs DK into evaluate_js): refuse guessed /new-style
		// or /api/v1/.../create URLs. The DK rule already prohibits these in
		// browser_navigate / browser_new_tab; test7/8 show the LLM bypassing it
		// via `fetch('/api/v1/sheet/create')` / `location.href='.../sheet/new'`,
		// hitting non-existent endpoints and looping. evaluate_js must not be a
		// loophole. Guidance mirrors DK: click the page UI to create documents.
		if msg := detectGuessedCreateURL(expr); msg != "" {
			return mcptypes.NewToolResultError(msg), nil
		}
		awaitPromise := req.GetBool("awaitPromise", false)
		result, err := EvaluateJS(ctx, cd, expr, awaitPromise)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("evaluate_js: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(result), nil
	}
}

func BrowserCDPCallTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_cdp_call",
		Description: "Send a raw CDP command and return the result. Aligned with Codex tab_cdp_call. For advanced use when standard tools don't cover the needed CDP method.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"method": map[string]any{"type": "string", "description": "CDP method name (e.g. 'Page.captureScreenshot', 'DOM.getDocument')"},
				"params": map[string]any{"type": "object", "description": "CDP method parameters as JSON object"},
			},
			Required: []string{"method"},
		},
	}
}
func BrowserCDPCallHandler(cd CDPClient, perms *config.PermissionStore) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		// CDP security check: raw CDP calls may be disabled
		if perms != nil && !perms.Get().AllowRawCDP {
			return mcptypes.NewToolResultError("CDP call is disabled. Enable 'CDP 调用' in extension popup."), nil
		}
		method, err := req.RequireString("method")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}
		var params json.RawMessage
		if p, ok := req.GetArguments()["params"]; ok {
			params, _ = json.Marshal(p)
		} else {
			params = json.RawMessage("{}")
		}
		// Fetch domain methods are also blocked via sendCDP even when raw CDP is allowed
		result, err := CDPCallWithPerms(ctx, cd, method, params, perms)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("cdp_call: not applied — %v", err)), nil
		}
		return mcptypes.NewToolResultText(result), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: browser_name_session — AI-controlled tab group naming
// ---------------------------------------------------------------------------

func BrowserNameSessionTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_name_session",
		Description: "Set the tab group title for AI-controlled browser tabs. The AI agent can use this to label the current browser session with a meaningful name and emoji (e.g. '📊 财务报表', '📝 OA 表单'). Aligned with Codex nameSession command.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "Tab group title (emoji + name, e.g. '📊 财务报表')",
				},
			},
			Required: []string{"title"},
		},
	}
}

func BrowserNameSessionHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		title, err := req.RequireString("title")
		if err != nil {
			return mcptypes.NewToolResultError(err.Error()), nil
		}

		params, _ := json.Marshal(map[string]any{"title": title})
		resp, err := router.SendRequest(ctx, "tabs.name_session", params)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("name_session: not applied — %v", err)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("name_session error: %v", resp.Error)), nil
		}

		return mcptypes.NewToolResultText(fmt.Sprintf("tab group renamed to %q", title)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: finish_session — end current browser control session
// ---------------------------------------------------------------------------

// BrowserFinishSessionTool returns the MCP tool definition for finish_session.
// It is the parameter-less counterpart of browser_name_session: the agent
// invokes it once the task is complete and before delivering results to the
// user, to signal the extension to drop the controlling breathing border and
// release CDP control for the current tab. It does NOT close the transport
// connection — the next operation will re-attach.
func BrowserFinishSessionTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "finish_session",
		Description: "End the current browser control session when the task is complete and you are about to deliver results to the user. Removes the controlling breathing border and ends current CDP control over the tab. Does NOT close the underlying connection — the next operation will re-attach. Call this once per completed task before handing back to the user.",
		InputSchema: mcptypes.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
			Required:   []string{},
		},
	}
}

// BrowserFinishSessionHandler forwards a "session.end" JSON-RPC request to the
// extension with nil params. Shape mirrors BrowserNameSessionHandler.
func BrowserFinishSessionHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		resp, err := router.SendRequest(ctx, "session.end", nil)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("finish_session: not applied — %v", err)), nil
		}
		if resp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("finish_session error: %v", resp.Error)), nil
		}

		return mcptypes.NewToolResultText("browser control session ended; indicator removed (connection preserved)"), nil
	}
}

// CDPCallWithPerms is like CDPCall but checks Fetch domain permission.
func CDPCallWithPerms(ctx context.Context, cd CDPClient, method string, params json.RawMessage, perms *config.PermissionStore) (string, error) {
	if perms != nil && !perms.Get().AllowFetchDomain {
		return "", fmt.Errorf("Fetch domain is disabled. Enable 'Fetch 域' in extension popup")
	}
	return CDPCall(ctx, cd, method, params)
}
