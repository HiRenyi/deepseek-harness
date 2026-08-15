package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/config"
	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// evalRescueFake serves successive Runtime.evaluate outcomes per call. Each
// outcome is either a result payload (non-nil raw) or a CDP-level error (err
// non-nil, e.g. -32000 "Object reference chain is too long"). Used to exercise
// the DR② IIFE-rescue and DR③ returnByValue:false remote-retry paths.
type evalRescueFake struct {
	calls   int
	results []json.RawMessage // successive Runtime.evaluate result payloads
	errs    []*jsonrpc.ResponseError // successive CDP errors (parallel slice)
}

func (e *evalRescueFake) Call(_ context.Context, method string, _ json.RawMessage) (*jsonrpc.Response, error) {
	resp := &jsonrpc.Response{}
	if method != "Runtime.evaluate" {
		return resp, nil
	}
	idx := e.calls
	e.calls++
	if idx < len(e.errs) && e.errs[idx] != nil {
		resp.Error = e.errs[idx]
		return resp, nil
	}
	if idx < len(e.results) {
		resp.Result = e.results[idx]
	}
	return resp, nil
}

// TestEvaluateJSIIFERescueOnSyntaxError (DR②): a top-level `return` throws
// SyntaxError under raw Runtime.evaluate; EvaluateJS must retry the expression
// wrapped in `(()=>{ <expr> })()` and return the wrapped result.
func TestEvaluateJSIIFERescueOnSyntaxError(t *testing.T) {
	f := &evalRescueFake{
		// Call 1: raw expression "if(x){return 1}" -> SyntaxError.
		results: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"undefined"},"exceptionDetails":{"text":"SyntaxError: Illegal return statement"}}`),
			// Call 2: IIFE-wrapped retry -> returns 1.
			json.RawMessage(`{"result":{"type":"number","value":1}}`),
		},
	}
	got, err := EvaluateJS(context.Background(), f, "if(x){return 1}", false)
	if err != nil {
		t.Fatalf("EvaluateJS err: %v", err)
	}
	if got != "1" {
		t.Errorf("result=%q, want \"1\" (IIFE-rescued return)", got)
	}
	if f.calls != 2 {
		t.Errorf("calls=%d, want 2 (raw then IIFE retry)", f.calls)
	}
}

// TestEvaluateJSRawExpressionUnaffectedByDR2 (DR② non-regression): a bare
// expression that parses fine must NOT be IIFE-wrapped — single call, raw
// completion value returned. Ensures DR② is strictly a rescue path.
func TestEvaluateJSRawExpressionUnaffectedByDR2(t *testing.T) {
	f := &evalRescueFake{
		results: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"number","value":42}}`),
		},
	}
	got, err := EvaluateJS(context.Background(), f, "6*7", false)
	if err != nil {
		t.Fatalf("EvaluateJS err: %v", err)
	}
	if got != "42" {
		t.Errorf("result=%q, want 42", got)
	}
	if f.calls != 1 {
		t.Errorf("calls=%d, want 1 (no IIFE wrap for valid bare expression)", f.calls)
	}
}

// TestEvaluateJSRemoteRetryOnDOMReturn (DR③): returnByValue:true cannot
// serialize a live DOM element, failing with CDP -32000 "Object reference chain
// is too long". EvaluateJS must retry with returnByValue:false and surface the
// RemoteObject description instead of crashing.
func TestEvaluateJSRemoteRetryOnDOMReturn(t *testing.T) {
	f := &evalRescueFake{
		// Call 0: raw expression, returnByValue:true -> CDP -32000 (live DOM).
		errs: []*jsonrpc.ResponseError{
			{Code: -32000, Message: "Object reference chain is too long"},
			nil,
		},
		// Call 1: EvaluateJS retries with returnByValue:false (evaluateJSAsRemote)
		// and surfaces the RemoteObject description.
		results: []json.RawMessage{
			json.RawMessage(`{}`), // index 0, unused (errs[0] wins)
			json.RawMessage(`{"result":{"type":"object","description":"button.create-create-item","className":"Function"}}`),
		},
	}
	got, err := EvaluateJS(context.Background(), f, "document.querySelector('button')", false)
	if err != nil {
		t.Fatalf("EvaluateJS err: %v", err)
	}
	if !strings.Contains(got, "button.create-create-item") {
		t.Errorf("result=%q, want to contain the RemoteObject description", got)
	}
	if !strings.Contains(got, "JSON.stringify") {
		t.Errorf("result=%q, want a hint to JSON.stringify if properties are needed", got)
	}
}

// TestEvaluateJSDetectGuessedCreateURL (DR①): the DK prohibition on guessed
// /new-style and /api/v1/.../create URLs is synced into browser_evaluate_js.
// Each fragment must trip the guard.
func TestEvaluateJSDetectGuessedCreateURL(t *testing.T) {
	cases := []string{
		`fetch('https://docs.qq.com/api/v1/sheet/create')`,
		`location.href = 'https://docs.qq.com/sheet/new'`,
		`window.open('/doc/new')`,
		`fetch('/api/v1/doc/abc')`,
		`location.href = '/slide/new'`,
	}
	for _, expr := range cases {
		if msg := detectGuessedCreateURL(expr); msg == "" {
			t.Errorf("expected DK URL guard to trip for %q", expr)
		}
	}
	// Clean expressions must not trip.
	for _, expr := range []string{
		`document.querySelector('button')`,
		`location.href`,
		`Array.from(document.querySelectorAll('a')).map(a=>a.href)`,
	} {
		if msg := detectGuessedCreateURL(expr); msg != "" {
			t.Errorf("DK URL guard falsely tripped for clean expr %q: %q", expr, msg)
		}
	}
}

// TestBrowserEvaluateJSHandlerRefusesGuessedCreateURL (DR① end-to-end): the
// handler returns a tool error naming the fragment and guiding the LLM to use
// the page UI instead of evaluating a fetch to a guessed endpoint.
func TestBrowserEvaluateJSHandlerRefusesGuessedCreateURL(t *testing.T) {
	perms := (*config.PermissionStore)(nil) // nil => perms check skipped; DK guard still runs after it
	f := &evalRescueFake{}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"expression": `fetch('https://docs.qq.com/api/v1/sheet/create')`,
	}}}
	res, err := BrowserEvaluateJSHandler(f, perms)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error result refusing guessed creation URL")
	}
	// No Runtime.evaluate should have been dispatched.
	if f.calls != 0 {
		t.Errorf("expected zero eval calls (refused before dispatch), got %d", f.calls)
	}
}
