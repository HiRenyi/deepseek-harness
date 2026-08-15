package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/cdp"
)

func TestBasicInteractiveElements(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{
						NodeType:      1,
						NodeName:      "BUTTON",
						BackendNodeID: 10,
						Attributes:    []string{"aria-label", "Submit"},
					},
					{
						NodeType:      1,
						NodeName:      "A",
						BackendNodeID: 11,
						Attributes:    []string{"href", "https://example.com"},
					},
					{
						NodeType:      1,
						NodeName:      "INPUT",
						BackendNodeID: 12,
						Attributes:    []string{"type", "text", "placeholder", "Enter name"},
					},
					{
						NodeType:      1,
						NodeName:      "DIV",
						BackendNodeID: 13,
						Attributes:    []string{"data-testid", "container"},
					},
				},
			},
		},
	}

	result := FilterDOM(context.Background(), root, nil, nil)
	output := result.String()

	// Should include button
	if !strings.Contains(output, `node_id=10`) || !strings.Contains(output, `tagName="button"`) {
		t.Errorf("missing button in output: %s", output)
	}
	// Should include anchor
	if !strings.Contains(output, `node_id=11`) || !strings.Contains(output, `tagName="a"`) {
		t.Errorf("missing anchor in output: %s", output)
	}
	// Should include input
	if !strings.Contains(output, `node_id=12`) || !strings.Contains(output, `tagName="input"`) {
		t.Errorf("missing input in output: %s", output)
	}
	// Should include div with data-testid
	if !strings.Contains(output, `node_id=13`) || !strings.Contains(output, `data-testid="container"`) {
		t.Errorf("missing div with data-testid in output: %s", output)
	}
}

func TestHiddenElements(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{
						NodeType:      1,
						NodeName:      "SCRIPT",
						BackendNodeID: 20,
						Children: []*cdp.DOMNode{
							{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 21},
						},
					},
					{
						NodeType:      1,
						NodeName:      "STYLE",
						BackendNodeID: 22,
					},
					{
						NodeType:      1,
						NodeName:      "DIV",
						BackendNodeID: 23,
						Attributes:    []string{"aria-hidden", "true"},
						Children: []*cdp.DOMNode{
							{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 24},
						},
					},
					{
						NodeType:      1,
						NodeName:      "BUTTON",
						BackendNodeID: 25,
						Attributes:    []string{"aria-label", "Visible"},
					},
				},
			},
		},
	}

	result := FilterDOM(context.Background(), root, nil, nil)
	output := result.String()

	// Script and its children should be hidden
	if strings.Contains(output, "node_id=20") {
		t.Errorf("script element should be hidden: %s", output)
	}
	if strings.Contains(output, "node_id=21") {
		t.Errorf("button inside script should be hidden: %s", output)
	}
	// Style should be hidden
	if strings.Contains(output, "node_id=22") {
		t.Errorf("style element should be hidden: %s", output)
	}
	// aria-hidden div and its children should be hidden
	if strings.Contains(output, "node_id=23") {
		t.Errorf("aria-hidden div should be hidden: %s", output)
	}
	if strings.Contains(output, "node_id=24") {
		t.Errorf("button inside aria-hidden div should be hidden: %s", output)
	}
	// Visible button should be present
	if !strings.Contains(output, "node_id=25") {
		t.Errorf("visible button should be present: %s", output)
	}
}

func TestRoleBasedInteractive(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{
						NodeType:      1,
						NodeName:      "DIV",
						BackendNodeID: 30,
						Attributes:    []string{"role", "button", "aria-label", "Custom Button"},
					},
					{
						NodeType:      1,
						NodeName:      "SPAN",
						BackendNodeID: 31,
						Attributes:    []string{"role", "link", "href", "#"},
					},
					{
						NodeType:      1,
						NodeName:      "SPAN",
						BackendNodeID: 32,
						// No role, no interactive attrs — should be excluded
					},
				},
			},
		},
	}

	result := FilterDOM(context.Background(), root, nil, nil)
	output := result.String()

	// Role=button div should be present
	if !strings.Contains(output, "node_id=30") || !strings.Contains(output, `role="button"`) {
		t.Errorf("role=button div should be present: %s", output)
	}
	// Role=link span should be present
	if !strings.Contains(output, "node_id=31") {
		t.Errorf("role=link span should be present: %s", output)
	}
	// Plain span without role should not be present
	if strings.Contains(output, "node_id=32") {
		t.Errorf("plain span should not be present: %s", output)
	}
}

func TestFilterIframeDOMSkipsMainFrameEmitsIframeInternal(t *testing.T) {
	// Main frame has a BUTTON (main-btn) and an IFRAME whose contentDocument
	// holds an unnamed generic div.card with a child #text "空白表格" plus a
	// BUTTON (inner). FilterIframeDOM must emit ONLY the iframe-internal nodes
	// (the AX perception gap) and skip the main-frame button (AX covers it).
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{
						NodeType:      1,
						NodeName:      "BUTTON",
						BackendNodeID: 10,
						Attributes:    []string{"aria-label", "main-btn"},
					},
					{
						NodeType:      1,
						NodeName:      "IFRAME",
						BackendNodeID: 40,
						ContentDocument: &cdp.DOMNode{
							NodeType: 1,
							NodeName: "#document",
							Children: []*cdp.DOMNode{
								{
									NodeType:      1,
									NodeName:      "DIV",
									BackendNodeID: 1408,
									Attributes:    []string{"class", "card--2L0vJ"},
									Children: []*cdp.DOMNode{
										{NodeType: 3, NodeValue: "空白表格"},
									},
								},
								{
									NodeType:      1,
									NodeName:      "BUTTON",
									BackendNodeID: 42,
									Attributes:    []string{"aria-label", "inner"},
								},
							},
						},
					},
				},
			},
		},
	}

	result := FilterIframeDOM(root)
	output := result.String()

	// Iframe-internal generic card surfaces with text= (AX gap this fills).
	if !strings.Contains(output, "node_id=1408") {
		t.Errorf("iframe-internal card node missing: %s", output)
	}
	if !strings.Contains(output, `text="空白表格"`) {
		t.Errorf("iframe-internal card accessible name (text=) missing: %s", output)
	}
	// Iframe-internal button surfaces too.
	if !strings.Contains(output, "node_id=42") {
		t.Errorf("iframe-internal button missing: %s", output)
	}
	// Main-frame button must be SKIPPED (already in AX tree).
	if strings.Contains(output, "node_id=10") {
		t.Errorf("main-frame button should be skipped to avoid bloat: %s", output)
	}
}

func TestFilterIframeDOMNoIframeIsEmpty(t *testing.T) {
	// No iframe -> no iframe-internal nodes -> empty result (main frame skipped).
	root := &cdp.DOMNode{
		NodeType: 1, NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{NodeType: 1, NodeName: "BODY", BackendNodeID: 1, Children: []*cdp.DOMNode{
				{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 5, Attributes: []string{"aria-label", "x"}},
			}},
		},
	}
	if got := FilterIframeDOM(root); len(got.Lines) != 0 {
		t.Errorf("expected no lines without iframe, got %d: %v", len(got.Lines), got.Lines)
	}
}

func TestIframeContentDocument(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{
						NodeType:      1,
						NodeName:      "IFRAME",
						BackendNodeID: 40,
						ContentDocument: &cdp.DOMNode{
							NodeType: 1,
							NodeName: "#document",
							Children: []*cdp.DOMNode{
								{
									NodeType:      1,
									NodeName:      "BUTTON",
									BackendNodeID: 41,
									Attributes:    []string{"aria-label", "Inner Button"},
								},
							},
						},
					},
				},
			},
		},
	}

	result := FilterDOM(context.Background(), root, nil, nil)
	output := result.String()

	// Button inside iframe contentDocument should be present
	if !strings.Contains(output, "node_id=41") {
		t.Errorf("button inside iframe should be present: %s", output)
	}
	if !strings.Contains(output, `aria-label="Inner Button"`) {
		t.Errorf("inner button should have aria-label: %s", output)
	}
}

func TestBooleanAttributes(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{
						NodeType:      1,
						NodeName:      "INPUT",
						BackendNodeID: 50,
						Attributes:    []string{"type", "checkbox", "checked", "", "disabled", ""},
					},
					{
						NodeType:      1,
						NodeName:      "SELECT",
						BackendNodeID: 51,
						Attributes:    []string{"required", "", "multiple", ""},
					},
				},
			},
		},
	}

	result := FilterDOM(context.Background(), root, nil, nil)
	output := result.String()

	// Boolean attributes should appear as name-only
	if !strings.Contains(output, "node_id=50") {
		t.Errorf("checkbox input should be present: %s", output)
	}
	// Check that "checked" appears in the output for node 50's line
	lines := strings.Split(output, "\n")
	var checkboxLine string
	for _, line := range lines {
		if strings.Contains(line, "node_id=50") {
			checkboxLine = line
			break
		}
	}
	if checkboxLine == "" {
		t.Fatalf("no line found for node_id=50")
	}
	if !strings.Contains(checkboxLine, "checked") {
		t.Errorf("checkbox should have 'checked' attribute: %s", checkboxLine)
	}
	if !strings.Contains(checkboxLine, "disabled") {
		t.Errorf("checkbox should have 'disabled' attribute: %s", checkboxLine)
	}
}

func TestGetAttributesHelper(t *testing.T) {
	node := &cdp.DOMNode{
		Attributes: []string{"id", "main", "class", "container", "data-testid", "root"},
	}
	attrs := node.GetAttributes()

	if attrs["id"] != "main" {
		t.Errorf("expected id=main, got %s", attrs["id"])
	}
	if attrs["class"] != "container" {
		t.Errorf("expected class=container, got %s", attrs["class"])
	}
	if attrs["data-testid"] != "root" {
		t.Errorf("expected data-testid=root, got %s", attrs["data-testid"])
	}
}

func TestTagNameHelper(t *testing.T) {
	node := &cdp.DOMNode{NodeName: "DIV"}
	if node.TagName() != "div" {
		t.Errorf("expected lowercase 'div', got %s", node.TagName())
	}

	node2 := &cdp.DOMNode{NodeName: "INPUT"}
	if node2.TagName() != "input" {
		t.Errorf("expected lowercase 'input', got %s", node2.TagName())
	}
}

// fakeBoxResolver is a test BoxResolver that returns scripted boxes per
// BackendNodeID, letting viewport filtering be exercised without a live CDP
// connection. (mcp-context-slimming D3.)
type fakeBoxResolver struct {
	boxes map[int][4]float64 // backendNodeID -> {minX, minY, maxX, maxY}
	ok    map[int]bool
}

func (f *fakeBoxResolver) BoxBounds(_ context.Context, bid int) (minX, minY, maxX, maxY float64, ok bool) {
	b, present := f.boxes[bid]
	if !present {
		return 0, 0, 0, 0, false
	}
	return b[0], b[1], b[2], b[3], f.ok[bid]
}

// TestViewportFilteringSkipsOffscreenNodes verifies that filterNode actually
// reads the viewport param (D3 dead-code fix): a node fully outside the
// viewport is skipped, a node partially intersecting is kept, and full:true
// (nil viewport) returns everything.
func TestViewportFilteringSkipsOffscreenNodes(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 60, Attributes: []string{"aria-label", "in-view"}},
					{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 61, Attributes: []string{"aria-label", "off-screen"}},
					{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 62, Attributes: []string{"aria-label", "partial"}},
				},
			},
		},
	}
	viewport := &cdp.ViewportRect{X: 0, Y: 0, Width: 800, Height: 600}
	resolver := &fakeBoxResolver{
		boxes: map[int][4]float64{
			60: {10, 10, 100, 40},   // fully inside
			61: {900, 700, 950, 720}, // fully outside (right+below)
			62: {750, 500, 900, 550}, // partial intersection (bottom-right corner)
		},
		ok: map[int]bool{60: true, 61: true, 62: true},
	}

	got := FilterDOM(context.Background(), root, viewport, resolver).String()
	if !strings.Contains(got, "node_id=60") {
		t.Errorf("in-view node should be present: %s", got)
	}
	if strings.Contains(got, "node_id=61") {
		t.Errorf("off-screen node should be skipped: %s", got)
	}
	if !strings.Contains(got, "node_id=62") {
		t.Errorf("partial-intersection node should be kept: %s", got)
	}

	// full mode (nil viewport) returns all three.
	full := FilterDOM(context.Background(), root, nil, resolver).String()
	for _, id := range []string{"node_id=60", "node_id=61", "node_id=62"} {
		if !strings.Contains(full, id) {
			t.Errorf("full mode should include %s: %s", id, full)
		}
	}
}

// TestViewportFilteringSkipsNonFiniteAndZeroArea verifies the rU-aligned
// guards: a node whose box resolver reports ok=false (CDP error / non-finite
// / zero-area) is skipped, not fatal.
func TestViewportFilteringSkipsNonFiniteAndZeroArea(t *testing.T) {
	root := &cdp.DOMNode{
		NodeType: 1,
		NodeName: "HTML",
		Children: []*cdp.DOMNode{
			{
				NodeType:      1,
				NodeName:      "BODY",
				BackendNodeID: 1,
				Children: []*cdp.DOMNode{
					{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 70, Attributes: []string{"aria-label", "zero-area"}},
					{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 71, Attributes: []string{"aria-label", "in-view"}},
					{NodeType: 1, NodeName: "BUTTON", BackendNodeID: 72, Attributes: []string{"aria-label", "unresolvable"}},
				},
			},
		},
	}
	viewport := &cdp.ViewportRect{X: 0, Y: 0, Width: 800, Height: 600}
	resolver := &fakeBoxResolver{
		boxes: map[int][4]float64{
			70: {10, 10, 10, 10}, // zero-area
			71: {10, 10, 100, 40}, // healthy
			// 72 absent -> resolver returns ok=false (CDP error)
		},
		ok: map[int]bool{70: true, 71: true},
	}
	got := FilterDOM(context.Background(), root, viewport, resolver).String()
	if strings.Contains(got, "node_id=70") {
		t.Errorf("zero-area node should be skipped: %s", got)
	}
	if !strings.Contains(got, "node_id=71") {
		t.Errorf("healthy node should be present: %s", got)
	}
	if strings.Contains(got, "node_id=72") {
		t.Errorf("unresolvable node should be skipped (not fatal): %s", got)
	}
}
