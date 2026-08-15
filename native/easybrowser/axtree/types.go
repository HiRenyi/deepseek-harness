package axtree

import "encoding/json"

// AXValue is a CDP accessibility value: {type, value}. value is raw so callers
// can decode per type (string/boolean/number). Maps to the AXValue in
// agent-browser element.rs.
type AXValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// AXNode is a node as returned by Accessibility.getFullAXTree (flat array).
type AXNode struct {
	NodeID           string       `json:"nodeId"`
	ParentID         string       `json:"parentId,omitempty"`
	ChildIDs         []string     `json:"childIds,omitempty"`
	Role             *AXValue     `json:"role"`
	Name             *AXValue     `json:"name,omitempty"`
	Value            *AXValue     `json:"value,omitempty"`
	Ignored          bool         `json:"ignored,omitempty"`
	BackendDOMNodeID int64        `json:"backendDOMNodeId,omitempty"`
	Properties       []AXProperty `json:"properties,omitempty"`
	FrameID          string       `json:"frameId,omitempty"`
}

// AXProperty is one entry in AXNode.Properties.
type AXProperty struct {
	Name  string   `json:"name"`
	Value *AXValue `json:"value"`
}

// CursorInfo holds cursor-interactive detection results for one element
// (injected via Runtime.evaluate, mirroring snapshot.rs:609).
type CursorInfo struct {
	HiddenInputKind    string // "radio" | "checkbox" | "" — for promoteHiddenInputs
	HiddenInputChecked string // "true" | "false" | "" (only meaningful when Kind != "")
}

// TreeNode is the tree node after buildTree: parent/child indices, role/name,
// ref state, and render attributes. Indices reference the tree slice (not CDP
// ids) so the tree is a single contiguous slice.
type TreeNode struct {
	Role      string // canonicalized role (lowercased by buildTree where needed)
	Name      string
	ValueText string

	// Attribute hints rendered as [...].
	Level    *int
	Checked  *string // "true"|"false"|"mixed"
	Expanded *bool
	Selected *bool
	Disabled *bool
	Required *bool
	URL      string

	BackendNodeID int64
	FrameID       string

	// Tree structure (indices into the tree slice). -1 = none.
	Children  []int
	ParentIdx int

	// ref assignment state.
	HasRef bool
	RefID  string // "eN"
	Nth    int    // disambiguation index, -1 = unique

	Cursor *CursorInfo

	Depth int // rendering indent depth
}

// SnapshotOpts controls GetAXSnapshot rendering.
type SnapshotOpts struct {
	Mode string // "compact" (default), "interactive", "full"
}
