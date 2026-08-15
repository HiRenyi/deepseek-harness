package axtree

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/browser-mcp/bridge/jsonrpc"
)

// resolveFake answers getBoxModel + getFullAXTree with scripted responses.
type resolveFake struct {
	boxErr       bool
	axNodesOnReq json.RawMessage
	calls        []string
}

func (f *resolveFake) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	f.calls = append(f.calls, method)
	resp := &jsonrpc.Response{}
	switch method {
	case "DOM.getBoxModel":
		if f.boxErr {
			resp.Error = &jsonrpc.ResponseError{Code: -32000, Message: "node not found"}
		} else {
			resp.Result = json.RawMessage(`{"model":{"border":[0,0,1,0,1,1,0,1]}}`)
		}
	case "Accessibility.getFullAXTree":
		resp.Result = f.axNodesOnReq
	}
	return resp, nil
}

func TestResolveRefFastPath(t *testing.T) {
	m := newRefMap()
	m.Assign("button", "OK", -1, "")
	m.SetBackend("e1", 42)
	f := &resolveFake{}
	bid, err := m.ResolveRef(context.Background(), f, "e1")
	if err != nil || bid != 42 {
		t.Fatalf("fast path: bid=%d err=%v", bid, err)
	}
}

func TestResolveRefSlowPath(t *testing.T) {
	m := newRefMap()
	m.Assign("button", "OK", -1, "")
	m.SetBackend("e1", 999) // stale
	ax := `{"nodes":[
		{"nodeId":"1","role":{"type":"role","value":"RootWebArea"},"childIds":["2"]},
		{"nodeId":"2","role":{"type":"role","value":"button"},"name":{"type":"string","value":"OK"},"backendDOMNodeId":77,"parentId":"1"}
	]}`
	f := &resolveFake{boxErr: true, axNodesOnReq: json.RawMessage(ax)}
	bid, err := m.ResolveRef(context.Background(), f, "e1")
	if err != nil {
		t.Fatalf("slow path err=%v", err)
	}
	if bid != 77 {
		t.Errorf("slow path bid=%d want 77", bid)
	}
	if e, _ := m.Lookup("e1"); e.BackendNodeID != 77 {
		t.Errorf("refmap not updated: %d", e.BackendNodeID)
	}
}

func TestResolveRefStaleBeyondRecovery(t *testing.T) {
	m := newRefMap()
	m.Assign("button", "Gone", -1, "")
	m.SetBackend("e1", 5)
	ax := `{"nodes":[{"nodeId":"1","role":{"type":"role","value":"RootWebArea"}}]}`
	f := &resolveFake{boxErr: true, axNodesOnReq: json.RawMessage(ax)}
	_, err := m.ResolveRef(context.Background(), f, "e1")
	if err == nil {
		t.Fatal("expected stale error")
	}
}
