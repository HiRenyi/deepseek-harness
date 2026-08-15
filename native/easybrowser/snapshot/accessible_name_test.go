package snapshot

import (
	"strings"
	"testing"

	"github.com/browser-mcp/bridge/cdp"
)

// TestAccessibleNameAriaLabelPriority (DM3): aria-label takes priority over
// aria-name, title, and subtree #text — mirroring Codex dom_cua_get_visible_dom.
func TestAccessibleNameAriaLabelPriority(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "BUTTON",
		Attributes: []string{"aria-label", "优先标签", "aria-name", "次级", "title", "提示"},
		Children:   []*cdp.DOMNode{{NodeType: 3, NodeValue: "子文本"}},
	}
	got := AccessibleName(node)
	if got != "优先标签" {
		t.Errorf("aria-label priority: got %q want \"优先标签\"", got)
	}
}

// TestAccessibleNameAriaNameOverTitle (DM3): when aria-label is absent,
// aria-name beats title and subtree text.
func TestAccessibleNameAriaNameOverTitle(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "BUTTON",
		Attributes: []string{"aria-name", "次级", "title", "提示"},
		Children:   []*cdp.DOMNode{{NodeType: 3, NodeValue: "子文本"}},
	}
	got := AccessibleName(node)
	if got != "次级" {
		t.Errorf("aria-name over title: got %q want \"次级\"", got)
	}
}

// TestAccessibleNameTitleOverSubtree (DM3): when aria-label/aria-name absent,
// title beats subtree #text.
func TestAccessibleNameTitleOverSubtree(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "BUTTON",
		Attributes: []string{"title", "提示文本"},
		Children:   []*cdp.DOMNode{{NodeType: 3, NodeValue: "子文本"}},
	}
	got := AccessibleName(node)
	if got != "提示文本" {
		t.Errorf("title over subtree: got %q want \"提示文本\"", got)
	}
}

// TestAccessibleNameSubtreeTextContent (DM3): a button whose label lives in a
// child #text node (no aria/title) surfaces the textContent — the card-title
// case exposed by CherryStudio against Tencent Docs (button with a
// <span>空白表格</span> child).
func TestAccessibleNameSubtreeTextContent(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "BUTTON",
		Attributes: []string{"class", "card"},
		Children: []*cdp.DOMNode{
			{NodeType: 1, NodeName: "SPAN", Children: []*cdp.DOMNode{
				{NodeType: 3, NodeValue: "空白表格"},
			}},
		},
	}
	got := AccessibleName(node)
	if got != "空白表格" {
		t.Errorf("subtree textContent fallback: got %q want \"空白表格\"", got)
	}
}

// TestAccessibleNameIframeContent (DM3): the textContent fallback descends into
// same-origin ContentDocument (DOM.getDocument pierce:true populates it), so an
// iframe card's title surfaces.
func TestAccessibleNameIframeContent(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "IFRAME",
		Attributes: []string{},
		ContentDocument: &cdp.DOMNode{
			NodeType: 1,
			NodeName: "#document",
			Children: []*cdp.DOMNode{
				{NodeType: 3, NodeValue: "iframe内文本"},
			},
		},
	}
	got := AccessibleName(node)
	if got != "iframe内文本" {
		t.Errorf("iframe contentDocument text: got %q want \"iframe内文本\"", got)
	}
}

// TestAccessibleNameLongTruncation (DM3): long textContent collapses whitespace
// and truncates by rune to accessibleNameMax with a trailing ellipsis.
func TestAccessibleNameLongTruncation(t *testing.T) {
	long := strings.Repeat("中文", 100) // 200 runes > 80
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "DIV",
		Attributes: []string{},
		Children:   []*cdp.DOMNode{{NodeType: 3, NodeValue: long}},
	}
	got := AccessibleName(node)
	runes := []rune(got)
	if len(runes) != accessibleNameMax+1 { // +1 for trailing ellipsis
		t.Errorf("truncation length: got %d runes want %d+1 ellipsis", len(runes), accessibleNameMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated name should end with ellipsis: %q", got)
	}
}

// TestAccessibleNameCollapsesWhitespace (DM3): internal whitespace runs
// (incl. newlines) collapse to single spaces.
func TestAccessibleNameCollapsesWhitespace(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "DIV",
		Attributes: []string{},
		Children:   []*cdp.DOMNode{{NodeType: 3, NodeValue: "  a\n\nb  c  "}},
	}
	got := AccessibleName(node)
	if got != "a b c" {
		t.Errorf("whitespace collapse: got %q want \"a b c\"", got)
	}
}

// TestAccessibleNameEmpty (DM3): a node with no aria/title/subtree-text returns
// "" (so formatNode omits the text= segment entirely).
func TestAccessibleNameEmpty(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:   1,
		NodeName:   "DIV",
		Attributes: []string{"class", "x"},
		Children:   []*cdp.DOMNode{{NodeType: 1, NodeName: "SPAN"}}, // empty child
	}
	got := AccessibleName(node)
	if got != "" {
		t.Errorf("empty name: got %q want \"\"", got)
	}
}

// TestFormatNodeEmitsText (DM3): formatNode appends `text=<name>` when the
// accessible name is non-empty, so the AI can grep for card titles by text.
func TestFormatNodeEmitsText(t *testing.T) {
	node := &cdp.DOMNode{
		NodeType:      1,
		NodeName:      "BUTTON",
		BackendNodeID: 77,
		Attributes:    []string{"class", "card"},
		Children: []*cdp.DOMNode{
			{NodeType: 3, NodeValue: "空白表格"},
		},
	}
	line := formatNode(node)
	if !strings.Contains(line, `text="空白表格"`) {
		t.Errorf("formatNode missing text= segment: %s", line)
	}
	if !strings.Contains(line, "node_id=77") {
		t.Errorf("formatNode lost node_id: %s", line)
	}
}
