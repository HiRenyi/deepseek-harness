package axtree

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
)

func TestPromoteHiddenInputs(t *testing.T) {
	tree := []TreeNode{
		{Role: "generic", BackendNodeID: 100},
		{Role: "generic", BackendNodeID: 200},
	}
	cursor := map[int64]CursorInfo{
		100: {HiddenInputKind: "radio", HiddenInputChecked: "true"},
		// 200 has no cursor hit -> unchanged
	}
	promoteHiddenInputs(tree, cursor)
	if tree[0].Role != "radio" {
		t.Errorf("node0 role=%q want radio", tree[0].Role)
	}
	if tree[0].Checked == nil || *tree[0].Checked != "true" {
		t.Errorf("node0 checked=%v want true", tree[0].Checked)
	}
	if tree[1].Role != "generic" {
		t.Errorf("node1 role=%q want generic (no cursor hit)", tree[1].Role)
	}
}

// cursorFakeCDP is a minimal CDPClient fake for cursor-detection tests. It
// records the call sequence and returns canned responses per method, mirroring
// the iframe_test.go fake pattern.
type cursorFakeCDP struct {
	calls []string

	// DOM.performSearch
	performSearchID    string
	performSearchCount int
	performSearchErr   bool

	// DOM.getSearchResults
	getSearchNodeIDs []int64
	getSearchErr     bool

	// DOM.describeNode
	describeBackend int64
	describeErr     bool

	// Runtime.evaluate
	evalValue string
}

func (f *cursorFakeCDP) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	f.calls = append(f.calls, method)
	resp := &jsonrpc.Response{}
	switch method {
	case "DOM.performSearch":
		if f.performSearchErr {
			resp.Error = &jsonrpc.ResponseError{Code: -32000, Message: "performSearch failed"}
			return resp, nil
		}
		out, _ := json.Marshal(struct {
			SearchID    string `json:"searchId"`
			ResultCount int    `json:"resultCount"`
		}{f.performSearchID, f.performSearchCount})
		resp.Result = out
	case "DOM.getSearchResults":
		if f.getSearchErr {
			resp.Error = &jsonrpc.ResponseError{Code: -32000, Message: "getSearchResults failed"}
			return resp, nil
		}
		out, _ := json.Marshal(struct {
			NodeIDs []int64 `json:"nodeIds"`
		}{f.getSearchNodeIDs})
		resp.Result = out
	case "DOM.describeNode":
		if f.describeErr {
			resp.Error = &jsonrpc.ResponseError{Code: -32000, Message: "describeNode failed"}
			return resp, nil
		}
		out, _ := json.Marshal(struct {
			Node struct {
				BackendNodeID int64 `json:"backendNodeId"`
			} `json:"node"`
		}{struct {
			BackendNodeID int64 `json:"backendNodeId"`
		}{f.describeBackend}})
		resp.Result = out
	case "Runtime.evaluate":
		out, _ := json.Marshal(struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}{struct {
			Value string `json:"value"`
		}{f.evalValue}})
		resp.Result = out
	case "Accessibility.getFullAXTree":
		// GetAXSnapshot drives fetchAXTree → getFullAXTree. Return an empty tree
		// so the snapshot short-circuits to "empty AX tree" degraded reason; the
		// test only asserts the call-sequence prefix (DOM.enable before
		// performSearch), not snapshot content.
		resp.Result = []byte(`{"nodes":[]}`)
	}
	return resp, nil
}

func TestDescribeByDataAttr_HappyPath(t *testing.T) {
	f := &cursorFakeCDP{
		performSearchID:    "sid-1",
		performSearchCount: 1,
		getSearchNodeIDs:   []int64{42},
		describeBackend:    999,
	}
	bid, err := describeByDataAttr(context.Background(), f, 0)
	if err != nil || bid != 999 {
		t.Fatalf("want bid=999 err=nil, got bid=%d err=%v (calls=%v)", bid, err, f.calls)
	}
	wantSeq := []string{"DOM.performSearch", "DOM.getSearchResults", "DOM.describeNode"}
	if len(f.calls) != len(wantSeq) {
		t.Fatalf("call seq len %d, want %d (%v)", len(f.calls), len(wantSeq), f.calls)
	}
	for i, m := range wantSeq {
		if f.calls[i] != m {
			t.Fatalf("call[%d]=%q, want %q (full=%v)", i, f.calls[i], m, f.calls)
		}
	}
}

func TestDescribeByDataAttr_ZeroResults(t *testing.T) {
	f := &cursorFakeCDP{performSearchID: "sid-0", performSearchCount: 0}
	bid, err := describeByDataAttr(context.Background(), f, 0)
	if err != nil || bid != 0 {
		t.Fatalf("want bid=0 err=nil on 0 results, got bid=%d err=%v", bid, err)
	}
	for _, m := range f.calls {
		if m == "DOM.getSearchResults" || m == "DOM.describeNode" {
			t.Fatalf("should not call %q after 0 results (calls=%v)", m, f.calls)
		}
	}
}

func TestDescribeByDataAttr_GetSearchResultsFails(t *testing.T) {
	f := &cursorFakeCDP{
		performSearchID: "sid-1", performSearchCount: 1,
		getSearchErr: true,
	}
	bid, err := describeByDataAttr(context.Background(), f, 0)
	if err == nil || !strings.Contains(err.Error(), "getSearchResults") {
		t.Fatalf("want getSearchResults err, got bid=%d err=%v", bid, err)
	}
	if bid != 0 {
		t.Fatalf("want bid=0 on err, got %d", bid)
	}
}

func TestDescribeByDataAttr_DescribeNodeFails(t *testing.T) {
	f := &cursorFakeCDP{
		performSearchID: "sid-1", performSearchCount: 1,
		getSearchNodeIDs: []int64{42},
		describeErr: true,
	}
	bid, err := describeByDataAttr(context.Background(), f, 0)
	if err == nil || !strings.Contains(err.Error(), "describeNode") {
		t.Fatalf("want describeNode err, got bid=%d err=%v", bid, err)
	}
	if bid != 0 {
		t.Fatalf("want bid=0 on err, got %d", bid)
	}
}

func TestFindCursorInteractiveElements_EndToEnd(t *testing.T) {
	// Runtime.evaluate returns 2 cursor hits; each resolves via the 3-step chain.
	// Both resolve to the same backendNodeId=555 (fake doesn't vary by idx), so
	// the map ends with 1 entry — the second hit's CursorInfo wins (checkbox).
	f := &cursorFakeCDP{
		evalValue:          `[{"i":0,"kind":"","checked":""},{"i":1,"kind":"checkbox","checked":"true"}]`,
		performSearchID:    "sid-x",
		performSearchCount: 1,
		getSearchNodeIDs:   []int64{10, 20},
		describeBackend:    555,
	}
	out, err := findCursorInteractiveElements(context.Background(), f)
	if err != nil {
		t.Fatalf("findCursor err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 cursor entry (both resolve to 555), got %d: %+v", len(out), out)
	}
	ci, ok := out[555]
	if !ok {
		t.Fatalf("entry 555 missing: %+v", out)
	}
	if ci.HiddenInputKind != "checkbox" {
		t.Fatalf("want kind=checkbox (last hit wins), got %q", ci.HiddenInputKind)
	}
}

// TestGetAXSnapshotEnablesDOMBeforePerformSearch: the AX path must call
// DOM.enable before any DOM.performSearch, otherwise performSearch fails with
// "DOM agent is not enabled" (observed 363/session in real-machine events.jsonl
// before this fix). The enable is idempotent and runs at GetAXSnapshot entry.
func TestGetAXSnapshotEnablesDOMBeforePerformSearch(t *testing.T) {
	f := &cursorFakeCDP{
		evalValue:          `[{"i":0,"kind":"","checked":""}]`,
		performSearchID:    "sid-1",
		performSearchCount: 1,
		getSearchNodeIDs:   []int64{42},
		describeBackend:    999,
	}
	// GetAXSnapshot returns a degraded result (empty AX tree from the fake),
	// but the call sequence is what we assert here.
	_, _ = GetAXSnapshot(context.Background(), f, newRefMap(), SnapshotOpts{Mode: "compact"})

	enableIdx, performSearchIdx := -1, -1
	for i, m := range f.calls {
		switch m {
		case "DOM.enable":
			if enableIdx == -1 {
				enableIdx = i
			}
		case "DOM.performSearch":
			if performSearchIdx == -1 {
				performSearchIdx = i
			}
		}
	}
	if enableIdx == -1 {
		t.Fatalf("DOM.enable not called (calls=%v)", f.calls)
	}
	if performSearchIdx == -1 {
		t.Fatalf("DOM.performSearch not called (calls=%v)", f.calls)
	}
	if enableIdx >= performSearchIdx {
		t.Fatalf("DOM.enable (idx=%d) must precede first DOM.performSearch (idx=%d); calls=%v", enableIdx, performSearchIdx, f.calls)
	}
}
