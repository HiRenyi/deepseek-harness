package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// TestBrowserNavigateChromeSchemeGuidance verifies the navigate handler
// detects a chrome:// active tab (via Runtime.evaluate window.location.href)
// and returns a guidance error WITHOUT firing Page.navigate (A item / D1).
func TestBrowserNavigateChromeSchemeGuidance(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			// GetURL probe: active tab is chrome://newtab/
			"Runtime.evaluate": json.RawMessage(`{"result":{"type":"string","value":"chrome://newtab/"}}`),
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url": "https://example.com",
	}}}
	res, err := BrowserNavigateHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true guidance, got non-error: %s", toolResultText(res))
	}
	txt := toolResultText(res)
	if !strings.Contains(txt, "chrome:// URL") || !strings.Contains(txt, "browser_new_tab") {
		t.Errorf("guidance text missing key phrases, got: %s", txt)
	}
	if !strings.Contains(txt, "https://example.com") {
		t.Errorf("guidance should echo target url, got: %s", txt)
	}
	// Must NOT have fired Page.navigate.
	for _, c := range f.calls {
		if c.method == "Page.navigate" {
			t.Errorf("Page.navigate must not be called on chrome:// active tab, but was called")
		}
	}
	// Must have probed the active tab URL.
	probed := false
	for _, c := range f.calls {
		if c.method == "Runtime.evaluate" {
			probed = true
		}
	}
	if !probed {
		t.Errorf("expected GetURL probe (Runtime.evaluate) to run, it did not")
	}
}

// TestBrowserNavigateFallsThroughOnHttpScheme verifies that on a normal
// http(s) active tab the chrome:// guard does not short-circuit: the handler
// proceeds to fire Page.navigate (B item regression guard). To avoid the
// 10s waitForLoad poll, Page.navigate is mocked to return a CDP error so the
// handler surfaces "navigate failed" — proving it passed the guard.
func TestBrowserNavigateFallsThroughOnHttpScheme(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Runtime.evaluate": json.RawMessage(`{"result":{"type":"string","value":"https://example.com/current"}}`),
		},
		errors: map[string]*jsonrpc.ResponseError{
			"Page.navigate": {Code: -32000, Message: "navigation rejected (test)"},
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url": "https://example.com/next",
	}}}
	res, err := BrowserNavigateHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected navigate not-applied error (fall-through), got: %s", toolResultText(res))
	}
	txt := toolResultText(res)
	if !strings.Contains(txt, "navigate: not applied") {
		t.Errorf("expected navigate not-applied surface, got: %s", txt)
	}
	// Must have fired Page.navigate (fall-through).
	fired := false
	for _, c := range f.calls {
		if c.method == "Page.navigate" {
			fired = true
		}
	}
	if !fired {
		t.Errorf("expected Page.navigate to fire on http active tab, it did not")
	}
}

// TestBrowserNavigateFallsThroughWhenGetURLFails verifies that if the active
// tab URL probe itself errors (e.g. debugger not yet attached to a chrome://
// page), the handler does not block — it falls through to Navigate, which will
// surface its own error. This avoids a false-positive guidance when the scheme
// cannot be determined.
func TestBrowserNavigateFallsThroughWhenGetURLFails(t *testing.T) {
	f := &fakeCDP{
		errors: map[string]*jsonrpc.ResponseError{
			"Runtime.evaluate": {Code: -32000, Message: "cannot evaluate"},
			"Page.navigate":    {Code: -32000, Message: "nav error (test)"},
		},
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url": "https://example.com",
	}}}
	res, err := BrowserNavigateHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected fall-through error, got: %s", toolResultText(res))
	}
	txt := toolResultText(res)
	// Must be the navigate not-applied surface, NOT the chrome:// guidance.
	if strings.Contains(txt, "chrome:// URL") {
		t.Errorf("GetURL failure must not trigger chrome:// guidance, got: %s", txt)
	}
	if !strings.Contains(txt, "navigate: not applied") {
		t.Errorf("expected navigate-failed surface, got: %s", txt)
	}
}

// TestIsRestrictedNavScheme covers the scheme classifier.
func TestIsRestrictedNavScheme(t *testing.T) {
	cases := map[string]bool{
		"chrome://newtab/":                    true,
		"chrome-extension://abc/options.html": true,
		"about:blank":                         true,
		"https://example.com":                 false,
		"http://localhost:8080":               false,
		"file:///C:/x":                        false,
		"":                                    false,
		"not a url":                           false,
	}
	for in, want := range cases {
		if got := isRestrictedNavScheme(in); got != want {
			t.Errorf("isRestrictedNavScheme(%q)=%v, want %v", in, got, want)
		}
	}
}
