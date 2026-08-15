package mcp

import (
	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegistryEntry pairs a tool definition with its handler.
type RegistryEntry struct {
	Tool    mcptypes.Tool
	Handler mcpserver.ToolHandlerFunc
}

// ToolRegistry is an ordered list of all registered tools.
type ToolRegistry []RegistryEntry

// BuildRegistry constructs the full tool registry. Both the MCP server and the
// REST API layer share this single source of truth — no duplicate registration.
func BuildRegistry(router *jsonrpc.Router, perms *config.PermissionStore) ToolRegistry {
	cdpClient := NewRouterCDPClient(router)

	r := ToolRegistry{}
	add := func(tool mcptypes.Tool, h mcpserver.ToolHandlerFunc) {
		r = append(r, RegistryEntry{Tool: tool, Handler: h})
	}

	// --- 感知层 (read-only) ---
	add(BrowserSnapshotTool(), BrowserSnapshotHandler(router))
	add(BrowserSnapshotAxTool(), BrowserSnapshotAxHandler(cdpClient))
	add(BrowserSnapshotVisibleTool(), BrowserSnapshotVisibleHandler(cdpClient))
	add(BrowserScreenshotTool(), BrowserScreenshotHandler(cdpClient))
	add(BrowserGetTextTool(), BrowserGetTextHandler(cdpClient))
	add(BrowserGetAttributeTool(), BrowserGetAttributeHandler(cdpClient))
	add(BrowserGetURLTool(), BrowserGetURLHandler(cdpClient))
	add(BrowserGetTitleTool(), BrowserGetTitleHandler(cdpClient))
	add(BrowserIsVisibleTool(), BrowserIsVisibleHandler(cdpClient))
	add(BrowserIsEnabledTool(), BrowserIsEnabledHandler(cdpClient))
	add(BrowserCountTool(), BrowserCountHandler(cdpClient))

	// --- 操作层 L1 DOM ---
	add(BrowserClickTool(), BrowserClickHandler(cdpClient))
	add(BrowserDoubleClickTool(), BrowserDoubleClickHandler(cdpClient))
	add(BrowserFillTool(), BrowserFillHandler(cdpClient))
	add(BrowserTypeTool(), BrowserTypeHandler(cdpClient))
	add(BrowserPressKeyTool(), BrowserPressKeyHandler(cdpClient))
	add(BrowserSelectOptionTool(), BrowserSelectOptionHandler(cdpClient))
	add(BrowserSetCheckedTool(), BrowserSetCheckedHandler(cdpClient))
	add(BrowserScrollTool(), BrowserScrollHandler(cdpClient))
	add(BrowserHoverTool(), BrowserHoverHandler(cdpClient))
	add(BrowserDragTool(), BrowserDragHandler(cdpClient))
	add(BrowserFileUploadTool(), BrowserFileUploadHandler(cdpClient))

	// --- 操作层 L2 DomCUA ---
	add(BrowserClickNodeTool(), BrowserClickNodeHandler(cdpClient))

	// --- 操作层 L3 CUA 坐标 ---
	add(BrowserClickAtTool(), BrowserClickAtHandler(cdpClient))
	add(BrowserDoubleClickAtTool(), BrowserDoubleClickAtHandler(cdpClient))
	add(BrowserMoveMouseTool(), BrowserMoveMouseHandler(cdpClient))
	add(BrowserScrollAtTool(), BrowserScrollAtHandler(cdpClient))
	add(BrowserDragTool2(), BrowserDragPathHandler(cdpClient))
	add(BrowserTypeAtTool(), BrowserTypeAtHandler(cdpClient))
	add(BrowserPressKeyComboTool(), BrowserPressKeyComboHandler(cdpClient))

	// --- 操作层 L4 JS React 兜底 ---
	add(BrowserJSClickTool(), BrowserJSClickHandler(cdpClient))
	add(BrowserJSFillTool(), BrowserJSFillHandler(cdpClient))

	// --- 导航层 ---
	add(BrowserNavigateTool(), BrowserNavigateHandler(cdpClient))
	add(BrowserGoBackTool(), BrowserGoBackHandler(cdpClient))
	add(BrowserGoForwardTool(), BrowserGoForwardHandler(cdpClient))
	add(BrowserReloadTool(), BrowserReloadHandler(cdpClient))
	add(BrowserNewTabTool(), BrowserNewTabHandler(cdpClient, router))

	// --- Tab 管理 ---
	add(BrowserListTabsTool(), BrowserListTabsHandler(router))
	add(BrowserSwitchTabTool(), BrowserSwitchTabHandler(router))
	add(BrowserSelectTabTool(), BrowserSelectTabHandler(router))
	add(BrowserAdoptTabTool(), BrowserAdoptTabHandler(router))
	add(BrowserCloseTabTool(), BrowserCloseTabHandler(cdpClient))

	// --- 等待层 ---
	add(BrowserWaitForElementTool(), BrowserWaitForElementHandler(cdpClient))
	add(BrowserWaitForURLTool(), BrowserWaitForURLHandler(cdpClient))
	add(BrowserWaitForTimeoutTool(), BrowserWaitForTimeoutHandler(cdpClient))

	// --- 其他 ---
	add(BrowserClipboardTool(), BrowserClipboardHandler(cdpClient))
	add(BrowserConsoleLogsTool(), BrowserConsoleLogsHandler(cdpClient))
	add(BrowserDownloadMediaTool(), BrowserDownloadMediaHandler(cdpClient))
	add(BrowserEvaluateJSTool(), BrowserEvaluateJSHandler(cdpClient, perms))
	add(BrowserCDPCallTool(), BrowserCDPCallHandler(cdpClient, perms))
	add(BrowserNameSessionTool(), BrowserNameSessionHandler(router))
	add(BrowserFinishSessionTool(), BrowserFinishSessionHandler(router))

	return r
}
