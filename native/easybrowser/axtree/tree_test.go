package axtree

import "testing"

func TestBuildTreeParentChild(t *testing.T) {
	nodes := []AXNode{
		{NodeID: "1", Role: val("RootWebArea"), ChildIDs: []string{"2", "3"}},
		{NodeID: "2", Role: val("button"), Name: valS("OK"), ParentID: "1", BackendDOMNodeID: 10},
		{NodeID: "3", Role: val("textbox"), Name: valS("User"), ParentID: "1", BackendDOMNodeID: 11},
	}
	tree, roots := buildTree(nodes)
	if len(roots) != 1 || tree[roots[0]].Role != "RootWebArea" {
		t.Fatalf("roots=%v", roots)
	}
	btn := indexOf(t, tree, "button")
	if tree[btn].BackendNodeID != 10 || tree[btn].ParentIdx != roots[0] {
		t.Errorf("button wiring wrong: %+v", tree[btn])
	}
}

func TestBuildTreeStaticTextMerge(t *testing.T) {
	nodes := []AXNode{
		{NodeID: "1", Role: val("generic"), ChildIDs: []string{"2", "3", "4"}},
		{NodeID: "2", Role: val("StaticText"), Name: valS("Hello"), ParentID: "1"},
		{NodeID: "3", Role: val("StaticText"), Name: valS("World"), ParentID: "1"},
		{NodeID: "4", Role: val("button"), Name: valS("Go"), ParentID: "1"},
	}
	tree, _ := buildTree(nodes)
	// First StaticText retains merged name; find the surviving StaticText.
	var st int = -1
	for i, n := range tree {
		if n.Role == "StaticText" {
			st = i
			break
		}
	}
	if st == -1 {
		t.Fatalf("no StaticText node found")
	}
	if tree[st].Name != "Hello World" {
		t.Errorf("merged name=%q want 'Hello World'", tree[st].Name)
	}
}

// helpers to build AXValue quickly.
func val(role string) *AXValue { return &AXValue{Type: "role", Value: raw(`"` + role + `"`)} }
func valS(s string) *AXValue  { return &AXValue{Type: "string", Value: raw(`"` + s + `"`)} }
func raw(s string) []byte     { return []byte(s) }

func indexOf(t *testing.T, tree []TreeNode, want string) int {
	t.Helper()
	for i, n := range tree {
		if n.Role == want || n.Name == want {
			return i
		}
	}
	t.Fatalf("node %q not found", want)
	return -1
}
