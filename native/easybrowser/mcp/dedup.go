package mcp

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"

	"github.com/browser-mcp/bridge/cdp"
)

// Snapshot size caps (mcp-context-slimming D4). Applied to browser_snapshot,
// browser_snapshot_visible, and browser_snapshot_ax output as a soft guard
// against extreme pages blowing up the model context. Tunable constants so
// DV can calibrate against real OA-class pages.
const (
	maxSnapshotLines = 2000
	maxSnapshotBytes = 50 * 1024 // 50 KiB
)

// truncateSnapshot applies the D4 line+byte soft cap to a snapshot string. It
// keeps the first maxSnapshotLines lines and the first maxSnapshotBytes bytes,
// appending a guidance footer when truncation occurred. The result is never an
// error (isError=false at the handler level) — the model is told how to fetch
// a scoped/complete view instead.
func truncateSnapshot(s string) string {
	lines := strings.Split(s, "\n")
	truncatedLines := false
	omittedLines := 0
	if len(lines) > maxSnapshotLines {
		omittedLines = len(lines) - maxSnapshotLines
		lines = lines[:maxSnapshotLines]
		truncatedLines = true
	}
	joined := strings.Join(lines, "\n")
	truncatedBytes := false
	omittedBytes := 0
	if len(joined) > maxSnapshotBytes {
		// Byte-truncate, then walk back to the last newline so we don't split
		// a line mid-way (keeps the line-text contract intact for the model).
		cut := maxSnapshotBytes
		if i := strings.LastIndex(joined[:cut], "\n"); i >= 0 && cut-i < 200 {
			cut = i
		}
		omittedBytes = len(joined) - cut
		joined = joined[:cut]
		truncatedBytes = true
		// Recompute line slice so the line/byte footers agree.
		lines = strings.Split(joined, "\n")
	}
	if !truncatedLines && !truncatedBytes {
		return s
	}
	footer := "...(truncated"
	if truncatedLines {
		footer += fmt.Sprintf(": %d more lines", omittedLines)
	}
	if truncatedBytes {
		footer += fmt.Sprintf(", %d more bytes", omittedBytes)
	}
	footer += "; use browser_snapshot_visible for viewport-scoped, or full:true with force for complete)"
	return joined + "\n" + footer
}

// dedupStore tracks the last snapshot structural signature per perception
// path (DOM vs AX), so consecutive identical snapshots return "unchanged"
// instead of the full payload (mcp-context-slimming D1/D2). Single attached-tab
// deployment model (mirrors axStuck); the signature includes the tab URL so a
// URL/tab change naturally resets the baseline without needing the actual
// attachedTabId. Not persisted — bridge process memory only.
var dedupStore struct {
	sync.Mutex
	dom string // last DOM snapshot signature
	ax  string // last AX snapshot signature
}

// dedupUnchanged returns true and an "unchanged" message when the new signature
// matches the stored baseline for the given path ("dom" or "ax"). When the
// signature differs, the baseline is updated and the function returns false.
// path selects which slot to compare against.
func dedupUnchanged(path, sig string, refs int) (bool, string) {
	dedupStore.Lock()
	defer dedupStore.Unlock()
	var slot *string
	switch path {
	case "dom":
		slot = &dedupStore.dom
	case "ax":
		slot = &dedupStore.ax
	default:
		return false, ""
	}
	if sig == *slot {
		return true, fmt.Sprintf(
			"unchanged since last snapshot (hash=%s, %d refs/nodes); call with force:true to re-fetch full",
			shortHash(sig), refs,
		)
	}
	*slot = sig
	return false, ""
}

// dedupReset clears the stored baseline for a path (e.g. when force:true is
// used, so the next non-force call always returns full).
func dedupReset(path string) {
	dedupStore.Lock()
	defer dedupStore.Unlock()
	switch path {
	case "dom":
		dedupStore.dom = ""
	case "ax":
		dedupStore.ax = ""
	}
}

// shortHash returns the first 8 hex chars of sha256(sig), enough to identify a
// baseline in the "unchanged" message without leaking the full signature.
func shortHash(sig string) string {
	h := sha256.Sum256([]byte(sig))
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i*2] = hex[h[i]>>4]
		out[i*2+1] = hex[h[i]&0x0f]
	}
	return string(out)
}

// domSignature builds a structural fingerprint of a DOM snapshot: the document
// URL + a recursive hash of tag names, depths, child counts, and dynamic text
// values (aria-label/value/text). Coarse enough to survive harmless attribute
// reordering but sensitive enough to catch real content changes (D2 risk:
// dynamic SPA updates). Used only to detect "no structural change between
// snapshots" — not for content equality. Also returns the count of element
// nodes walked, surfaced in the "unchanged" message.
func domSignature(root *cdp.DOMNode, url string) (string, int) {
	if root == nil {
		return "url=" + url + "|empty", 0
	}
	var b strings.Builder
	b.WriteString("url=")
	b.WriteString(url)
	b.WriteString("|")
	count := walkDOMSignature(&b, root, 0)
	return b.String(), count
}

// walkDOMSignature appends a recursive structural fingerprint: tag, depth,
// child count, and the dynamic attributes that indicate content (aria-label,
// value, and the accessible-name text). Hidden subtrees are skipped. Returns
// the number of element nodes walked.
func walkDOMSignature(b *strings.Builder, node *cdp.DOMNode, depth int) int {
	if node == nil {
		return 0
	}
	if isHiddenDOM(node) {
		return 0
	}
	count := 0
	if node.NodeType == 1 && !strings.HasPrefix(node.NodeName, "#") {
		count++
		b.WriteString(node.TagName())
		b.WriteByte(':')
		fmt.Fprintf(b, "d%d", depth)
		fmt.Fprintf(b, "c%d", childCount(node))
		attrs := node.GetAttributes()
		// Dynamic content signals — these changing means the page content
		// changed, even if the tag structure is identical.
		for _, k := range []string{"aria-label", "value", "placeholder", "role", "href"} {
			if v, ok := attrs[k]; ok && v != "" {
				b.WriteByte('|')
				b.WriteString(k)
				b.WriteByte('=')
				b.WriteString(v)
			}
		}
		b.WriteByte(';')
	}
	if node.ContentDocument != nil {
		count += walkDOMSignature(b, node.ContentDocument, depth+1)
	}
	for _, sr := range node.ShadowRoots {
		count += walkDOMSignature(b, sr, depth+1)
	}
	for _, c := range node.Children {
		count += walkDOMSignature(b, c, depth+1)
	}
	return count
}

// childCount counts element children (matches the structural shape, not text).
func childCount(node *cdp.DOMNode) int {
	n := len(node.Children)
	if node.ContentDocument != nil {
		n++
	}
	if len(node.ShadowRoots) > 0 {
		n += len(node.ShadowRoots)
	}
	return n
}

// isHiddenDOM is the snapshot-package hidden check (mirrors isHidden in
// filter.go but takes a *cdp.DOMNode directly, used only by the signature walk
// to avoid skipping into hidden subtrees).
func isHiddenDOM(node *cdp.DOMNode) bool {
	tag := node.TagName()
	switch tag {
	case "script", "style", "head", "meta", "link", "noscript", "template":
		return true
	}
	attrs := node.GetAttributes()
	if attrs["aria-hidden"] == "true" || attrs["hidden"] != "" {
		return true
	}
	if style, ok := attrs["style"]; ok {
		if strings.Contains(style, "display:none") || strings.Contains(style, "display: none") ||
			strings.Contains(style, "visibility:hidden") || strings.Contains(style, "visibility: hidden") {
			return true
		}
	}
	return false
}
