package snapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/browser-mcp/bridge/cdp"
)

// BoxResolver resolves the bounding box (in page CSS coordinates) of a DOM
// node by its CDP BackendNodeID. It is the seam at which the pure-data
// snapshot package re-enters the CDP layer for viewport visibility checks.
//
// BoxBounds returns the min/max x/y of the node's border box and ok=true when
// the box is determinate and usable for viewport intersection. ok=false means
// "skip this node for visibility purposes" — mirroring the Codex rU
// `catch{return null}` semantics: CDP error, detached/hidden node, missing or
// short quad, non-finite coordinates, or zero-area boxes all yield ok=false.
// (mcp-context-slimming D3.)
type BoxResolver interface {
	BoxBounds(ctx context.Context, backendNodeID int) (minX, minY, maxX, maxY float64, ok bool)
}

// SnapshotResult holds the filtered DOM snapshot output.
type SnapshotResult struct {
	Lines []string `json:"lines"`
}

// String returns the snapshot as line-delimited text.
func (r *SnapshotResult) String() string {
	return strings.Join(r.Lines, "\n")
}

// Interactive HTML tags that should always be included.
var interactiveTags = map[string]bool{
	"a": true, "button": true, "details": true, "input": true,
	"option": true, "select": true, "summary": true, "textarea": true,
}

// Attributes that indicate interactivity.
var interactiveAttrs = map[string]bool{
	"contenteditable": true, "href": true, "onclick": true, "role": true, "tabindex": true,
}

// Attributes to include in output.
var outputAttrs = map[string]bool{
	"name": true, "aria-label": true, "aria-name": true, "data-testid": true,
	"placeholder": true, "role": true, "href": true, "type": true,
	"title": true, "alt": true, "value": true, "src": true,
	"id": true, "class": true,
}

// Boolean attributes that should be output as name-only when present.
var booleanAttrs = map[string]bool{
	"checked": true, "disabled": true, "multiple": true,
	"readonly": true, "required": true, "selected": true,
}

// Hidden elements that should be skipped along with their subtrees.
var hiddenTags = map[string]bool{
	"script": true, "style": true, "head": true, "meta": true,
	"link": true, "noscript": true, "template": true,
}

// FilterDOM walks the DOM tree rooted at root, keeps interactive/visible
// elements, and returns a SnapshotResult with one line per kept node.
//
// When viewport is non-nil AND resolver is non-nil, filterNode performs
// viewport visibility culling aligned with the Codex rU logic used by
// browser_snapshot_visible: a candidate node whose box is fully outside the
// viewport, zero-area, or non-finite is skipped (partial intersection is
// kept). When either viewport or resolver is nil, no viewport filtering is
// applied (full mode — backward-compatible with pre-D3 callers).
// (mcp-context-slimming D3: fixes the dead-code viewport param.)
func FilterDOM(ctx context.Context, root *cdp.DOMNode, viewport *cdp.ViewportRect, resolver BoxResolver) *SnapshotResult {
	result := &SnapshotResult{}
	filterNode(ctx, root, viewport, resolver, result)
	return result
}

// FilterIframeDOM walks the DOM tree rooted at root and emits ONLY interactive/
// output-attr nodes that live inside an iframe contentDocument subtree — the AX
// perception gap (AX can't give refs for unnamed generic cards inside iframes,
// e.g. docs.qq.com template cards "空白表格").
//
// Main-frame nodes are skipped: they are already represented in the AX tree
// (refs), so emitting them again would just bloat the dual-emit supplement.
// Shadow roots inside an iframe are still drilled (inIframe stays true).
//
// DQ (2026-06-24): browser_snapshot_ax appends this as a DOM supplement when
// its AX tree contains an Iframe node, so the LLM gets iframe-internal
// node_ids alongside AX refs without deciding to switch perception paths.
func FilterIframeDOM(root *cdp.DOMNode) *SnapshotResult {
	result := &SnapshotResult{}
	filterIframeNode(root, false, result)
	return result
}

// filterIframeNode mirrors filterNode but only emits nodes reached AFTER
// descending into a contentDocument (inIframe=true). Main-frame nodes are
// still traversed (so we can reach their iframe subtrees) but never emitted.
func filterIframeNode(node *cdp.DOMNode, inIframe bool, result *SnapshotResult) {
	if node == nil {
		return
	}

	// Skip hidden subtrees
	if isHidden(node) {
		return
	}

	// Descending into an iframe contentDocument flips inIframe on for the whole
	// subtree. Shadow roots do not change frame membership.
	if node.ContentDocument != nil {
		filterIframeNode(node.ContentDocument, true, result)
	}

	for _, sr := range node.ShadowRoots {
		filterIframeNode(sr, inIframe, result)
	}

	for _, child := range node.Children {
		filterIframeNode(child, inIframe, result)
	}

	// Only iframe-internal element nodes are candidates for emission.
	if !inIframe {
		return
	}
	if node.NodeType != 1 {
		return
	}
	if strings.HasPrefix(node.NodeName, "#") {
		return
	}

	if isInteractive(node) || hasOutputAttrs(node) {
		line := formatNode(node)
		if line != "" {
			result.Lines = append(result.Lines, line)
		}
	}
}

// filterNode recursively walks the DOM tree, adding matching nodes to result.
// When viewport and resolver are both non-nil, candidate nodes are culled by
// viewport visibility (Codex rU semantics): fully-outside, zero-area, or
// non-finite boxes are skipped; partial intersection is kept. Children are
// still recursed regardless of a node's own visibility so that off-screen
// parents can have on-screen descendants. (mcp-context-slimming D3.)
func filterNode(ctx context.Context, node *cdp.DOMNode, viewport *cdp.ViewportRect, resolver BoxResolver, result *SnapshotResult) {
	if node == nil {
		return
	}

	// Skip hidden subtrees
	if isHidden(node) {
		return
	}

	// Process contentDocument (iframes)
	if node.ContentDocument != nil {
		filterNode(ctx, node.ContentDocument, viewport, resolver, result)
	}

	// Process shadow roots
	for _, sr := range node.ShadowRoots {
		filterNode(ctx, sr, viewport, resolver, result)
	}

	// Process children
	for _, child := range node.Children {
		filterNode(ctx, child, viewport, resolver, result)
	}

	// Only include element nodes (nodeType 1)
	if node.NodeType != 1 {
		return
	}

	// Skip #document, #text, #comment etc.
	if strings.HasPrefix(node.NodeName, "#") {
		return
	}

	// Include if interactive or has output-worthy attributes
	if isInteractive(node) || hasOutputAttrs(node) {
		// D3: viewport visibility culling. When a viewport and resolver are
		// supplied, skip nodes whose box is fully outside the viewport,
		// zero-area, or non-finite (Codex rU). Partial intersection is kept.
		// A node with BackendNodeID <= 0 cannot be box-resolved (e.g. shadow
		// root wrappers) — keep it to avoid dropping legitimately-attached
		// subtree anchors that the box call cannot represent.
		if viewport != nil && resolver != nil && node.BackendNodeID > 0 {
			if !nodeInViewportBox(ctx, resolver, node.BackendNodeID, viewport) {
				return
			}
		}
		line := formatNode(node)
		if line != "" {
			result.Lines = append(result.Lines, line)
		}
	}
}

// nodeInViewportBox is the snapshot-package analogue of the mcp.nodeInViewport
// rU check: it resolves the node's border box via the BoxResolver and returns
// true only when the box is at least partially within the viewport. Returns
// false (skip the node, do NOT abort the snapshot) when:
//   - the resolver errors or reports ok=false (Codex `catch{return null}`),
//   - the box is fully outside the viewport,
//   - the box is zero-area,
//   - any coordinate is non-finite.
// Partial intersection (not fully outside) is kept, matching Codex rU.
func nodeInViewportBox(ctx context.Context, resolver BoxResolver, backendNodeID int, vp *cdp.ViewportRect) bool {
	minX, minY, maxX, maxY, ok := resolver.BoxBounds(ctx, backendNodeID)
	if !ok {
		return false
	}
	// Zero-area box (Codex rU: D<=x || w<=S).
	if maxX <= minX || maxY <= minY {
		return false
	}
	vpLeft := vp.X
	vpRight := vp.X + vp.Width
	vpTop := vp.Y
	vpBottom := vp.Y + vp.Height
	// Fully outside the viewport.
	if maxX <= vpLeft || minX >= vpRight || maxY <= vpTop || minY >= vpBottom {
		return false
	}
	return true
}

// isHidden returns true if the node or any ancestor attribute indicates it is hidden.
func isHidden(node *cdp.DOMNode) bool {
	tag := node.TagName()
	if hiddenTags[tag] {
		return true
	}
	attrs := node.GetAttributes()
	if attrs["aria-hidden"] == "true" {
		return true
	}
	if attrs["hidden"] != "" {
		return true
	}
	if style, ok := attrs["style"]; ok {
		if strings.Contains(style, "display:none") || strings.Contains(style, "display: none") {
			return true
		}
		if strings.Contains(style, "visibility:hidden") || strings.Contains(style, "visibility: hidden") {
			return true
		}
	}
	return false
}

// isInteractive returns true if the node is an interactive element.
func isInteractive(node *cdp.DOMNode) bool {
	tag := node.TagName()
	if interactiveTags[tag] {
		return true
	}
	attrs := node.GetAttributes()
	for attr := range interactiveAttrs {
		if _, ok := attrs[attr]; ok {
			return true
		}
	}
	// Role-based interactivity
	if role, ok := attrs["role"]; ok {
		switch role {
		case "button", "link", "checkbox", "radio", "textbox",
			"combobox", "menuitem", "tab", "switch", "searchbox",
			"spinbutton", "slider", "menuitemcheckbox", "menuitemradio":
			return true
		}
	}
	return false
}

// hasOutputAttrs returns true if the node has any attributes worth outputting.
func hasOutputAttrs(node *cdp.DOMNode) bool {
	attrs := node.GetAttributes()
	for attr := range outputAttrs {
		if _, ok := attrs[attr]; ok {
			return true
		}
	}
	return false
}

// formatNode formats a node as: node_id=123 tagName="button" aria-label="Submit"
func formatNode(node *cdp.DOMNode) string {
	tag := node.TagName()
	attrs := node.GetAttributes()

	parts := []string{fmt.Sprintf("node_id=%d", node.BackendNodeID)}

	// Add tag name
	parts = append(parts, fmt.Sprintf("tagName=%q", tag))

	// Add output attributes in a deterministic order
	attrOrder := []string{
		"name", "aria-label", "aria-name", "data-testid",
		"placeholder", "role", "href", "type", "title", "alt",
		"value", "src", "id", "class",
	}

	for _, key := range attrOrder {
		if val, ok := attrs[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%q", key, val))
		}
	}

	// Add boolean attributes
	for _, key := range []string{"checked", "disabled", "multiple", "readonly", "required", "selected"} {
		if _, ok := attrs[key]; ok {
			parts = append(parts, key)
		}
	}

	// DM (2026-06-23): accessible name so the AI can locate nodes whose label
	// lives in a child #text node (e.g. a card whose title is <span>空白表格</span>
	// rather than an aria-label). Priority mirrors Codex dom_cua_get_visible_dom:
	// aria-label > aria-name > title > subtree #text nodeValue (textContent).
	if name := AccessibleName(node); name != "" {
		parts = append(parts, fmt.Sprintf("text=%q", name))
	}

	return strings.Join(parts, " ")
}

// accessibleNameMax bounds the accessible name length; longer names are
// truncated with a trailing ellipsis to keep snapshot lines readable.
const accessibleNameMax = 80

// AccessibleName returns the accessible name for a DOM node, mirroring the
// ARIA computation priority used by Codex dom_cua_get_visible_dom:
// aria-label > aria-name > title > subtree #text nodeValue (textContent).
// filterNode skips #text nodes during traversal (NodeType != 1), so the
// textContent fallback collects them here without changing the main loop.
// Exported so the visible-DOM path (mcp.collectVisibleDOM) stays consistent
// with the full-filter path. (DM, 2026-06-23.)
func AccessibleName(node *cdp.DOMNode) string {
	attrs := node.GetAttributes()
	if v := strings.TrimSpace(attrs["aria-label"]); v != "" {
		return truncateText(v)
	}
	if v := strings.TrimSpace(attrs["aria-name"]); v != "" {
		return truncateText(v)
	}
	if v := strings.TrimSpace(attrs["title"]); v != "" {
		return truncateText(v)
	}
	if t := collectText(node); strings.TrimSpace(t) != "" {
		return truncateText(t)
	}
	return ""
}

// collectText concatenates all descendant #text nodeValue (textContent) of a
// node. Same-origin iframe content (ContentDocument, populated by
// DOM.getDocument pierce:true) is included so iframe card titles surface.
func collectText(node *cdp.DOMNode) string {
	if node == nil {
		return ""
	}
	var b strings.Builder
	if node.NodeType == 3 { // Node.TEXT_NODE
		b.WriteString(node.NodeValue)
	}
	for _, c := range node.Children {
		b.WriteString(collectText(c))
	}
	if node.ContentDocument != nil {
		b.WriteString(collectText(node.ContentDocument))
	}
	return b.String()
}

// truncateText collapses internal whitespace runs to single spaces and
// truncates by rune count to accessibleNameMax with a trailing ellipsis.
func truncateText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse runs of whitespace (incl. newlines) to a single space.
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	collapsed := b.String()
	r := []rune(collapsed)
	if len(r) > accessibleNameMax {
		return string(r[:accessibleNameMax]) + "…"
	}
	return collapsed
}
