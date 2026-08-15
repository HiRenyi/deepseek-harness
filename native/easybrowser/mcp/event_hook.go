package mcp

import (
	"context"
	"strings"
	"time"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcpserver "github.com/mark3labs/mcp-go/server"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// Event is one structured record of an MCP tool call or CDP command, consumed
// by the package-main eventSink (F6 C-foundation) for JSONL disk logging +
// in-memory subscribe. Fields are best-effort; summaries are short and MUST
// NOT carry PII (callers pass only coarse status like "ok"/"error"). Ts is
// stamped by the sink at write time if zero.
type Event struct {
	Ts            time.Time `json:"ts"`
	Type          string    `json:"type"` // "mcp" | "cdp"
	Method        string    `json:"method"`
	ParamsSummary string    `json:"params_summary,omitempty"`
	ResultSummary string    `json:"result_summary,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	SessionID     string    `json:"session_id,omitempty"`
	TabID         int       `json:"tab_id,omitempty"`
}

// EmitHook is set by package main (wired to eventSink.emit) so the mcp package
// can record structured events without importing main (which would be a cycle).
// nil = no-op (init failed or sink disabled) — callers MUST tolerate nil.
var EmitHook func(Event)

// emitEvent fans an Event out to the registered EmitHook (no-op when nil). This
// is the single entry point the mcp package uses; it NEVER panics.
func emitEvent(e Event) {
	if EmitHook != nil {
		EmitHook(e)
	}
}

// truncStr clips s to n runes (best-effort, byte-safe for ASCII summaries) to
// keep summaries short and PII-free. Used for error snippets only.
func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// firstErrorText extracts the first TextContent's Text from a CallToolResult's
// Content slice. Returns "" when the result is nil, has no Content, or no
// TextContent (e.g. only ImageContent) — callers fall back to the coarse
// "error" summary in that case. Used by wrapToolHandler so the events.jsonl
// IsError line carries a 60-char error snippet (enough to identify
// "unknown ref e72 — not in current snapshot") instead of opaque "error",
// making ref/click failures post-mortem diagnosable without ad-hoc logging.
func firstErrorText(res *mcptypes.CallToolResult) string {
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

// wrapToolHandler wraps an MCP tool handler with timing + a structured emit
// (Type="mcp", Method=<tool name>, DurationMs, short result summary). The
// summary is a coarse status ("ok"/"error"/"err:<snip>") — NEVER full params or
// results, to avoid PII/size bloat. Errors from the handler are still returned
// unchanged; the emit is purely observational.
func wrapToolHandler(name string, h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		// A4: in-flight 工具调用计数，供 /health in_use 判定。
		incToolCall()
		defer decToolCall()
		start := time.Now()
		res, err := h(ctx, req)
		e := Event{
			Type:       "mcp",
			Method:     name,
			DurationMs: time.Since(start).Milliseconds(),
		}
		// Detect "no tab attached" / "not in MCP control group" BEFORE building
		// the truncated ResultSummary: the raw error is a long cascade
		// ("AX degraded (...) ... no tab attached ...") whose 60-char truncation
		// loses the keyword. Translate to a short cause tag for the event +
		// replace the AI-facing res.Content with an actionable Chinese message.
		// The extension's resolveTargetTabId (background.ts:565) and
		// assertControllable (:583) throw these when the AI calls a CDP tool
		// before enrolling a tab (No-Active-Tab-Hijack security boundary —
		// design unchanged); the raw cascade is alarming and unactionable.
		noTab := false
		if err == nil && res != nil && res.IsError {
			if txt := firstErrorText(res); strings.Contains(txt, "no tab attached") || strings.Contains(txt, "not in MCP control group") {
				noTab = true
				e.ResultSummary = "err:no tab attached (→friendly)"
				res.Content = []mcptypes.Content{mcptypes.NewTextContent(noTabAttachedFriendlyMsg)}
			}
		}
		if !noTab {
			switch {
			case err != nil:
				e.ResultSummary = "err:" + truncStr(err.Error(), 60)
			case res != nil && res.IsError:
				if txt := firstErrorText(res); txt != "" {
					e.ResultSummary = "err:" + truncStr(txt, 60)
				} else {
					e.ResultSummary = "error"
				}
			case res != nil:
				e.ResultSummary = "ok"
			default:
				e.ResultSummary = "nil"
			}
		}
		emitEvent(e)
		return res, err
	}
}

// noTabAttachedFriendlyMsg is the actionable Chinese message shown to the AI
// when a CDP tool is called before any tab is enrolled (or after the enrolled
// tab was closed). The extension's No-Active-Tab-Hijack security boundary
// requires explicit enrollment via browser_new_tab / browser_adopt_tab — this
// message tells the AI exactly that, instead of the raw CDP error cascade.
const noTabAttachedFriendlyMsg = "尚未关联浏览器标签页——请先调用 browser_new_tab 打开新标签页，或 browser_adopt_tab 关联已有标签页，然后再试。"

// addTool registers a tool on the MCP server with its handler wrapped for
// structured event emission. This is the single registration path so every
// MCP tool call is observed by the F6 sink.
func addTool(s *mcpserver.MCPServer, tool mcptypes.Tool, h mcpserver.ToolHandlerFunc) {
	s.AddTool(tool, wrapToolHandler(tool.Name, h))
}

// cdpSendTiming wraps the observable parts of a CDP send (the JSON-RPC round
// trip) with timing + a structured emit (Type="cdp", Method=<CDP method>,
// DurationMs, short result summary). Called by sendCDP right around
// router.SendRequest. Returns the same (resp, err) it was passed so sendCDP
// stays a thin wrapper.
func cdpSendTiming(method string, resp *jsonrpc.Response, err error, start time.Time) (*jsonrpc.Response, error) {
	e := Event{
		Type:       "cdp",
		Method:     method,
		DurationMs: time.Since(start).Milliseconds(),
	}
	switch {
	case err != nil:
		e.ResultSummary = "err:" + truncStr(err.Error(), 60)
	case resp != nil && resp.Error != nil:
		// Coarse: don't dump the RPC error message (could echo params).
		e.ResultSummary = "rpc-error:" + truncStr(strings.TrimSpace(resp.Error.Message), 40)
	default:
		e.ResultSummary = "ok"
	}
	emitEvent(e)
	return resp, err
}
