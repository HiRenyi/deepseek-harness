package axtree

import (
	"encoding/json"
	"testing"
)

func TestAXValueDecode(t *testing.T) {
	raw := `{"role":{"type":"role","value":"button"},"name":{"type":"string","value":"Submit"},"backendDOMNodeId":17}`
	var n AXNode
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.BackendDOMNodeID != 17 {
		t.Errorf("backendDOMNodeId=%d want 17", n.BackendDOMNodeID)
	}
}
