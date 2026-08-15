package axtree

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/browser-mcp/bridge/cdp"
)

// Degrade judgment (RFC §8 / coverage-threshold change):
// We TRUST a non-empty AX tree that assigns at least one ref. Degrading only
// on failure / empty tree / zero assigned refs avoids mis-classifying simple
// pages (login/about/landing pages with few interactives, e.g. example.com) as
// "low coverage". An absolute interactive-ref threshold (the former COVERAGE_MIN=5)
// conflated "page has few interactives" with "AX is unreliable" and forced the
// AXTree primary path to degrade on exactly the simple pages it should serve.
// This also avoids the extra DOM.getDocument call a relative-coverage check
// would need. countInteractiveRefs is retained only for reporting/logging.

// fetchAXTree calls Accessibility.getFullAXTree. frameID=="" fetches the main
// (current) frame; a non-empty frameID fetches that child frame's AX tree
// (same-origin only — cross-origin iframes require a dedicated target session
// which the bridge's sessionless CDPClient cannot attach to; such requests fail
// and the caller isolates the error). Mirrors agent-browser take_snapshot's
// resolve_ax_session same-origin branch (frameId param on parent session).
func fetchAXTree(ctx context.Context, c CDPClient, frameID string) ([]AXNode, error) {
	_, _ = c.Call(ctx, "Accessibility.enable", mustMarshalRaw(struct{}{}))
	var params any
	if frameID != "" {
		params = struct {
			FrameID string `json:"frameId"`
		}{frameID}
	} else {
		params = struct{}{}
	}
	resp, err := c.Call(ctx, "Accessibility.getFullAXTree", mustMarshalRaw(params))
	if err != nil {
		return nil, fmt.Errorf("getFullAXTree: %w", err)
	}
	if resp == nil || resp.Error != nil {
		return nil, fmt.Errorf("getFullAXTree error: %v", resp)
	}
	var out struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("parse AX tree: %w", err)
	}
	return out.Nodes, nil
}

// assignRefs assigns stable ref IDs to qualifying nodes and records them in
// the RefMap. Qualification (RFC §5):
//   - INTERACTIVE_ROLES -> always
//   - CONTENT_ROLES with non-empty name -> yes
//   - cursor-interactive hit -> yes
//
// nth disambiguation is recorded in RefMap only (never rendered). Returns the
// set of ref ids present in this frame (for the caller to union across frames
// and Prune once at the top). Does NOT Prune — per-frame Prune would wipe
// prior frames' refs; the top-level GetAXSnapshot Prunes the union keep set so
// stale refs across the whole page are cleaned in one pass. Mirrors
// agent-browser threading ref_map.next_ref across frames without pruning.
func assignRefs(tree []TreeNode, cursor map[int64]CursorInfo, m *RefMap) map[string]bool {
	tr := newRoleNameTracker()
	type prov struct {
		role, name string
		idx        int
	}
	provisional := make([]prov, len(tree))

	// Pass 1: track + decide qualification + assign provisional nth.
	for i := range tree {
		tn := &tree[i]
		_, cursorHit := cursor[tn.BackendNodeID]
		qual := INTERACTIVE_ROLES[tn.Role] || (CONTENT_ROLES[tn.Role] && tn.Name != "") || cursorHit
		if !qual {
			continue
		}
		nth := tr.track(tn.Role, tn.Name)
		provisional[i] = prov{tn.Role, tn.Name, nth}
	}
	// Pass 2: finalize nth + allocate ref + set backendNodeId.
	keep := map[string]bool{}
	for i := range tree {
		pv := provisional[i]
		if pv.role == "" { // unqualified
			continue
		}
		nth := tr.finalize(pv.role, pv.name, pv.idx)
		refID := m.Assign(pv.role, pv.name, nth, tree[i].FrameID)
		tree[i].RefID = refID
		tree[i].HasRef = true
		tree[i].Nth = nth
		m.SetBackend(refID, tree[i].BackendNodeID)
		keep[refID] = true
	}
	return keep
}

// countInteractiveRefs counts nodes that received a ref via an interactive role.
func countInteractiveRefs(tree []TreeNode) int {
	n := 0
	for i := range tree {
		if tree[i].HasRef && INTERACTIVE_ROLES[tree[i].Role] {
			n++
		}
	}
	return n
}

// invisibleName reports whether name is only invisible chars (render skips it).
func invisibleName(s string) bool {
	return strings.TrimSpace(s) == ""
}

// ---- rendering (RFC §6) ----

// renderTree emits the tree as text in the agent-browser format:
//
//	<indent>- <role> "<json-escaped-name>" [attrs, ref=eN] [: <value>]
//
// Skip rules: empty role, generic-without-ref with <=1 child, StaticText with
// only invisible chars, RootWebArea/WebArea (pass-through children).
func renderTree(tree []TreeNode, roots []int, mode string) string {
	var b strings.Builder
	for _, r := range roots {
		renderNode(&b, tree, r, 0, mode)
	}
	out := b.String()
	if mode == "compact" || mode == "interactive" || mode == "" {
		out = compactTree(out, mode)
	}
	return out
}

func renderNode(b *strings.Builder, tree []TreeNode, idx, depth int, mode string) {
	tn := tree[idx]
	// Pass-through cases: emit nothing for this node, recurse children.
	if tn.Role == "" || tn.Role == "RootWebArea" || tn.Role == "WebArea" {
		for _, c := range tn.Children {
			renderNode(b, tree, c, depth, mode)
		}
		return
	}
	if tn.Role == "StaticText" && invisibleName(tn.Name) {
		return
	}
	if tn.Role == "generic" && !tn.HasRef && len(tn.Children) <= 1 {
		for _, c := range tn.Children {
			renderNode(b, tree, c, depth, mode)
		}
		return
	}
	line := formatLine(tn, depth)
	if line != "" {
		fmt.Fprintln(b, line)
	}
	for _, c := range tn.Children {
		renderNode(b, tree, c, depth+1, mode)
	}
}

// formatLine builds one rendered line with precise attribute order:
// level/checked/expanded/selected/disabled/required/ref/url. nth is intentionally
// NOT rendered (only recorded internally).
func formatLine(tn TreeNode, depth int) string {
	indent := strings.Repeat("  ", depth)
	var attrs []string
	if tn.Level != nil {
		attrs = append(attrs, fmt.Sprintf("level=%d", *tn.Level))
	}
	if tn.Checked != nil {
		attrs = append(attrs, fmt.Sprintf("checked=%s", *tn.Checked))
	}
	if tn.Expanded != nil {
		attrs = append(attrs, fmt.Sprintf("expanded=%t", *tn.Expanded))
	}
	if tn.Selected != nil {
		attrs = append(attrs, fmt.Sprintf("selected=%t", *tn.Selected))
	}
	if tn.Disabled != nil {
		attrs = append(attrs, fmt.Sprintf("disabled=%t", *tn.Disabled))
	}
	if tn.Required != nil {
		attrs = append(attrs, fmt.Sprintf("required=%t", *tn.Required))
	}
	if tn.HasRef {
		attrs = append(attrs, "ref="+tn.RefID)
	}
	if tn.URL != "" {
		attrs = append(attrs, "url="+tn.URL)
	}
	name := jsonEscapeString(tn.Name)
	line := fmt.Sprintf("%s- %s %s", indent, tn.Role, quoted(name))
	if len(attrs) > 0 {
		line += " [" + strings.Join(attrs, ", ") + "]"
	}
	if tn.ValueText != "" && tn.ValueText != tn.Name {
		line += ": " + tn.ValueText
	}
	return line
}

// quoted returns a JSON-quoted string (empty name -> "").
func quoted(s string) string { return string(mustMarshalRaw(s)) }

// jsonEscapeString returns the inner content (without surrounding quotes) of a
// JSON-escaped string, used so we can format `"name"` uniformly.
func jsonEscapeString(s string) string {
	q := quoted(s)
	return q[1 : len(q)-1]
}

// compactTree keeps lines that contain "ref=" or a value (": ") plus their
// ancestors (lines with strictly smaller indent, up to indent 0). Mirrors
// snapshot.rs compact_tree.
func compactTree(rendered, mode string) string {
	lines := strings.Split(rendered, "\n")
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if strings.Contains(l, "ref=") || (strings.Contains(l, ": ") && !strings.Contains(l, "[ref=")) {
			keep[i] = true
		}
	}
	// Mark ancestors: any earlier line with strictly smaller indent than a kept line.
	for i, l := range lines {
		if !keep[i] {
			continue
		}
		myIndent := countIndent(l)
		for j := i - 1; j >= 0; j-- {
			if countIndent(lines[j]) < myIndent {
				keep[j] = true
				myIndent = countIndent(lines[j]) // ascend only
			}
		}
	}
	var out []string
	for i, l := range lines {
		if l == "" {
			continue
		}
		if keep[i] {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return "(no interactive elements)"
	}
	return strings.Join(out, "\n")
}

func countIndent(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' {
			n++
		} else {
			break
		}
	}
	return n / 2
}

// ---- orchestrator + degradation (RFC §8) ----

// AXResult is the output of GetAXSnapshot: the rendered tree, the count of
// interactive refs found, and whether AX should be considered failed/sparse.
// The caller (MCP layer) owns the DOM degrade decision because it holds the
// GetVisibleDOM dependency (out of axtree's reach to avoid a import cycle).
type AXResult struct {
	Text            string
	InteractiveRefs int
	DegradedReason  string // "" = healthy; otherwise why degrade
	// UnexpandedIframes is the count of Iframe nodes whose child content could
	// NOT be drilled (cross-origin, describeNode failure, or empty child tree).
	// Diagnostic only — the DN hint is driven by IframesPresent.
	// IframesPresent is the count of EVERY Iframe role node in the main frame's
	// AX tree, regardless of drill outcome. The MCP handler surfaces a
	// switch-to-DOM hint when > 0: even when DL drills a same-origin iframe
	// successfully, iframe-internal interactive elements are often unnamed
	// generic divs (e.g. template cards) that the AX whitelist filters out, so
	// the LLM must retry with browser_snapshot (DOM pierce:true) which exposes
	// them via text=. (DN, 2026-06-23; broadened 2026-06-24 per real-machine
	// docs.qq.com regression where 空白表格 cards were unnamed generics.)
	UnexpandedIframes int
	IframesPresent   int
}

// GetAXSnapshot runs the full AX pipeline and returns coverage info. It does
// NOT perform DOM degradation itself (that needs mcp.GetVisibleDOM, out of
// axtree's dependency reach). On AX failure/sparse it returns a DegradedReason.
//
// DL (2026-06-23): drills child iframes. The main frame's getFullAXTree only
// returns the current frame; Iframe role nodes are leaf placeholders. For each
// Iframe with a ref + backendNodeId, resolve the child frame ID (DOM.describeNode
// → contentDocument.frameId) and snapshot it via getFullAXTree({frameId}), then
// stitch the child's rendered text after the Iframe line. Same-origin only
// (cross-origin needs a dedicated target session the bridge can't attach to —
// such requests fail and are silently skipped, matching agent-browser). Only
// recurses from the main frame (no iframe-within-iframe expansion) to bound
// depth. Mirrors agent-browser snapshot.rs:490-556.
func GetAXSnapshot(ctx context.Context, c CDPClient, m *RefMap, opts SnapshotOpts) (AXResult, error) {
	// AX path never calls DOM.getDocument (which implicitly enables DOM), so
	// the DOM.performSearch/describeNode calls in cursor detection and iframe
	// drilling would fail with "DOM agent is not enabled". Idempotent enable,
	// mirroring fetchAXTree's Accessibility.enable (line 29). The bridge uses a
	// sessionless CDPClient and Page.navigate can reset domain state, so there
	// is no reliable "already enabled" flag — enable every snapshot. Observed:
	// 363 performSearch "not enabled" errors per session before this fix.
	_, _ = c.Call(ctx, "DOM.enable", mustMarshalRaw(struct{}{}))

	cursor, _ := findCursorInteractiveElements(ctx, c)

	tree, _, text, keep, irefs, err := snapshotFrame(ctx, c, m, opts, "", cursor)
	if err != nil {
		return AXResult{DegradedReason: "getFullAXTree failed: " + err.Error()}, nil
	}
	if len(tree) == 0 {
		return AXResult{DegradedReason: "empty AX tree"}, nil
	}
	totalRefs := 0
	for i := range tree {
		if tree[i].HasRef {
			totalRefs++
		}
	}
	if totalRefs == 0 {
		return AXResult{InteractiveRefs: irefs, DegradedReason: "degenerate AX tree: 0 refs"}, nil
	}

	// Drill child iframes (main frame only). visited bounds recursion on
	// frameId (defensive — same-origin child frames are already single-level).
	stitched := text
	visited := map[string]bool{"": true}
	var unexpandedIframes int
	var iframesPresent int
	for i := range tree {
		tn := tree[i]
		if tn.Role != "Iframe" {
			continue
		}
		// Every Iframe node present in the main tree bumps IframesPresent so
		// the DN hint fires unconditionally — iframe-internal targets are often
		// unnamed generics invisible to AX even when drilling succeeds.
		iframesPresent++
		if !tn.HasRef || tn.BackendNodeID == 0 {
			continue
		}
		childFid, ferr := resolveIframeFrameID(ctx, c, tn.BackendNodeID)
		if ferr != nil || childFid == "" || visited[childFid] {
			unexpandedIframes++
			continue
		}
		visited[childFid] = true
		_, _, childText, childKeep, childIrefs, cerr := snapshotFrame(ctx, c, m, opts, childFid, cursor)
		if cerr != nil {
			unexpandedIframes++
			continue // error isolation: cross-origin / failed child stays as Iframe line
		}
		if childText == "" || childText == "(no interactive elements)" {
			unexpandedIframes++
			continue
		}
		for k := range childKeep {
			keep[k] = true
		}
		irefs += childIrefs
		stitched = stitchChildAfterIframe(stitched, tn.RefID, childText)
	}

	// Prune once across all frames: refs not seen this snapshot are stale.
	m.Prune(keep)

	return AXResult{Text: stitched, InteractiveRefs: irefs, UnexpandedIframes: unexpandedIframes, IframesPresent: iframesPresent}, nil
}

// snapshotFrame fetches, builds, assigns refs (no Prune), and renders one
// frame's AX tree. frameID=="" = main frame. Returns the tree (for the caller
// to drill Iframe nodes), rendered text, this frame's keep set (for union
// Prune at the top), and interactive-ref count. No Prune so the caller can
// recurse across frames and Prune once.
func snapshotFrame(ctx context.Context, c CDPClient, m *RefMap, opts SnapshotOpts, frameID string, cursor map[int64]CursorInfo) (tree []TreeNode, roots []int, text string, keep map[string]bool, irefs int, err error) {
	nodes, err := fetchAXTree(ctx, c, frameID)
	if err != nil {
		return nil, nil, "", nil, 0, err
	}
	if len(nodes) == 0 {
		return nil, nil, "", nil, 0, fmt.Errorf("empty AX tree")
	}
	tree, roots = buildTree(nodes)
	promoteHiddenInputs(tree, cursor)
	keep = assignRefs(tree, cursor, m)
	irefs = countInteractiveRefs(tree)
	text = renderTree(tree, roots, opts.Mode)
	return tree, roots, text, keep, irefs, nil
}

// resolveIframeFrameID resolves the child frame ID for an iframe element given
// its backendNodeId, via DOM.describeNode. Tries node.contentDocument.frameId
// first (standard for iframes — contentDocument is the iframe's child document,
// whose frameId IS the child frame), then node.frameId as fallback. Mirrors
// agent-browser resolve_iframe_frame_id (snapshot.rs:575-607).
func resolveIframeFrameID(ctx context.Context, c CDPClient, backendNodeID int64) (string, error) {
	params := mustMarshalRaw(struct {
		BackendNodeID int64 `json:"backendNodeId"`
	}{backendNodeID})
	resp, err := c.Call(ctx, "DOM.describeNode", params)
	if err != nil {
		return "", fmt.Errorf("describeNode: %w", err)
	}
	if resp == nil || resp.Error != nil {
		return "", fmt.Errorf("describeNode error: %v", resp)
	}
	var out struct {
		Node *cdp.DOMNode `json:"node"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return "", err
	}
	if out.Node == nil {
		return "", fmt.Errorf("describeNode: no node")
	}
	if out.Node.ContentDocument != nil && out.Node.ContentDocument.FrameID != "" {
		return out.Node.ContentDocument.FrameID, nil
	}
	if out.Node.FrameID != "" {
		return out.Node.FrameID, nil
	}
	return "", fmt.Errorf("no frameId on iframe node")
}

// stitchChildAfterIframe inserts childText (a child frame's rendered AX
// snapshot) immediately after the Iframe line identified by refID, indented one
// level deeper than the Iframe line. Mirrors agent-browser snapshot.rs:527-556.
func stitchChildAfterIframe(output, refID, childText string) string {
	marker := "[ref=" + refID + "]"
	pos := strings.Index(output, marker)
	if pos < 0 {
		return output
	}
	// End of the Iframe line (next newline, or end of output).
	lineEnd := pos + len(marker)
	if nl := strings.IndexByte(output[lineEnd:], '\n'); nl >= 0 {
		lineEnd += nl
	} else {
		lineEnd = len(output)
	}
	// Indent of the Iframe line.
	lineStart := 0
	if nl := strings.LastIndexByte(output[:pos], '\n'); nl >= 0 {
		lineStart = nl + 1
	}
	iframeLine := output[lineStart:lineEnd]
	iframeIndent := len(iframeLine) - len(strings.TrimLeft(iframeLine, " "))
	prefix := strings.Repeat(" ", iframeIndent+2)

	childText = strings.TrimSuffix(childText, "\n")
	var b strings.Builder
	for _, line := range strings.Split(childText, "\n") {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	indented := b.String()

	if lineEnd == len(output) {
		return output + "\n" + indented
	}
	return output[:lineEnd+1] + indented + output[lineEnd+1:]
}
