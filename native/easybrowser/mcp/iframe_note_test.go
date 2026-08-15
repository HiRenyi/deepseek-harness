package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// dnFakeCDP dispatches getFullAXTree on params' frameId and serves
// DOM.describeNode for the Iframe element. Mirrors the CDP shape the
// browser_snapshot_ax handler drives during iframe drilling (DN).
type dnFakeCDP struct {
	mainAX  json.RawMessage // no-frameId getFullAXTree
	childAX json.RawMessage // child-frame getFullAXTree
}

func (f *dnFakeCDP) Call(_ context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	resp := &jsonrpc.Response{}
	switch method {
	case "Accessibility.enable":
		// no-op
	case "Accessibility.getFullAXTree":
		var p struct {
			FrameID string `json:"frameId"`
		}
		_ = json.Unmarshal(params, &p)
		if p.FrameID == "" {
			resp.Result = f.mainAX
		} else {
			resp.Result = f.childAX
		}
	case "DOM.describeNode":
		// Report the iframe's contentDocument carries childFrameId-1.
		out := struct {
			Node *cdn `json:"node"`
		}{Node: &cdn{ContentDocument: &cdn{FrameID: "childFrameId-1"}}}
		raw, _ := json.Marshal(out)
		resp.Result = raw
	case "Runtime.evaluate":
		resp.Result = json.RawMessage(`{"result":{"value":"[]"}}`)
	}
	return resp, nil
}

// cdn mirrors cdp.DOMNode's relevant json tags for describeNode test marshalling.
type cdn struct {
	BackendNodeID    int64  `json:"backendNodeId,omitempty"`
	FrameID          string `json:"frameId,omitempty"`
	ContentDocument  *cdn   `json:"contentDocument,omitempty"`
}

// TestBrowserSnapshotAxIframeNoteAppended (DN3): when an Iframe is present in
// the AX tree, the handler appends a switch-to-DOM note so the LLM retries with
// browser_snapshot when the target lives inside the iframe. Fires even when DL
// drilling succeeds, because iframe-internal targets are often unnamed generics
// the AX whitelist filters out (2026-06-24 docs.qq.com regression).
func TestBrowserSnapshotAxIframeNoteAppended(t *testing.T) {
	// Main frame: RootWebArea + button + Iframe (backendNodeId=100). The Iframe
	// resolves to childFrameId-1; the child frame returns a degenerate tree so
	// the iframe counts as unexpanded — but the note fires on IframesPresent
	// regardless, so this also covers the failure path.
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"打开"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"Iframe"},"name":{"type":"string","value":"表格选择"},"backendDOMNodeId":100,"parentId":"1"}
	]}`
	child := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"generic"},"parentId":"1"}
	]}`
	f := &dnFakeCDP{
		mainAX:  json.RawMessage(main),
		childAX: json.RawMessage(child),
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserSnapshotAxHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	text := toolResultText(res)
	if !strings.Contains(text, "[note:") {
		t.Errorf("expected iframe note, got:\n%s", text)
	}
	if !strings.Contains(text, "iframe(s) present") {
		t.Errorf("note wording changed, got:\n%s", text)
	}
	// Main button must survive.
	if !strings.Contains(text, `button "打开"`) {
		t.Errorf("main tree lost when appending iframe note:\n%s", text)
	}
}

// TestBrowserSnapshotAxIframeNoteFiresEvenWhenDrillSucceeds (DN, 2026-06-24):
// the docs.qq.com regression — an iframe whose child frame drills successfully
// (returns an interactive button) must STILL append the note, because the
// iframe-internal cards the LLM actually needs may be unnamed generics filtered
// from AX. The note is the signal to switch to browser_snapshot (DOM pierce).
func TestBrowserSnapshotAxIframeNoteFiresEvenWhenDrillSucceeds(t *testing.T) {
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"新建"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"Iframe"},"name":{"type":"string","value":""},"backendDOMNodeId":100,"parentId":"1"}
	]}`
	// Child frame drills successfully: an interactive button "立即开通" is
	// stitched. But the real target ("空白表格" card) is an unnamed generic not
	// in this AX tree — the note must still fire to guide DOM retry.
	child := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"立即开通"},"backendDOMNodeId":200,"parentId":"1"}
	]}`
	f := &dnFakeCDP{
		mainAX:  json.RawMessage(main),
		childAX: json.RawMessage(child),
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserSnapshotAxHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	text := toolResultText(res)
	if !strings.Contains(text, "[note:") {
		t.Errorf("note must fire even when iframe drilling succeeds (docs.qq.com regression), got:\n%s", text)
	}
	if !strings.Contains(text, `button "立即开通"`) {
		t.Errorf("drilled child button must be stitched:\n%s", text)
	}
}

// TestBrowserSnapshotAxNoIframeNoNote (DN3): a healthy tree without an Iframe
// must NOT append the switch-to-DOM note (avoid noise on normal pages).
func TestBrowserSnapshotAxNoIframeNoNote(t *testing.T) {
	// Two buttons, no Iframe -> no drilling, UnexpandedIframes == 0, no note.
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"a"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"button"},"name":{"type":"string","value":"b"},"backendDOMNodeId":11,"parentId":"1"}
	]}`
	f := &dnFakeCDP{mainAX: json.RawMessage(main)}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserSnapshotAxHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	text := toolResultText(res)
	if strings.Contains(text, "[note:") {
		t.Errorf("no-iframe healthy tree must not emit the iframe note, got:\n%s", text)
	}
}

// dqFakeCDP extends dnFakeCDP with a DOM.getDocument payload so DQ1's
// iframeDOMSupplement (DOM pierce → FilterIframeDOM) can be exercised.
type dqFakeCDP struct {
	dnFakeCDP
	domDoc json.RawMessage
}

func (f *dqFakeCDP) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	if method == "DOM.getDocument" {
		return &jsonrpc.Response{Result: f.domDoc}, nil
	}
	return f.dnFakeCDP.Call(ctx, method, params)
}

// TestBrowserSnapshotAxDualEmitsIframeDOMSupplement (DQ1): when the AX tree
// contains an Iframe, the handler proactively appends the iframe-internal DOM
// nodes (text=/node_id) so the LLM can act without switching perception paths.
// The unnamed generic card "空白表格" (no AX ref) surfaces via DOM text=.
func TestBrowserSnapshotAxDualEmitsIframeDOMSupplement(t *testing.T) {
	resetAxStuck()
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"打开"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"Iframe"},"backendDOMNodeId":100,"parentId":"1"}
	]}`
	child := `{"nodes":[{"nodeId":"1","role":{"type":"role","value":"RootWebArea"}}]}`
	dom := `{"root":{"nodeId":1,"nodeType":9,"nodeName":"#document","children":[
		{"nodeId":2,"nodeType":1,"nodeName":"html","children":[
			{"nodeId":3,"nodeType":1,"nodeName":"body","backendNodeId":1,"children":[
				{"nodeId":4,"nodeType":1,"nodeName":"IFRAME","backendNodeId":40,"contentDocument":{
					"nodeId":50,"nodeType":1,"nodeName":"#document","children":[
						{"nodeId":51,"nodeType":1,"nodeName":"DIV","backendNodeId":1408,"attributes":["class","card--2L0vJ"],"children":[
							{"nodeId":52,"nodeType":3,"nodeValue":"空白表格"}
						]}
					]
				}}
			]}
		]}
	]}}`
	f := &dqFakeCDP{
		dnFakeCDP: dnFakeCDP{mainAX: json.RawMessage(main), childAX: json.RawMessage(child)},
		domDoc:    json.RawMessage(dom),
	}
	req := mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: map[string]any{}}}
	res, err := BrowserSnapshotAxHandler(f)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	text := toolResultText(res)

	// The iframe-internal card surfaced with its DOM node_id + text=.
	if !strings.Contains(text, "node_id=1408") {
		t.Errorf("dual-emit should surface iframe card node_id=1408, got:\n%s", text)
	}
	if !strings.Contains(text, `text="空白表格"`) {
		t.Errorf("dual-emit should surface the card accessible name, got:\n%s", text)
	}
	if !strings.Contains(text, "DOM supplement") {
		t.Errorf("supplement header missing, got:\n%s", text)
	}
}

