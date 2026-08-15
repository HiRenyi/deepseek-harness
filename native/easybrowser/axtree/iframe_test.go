package axtree

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
)

// iframeFakeCDP dispatches Accessibility.getFullAXTree on the params' frameId
// (main vs child frame), serves DOM.describeNode for the Iframe element, and
// returns an empty cursor eval. Mirrors the real CDP shape GetAXSnapshot drives.
type iframeFakeCDP struct {
	mainAX   json.RawMessage // returned for getFullAXTree with no frameId
	childAX  json.RawMessage // returned for getFullAXTree with frameId == childFrameID
	childFid string          // the frameId describeNode reports (contentDocument.frameId)

	describeNodeFor int64   // backendNodeId of the Iframe element (input to describeNode)
	describeNodeErr  bool   // if true, DOM.describeNode returns an error response
	gotChildCall    bool    // set when the child-frame getFullAXTree was invoked
}

func (f *iframeFakeCDP) Call(_ context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	resp := &jsonrpc.Response{}
	switch method {
	case "Accessibility.enable":
		// no-op
	case "Accessibility.getFullAXTree":
		// Inspect params for a frameId. Main frame call has none.
		var p struct {
			FrameID string `json:"frameId"`
		}
		_ = json.Unmarshal(params, &p)
		if p.FrameID == "" {
			resp.Result = f.mainAX
			return resp, nil
		}
		f.gotChildCall = true
		resp.Result = f.childAX
		return resp, nil
	case "DOM.describeNode":
		if f.describeNodeErr {
			resp.Error = &jsonrpc.ResponseError{Code: -1, Message: "describeNode failed"}
			return resp, nil
		}
		out := struct {
			Node *cdpDOMNode `json:"node"`
		}{Node: &cdpDOMNode{
			BackendNodeID: f.describeNodeFor,
			ContentDocument: &cdpDOMNode{
				FrameID: f.childFid,
			},
		}}
		raw, _ := json.Marshal(out)
		resp.Result = raw
		return resp, nil
	case "Runtime.evaluate":
		resp.Result = json.RawMessage(`{"result":{"value":"[]"}}`)
	}
	return resp, nil
}

// cdpDOMNode is a local mirror of cdp.DOMNode for test JSON marshalling (avoids
// importing bridge/cdp into the axtree test package, which would create an
// import cycle in test wiring since axtree already depends on cdp — but the
// struct tags must match cdp.DOMNode's json tags exactly).
type cdpDOMNode struct {
	NodeType       int          `json:"nodeType,omitempty"`
	NodeName       string       `json:"nodeName,omitempty"`
	NodeValue      string       `json:"nodeValue,omitempty"`
	BackendNodeID  int64        `json:"backendNodeId,omitempty"`
	FrameID        string       `json:"frameId,omitempty"`
	ContentDocument *cdpDOMNode `json:"contentDocument,omitempty"`
}

// TestGetAXSnapshotDrillsIframeStitchesChild (DL2): a main frame with an Iframe
// role node must have its child frame's rendered text stitched after the Iframe
// line, indented one level deeper. The child-frame button "空白表格" must appear
// in the output with a stable ref. Mirrors agent-browser snapshot.rs:490-556.
func TestGetAXSnapshotDrillsIframeStitchesChild(t *testing.T) {
	// Main frame: RootWebArea > button "打开" + Iframe (backendNodeId=100).
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"打开"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"Iframe"},"name":{"type":"string","value":"表格选择"},"backendDOMNodeId":100,"parentId":"1"}
	]}`
	// Child frame: RootWebArea > button "空白表格" (backendNodeId=200) — the card
	// CherryStudio found invisible before DL.
	child := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"空白表格"},"backendDOMNodeId":200,"parentId":"1"}
	]}`
	f := &iframeFakeCDP{
		mainAX:         json.RawMessage(main),
		childAX:        json.RawMessage(child),
		childFid:       "child-frame-1",
		describeNodeFor: 100,
	}
	res, err := GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{Mode: "compact"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.DegradedReason != "" {
		t.Fatalf("must not degrade: %s", res.DegradedReason)
	}
	if !f.gotChildCall {
		t.Errorf("expected child-frame getFullAXTree call to be issued")
	}
	if !strings.Contains(res.Text, "空白表格") {
		t.Errorf("child-frame button \"空白表格\" missing from snapshot:\n%s", res.Text)
	}
	// Stitching order: child text must appear AFTER the Iframe line.
	iframePos := strings.Index(res.Text, "Iframe")
	buttonPos := strings.Index(res.Text, "空白表格")
	if iframePos < 0 || buttonPos < 0 || buttonPos < iframePos {
		t.Errorf("child text not stitched after Iframe line:\n%s", res.Text)
	}
	// UnexpandedIframes must be 0: the child frame expanded successfully.
	if res.UnexpandedIframes != 0 {
		t.Errorf("expected 0 unexpanded iframes, got %d", res.UnexpandedIframes)
	}
}

// TestGetAXSnapshotIframeChildFailureIsolated (DL2): when the child-frame
// getFullAXTree fails (or yields an empty/non-interactive tree), the main tree
// is preserved and the Iframe counts as unexpanded. Mirrors agent-browser's
// silent error isolation for cross-origin / failed child frames.
func TestGetAXSnapshotIframeChildFailureIsolated(t *testing.T) {
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"打开"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"Iframe"},"name":{"type":"string","value":"表格选择"},"backendDOMNodeId":100,"parentId":"1"}
	]}`
	// Child frame: degenerate (generic only) -> snapshotFrame returns empty /
	// no-interactive, child not stitched, counted as unexpanded.
	child := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"generic"},"parentId":"1"}
	]}`
	f := &iframeFakeCDP{
		mainAX:         json.RawMessage(main),
		childAX:        json.RawMessage(child),
		childFid:       "child-frame-1",
		describeNodeFor: 100,
	}
	res, err := GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{Mode: "compact"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.DegradedReason != "" {
		t.Fatalf("main tree must stay healthy when child fails: %s", res.DegradedReason)
	}
	// Main button must survive.
	if !strings.Contains(res.Text, `button "打开"`) {
		t.Errorf("main tree button lost when child frame failed:\n%s", res.Text)
	}
	if res.UnexpandedIframes != 1 {
		t.Errorf("expected 1 unexpanded iframe, got %d", res.UnexpandedIframes)
	}
}

// TestGetAXSnapshotIframeDescribeNodeFailure (DL2): when DOM.describeNode fails
// to resolve the child frame id, the Iframe is unexpanded but the main tree is
// preserved.
func TestGetAXSnapshotIframeDescribeNodeFailure(t *testing.T) {
	main := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"打开"},"backendDOMNodeId":10,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"Iframe"},"name":{"type":"string","value":"表格选择"},"backendDOMNodeId":100,"parentId":"1"}
	]}`
	f := &iframeFakeCDP{
		mainAX:         json.RawMessage(main),
		childAX:        json.RawMessage(`{"nodes":[]}`),
		childFid:       "",
		describeNodeFor: 100,
		describeNodeErr: true,
	}
	res, err := GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{Mode: "compact"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.DegradedReason != "" {
		t.Fatalf("main tree must stay healthy when describeNode fails: %s", res.DegradedReason)
	}
	if !strings.Contains(res.Text, `button "打开"`) {
		t.Errorf("main tree lost when describeNode failed:\n%s", res.Text)
	}
	if res.UnexpandedIframes != 1 {
		t.Errorf("expected 1 unexpanded iframe on describeNode failure, got %d", res.UnexpandedIframes)
	}
}

// TestStitchChildAfterIframeIndentation (DL2): child text (rendered at depth 0,
// no leading indent) must be indented one level (2 spaces) deeper than the
// Iframe line, mirroring snapshot.rs:527-556.
func TestStitchChildAfterIframeIndentation(t *testing.T) {
	// Iframe at indent 6 ("      - Iframe"), child line should be at indent 8.
	output := "  - main \"x\" [ref=e1]\n      - Iframe \"表格选择\" [ref=e2]\n  - button \"y\" [ref=e3]\n"
	// childText is renderTree output of the child frame: root-level nodes at
	// depth 0, so the button line has NO leading indent.
	child := "- button \"空白表格\" [ref=e4]\n"
	out := stitchChildAfterIframe(output, "e2", child)
	lines := strings.Split(out, "\n")
	var found bool
	for _, l := range lines {
		if strings.Contains(l, "空白表格") {
			// Expect exactly 8 leading spaces (iframe 6 + 2).
			indent := 0
			for _, r := range l {
				if r == ' ' {
					indent++
				} else {
					break
				}
			}
			if indent != 8 {
				t.Errorf("child line indent=%d want 8: %q", indent, l)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("child text not stitched:\n%s", out)
	}
	// Trailing main content must survive the stitch.
	if !strings.Contains(out, `button "y"`) {
		t.Errorf("trailing main content lost:\n%s", out)
	}
}
