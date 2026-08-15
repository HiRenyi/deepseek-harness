package axtree

import (
	"encoding/json"
	"strings"
)

// extractAXString returns the human string from an AXValue (role/name). Handles
// {"type":"role","value":"button"} and {"type":"string","value":"Submit"}.
func extractAXString(v *AXValue) string {
	if v == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err == nil {
		return s
	}
	// value may be a non-string JSON; fall back to raw text.
	return strings.TrimSpace(string(v.Value))
}

// buildTree converts the flat CDP AXNode array into a TreeNode slice with
// parent/child indices. Mirrors agent-browser build_tree:
//   - skip ignored nodes except RootWebArea
//   - skip InlineTextBox placeholders (index alignment preserved via idMap)
//   - merge consecutive StaticText children
func buildTree(nodes []AXNode) ([]TreeNode, []int) {
	// First pass: TreeNode per kept node, idMap[cdpNodeId] -> treeIdx.
	tree := make([]TreeNode, 0, len(nodes))
	idMap := map[string]int{}
	for _, n := range nodes {
		role := extractAXString(n.Role)
		if n.Ignored && role != "RootWebArea" {
			continue
		}
		if role == "InlineTextBox" {
			continue
		}
		tn := TreeNode{
			Role:          role,
			Name:          extractAXString(n.Name),
			ValueText:     extractAXString(n.Value),
			BackendNodeID: n.BackendDOMNodeID,
			FrameID:       n.FrameID,
			ParentIdx:     -1,
			Nth:           -1,
		}
		applyProperties(&tn, n.Properties)
		idMap[n.NodeID] = len(tree)
		tree = append(tree, tn)
	}

	// Second pass: wire children/parent by childIds.
	for _, n := range nodes {
		parentIdx, ok := idMap[n.NodeID]
		if !ok {
			continue
		}
		for _, cid := range n.ChildIDs {
			childIdx, ok := idMap[cid]
			if !ok {
				continue
			}
			tree[parentIdx].Children = append(tree[parentIdx].Children, childIdx)
			tree[childIdx].ParentIdx = parentIdx
		}
	}

	// StaticText merge: collapse consecutive StaticText siblings into the first.
	for i := range tree {
		mergeStaticText(&tree, i)
	}

	return tree, findRoots(tree)
}

// applyProperties pulls checked/expanded/selected/disabled/required/level/url
// out of AXNode.Properties onto the TreeNode's hint fields.
func applyProperties(tn *TreeNode, props []AXProperty) {
	for _, p := range props {
		if p.Value == nil {
			continue
		}
		switch p.Name {
		case "level":
			var lv int
			if err := json.Unmarshal(p.Value.Value, &lv); err == nil {
				t := lv
				tn.Level = &t
			}
		case "checked":
			var s string
			if err := json.Unmarshal(p.Value.Value, &s); err == nil {
				tn.Checked = &s
			}
		case "expanded":
			var b bool
			if err := json.Unmarshal(p.Value.Value, &b); err == nil {
				tn.Expanded = &b
			}
		case "selected":
			var b bool
			if err := json.Unmarshal(p.Value.Value, &b); err == nil {
				tn.Selected = &b
			}
		case "disabled":
			var b bool
			if err := json.Unmarshal(p.Value.Value, &b); err == nil {
				tn.Disabled = &b
			}
		case "required":
			var b bool
			if err := json.Unmarshal(p.Value.Value, &b); err == nil {
				tn.Required = &b
			}
		case "url":
			var u string
			if err := json.Unmarshal(p.Value.Value, &u); err == nil {
				tn.URL = u
			}
		}
	}
}

// mergeStaticText folds consecutive StaticText children of node i into the
// first such child (append names with a space), marking the rest by clearing
// their Role so renderTree skips them.
func mergeStaticText(tree *[]TreeNode, i int) {
	kids := (*tree)[i].Children
	if len(kids) < 2 {
		return
	}
	first := -1
	for _, c := range kids {
		if (*tree)[c].Role == "StaticText" {
			first = c
			break
		}
	}
	if first == -1 {
		return
	}
	for _, c := range kids {
		if c == first {
			continue
		}
		if (*tree)[c].Role == "StaticText" && (*tree)[c].Name != "" {
			if (*tree)[first].Name != "" {
				(*tree)[first].Name += " "
			}
			(*tree)[first].Name += (*tree)[c].Name
			(*tree)[c].Role = "" // mark for render skip
		}
	}
}

func findRoots(tree []TreeNode) []int {
	var roots []int
	for i := range tree {
		if tree[i].ParentIdx == -1 {
			roots = append(roots, i)
		}
	}
	return roots
}
