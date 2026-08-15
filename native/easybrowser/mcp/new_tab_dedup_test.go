package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// cannedRouterSender builds a jsonrpc.Router whose sender inspects the outgoing
// request method and immediately delivers a canned response (via HandleResponse)
// keyed by the request's ID. This lets handler tests drive tabs.list /
// tabs.create without the real async transport.
type cannedRouterSender struct {
	mu          sync.Mutex
	sent        []jsonrpc.Request
	byMethod    map[string]json.RawMessage // method -> result payload
	errByMethod map[string]*jsonrpc.ResponseError
	router      *jsonrpc.Router
}

func (s *cannedRouterSender) send(data []byte) error {
	var req jsonrpc.Request
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}
	s.mu.Lock()
	s.sent = append(s.sent, req)
	resp := jsonrpc.Response{JSONRPC: "2.0", ID: req.ID}
	if e, ok := s.errByMethod[req.Method]; ok {
		ecopy := *e
		resp.Error = &ecopy
	} else if r, ok := s.byMethod[req.Method]; ok {
		resp.Result = r
	}
	respBytes, _ := json.Marshal(resp)
	s.mu.Unlock()
	// Deliver response on a fresh goroutine to avoid reentrancy issues; the
	// pending channel is buffered (size 1) so this is safe.
	go s.router.HandleResponse(respBytes)
	return nil
}

func newCannedRouter() *cannedRouterSender {
	s := &cannedRouterSender{
		byMethod:    map[string]json.RawMessage{},
		errByMethod: map[string]*jsonrpc.ResponseError{},
	}
	s.router = jsonrpc.NewRouter(s.send, 2*time.Second)
	return s
}

func (s *cannedRouterSender) sentMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.sent))
	for i, r := range s.sent {
		out[i] = r.Method
	}
	return out
}

// TestBrowserNewTabSoftDedupSameOrigin verifies that when a managed tab with
// the same origin already exists, BrowserNewTabHandler does NOT call
// tabs.create and returns a soft (isError=false) hint naming the tabId (C2).
func TestBrowserNewTabSoftDedupSameOrigin(t *testing.T) {
	s := newCannedRouter()
	s.byMethod["tabs.list"] = json.RawMessage(`[{"id":42,"url":"https://example.com/page1"}]`)
	handler := BrowserNewTabHandler(&fakeCDP{}, s.router)

	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url": "https://example.com/page2",
	}}}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("dedup hint must be non-error (isError=false), got error: %s", toolResultText(res))
	}
	txt := toolResultText(res)
	if !strings.Contains(txt, "same-origin tab already open") ||
		!strings.Contains(txt, "tabId=42") ||
		!strings.Contains(txt, "browser_switch_tab(42)") ||
		!strings.Contains(txt, "force=true") {
		t.Errorf("dedup hint missing key phrases, got: %s", txt)
	}
	// Must NOT have called tabs.create.
	for _, m := range s.sentMethods() {
		if m == "tabs.create" {
			t.Errorf("tabs.create must not be called on same-origin dedup hit")
		}
	}
}

// TestBrowserNewTabNoMatchCreatesTab verifies that when no same-origin managed
// tab exists, the handler falls through to tabs.create (C2 no-match path).
func TestBrowserNewTabNoMatchCreatesTab(t *testing.T) {
	s := newCannedRouter()
	s.byMethod["tabs.list"] = json.RawMessage(`[{"id":42,"url":"https://other.example.com"}]`)
	s.byMethod["tabs.create"] = json.RawMessage(`{"id":99,"url":"https://example.com/x"}`)
	handler := BrowserNewTabHandler(&fakeCDP{}, s.router)

	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url": "https://example.com/page",
	}}}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", toolResultText(res))
	}
	if !strings.Contains(toolResultText(res), "opened new tab") {
		t.Errorf("expected 'opened new tab', got: %s", toolResultText(res))
	}
	methods := s.sentMethods()
	listed, created := false, false
	for _, m := range methods {
		if m == "tabs.list" {
			listed = true
		}
		if m == "tabs.create" {
			created = true
		}
	}
	if !listed {
		t.Errorf("expected tabs.list probe to run, it did not (sent: %v)", methods)
	}
	if !created {
		t.Errorf("expected tabs.create to run on no-match, it did not (sent: %v)", methods)
	}
}

// TestBrowserNewTabForceSkipsDedup verifies force=true bypasses the same-origin
// probe and creates the tab directly (C2 escape hatch).
func TestBrowserNewTabForceSkipsDedup(t *testing.T) {
	s := newCannedRouter()
	// Even though a same-origin tab exists, force must skip the dedup.
	s.byMethod["tabs.list"] = json.RawMessage(`[{"id":42,"url":"https://example.com/old"}]`)
	s.byMethod["tabs.create"] = json.RawMessage(`{"id":99,"url":"https://example.com/new"}`)
	handler := BrowserNewTabHandler(&fakeCDP{}, s.router)

	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url":   "https://example.com/new",
		"force": true,
	}}}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", toolResultText(res))
	}
	if !strings.Contains(toolResultText(res), "opened new tab") {
		t.Errorf("expected 'opened new tab', got: %s", toolResultText(res))
	}
	// force=true must NOT call tabs.list.
	for _, m := range s.sentMethods() {
		if m == "tabs.list" {
			t.Errorf("tabs.list must not be called when force=true, sent: %v", s.sentMethods())
		}
	}
	created := false
	for _, m := range s.sentMethods() {
		if m == "tabs.create" {
			created = true
		}
	}
	if !created {
		t.Errorf("expected tabs.create to run with force=true, it did not")
	}
}

// TestBrowserNewTabDedupIgnoresUnparseableTarget verifies that an unparseable
// target URL (no scheme/host) does not trigger dedup — handler falls through.
func TestBrowserNewTabDedupIgnoresUnparseableTarget(t *testing.T) {
	s := newCannedRouter()
	s.byMethod["tabs.list"] = json.RawMessage(`[]`)
	s.byMethod["tabs.create"] = json.RawMessage(`{"id":99}`)
	handler := BrowserNewTabHandler(&fakeCDP{}, s.router)
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{
		"url": "not-a-url",
	}}}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", toolResultText(res))
	}
	// Even though tabs.list returns [], handler should still have created.
	created := false
	for _, m := range s.sentMethods() {
		if m == "tabs.create" {
			created = true
		}
	}
	if !created {
		t.Errorf("expected tabs.create on unparseable target, sent: %v", s.sentMethods())
	}
}

// TestSameOrigin covers the origin comparison helper.
func TestSameOrigin(t *testing.T) {
	must := func(s string) *url.URL { u, _ := url.Parse(s); return u }
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://example.com/a", "https://example.com/b", true},
		{"https://example.com", "https://example.com:443", true},
		{"http://example.com", "http://example.com:80", true},
		{"https://example.com", "http://example.com", false},
		{"https://example.com", "https://other.example.com", false},
		{"https://example.com:8080", "https://example.com:9090", false},
		{"https://example.com", "https://example.com:8080", false},
	}
	for _, c := range cases {
		if got := sameOrigin(must(c.a), must(c.b)); got != c.want {
			t.Errorf("sameOrigin(%q,%q)=%v, want %v", c.a, c.b, got, c.want)
		}
	}
}
