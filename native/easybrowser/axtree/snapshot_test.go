package axtree

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
)

func TestAssignRefsInteractiveAndContent(t *testing.T) {
	m := newRefMap()
	tree := []TreeNode{
		{Role: "button", Name: "OK", BackendNodeID: 1},
		{Role: "heading", Name: "Title", BackendNodeID: 2},
		{Role: "heading", Name: "", BackendNodeID: 3}, // content role, no name -> no ref
		{Role: "generic", Name: "", BackendNodeID: 4},
	}
	assignRefs(tree, map[int64]CursorInfo{}, m)
	if !tree[0].HasRef || tree[0].RefID != "e1" {
		t.Errorf("button ref=%q want e1", tree[0].RefID)
	}
	if !tree[1].HasRef {
		t.Errorf("heading with name should get ref")
	}
	if tree[2].HasRef {
		t.Errorf("heading without name must NOT get ref")
	}
	if tree[3].HasRef {
		t.Errorf("generic without cursor hit must NOT get ref")
	}
}

func TestAssignRefsNthInternal(t *testing.T) {
	m := newRefMap()
	tree := []TreeNode{
		{Role: "textbox", Name: "Note", BackendNodeID: 1},
		{Role: "textbox", Name: "Note", BackendNodeID: 2},
		{Role: "textbox", Name: "Note", BackendNodeID: 3},
	}
	assignRefs(tree, map[int64]CursorInfo{}, m)
	ids := []string{tree[0].RefID, tree[1].RefID, tree[2].RefID}
	if ids[0] == ids[1] || ids[1] == ids[2] {
		t.Errorf("expected 3 distinct refs, got %v", ids)
	}
	if tree[0].Nth != 0 || tree[1].Nth != 1 || tree[2].Nth != 2 {
		t.Errorf("nth got %d,%d,%d want 0,1,2", tree[0].Nth, tree[1].Nth, tree[2].Nth)
	}
}

func TestRenderTreeCompact(t *testing.T) {
	m := newRefMap()
	tree := []TreeNode{
		{Role: "RootWebArea", Children: []int{1}},
		{Role: "button", Name: "OK", BackendNodeID: 1},
	}
	tree[1].ParentIdx = 0
	assignRefs(tree, map[int64]CursorInfo{}, m)
	out := renderTree(tree, []int{0}, "compact")
	if !strings.Contains(out, `- button "OK" [ref=e1]`) {
		t.Errorf("render missing ref line:\n%s", out)
	}
}

func TestCompactKeepsAncestors(t *testing.T) {
	rendered := "  - heading \"Form\"\n    - button \"OK\" [ref=e1]"
	out := compactTree(rendered, "compact")
	if !strings.Contains(out, `heading "Form"`) {
		t.Errorf("ancestor dropped:\n%s", out)
	}
	if !strings.Contains(out, `button "OK" [ref=e1]`) {
		t.Errorf("ref line dropped:\n%s", out)
	}
}

func TestCompactEmptyInteractive(t *testing.T) {
	out := compactTree("- generic \"x\"\n", "compact")
	if out != "(no interactive elements)" {
		t.Errorf("empty compact=%q want placeholder", out)
	}
}

// axFakeCDP feeds getFullAXTree + Runtime.evaluate canned responses.
type axFakeCDP struct {
	axNodes json.RawMessage
	eval   json.RawMessage
	calls  []string
}

func (f *axFakeCDP) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	f.calls = append(f.calls, method)
	resp := &jsonrpc.Response{}
	switch method {
	case "Accessibility.getFullAXTree":
		resp.Result = f.axNodes
	case "Runtime.evaluate":
		resp.Result = f.eval
	}
	return resp, nil
}

func TestGetAXSnapshotHealthy(t *testing.T) {
	// 5 buttons -> healthy.
	ax := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2","3","4","5","6"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"a"},"backendDOMNodeId":1,"parentId":"1"},
		{"nodeId":"3","role":{"type":"role","value":"button"},"name":{"type":"string","value":"b"},"backendDOMNodeId":2,"parentId":"1"},
		{"nodeId":"4","role":{"type":"role","value":"button"},"name":{"type":"string","value":"c"},"backendDOMNodeId":3,"parentId":"1"},
		{"nodeId":"5","role":{"type":"role","value":"button"},"name":{"type":"string","value":"d"},"backendDOMNodeId":4,"parentId":"1"},
		{"nodeId":"6","role":{"type":"role","value":"button"},"name":{"type":"string","value":"e"},"backendDOMNodeId":5,"parentId":"1"}
	]}`
	f := &axFakeCDP{axNodes: json.RawMessage(ax), eval: json.RawMessage(`{"result":{"value":"[]"}}`)}
	res, err := GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{Mode: "compact"})
	if err != nil || res.DegradedReason != "" {
		t.Fatalf("expected healthy, got %+v err=%v", res, err)
	}
}

// TestGetAXSnapshotSimplePageNotDegraded: a page with a single interactive
// element (1 button) must NOT degrade — that was the COVERAGE_MIN=5 defect.
// Simple/login/about pages are exactly where the AXTree primary path should
// serve the agent. The 1-button tree has 1 assigned ref, so it is healthy.
func TestGetAXSnapshotSimplePageNotDegraded(t *testing.T) {
	ax := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"x"},"backendDOMNodeId":1,"parentId":"1"}
	]}`
	f := &axFakeCDP{axNodes: json.RawMessage(ax), eval: json.RawMessage(`{"result":{"value":"[]"}}`)}
	res, err := GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.DegradedReason != "" {
		t.Errorf("simple 1-button page must not degrade under strategy D, got reason=%q", res.DegradedReason)
	}
	if !strings.Contains(res.Text, "ref=e") {
		t.Errorf("expected ref in healthy simple-page snapshot, got %q", res.Text)
	}
}

// TestGetAXSnapshotDegenerateZeroRefsDegrades: when the AX tree assigns ZERO
// refs (degenerate — RootWebArea + generic only, no interactive/content elements),
// the snapshot would be empty, so it degrades to DOM instead.
func TestGetAXSnapshotDegenerateZeroRefsDegrades(t *testing.T) {
	ax := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"generic"},"parentId":"1"}
	]}`
	f := &axFakeCDP{axNodes: json.RawMessage(ax), eval: json.RawMessage(`{"result":{"value":"[]"}}`)}
	res, _ := GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{})
	if res.DegradedReason == "" {
		t.Errorf("expected degenerate (zero-ref) tree to degrade, got %+v", res)
	}
}
