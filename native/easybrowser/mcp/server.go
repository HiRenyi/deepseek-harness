package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/browser-mcp/bridge/cdp"
	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/jsonrpc"
	"github.com/browser-mcp/bridge/snapshot"
	mcpserver "github.com/mark3labs/mcp-go/server"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// ServerVersion is the MCP server version reported to clients in the
// `initialize` handshake. Set at build time via ldflags
// `-X github.com/browser-mcp/bridge/mcp.ServerVersion=<ver>` (build.ps1 passes
// the same git-describe tag as main.version, so the MCP client sees the same
// version as `bridge --version` and the dashboard). Defaults to "dev" for
// unbuilt/`go run` sessions.
var ServerVersion = "dev"

// CDPExecuteParams is kept for reference / external callers that build the
// envelope by hand. Internal code uses sendCDP() which wraps this shape.
type CDPExecuteParams struct {
	TabID     *int            `json:"tabId,omitempty"`
	CDPMethod string          `json:"cdpMethod"`
	CDPParams json.RawMessage `json:"cdpParams,omitempty"`
}

// BrowserSnapshotTool returns the MCP tool definition for browser_snapshot.
func BrowserSnapshotTool() mcptypes.Tool {
	return mcptypes.Tool{
		Name:        "browser_snapshot",
		Description: "Capture a filtered DOM snapshot of the current browser tab, showing interactive elements and their attributes in a concise text format. By default the snapshot is viewport-scoped (only nodes in or partially intersecting the viewport) to keep the payload small; pass full:true for the complete DOM. Consecutive identical snapshots return a short 'unchanged' notice — pass force:true to re-fetch the full payload.",
		InputSchema: mcptypes.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"tabId": map[string]any{
					"type":        "string",
					"description": "The target tab ID (optional, uses active tab if omitted)",
				},
				"full": map[string]any{
					"type":        "boolean",
					"description": "When true, return the complete DOM (skip viewport culling). Default false (viewport-scoped, Codex browser_snapshot_visible aligned).",
				},
				"force": map[string]any{
					"type":        "boolean",
					"description": "When true, skip the dedup 'unchanged' short-circuit and always return the full snapshot. Default false.",
				},
			},
		},
	}
}

// cdpExecuteRequest is the JSON-RPC params envelope the Chrome extension
// expects: method "cdp.execute" with {tabId?, cdpMethod, cdpParams}. The
// extension's background.ts only responds to "cdp.execute" (it rejects any
// other method with -32601), so CDP methods must be wrapped, not sent raw.
type cdpExecuteRequest struct {
	TabID     *int            `json:"tabId,omitempty"` // nil => extension uses active tab
	CDPMethod string          `json:"cdpMethod"`
	CDPParams json.RawMessage `json:"cdpParams,omitempty"`
}

// sendCDP wraps a raw CDP method+params in a "cdp.execute" JSON-RPC request
// the extension understands and routes through chrome.debugger.sendCommand.
// Passing a nil tabId lets the extension target the active tab.
// If perms is non-nil, Fetch domain methods are blocked when AllowFetchDomain is false.
func sendCDP(ctx context.Context, router *jsonrpc.Router, method string, params json.RawMessage, perms ...*config.PermissionStore) (*jsonrpc.Response, error) {
	// Check Fetch domain permission if a PermissionStore is provided
	if len(perms) > 0 && perms[0] != nil {
		if strings.HasPrefix(method, "Fetch.") && !perms[0].Get().AllowFetchDomain {
			return nil, fmt.Errorf("Fetch domain is disabled. Enable 'Fetch 域' in extension popup")
		}
	}
	envelope, err := json.Marshal(cdpExecuteRequest{
		TabID:     nil,
		CDPMethod: method,
		CDPParams: params,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal cdp.execute envelope: %w", err)
	}
	start := time.Now()
	resp, sErr := router.SendRequest(ctx, "cdp.execute", envelope)
	return cdpSendTiming(method, resp, sErr, start)
}

// BrowserSnapshotHandler returns a handler function for the browser_snapshot tool.
// It sends two CDP requests (DOM.getDocument + Page.getLayoutMetrics) via the JSON-RPC router,
// parses the responses, filters the DOM, and returns line-text output.
//
// mcp-context-slimming:
//   - D3: full defaults to viewport-scoped (filterNode reads viewport via a
//     CDP-backed BoxResolver); full:true skips viewport culling.
//   - D1/D2: consecutive identical structural signatures return a short
//     "unchanged" notice; force:true skips dedup.
//   - D4: output is soft-capped at maxSnapshotLines/maxSnapshotBytes.
func BrowserSnapshotHandler(router *jsonrpc.Router) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, request mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		full := request.GetBool("full", false)
		force := request.GetBool("force", false)

		// 1. Send DOM.getDocument request
		domParams, _ := json.Marshal(map[string]any{
			"depth":  -1,
			"pierce": true,
		})
		domResp, err := sendCDP(ctx, router, "DOM.getDocument", domParams)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("DOM.getDocument: not applied — %v", err)), nil
		}
		if domResp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("DOM.getDocument error: %v", domResp.Error)), nil
		}

		// 2. Send Page.getLayoutMetrics request
		layoutParams, _ := json.Marshal(map[string]any{})
		layoutResp, err := sendCDP(ctx, router, "Page.getLayoutMetrics", layoutParams)
		if err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("Page.getLayoutMetrics: not applied — %v", err)), nil
		}
		if layoutResp.Error != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("Page.getLayoutMetrics error: %v", layoutResp.Error)), nil
		}

		// 3. Parse DOM document
		var domDoc cdp.DOMDocument
		if err := json.Unmarshal(domResp.Result, &domDoc); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("parse DOM response: %v", err)), nil
		}

		// 4. Parse layout metrics
		var layoutMetrics cdp.LayoutMetrics
		if err := json.Unmarshal(layoutResp.Result, &layoutMetrics); err != nil {
			return mcptypes.NewToolResultError(fmt.Sprintf("parse layout response: %v", err)), nil
		}

		// D1/D2: dedup hash gating — compute signature BEFORE the expensive
		// filter and short-circuit on a matching baseline (unless force).
		// The signature includes the document URL so a URL/tab change resets.
		url := ""
		if domDoc.Root != nil {
			url = domDoc.Root.DocumentURL
		}
		sig, nodeCount := domSignature(domDoc.Root, url)
		// Include the view mode in the signature so switching between
		// viewport-scoped and full does NOT short-circuit on the same page
		// (the model asked for a different view, so return it).
		viewMode := "viewport"
		if full {
			viewMode = "full"
		}
		sig = viewMode + "|" + sig
		if !force {
			if unchanged, msg := dedupUnchanged("dom", sig, nodeCount); unchanged {
				return mcptypes.NewToolResultText(msg), nil
			}
		} else {
			dedupReset("dom")
		}

		// 5. Determine viewport (D3: nil viewport when full:true => no culling).
		var viewport *cdp.ViewportRect
		if !full {
			if layoutMetrics.CSSVisualViewport != nil {
				vv := layoutMetrics.CSSVisualViewport
				viewport = &cdp.ViewportRect{
					X:      vv.PageX,
					Y:      vv.PageY,
					Width:  vv.ClientWidth,
					Height: vv.ClientHeight,
				}
			} else if layoutMetrics.CSSContentSize != nil {
				viewport = layoutMetrics.CSSContentSize
			}
		}

		// 6. Filter DOM — viewport-scoped by default (D3), full when full:true
		// or when no viewport could be resolved. The routerBoxResolver bridges
		// into CDP for DOM.getBoxModel per candidate node (Codex rU).
		var resolver snapshot.BoxResolver
		if viewport != nil {
			resolver = newRouterBoxResolver(router)
		}
		snap := snapshot.FilterDOM(ctx, domDoc.Root, viewport, resolver)

		// D4: size cap.
		out := truncateSnapshot(snap.String())
		return mcptypes.NewToolResultText(out), nil
	}
}

// routerBoxResolver adapts the JSON-RPC router into a snapshot.BoxResolver,
// issuing DOM.getBoxModel per BackendNodeID and returning the border quad's
// min/max x/y. ok=false on any CDP/parse error or short quad (Codex rU
// `catch{return null}` semantics). (mcp-context-slimming D3.)
type routerBoxResolver struct {
	router *jsonrpc.Router
}

func newRouterBoxResolver(router *jsonrpc.Router) *routerBoxResolver {
	return &routerBoxResolver{router: router}
}

func (r *routerBoxResolver) BoxBounds(ctx context.Context, backendNodeID int) (minX, minY, maxX, maxY float64, ok bool) {
	if backendNodeID <= 0 {
		return 0, 0, 0, 0, false
	}
	params, _ := json.Marshal(map[string]any{
		"backendNodeId": backendNodeID,
	})
	resp, err := sendCDP(ctx, r.router, "DOM.getBoxModel", params)
	if err != nil || resp == nil || resp.Error != nil {
		return 0, 0, 0, 0, false
	}
	var bm cdp.BoxModelResponse
	if err := json.Unmarshal(resp.Result, &bm); err != nil {
		return 0, 0, 0, 0, false
	}
	b := bm.Model.Border
	if len(b) < 8 {
		return 0, 0, 0, 0, false
	}
	minX = min4(b[0], b[2], b[4], b[6])
	maxX = max4(b[0], b[2], b[4], b[6])
	minY = min4(b[1], b[3], b[5], b[7])
	maxY = max4(b[1], b[3], b[5], b[7])
	return minX, minY, maxX, maxY, true
}

func min4(a, b, c, d float64) float64 {
	m := a
	for _, v := range []float64{b, c, d} {
		if v < m {
			m = v
		}
	}
	return m
}

func max4(a, b, c, d float64) float64 {
	m := a
	for _, v := range []float64{b, c, d} {
		if v > m {
			m = v
		}
	}
	return m
}

// NewMCPServer creates an MCP server with all browser tools registered.
// perms controls CDP permission checks; if nil, all permissions are allowed.
// Tools are sourced from BuildRegistry (shared with the REST API layer).
func NewMCPServer(router *jsonrpc.Router, perms *config.PermissionStore) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer(
		"browser-mcp",
		ServerVersion,
		mcpserver.WithToolCapabilities(false),
		// A4: 会话生命周期钩子驱动 activeSessions 计数，供 /health in_use。
		// session-tab-isolation: OnRegisterSession 发 session.start 让扩展释放旧 attachedTab。
		mcpserver.WithHooks(sessionHooks(router)),
	)

	for _, entry := range BuildRegistry(router, perms) {
		addTool(s, entry.Tool, entry.Handler)
	}

	return s
}

// NewSSEServer creates an SSE-based MCP server.
func NewSSEServer(mcpServer *mcpserver.MCPServer, opts ...mcpserver.SSEOption) *mcpserver.SSEServer {
	return mcpserver.NewSSEServer(mcpServer, opts...)
}

// NewStreamableHTTPServer creates a Streamable HTTP-based MCP server.
func NewStreamableHTTPServer(mcpServer *mcpserver.MCPServer, opts ...mcpserver.StreamableHTTPOption) *mcpserver.StreamableHTTPServer {
	return mcpserver.NewStreamableHTTPServer(mcpServer, opts...)
}

// WithSSECORS exposes mcpserver.WithSSECORS for CORS configuration.
func WithSSECORS(opts ...mcpserver.CORSOption) mcpserver.SSEOption {
	return mcpserver.WithSSECORS(opts...)
}

// WithStreamableHTTPCORS exposes mcpserver.WithStreamableHTTPCORS for CORS configuration.
func WithStreamableHTTPCORS(opts ...mcpserver.CORSOption) mcpserver.StreamableHTTPOption {
	return mcpserver.WithStreamableHTTPCORS(opts...)
}

// WithCORSAllowedOrigins exposes mcpserver.WithCORSAllowedOrigins.
func WithCORSAllowedOrigins(origins ...string) mcpserver.CORSOption {
	return mcpserver.WithCORSAllowedOrigins(origins...)
}
