package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	mcpbridge "github.com/browser-mcp/bridge/mcp"
	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// RESTHandler provides a thin REST API layer over the MCP tool registry.
// All 47 browser tools are exposed as POST /api/tool/{name} endpoints,
// plus GET /api/tools for discovery and POST /api/batch for sequential execution.
type RESTHandler struct {
	registry map[string]mcpbridge.RegistryEntry
	order    []string // tool names in registration order
}

// NewRESTHandler builds the REST handler from the shared tool registry.
func NewRESTHandler(router *jsonrpc.Router, perms *config.PermissionStore) *RESTHandler {
	reg := mcpbridge.BuildRegistry(router, perms)
	m := make(map[string]mcpbridge.RegistryEntry, len(reg))
	order := make([]string, 0, len(reg))
	for _, entry := range reg {
		m[entry.Tool.Name] = entry
		order = append(order, entry.Tool.Name)
	}
	return &RESTHandler{registry: m, order: order}
}

// toolResponse is the standard JSON envelope for single tool calls.
type toolResponse struct {
	OK      bool   `json:"ok"`
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// DurationMs reports handler execution time.
	DurationMs int64 `json:"duration_ms"`
}

// ServeHTTP handles POST /api/tool/{name}.
func (h *RESTHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "POST required"})
		return
	}

	// Extract tool name from path: /api/tool/browser_click → browser_click
	name := strings.TrimPrefix(r.URL.Path, "/api/tool/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing tool name in path"})
		return
	}

	entry, found := h.registry[name]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown tool: " + name})
		return
	}

	// Parse JSON body as tool arguments
	var args map[string]any
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid JSON body: " + err.Error()})
			return
		}
	}
	if args == nil {
		args = map[string]any{}
	}

	// Execute
	result := h.execute(r.Context(), entry, args)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}

// execute runs a single tool and returns the standard response envelope.
func (h *RESTHandler) execute(ctx context.Context, entry mcpbridge.RegistryEntry, args map[string]any) toolResponse {
	req := mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Arguments: args,
		},
	}

	// 30s timeout per tool call (matches bridge default CDP timeout)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	res, err := entry.Handler(ctx, req)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return toolResponse{OK: false, Error: err.Error(), DurationMs: elapsed}
	}
	if res == nil {
		return toolResponse{OK: true, Result: "", DurationMs: elapsed}
	}

	// Extract text content from result
	text := extractResultText(res)
	if res.IsError {
		return toolResponse{OK: true, Result: text, IsError: true, Error: text, DurationMs: elapsed}
	}
	return toolResponse{OK: true, Result: text, DurationMs: elapsed}
}

// extractResultText pulls text from CallToolResult content items.
func extractResultText(res *mcptypes.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcptypes.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// --- GET /api/tools ---

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

// ListTools handles GET /api/tools — returns all tool definitions.
func (h *RESTHandler) ListTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	tools := make([]toolInfo, 0, len(h.order))
	for _, name := range h.order {
		entry := h.registry[name]
		tools = append(tools, toolInfo{
			Name:        entry.Tool.Name,
			Description: entry.Tool.Description,
			InputSchema: entry.Tool.InputSchema,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"count": len(tools),
		"tools": tools,
	})
}

// --- POST /api/batch ---

type batchStep struct {
	Tool   string         `json:"tool"`
	Params map[string]any `json:"params"`
}

type batchRequest struct {
	Steps []batchStep `json:"steps"`
	// StopOnError controls whether execution halts on first error (default true).
	StopOnError *bool `json:"stop_on_error,omitempty"`
}

type batchStepResult struct {
	Step       int    `json:"step"`
	Tool       string `json:"tool"`
	OK         bool   `json:"ok"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// Batch handles POST /api/batch — sequential execution of multiple tool calls.
func (h *RESTHandler) Batch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "POST required"})
		return
	}

	var req batchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	if len(req.Steps) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "empty steps"})
		return
	}

	stopOnError := true
	if req.StopOnError != nil {
		stopOnError = *req.StopOnError
	}

	results := make([]batchStepResult, 0, len(req.Steps))
	allOK := true

	for i, step := range req.Steps {
		entry, found := h.registry[step.Tool]
		if !found {
			sr := batchStepResult{Step: i, Tool: step.Tool, OK: false, Error: "unknown tool: " + step.Tool}
			results = append(results, sr)
			allOK = false
			if stopOnError {
				break
			}
			continue
		}

		params := step.Params
		if params == nil {
			params = map[string]any{}
		}

		resp := h.execute(r.Context(), entry, params)
		sr := batchStepResult{
			Step:       i,
			Tool:       step.Tool,
			OK:         resp.OK,
			Result:     resp.Result,
			Error:      resp.Error,
			IsError:    resp.IsError,
			DurationMs: resp.DurationMs,
		}
		results = append(results, sr)

		if (!resp.OK || resp.IsError) && stopOnError {
			allOK = false
			break
		}
		if !resp.OK || resp.IsError {
			allOK = false
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"ok":      allOK,
		"results": results,
		"total":   len(req.Steps),
		"executed": len(results),
	})
}
