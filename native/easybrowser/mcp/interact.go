package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/browser-mcp/bridge/axtree"
	"github.com/browser-mcp/bridge/cdp"
)

// mustMarshal marshals v to json.RawMessage, panicking on error (used only for
// fixed-shape param structs where marshal failure is a programming bug).
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustMarshal: %v", err))
	}
	return b
}

// fillTargetSeq makes each data-bridge-fill-target attribute globally unique so
// concurrent RobustFill calls never collide on the same querySelector.
var fillTargetSeq uint64

// nextFillTargetUUID returns a unique attribute value used to re-find a node
// from page main world (Runtime.evaluate) after stamping it via callFunctionOn
// (which binds this=node but whose focus does not trigger alloy edit-mode).
func nextFillTargetUUID() string {
	return fmt.Sprintf("b%d-%d", time.Now().UnixNano(), atomic.AddUint64(&fillTargetSeq, 1))
}

// ResolvePoint resolves a backendNodeId to a viewport center point (x, y).
// Aligned with Codex getBackendNodeViewportPoint:
//  1. DOM.scrollIntoViewIfNeeded (bring element into view)
//  2. DOM.getContentQuads -> use first quad's center (preferred)
//  3. fallback DOM.getBoxModel -> use border quad's center
func ResolvePoint(ctx context.Context, c CDPClient, backendNodeID int) (x, y float64, err error) {
	// 1. scroll into view
	scrollParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	if _, err := c.Call(ctx, "DOM.scrollIntoViewIfNeeded", scrollParams); err != nil {
		return 0, 0, fmt.Errorf("scrollIntoViewIfNeeded: %w", err)
	}

	// 2. prefer content quads
	quadsParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resp, _ := c.Call(ctx, "DOM.getContentQuads", quadsParams)
	if resp != nil && resp.Error == nil {
		var q cdp.ContentQuadsResponse
		if json.Unmarshal(resp.Result, &q) == nil && len(q.Quads) > 0 {
			if cx, cy, ok := q.Quads[0].Center(); ok {
				return cx, cy, nil
			}
		}
	}

	// 3. fallback to box model
	boxParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resp, err = c.Call(ctx, "DOM.getBoxModel", boxParams)
	if err != nil {
		return 0, 0, fmt.Errorf("getBoxModel: %w (node may be detached/hidden — re-snapshot with browser_snapshot(_visible) and retry with a fresh node_id/ref)", err)
	}
	if resp != nil && resp.Error != nil {
		return 0, 0, fmt.Errorf("getBoxModel error: %v (node may be detached/hidden — re-snapshot with browser_snapshot(_visible) and retry with a fresh node_id/ref)", resp.Error)
	}
	var bm cdp.BoxModelResponse
	if err := json.Unmarshal(resp.Result, &bm); err != nil {
		return 0, 0, fmt.Errorf("parse box model: %w", err)
	}
	cx, cy, ok := bm.Model.Border.Center()
	if !ok {
		return 0, 0, fmt.Errorf("missing DOM geometry for backend node %d (node has no rendered box — it may be display:none, off-screen, or in a closed popup; re-snapshot and retry)", backendNodeID)
	}
	return cx, cy, nil
}

// dispatchMouse sends an Input.dispatchMouseEvent. Aligned with Codex
// dispatchMouseDown/Up/MouseMove: includes button + buttons + clickCount fields.
func dispatchMouse(ctx context.Context, c CDPClient, typ string, x, y float64, button string, buttons, clickCount int) error {
	params := mustMarshal(struct {
		Type       string  `json:"type"`
		X          float64 `json:"x"`
		Y          float64 `json:"y"`
		Button     string  `json:"button"`
		Buttons    int     `json:"buttons"`
		ClickCount int     `json:"clickCount,omitempty"`
	}{typ, x, y, button, buttons, clickCount})
	resp, err := c.Call(ctx, "Input.dispatchMouseEvent", params)
	if err != nil {
		return fmt.Errorf("dispatchMouseEvent %s: %w", typ, err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("dispatchMouseEvent %s error: %v", typ, resp.Error)
	}
	return nil
}

// clickOpTimeout bounds the entire ClickPoint operation (resolve + dispatch +
// waitForLoad), aligning with Codex clickPoint's timeoutMs default 1e4 (10s).
// The bridge router already enforces a 30s per-CDP-round-trip timeout, but
// ClickPoint issues several sequential round-trips (scrollIntoViewIfNeeded +
// getContentQuads + getBoxModel + 3× dispatchMouse); without an aggregate
// bound these can accumulate to 90s+ on a hung page (real-machine Drift-B:
// 97s no response). This deadline is the operation-level cap.
const clickOpTimeout = 10 * time.Second

// ClickPoint resolves the node's center and performs a click sequence.
// Aligned with Codex clickPoint: mouseMoved (move to target) then for each
// click count: mousePressed + mouseReleased, followed by waitForPageLoadEvent
// (timeout swallowed, not a failure — matches Codex `a=s.catch(()=>{})`).
func ClickPoint(ctx context.Context, c CDPClient, backendNodeID int, clickCount int) error {
	// Dispatch phase (resolve + mouseMove + N× pressed/released) is bounded by
	// an aggregate 10s deadline (clickOpTimeout, aligns Codex 1e4). Without it,
	// ClickPoint's several sequential CDP round-trips can accumulate to 90s+ on
	// a hung page (real-machine Drift-B: 97s no response). The bridge router's
	// 30s per-round-trip timeout alone does not bound the sum.
	opCtx, cancel := context.WithTimeout(ctx, clickOpTimeout)
	defer cancel()
	x, y, err := ResolvePoint(opCtx, c, backendNodeID)
	if err != nil {
		if ctxErr := opCtx.Err(); ctxErr != nil {
			return fmt.Errorf("click timeout after 10s: %w", ctxErr)
		}
		return err
	}
	// Hit-target occlusion check (design D1, agent-browser BLOCKER_AT_JS port,
	// memory click-occlusion-source-agent-browser). Before dispatching, confirm
	// the target is the topmost hit-test element at (x,y) — otherwise a visual
	// overlay (腾讯文档模板弹窗 iframe, glow, backdrop) with pointer-events:none
	// would let the click pass through to an occluded lower-layer element and
	// trigger it (穿透). detect-and-error: return the obstruction, do NOT
	// dispatch — let the agent dismiss the cover first. Skipped for cross-origin
	// OOPIF / detached nodes (cannot walk to top doc; proceed).
	if err := CheckHitTarget(opCtx, c, backendNodeID, x, y); err != nil {
		if ctxErr := opCtx.Err(); ctxErr != nil {
			return fmt.Errorf("click timeout after 10s: %w", ctxErr)
		}
		return err
	}
	// 1. mouseMoved (move to target point)
	if err := dispatchMouse(opCtx, c, "mouseMoved", x, y, "none", 0, 0); err != nil {
		if ctxErr := opCtx.Err(); ctxErr != nil {
			return fmt.Errorf("click timeout after 10s: %w", ctxErr)
		}
		return err
	}
	// 2. N x (pressed + released)
	for i := 1; i <= clickCount; i++ {
		if err := dispatchMouse(opCtx, c, "mousePressed", x, y, "left", 1, i); err != nil {
			if ctxErr := opCtx.Err(); ctxErr != nil {
				return fmt.Errorf("click timeout after 10s: %w", ctxErr)
			}
			return err
		}
		if err := dispatchMouse(opCtx, c, "mouseReleased", x, y, "left", 0, i); err != nil {
			if ctxErr := opCtx.Err(); ctxErr != nil {
				return fmt.Errorf("click timeout after 10s: %w", ctxErr)
			}
			return err
		}
	}
	// 3. waitForPageLoadEvent equivalent (Codex alignment): poll readyState on
	// the PARENT ctx (not opCtx) so its timeout is swallowed independently — a
	// slow-loading page after a successful click is NOT a click failure, matching
	// Codex `a=s.catch(()=>{})`. opCtx was for the dispatch phase only.
	_, _ = waitForLoad(ctx, c, clickOpTimeout)
	return nil
}

// interactiveRoles are the ARIA/host-language roles whose click is expected to
// have an observable effect. effect=none on these roles triggers the fallback
// chain (Drift-A). Non-interactive roles (generic/div/span) skip the chain.
var interactiveRoles = map[string]bool{
	"button":   true,
	"link":     true,
	"menuitem": true,
	"tab":      true,
	"option":   true,
}

// ClickEffect is the result of ClickWithEffect's effect probe.
type ClickEffect struct {
	NodeID     int    // backendNodeId clicked
	Effect     string // "navigation" | "toggle" | "none"
	URLBefore  string
	URLAfter   string
	Fallbacks  []string // ordered list of fallbacks attempted
	Screenshot string   // base64 JPEG, only attached when all fallbacks exhausted with effect=none
}

// describeNodeRole resolves a backendNodeId's interactive role via
// DOM.describeNode. Returns the ARIA role attribute if set, otherwise infers
// from nodeName: a→link, button→button, [role=menuitem/tab/option] etc.
// Returns "generic" for non-interactive elements.
func describeNodeRole(ctx context.Context, c CDPClient, backendNodeID int) string {
	params := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resp, err := c.Call(ctx, "DOM.describeNode", params)
	if err != nil || resp == nil || resp.Error != nil {
		return "generic"
	}
	var d struct {
		Node struct {
			NodeName   string   `json:"nodeName"`
			LocalName  string   `json:"localName"`
			Attributes []string `json:"attributes"`
		} `json:"node"`
	}
	if json.Unmarshal(resp.Result, &d) != nil {
		return "generic"
	}
	attrs := make(map[string]string, len(d.Node.Attributes)/2)
	for i := 0; i+1 < len(d.Node.Attributes); i += 2 {
		attrs[d.Node.Attributes[i]] = d.Node.Attributes[i+1]
	}
	if r, ok := attrs["role"]; ok && r != "" {
		return r
	}
	switch strings.ToLower(d.Node.LocalName) {
	case "a":
		return "link"
	case "button":
		return "button"
	case "option":
		return "option"
	}
	return "generic"
}

// toggleState captures the click-relevant ARIA state of a node (the subset
// whose change indicates a toggle effect). Empty string = attribute absent.
func toggleState(attrs map[string]string) string {
	if v, ok := attrs["aria-expanded"]; ok {
		return "aria-expanded=" + v
	}
	if v, ok := attrs["aria-pressed"]; ok {
		return "aria-pressed=" + v
	}
	if v, ok := attrs["aria-selected"]; ok {
		return "aria-selected=" + v
	}
	return ""
}

// ClickWithEffect performs a Codex-aligned ClickPoint and then probes whether
// the click had an observable effect (navigation = URL changed, toggle =
// aria-expanded/pressed/selected changed, none = neither). When effect=none
// and the target is an interactive role, it runs the bridge fallback chain
// (beyond Codex, which has no verify/fallback): JSClick → coordinate retry →
// screenshot. The honest report lets the AI distinguish a real click from a
// silent React no-op (real-machine Drift-A: click reported success but URL
// stayed on desktop, new sheet never created).
//
// ClickPoint already bounds dispatch with a 10s aggregate timeout (Drift-B).
// Each fallback step re-enters ClickPoint and is thus individually bounded.
func ClickWithEffect(ctx context.Context, c CDPClient, backendNodeID int) (*ClickEffect, error) {
	eff := &ClickEffect{NodeID: backendNodeID, Effect: "none"}

	// 1. pre-probe: url + ARIA state
	urlBefore, _ := GetURL(ctx, c)
	eff.URLBefore = urlBefore
	attrsBefore, _ := GetAttributes(ctx, c, backendNodeID)
	stateBefore := toggleState(attrsBefore)
	role := describeNodeRole(ctx, c, backendNodeID)

	// 2. primary click (Codex clickPoint, bounded by Drift-B timeout)
	if err := ClickPoint(ctx, c, backendNodeID, 1); err != nil {
		return nil, err
	}
	eff.URLAfter, _ = GetURL(ctx, c)
	probeEffect(ctx, c, eff, backendNodeID, stateBefore)

	// 3. fallback chain only when effect=none and role is interactive
	if eff.Effect == "none" && interactiveRoles[role] {
		// 3a. JSClick (Tier-4 React synthetic event)
		if jerr := JSClick(ctx, c, backendNodeID); jerr == nil {
			eff.Fallbacks = append(eff.Fallbacks, "JSClick")
			_, _ = waitForLoad(ctx, c, clickOpTimeout)
			eff.URLAfter, _ = GetURL(ctx, c)
			probeEffect(ctx, c, eff, backendNodeID, stateBefore)
		}

		// 3b. coordinate retry — re-resolve box (reflow may have shifted it).
		// ClickPoint already runs waitForLoad internally, so no separate settle
		// is needed here (unlike JSClick above, which bypasses ClickPoint).
		if eff.Effect == "none" {
			if cerr := ClickPoint(ctx, c, backendNodeID, 1); cerr == nil {
				eff.Fallbacks = append(eff.Fallbacks, "coord-retry")
				eff.URLAfter, _ = GetURL(ctx, c)
				probeEffect(ctx, c, eff, backendNodeID, stateBefore)
			}
		}

		// 3c. screenshot — last resort, let the AI judge
		if eff.Effect == "none" {
			if shot, serr := Screenshot(ctx, c); serr == nil {
				eff.Fallbacks = append(eff.Fallbacks, "screenshot")
				eff.Screenshot = shot
			}
		}
	}
	return eff, nil
}

// probeEffect re-evaluates the click effect from the before/after URL and
// ARIA state. Navigation takes precedence (URL changed); otherwise a change in
// the toggle-relevant ARIA attribute (aria-expanded/pressed/selected) marks a
// toggle; otherwise none. stateBefore is the pre-click toggleState() snapshot;
// the after-state is re-fetched from the page via DOM.getAttributes.
func probeEffect(ctx context.Context, c CDPClient, e *ClickEffect, backendNodeID int, stateBefore string) {
	if e.URLAfter != "" && e.URLAfter != e.URLBefore {
		e.Effect = "navigation"
		return
	}
	if stateBefore == "" {
		e.Effect = "none"
		return
	}
	attrsAfter, _ := GetAttributes(ctx, c, backendNodeID)
	if toggleState(attrsAfter) != stateBefore {
		e.Effect = "toggle"
		return
	}
	e.Effect = "none"
}


// dispatchKey sends a named key's keyDown then keyUp via Input.dispatchKeyEvent.
// Aligned with Codex cua_keypress / dispatchKeyPress -> Dc(cdp, tabId, keys).
func dispatchKey(ctx context.Context, c CDPClient, key string) error {
	base := struct {
		Type                  string `json:"type"`
		Key                   string `json:"key"`
		Code                  string `json:"code,omitempty"`
		WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode,omitempty"`
	}{
		Key:                   key,
		Code:                  keyToCode(key),
		WindowsVirtualKeyCode: keyToVKCode(key),
	}
	// keyDown
	base.Type = "keyDown"
	resp, err := c.Call(ctx, "Input.dispatchKeyEvent", mustMarshal(base))
	if err != nil {
		return fmt.Errorf("dispatchKeyEvent keyDown %s: %w", key, err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("dispatchKeyEvent keyDown %s error: %v", key, resp.Error)
	}
	// keyUp
	base.Type = "keyUp"
	resp, err = c.Call(ctx, "Input.dispatchKeyEvent", mustMarshal(base))
	if err != nil {
		return fmt.Errorf("dispatchKeyEvent keyUp %s: %w", key, err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("dispatchKeyEvent keyUp %s error: %v", key, resp.Error)
	}
	return nil
}

// TypeKeys dispatches a sequence of named keys (special keys / modifier combos).
// Aligned with Codex cua_keypress: keys is an array like ["Control","a"] or ["Enter"].
// For printable text input use RobustFill; TypeKeys is for special/combo keys.
func TypeKeys(ctx context.Context, c CDPClient, keys []string) error {
	for _, key := range keys {
		if err := dispatchKey(ctx, c, key); err != nil {
			return err
		}
	}
	return nil
}

// dispatchChar sends a single printable character via Input.dispatchKeyEvent
// with type "char" (triggers framework input/change listeners on controlled
// components — the reason path B uses per-char dispatch instead of insertText).
func dispatchChar(ctx context.Context, c CDPClient, ch string) error {
	params := mustMarshal(struct {
		Type string `json:"type"` // "char"
		Text string `json:"text"` // the character
	}{"char", ch})
	resp, err := c.Call(ctx, "Input.dispatchKeyEvent", params)
	if err != nil {
		return fmt.Errorf("dispatchKeyEvent char %q: %w", ch, err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("dispatchKeyEvent char %q error: %v", ch, resp.Error)
	}
	return nil
}

// insertText injects text into the focused element using Input.insertText.
// Aligned with Codex cua_type which uses Input.insertText for text injection.
// This triggers the browser's input event, which React listens to via synthetic events.
// Returns error if the CDP method fails (caller should fallback to per-char dispatch).
func insertText(ctx context.Context, c CDPClient, text string) error {
	params := mustMarshal(struct {
		Text string `json:"text"`
	}{text})
	resp, err := c.Call(ctx, "Input.insertText", params)
	if err != nil {
		return fmt.Errorf("insertText: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("insertText error: %v", resp.Error)
	}
	return nil
}

// tryClearContentEditable clears a contenteditable target via DOM.resolveNode +
// Runtime.callFunctionOn (Selection.deleteFromDocument). Returns:
//   - cleared=true, err=nil: target was contenteditable and cleared (textContent empty).
//   - cleared=false, err=nil: target is NOT contenteditable — caller uses original keyboard clear.
//   - cleared=false, err!=nil: target is contenteditable but clear failed — caller falls back
//     to keyboard clear (best-effort, non-blocking).
func tryClearContentEditable(ctx context.Context, c CDPClient, backendNodeID int) (bool, error) {
	attrs, err := GetAttributes(ctx, c, backendNodeID)
	if err != nil {
		return false, nil // getAttributes failed → not detected as CE, caller falls back
	}
	if attrs["contenteditable"] != "true" {
		return false, nil // not contenteditable → original keyboard clear path
	}
	objectID, err := resolveNodeID(ctx, c, backendNodeID)
	if err != nil {
		return false, fmt.Errorf("clearCE resolveNode: %w", err)
	}
	params := mustMarshal(struct {
		ObjectID      string `json:"objectId"`
		Function      string `json:"functionDeclaration"`
		ReturnByValue bool   `json:"returnByValue"`
	}{objectID, clearContentEditableJS, true})
	resp, err := c.Call(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return false, fmt.Errorf("clearCE callFunctionOn: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return false, fmt.Errorf("clearCE callFunctionOn error: %v", resp.Error)
	}
	var cf cdp.CallFunctionOnResult
	if err := json.Unmarshal(resp.Result, &cf); err != nil {
		return false, fmt.Errorf("parse clearCE result: %w", err)
	}
	// cf.Result.Value is the raw textContent returned by clearContentEditableJS
	// (not JSON-encoded — unlike hitTargetCheckJS which returns JSON.stringify).
	text := cf.Result.Value
	if strings.TrimSpace(text) != "" {
		return false, fmt.Errorf("clearCE: textContent not empty after clear: %q", text)
	}
	return true, nil
}

// tryFillContentEditable fills a contenteditable target via a two-step path
// that works on alloy/canvas apps (model≠DOM, e.g. 腾讯文档表格 formula-input):
//  1. Stamp a one-shot data-bridge-fill-target=<uuid> attribute via one
//     Runtime.callFunctionOn (trivial setAttribute, needs no alloy edit-mode).
//  2. Runtime.evaluate (page main world) an IIFE that querySelectors by the
//     attribute, focus() (triggers alloy edit-mode + model→DOM sync — UNLIKE
//     callFunctionOn's proxy focus), selectNodeContents (selects the synced
//     old value), execCommand insertText (clean replace), and removes the attr.
//
// Returns:
//   - filled=true, err=nil: target was contenteditable; stamp+eval fill done.
//   - filled=false, err=nil: target is NOT contenteditable — caller uses the
//     original insertText/dispatchChar path.
//   - filled=false, err!=nil: target is contenteditable but fill failed — caller
//     falls back to keyboard clear + insertText (best-effort, non-blocking).
//
// DV 2026-07-15 (alloy-canvas-fill-commit): the prior sw-canvas-perception
// callFunctionOn (this=node) path disrupted alloy editor routing — it cleared
// the synced old value and the new value never landed (focus re-routed to the
// in-cell editor). The evaluate path's direct page-context node.focus() enters
// alloy edit-mode → clean replace. General node re-targeting via attribute
// stamping — no app-specific selector.
func tryFillContentEditable(ctx context.Context, c CDPClient, backendNodeID int, value string) (bool, error) {
	attrs, err := GetAttributes(ctx, c, backendNodeID)
	if err != nil {
		return false, nil // getAttributes failed → not detected as CE, caller falls back
	}
	if attrs["contenteditable"] != "true" {
		return false, nil // not contenteditable → original insertText path
	}
	// 0. Trusted double-click the target to FOCUS it before stamp+eval. DV
	//    2026-07-15 (alloy-canvas-fill-commit): without this, selectNodeContents
	//    sets a selection on an UNFOCUSED contenteditable (cell selected ≠ CE
	//    focused) → execCommand insertText operates on the actually-focused element
	//    (canvas/operate-board) → append residue. Double-clicking the CE target
	//    itself focuses it (proven on 腾讯文档表格 formula-input e24: A1=TABVAL →
	//    double-click e24 → eval insertText FBAR → clean replace → Tab commit →
	//    A1=FBAR). For plain contenteditable this is a harmless extra focus.
	//    ClickPoint error → fall back to caller's keyboard path (best-effort).
	if err := ClickPoint(ctx, c, backendNodeID, 2); err != nil {
		return false, fmt.Errorf("fillCE focus double-click: %w", err)
	}
	// 0b. Modest head-start for alloy's async edit-mode entry. DV 2026-07-15
	//     (alloy-canvas-fill-commit) DISPROVED the timing hypothesis: a 2000ms
	//     Go-side sleep (≈ the manual path's 1-3s MCP tool latency) STILL partials
	//     (ZZFINALDONE→ZZFINALDONWIN). So the automated-vs-manual gap is NOT timing.
	//     The real gap is BATCHING (see step 2 comment) — selectNodeContents selects
	//     only the last char in the one-call browser_fill path regardless of delay.
	//     The fix is the IIFE's selection-bypass fallback (textContent reset +
	//     input/change dispatch), not the sleep. This 200ms is just a small
	//     head-start so alloy's focus/edit-mode entry has begun; it does NOT gate
	//     the fill's correctness. DV 2026-07-15.
	time.Sleep(200 * time.Millisecond)
	// 1. Build a node-selector expression. PREFER the node's own id (read from
	//    attrs) → single-eval by id, NO stamp. DV 2026-07-15 (alloy-canvas-fill-commit):
	//    stamping (Runtime.callFunctionOn setAttribute OR DOM.setAttributeValue) on
	//    an alloy editor triggers its mutation observer → re-render → the edit-mode
	//    the double-click just established expires → the eval's selectNodeContents
	//    doesn't bind → partial residue (E2V→E2FINAL, FBAR→FBAANEW). The id-based
	//    single-eval path (no stamp) is clean (A1=E2V → double-click → eval insertText
	//    CLN → clean). Using attrs["id"] is general (reads the node's OWN id, not an
	//    app-specific hardcoded selector). For id-less nodes, fall back to the
	//    DOM-domain stamp+querySelector path (plain CE has no mutation observer →
	//    no disruption).
	var selector string
	if id := attrs["id"]; id != "" {
		selector = "document.getElementById(" + strconvQuote(id) + ")"
	} else {
		uuid := nextFillTargetUUID()
		if err := stampNodeAttributeDOM(ctx, c, backendNodeID, "data-bridge-fill-target", uuid); err != nil {
			return false, fmt.Errorf("fillCE stamp: %w", err)
		}
		selector = "document.querySelector('[data-bridge-fill-target=" + strconvQuote(uuid) + "]')"
	}
	// 2. Single-shot fill via Runtime.evaluate (page main world, SYNC). The IIFE
	//    (fillContentEditableEvalJS) does a TEXTCONTENT-BYPASS fill: focus +
	//    textContent='' + textContent=val + dispatch input/change/blur events so
	//    model≠DOM engines (alloy canvas) sync the DOM value to their model. It
	//    does NOT use selectNodeContents/execCommand — DV 2026-07-15 proved the
	//    automated browser_fill path selectNodeContents selects only the last char
	//    (a structural one-call vs two-call CDP gap, NOT timing/IIFE/dispatcher),
	//    and execCommand's input event then pollutes alloy's model with the
	//    PARTIAL result. textContent bypass sidesteps both. Sync (not
	//    async/awaitPromise) so the eval is atomic against the canvas-page CDP
	//    flakiness. The Go-side sleep in 0b is just a head-start, not a gate. DV
	//    2026-07-15.
	expr := fmt.Sprintf(fillContentEditableEvalJS, selector, strconvQuote(value))
	if _, err := evalJS(ctx, c, expr); err != nil {
		return false, fmt.Errorf("fillCE evaluate: %w", err)
	}
	return true, nil
}

// RobustFill fills an input element with text.
// Uses Input.insertText (path C, aligned with Codex cua_type) as the preferred method,
// falling back to per-char dispatchKeyEvent (path B) if insertText fails.
//  1. ResolvePoint + click (focus)
//  2a. contenteditable targets: stamp a one-shot data-bridge-fill-target
//      attribute via one Runtime.callFunctionOn (trivial setAttribute, no
//      edit-mode), then fill via Runtime.evaluate (page main world):
//      querySelector+focus+selectNodeContents+execCommand insertText (clean
//      replace) — tryFillContentEditable. The evaluate node.focus() triggers
//      alloy/RTE edit-mode (model→DOM sync) — UNLIKE the prior callFunctionOn
//      proxy focus which alloy re-routed to the in-cell editor (cleared the
//      synced value, new value never landed). DV 2026-07-15 (alloy-canvas-fill-commit,
//      腾讯文档表格 formula-input): clean replace, no residue/clear-empty.
//      ⚠ RESOLVED 2026-07-15 (alloy-canvas-fill-commit): the callFunctionOn gap
//      on canvas apps with a separate model (Tencent Docs alloy) is closed by
//      stamping + filling via Runtime.evaluate. See design doc + memory
//      [[canvas-app-perception-blind-spot]].
//  2b. Non-CE or evaluate-failure falls back to: Selection clear
//      (tryClearContentEditable) or keyboard clear (Ctrl+A+Backspace) +
//      Input.insertText / per-char dispatch.
//  3. Tab (blur, trigger onChange/onBlur) — commits the synced model value.
func RobustFill(ctx context.Context, c CDPClient, backendNodeID int, text string) error {
	// 2a. contenteditable: stamp attribute via callFunctionOn + fill via
	//     Runtime.evaluate (tryFillContentEditable) BEFORE ClickPoint. The
	//     evaluate path runs focus+selectNodeContents+insertText in page main
	//     world, which triggers alloy edit-mode (DV 2026-07-15). CE path returns
	//     early (Tab commit); non-CE / evaluate-failure falls through to the
	//     original click-focus + keyboard clear + insertText path.
	filled, _ := tryFillContentEditable(ctx, c, backendNodeID, text)
	if filled {
		// 3. Tab commit (model synced by the execCommand input event).
		if err := dispatchKey(ctx, c, "Tab"); err != nil {
			return fmt.Errorf("blur: %w", err)
		}
		return nil
	}
	// 2b. Non-CE or execCommand failure → original click-focus + keyboard clear +
	//     insertText path.
	// 1. focus via click
	if err := ClickPoint(ctx, c, backendNodeID, 1); err != nil {
		return fmt.Errorf("focus: %w", err)
	}
	// clear. contenteditable targets use Selection.deleteFromDocument via
	// Runtime.callFunctionOn — Ctrl+A+Backspace leaves residue on contenteditable
	// (2026-07-13 Tencent Docs formula-input). Non-CE or eval-failure falls back
	// to the original keyboard clear (best-effort, non-blocking).
	cleared, clearErr := tryClearContentEditable(ctx, c, backendNodeID)
	if !cleared {
		if err := clearViaKeyboard(ctx, c); err != nil {
			if clearErr != nil {
				return fmt.Errorf("clear (CE failed: %v; keyboard also failed: %w)", clearErr, err)
			}
			return err
		}
	}
	// 3. try insertText, fallback to per-char dispatch
	if err := insertText(ctx, c, text); err != nil {
		// fallback: per-char dispatch (path B)
		for _, r := range text {
			if err := dispatchChar(ctx, c, string(r)); err != nil {
				return fmt.Errorf("char %q: %w", r, err)
			}
		}
	}
	// 4. blur via Tab (trigger onChange/onBlur)
	if err := dispatchKey(ctx, c, "Tab"); err != nil {
		return fmt.Errorf("blur: %w", err)
	}
	return nil
}

// clearViaKeyboard is the original Ctrl+A+Backspace clear path, used for plain
// <input>/<textarea> and as fallback when contenteditable clear fails.
func clearViaKeyboard(ctx context.Context, c CDPClient) error {
	if err := dispatchKey(ctx, c, "Control"); err != nil {
		return fmt.Errorf("ctrl down: %w", err)
	}
	if err := dispatchKey(ctx, c, "a"); err != nil {
		return fmt.Errorf("a key: %w", err)
	}
	if err := dispatchKey(ctx, c, "Control"); err != nil {
		return fmt.Errorf("ctrl up: %w", err)
	}
	if err := dispatchKey(ctx, c, "Backspace"); err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	return nil
}

// waitForLoad polls document.readyState via Runtime.evaluate until "complete"
// or timeout. Does not error on timeout — returns (loaded, nil) so the caller
// can decide. Uses polling because the extension's cdp.execute only supports
// request-response, not CDP event forwarding.
func waitForLoad(ctx context.Context, c CDPClient, timeout time.Duration) (bool, error) {
	// initial delay to let navigation start
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return false, ctx.Err()
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		params := mustMarshal(struct {
			Expression    string `json:"expression"`
			ReturnByValue bool   `json:"returnByValue"`
		}{"document.readyState", true})
		resp, err := c.Call(ctx, "Runtime.evaluate", params)
		if err == nil && resp != nil && resp.Error == nil {
			var eval struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(resp.Result, &eval) == nil && eval.Result.Value == "complete" {
				return true, nil
			}
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil // timeout, not an error
}

// Navigate navigates the active tab to url, then waits for the page to load.
// Aligned with Codex navigate_tab_url + waitForPageLoadEvent.
// Uses polling (Runtime.evaluate readyState) instead of CDP events because
// the extension only supports request-response.
func Navigate(ctx context.Context, c CDPClient, url string) (*cdp.NavigateResult, error) {
	params := mustMarshal(struct {
		URL string `json:"url"`
	}{url})
	resp, err := c.Call(ctx, "Page.navigate", params)
	if err != nil {
		return nil, fmt.Errorf("Page.navigate: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return nil, fmt.Errorf("Page.navigate error: %v", resp.Error)
	}
	var result cdp.NavigateResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse navigate result: %w", err)
	}
	if result.ErrorText != "" {
		return &result, fmt.Errorf("navigation failed: %s", result.ErrorText)
	}
	// wait for page load (polling readyState)
	result.Loaded, _ = waitForLoad(ctx, c, 10*time.Second)
	// Probe main-frame HTTP status via PerformanceNavigationTiming.responseStatus
	// (bridge enhancement beyond Codex, which only checks errorText and misses
	// 4xx/5xx — real-machine Drift-C: 404 reported as loaded=true). Uses the
	// standard Web API via Runtime.evaluate (request-response, no CDP events —
	// bridge jsonrpc/router.go drops event:<method> notifications, so Network
	// domain events cannot be consumed without a transport enhancement).
	result.HTTPStatus = probeHTTPStatus(ctx, c)
	return &result, nil
}

// probeHTTPStatus returns the main-frame HTTP status code via
// PerformanceNavigationTiming.responseStatus, or -1 if unavailable (eval
// failed, no navigation entry, cross-origin restriction). Does not block on
// failure — callers report http_status=unknown (navigation spec fire-and-check).
func probeHTTPStatus(ctx context.Context, c CDPClient) int {
	params := mustMarshal(struct {
		Expression   string `json:"expression"`
		ReturnByValue bool   `json:"returnByValue"`
	}{
		Expression:   `(()=>{const e=performance.getEntriesByType('navigation')[0];return e&&typeof e.responseStatus==='number'?e.responseStatus:null;})()`,
		ReturnByValue: true,
	})
	resp, err := c.Call(ctx, "Runtime.evaluate", params)
	if err != nil || resp == nil || resp.Error != nil {
		return -1
	}
	var eval struct {
		Result struct {
			Type  string `json:"type"`
			Value any    `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(resp.Result, &eval) != nil {
		return -1
	}
	switch v := eval.Result.Value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return -1
	}
}

// ---------------------------------------------------------------------------
// Interaction Tools V2
// ---------------------------------------------------------------------------

// Screenshot captures a JPEG screenshot of the current page.
// Aligned with Codex tab_screenshot -> Page.captureScreenshot.
func Screenshot(ctx context.Context, c CDPClient) (string, error) {
	params := mustMarshal(struct {
		Format  string `json:"format"`
		Quality int    `json:"quality"`
	}{"jpeg", 80})
	resp, err := c.Call(ctx, "Page.captureScreenshot", params)
	if err != nil {
		return "", fmt.Errorf("captureScreenshot: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return "", fmt.Errorf("captureScreenshot error: %v", resp.Error)
	}
	var result cdp.ScreenshotResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse screenshot result: %w", err)
	}
	return result.Data, nil
}

// Scroll scrolls the page by scrollX/scrollY. If nodeId is provided, scrolls
// that element into view first. Aligned with Codex cua_scroll.
func Scroll(ctx context.Context, c CDPClient, scrollX, scrollY int, backendNodeID *int) error {
	var x, y float64 = 400, 300 // default viewport center
	if backendNodeID != nil {
		cx, cy, err := ResolvePoint(ctx, c, *backendNodeID)
		if err == nil {
			x, y = cx, cy
		}
	}
	params := mustMarshal(struct {
		Type    string  `json:"type"`
		X       float64 `json:"x"`
		Y       float64 `json:"y"`
		DeltaX  int     `json:"deltaX"`
		DeltaY  int     `json:"deltaY"`
	}{"mouseWheel", x, y, scrollX, scrollY})
	resp, err := c.Call(ctx, "Input.dispatchMouseEvent", params)
	if err != nil {
		return fmt.Errorf("scroll: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("scroll error: %v", resp.Error)
	}
	return nil
}

// resolveNodeID converts a backendNodeId to a RemoteObject objectId via DOM.resolveNode.
func resolveNodeID(ctx context.Context, c CDPClient, backendNodeID int) (string, error) {
	params := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resp, err := c.Call(ctx, "DOM.resolveNode", params)
	if err != nil {
		return "", fmt.Errorf("resolveNode: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return "", fmt.Errorf("resolveNode error: %v", resp.Error)
	}
	var result cdp.ResolveNodeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse resolveNode result: %w", err)
	}
	return result.Object.ObjectID, nil
}

// stampNodeAttributeDOM stamps a one-shot attribute on a node via the DOM
// domain (DOM.describeNode + DOM.setAttributeValue) — NO JavaScript execution,
// NO this=node binding. Unlike Runtime.callFunctionOn, it does NOT hijack the
// focus a trusted double-click just established on an alloy contenteditable
// (DV 2026-07-15 alloy-canvas-fill-commit: callFunctionOn stamp disrupted
// focus → eval selectNodeContents didn't bind → residue). General re-targeting:
// works for any backendNodeId, no app-specific selector. Used by
// tryFillContentEditable so the subsequent Runtime.evaluate can re-find the
// stamped node by attribute.
func stampNodeAttributeDOM(ctx context.Context, c CDPClient, backendNodeID int, attr, val string) error {
	descParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	descResp, err := c.Call(ctx, "DOM.describeNode", descParams)
	if err != nil {
		return fmt.Errorf("describeNode: %w", err)
	}
	if descResp != nil && descResp.Error != nil {
		return fmt.Errorf("describeNode error: %v", descResp.Error)
	}
	var desc struct {
		Node struct {
			NodeID int `json:"nodeId"`
		} `json:"node"`
	}
	if err := json.Unmarshal(descResp.Result, &desc); err != nil {
		return fmt.Errorf("parse describeNode: %w", err)
	}
	if desc.Node.NodeID == 0 {
		return fmt.Errorf("describeNode returned nodeId=0")
	}
	setParams := mustMarshal(struct {
		NodeID int    `json:"nodeId"`
		Name   string `json:"name"`
		Value  string `json:"value"`
	}{desc.Node.NodeID, attr, val})
	setResp, err := c.Call(ctx, "DOM.setAttributeValue", setParams)
	if err != nil {
		return fmt.Errorf("setAttributeValue: %w", err)
	}
	if setResp != nil && setResp.Error != nil {
		return fmt.Errorf("setAttributeValue error: %v", setResp.Error)
	}
	return nil
}

// SelectOption sets a <select> element's value and dispatches change event.
// Uses DOM.resolveNode + Runtime.callFunctionOn. Aligned with Codex playwright_locator_select_option.
func SelectOption(ctx context.Context, c CDPClient, backendNodeID int, value string) error {
	objectID, err := resolveNodeID(ctx, c, backendNodeID)
	if err != nil {
		return fmt.Errorf("select resolveNode: %w", err)
	}
	params := mustMarshal(struct {
		ObjectID    string          `json:"objectId"`
		Function    string          `json:"functionDeclaration"`
		Arguments   []any           `json:"arguments,omitempty"`
		ReturnByValue bool          `json:"returnByValue"`
	}{
		ObjectID:      objectID,
		Function:      "function(el, val) { el.value = val; el.dispatchEvent(new Event('change', {bubbles: true})); }",
		Arguments:     []any{map[string]string{"value": value}},
		ReturnByValue: true,
	})
	resp, err := c.Call(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return fmt.Errorf("callFunctionOn: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("callFunctionOn error: %v", resp.Error)
	}
	return nil
}

// GetText reads an element's innerText. Aligned with Codex playwright_locator_inner_text.
func GetText(ctx context.Context, c CDPClient, backendNodeID int) (string, error) {
	objectID, err := resolveNodeID(ctx, c, backendNodeID)
	if err != nil {
		return "", fmt.Errorf("get_text resolveNode: %w", err)
	}
	params := mustMarshal(struct {
		ObjectID      string `json:"objectId"`
		Function      string `json:"functionDeclaration"`
		ReturnByValue bool   `json:"returnByValue"`
	}{
		ObjectID:      objectID,
		Function:      "function(el) { return el.innerText; }",
		ReturnByValue: true,
	})
	resp, err := c.Call(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return "", fmt.Errorf("callFunctionOn: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return "", fmt.Errorf("callFunctionOn error: %v", resp.Error)
	}
	var result cdp.CallFunctionOnResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse callFunctionOn result: %w", err)
	}
	return result.Result.Value, nil
}

// clearContentEditableJS clears a contenteditable element via the Selection API.
// Ctrl+A+Backspace leaves residue on contenteditable (2026-07-13 Tencent Docs
// formula-input: "标记1"→"标记"); Selection.deleteFromDocument clears cleanly.
// Returns the remaining textContent (caller checks empty).
const clearContentEditableJS = `function(){
  var e = this;
  try { e.focus(); } catch (_) {}
  var s = window.getSelection();
  if (s) {
    var r = document.createRange();
    r.selectNodeContents(e);
    s.removeAllRanges();
    s.addRange(r);
    s.deleteFromDocument();
  } else {
    e.textContent = '';
  }
  try { e.dispatchEvent(new InputEvent('input', {inputType:'deleteContent', bubbles:true})); }
  catch (_) { try { e.dispatchEvent(new Event('input', {bubbles:true})); } catch (__) {} }
  return (e.textContent || '').slice(0, 50);
}`

// fillContentEditableEvalJS is a page-main-world SYNC IIFE (Runtime.evaluate,
// returnByValue) that re-finds the target node, focuses it (alloy edit-mode
// entry), clean-replaces its value via direct textContent reset, dispatches
// input/change/blur events so model≠DOM engines (alloy canvas) sync the DOM
// value to their model, and removes the stamp.
//
// DV 2026-07-15 (alloy-canvas-fill-commit, 腾讯文档表格 formula-input) —
// TEXTCONTENT-BYPASS is the load-bearing mechanism. The prior approach
// (selectNodeContents + execCommand insertText) clean-replaces on the MANUAL
// path (browser_double_click + browser_evaluate_js as TWO tool calls) but
// DETERMINISTICALLY partials on the AUTOMATED browser_fill path: selectNodeContents
// selects only the LAST char in the one-call path (a structural CDP
// session/call-sequencing gap — proven NOT timing/150-2000ms, NOT IIFE, NOT
// double-click, NOT eval-dispatcher; only batching differs), so execCommand
// insertText replaces just the last char (ZZ2→ZZFINAL2). Worse, execCommand's
// own input event syncs alloy's model with the PARTIAL result, so a subsequent
// textContent force is reverted by the model re-sync (ZZFINALDONE→ZZFINALDONWIOGEND).
//
// textContent reset BYPASSES the selection entirely (no selectNodeContents, no
// execCommand) so the one-call selection-binding gap is irrelevant, AND no
// partial execCommand input pollutes the model. The dispatched InputEvent
// (inputType:insertText) + input + change + blur ask alloy to read the new DOM
// textContent and sync its model. DV 2026-07-15: A1 model synced to the new value
// via this path (manual check, re-select cell → formula-input shows new value).
// SYNC single-shot is atomic against the canvas-page CDP flakiness (a dropped
// connection can't truncate a JS loop mid-flight).
// %s[0] = node selector expression, %s[1] = JSON-quoted value.
const fillContentEditableEvalJS = `(function(){
  var n = %s;
  if (!n) return JSON.stringify({ok:false, err:'node-gone'});
  try { n.focus(); } catch (_) {}
  n.textContent = '';
  n.textContent = %s;
  try { n.dispatchEvent(new InputEvent('input', {inputType:'insertText', bubbles:true})); } catch (_) {}
  try { n.dispatchEvent(new Event('input', {bubbles:true})); } catch (_) {}
  try { n.dispatchEvent(new Event('change', {bubbles:true})); } catch (_) {}
  try { n.dispatchEvent(new Event('blur', {bubbles:true})); } catch (_) {}
  n.removeAttribute('data-bridge-fill-target');
  return JSON.stringify({ok:true, html:(n.textContent||'').slice(0,80)});
})()`

// hitTargetCheckJS is a port of agent-browser BLOCKER_AT_JS
// (cli/src/native/element.rs:731-761) + check_node_interception ownerDocument
// walk (:402-460), adapted to Runtime.callFunctionOn where `this` is the target
// element and args [x, y] are viewport coordinates. Returns JSON.stringify({...})
// so the existing CallFunctionOnResult.Value (string) field decodes it without a
// type change (object returns would break Value string).
//
// Semantics (aligned with agent-browser, memory click-occlusion-source-agent-browser):
//   - walk ownerDocument to top doc (same-origin; cross-origin parent.document
//     throws SecurityError → cross-origin-iframe-skip, caller proceeds).
//   - elementFromPoint(x,y) at top; if hit is IFRAME/FRAME, recurse into
//     contentDocument re-resolving in frame coordinate space (same-origin
//     piercing + frame offset subtraction). This is the 腾讯文档模板弹窗 case.
//   - hit == target / target ancestor / target descendant (via shadow-DOM-aware
//     up() = parentNode||host||getRootNode().host) → not occluded.
//   - <label>/control association → not occlusion.
//   - target pointer-events:none → target-pointer-events-none (distinct from
//     obscured).
//   - else → obscured, hit = CSS-like description.
const hitTargetCheckJS = `function(x, y) {
  function ret(ok, reason, hit){ return JSON.stringify({ok:ok, reason:reason, hit:hit||""}); }
  var d = this.ownerDocument;
  if (!d) return ret(false, "no-owner-document");
  while (d && d.defaultView && d.defaultView.parent && d.defaultView.parent.document !== d) {
    try { d = d.defaultView.parent.document; }
    catch (e) { return ret(false, "cross-origin-iframe-skip"); }
  }
  var doc = d, lx = x, ly = y;
  var hit = doc.elementFromPoint(lx, ly);
  while (hit && (hit.tagName === 'IFRAME' || hit.tagName === 'FRAME') && hit.contentDocument && hit !== this) {
    var r = hit.getBoundingClientRect();
    lx -= r.x + hit.clientLeft;
    ly -= r.y + hit.clientTop;
    doc = hit.contentDocument;
    hit = doc.elementFromPoint(lx, ly);
  }
  if (!hit || hit === this) return ret(true);
  var up = function(n){ return n.parentNode || n.host || (n.getRootNode && n.getRootNode().host) || null; };
  for (var n = hit; n; n = up(n)) if (n === this) return ret(true);
  for (var m = this; m; m = up(m)) if (m === hit) return ret(true);
  var hitLabel = hit.closest ? hit.closest('label') : null;
  if (hitLabel && (hitLabel.control === this || hitLabel.contains(this))) return ret(true);
  var elLabel = this.closest ? this.closest('label') : null;
  if (elLabel && elLabel.contains(hit)) return ret(true);
  function desc(h){ var s = (h.tagName || '').toLowerCase(); if (h.id) s += '#' + h.id; else if (typeof h.className === 'string' && h.className.trim()) s += '.' + h.className.trim().split(/\s+/).slice(0,2).join('.'); return s; }
  var cs = getComputedStyle(this);
  if (cs.pointerEvents === 'none') return ret(false, 'target-pointer-events-none', desc(hit));
  return ret(false, 'obscured', desc(hit));
}`

// hitTargetResult decodes the JSON string returned by hitTargetCheckJS.
type hitTargetResult struct {
	Ok     bool   `json:"ok"`
	Reason string `json:"reason"`
	Hit    string `json:"hit"`
}

// CheckHitTarget runs the BLOCKER_AT_JS occlusion probe against the target
// element (backendNodeID) at viewport (x, y), via DOM.resolveNode +
// Runtime.callFunctionOn (same pattern as SelectOption/GetText).
//
// Returns nil when the click may proceed: target unobstructed, OR the check was
// skipped (cross-origin-iframe-skip / no-owner-document — cannot walk to top
// doc, like agent-browser's OOPIF dedicated-session case we do not implement).
// Returns an error when the target is obscured or pointer-events:none
// (agent-browser detect-and-error: do NOT dispatch the click; let the agent
// dismiss the covering element first). The error message carries the hit
// description so the agent can act on it.
func CheckHitTarget(ctx context.Context, c CDPClient, backendNodeID int, x, y float64) error {
	objectID, err := resolveNodeID(ctx, c, backendNodeID)
	if err != nil {
		return fmt.Errorf("hitTarget resolveNode: %w", err)
	}
	params := mustMarshal(struct {
		ObjectID      string `json:"objectId"`
		Function      string `json:"functionDeclaration"`
		Arguments     []any  `json:"arguments,omitempty"`
		ReturnByValue bool   `json:"returnByValue"`
	}{
		ObjectID:      objectID,
		Function:      hitTargetCheckJS,
		Arguments:     []any{map[string]any{"value": x}, map[string]any{"value": y}},
		ReturnByValue: true,
	})
	resp, err := c.Call(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return fmt.Errorf("hitTarget callFunctionOn: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("hitTarget callFunctionOn error: %v", resp.Error)
	}
	var cf cdp.CallFunctionOnResult
	if err := json.Unmarshal(resp.Result, &cf); err != nil {
		return fmt.Errorf("parse hitTarget result: %w", err)
	}
	var r hitTargetResult
	if err := json.Unmarshal([]byte(cf.Result.Value), &r); err != nil {
		return fmt.Errorf("decode hitTarget value: %w", err)
	}
	switch r.Reason {
	case "", "ok":
		return nil
	case "cross-origin-iframe-skip", "no-owner-document":
		// Cannot walk to top doc (OOPIF / detached) — skip check, proceed.
		return nil
	case "target-pointer-events-none":
		return fmt.Errorf("target not clickable at (%.0f,%.0f): target-pointer-events-none (hit: %s)", x, y, r.Hit)
	default: // "obscured"
		return fmt.Errorf("target not clickable at (%.0f,%.0f): %s (hit: %s); covered by overlay/modal — dismiss the covering element first, then retry", x, y, r.Reason, r.Hit)
	}
}

// GetAttributes returns the DOM attributes of the node identified by
// backendNodeID, as a name→value map. Aligned with Codex
// playwright_locator_get_attribute.
//
// Implementation note (alloy-canvas-fill-commit DV 2026-07-16): this USED to
// call DOM.getAttributes with {backendNodeId}, but Chrome's DOM.getAttributes
// requires nodeId — calling it with only backendNodeId fails with
// "params.nodeId — mandatory field missing" (JSON-RPC -32602). That silently
// broke tryFillContentEditable (GetAttributes errored → tryFill returned
// (false,nil) at branch 1 → RobustFill fell to the keyboard fallback → partial
// residue on alloy canvas) AND broke toggle-effect detection in
// ClickWithEffect.probeEffect (aria-expanded read errored → every toggle click
// was judged effect=none → fallback chain). Fix: use DOM.describeNode, which
// honors backendNodeId and returns the Node with the flat Attributes array.
// Proven shape by axtree.resolveIframeFrameID (same backendNodeId→node pattern).
func GetAttributes(ctx context.Context, c CDPClient, backendNodeID int) (map[string]string, error) {
	params := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resp, err := c.Call(ctx, "DOM.describeNode", params)
	if err != nil {
		return nil, fmt.Errorf("describeNode (getAttributes): %w", err)
	}
	if resp != nil && resp.Error != nil {
		return nil, fmt.Errorf("describeNode error: %v", resp.Error)
	}
	var out struct {
		Node *cdp.DOMNode `json:"node"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("parse describeNode result: %w", err)
	}
	if out.Node == nil {
		return nil, fmt.Errorf("describeNode: no node for backendNodeId %d", backendNodeID)
	}
	return out.Node.GetAttributes(), nil
}

// WaitForElement polls document.querySelector until the element is found (or not found).
// Aligned with Codex playwright_locator_wait_for.
func WaitForElement(ctx context.Context, c CDPClient, selector string, timeout time.Duration, wantVisible bool) (bool, error) {
	jsExpr := fmt.Sprintf(`document.querySelector(%s) != null`, strconvQuote(selector))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		params := mustMarshal(struct {
			Expression    string `json:"expression"`
			ReturnByValue bool   `json:"returnByValue"`
		}{jsExpr, true})
		resp, err := c.Call(ctx, "Runtime.evaluate", params)
		if err == nil && resp != nil && resp.Error == nil {
			var eval struct {
				Result struct {
					Value bool `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(resp.Result, &eval) == nil {
				if eval.Result.Value == wantVisible {
					return true, nil
				}
			}
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

// strconvQuote wraps strconv.Quote without importing strconv at package level.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// Navigation History + Checkbox (nav-history-checkbox change)
// ---------------------------------------------------------------------------

// GoBack navigates to the previous page in history.
// Aligned with Codex navigate_tab_back.
func GoBack(ctx context.Context, c CDPClient) error {
	params := mustMarshal(struct{}{})
	resp, err := c.Call(ctx, "Page.getNavigationHistory", params)
	if err != nil {
		return fmt.Errorf("getNavigationHistory: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("getNavigationHistory error: %v", resp.Error)
	}
	var hist cdp.NavigationHistoryResult
	if err := json.Unmarshal(resp.Result, &hist); err != nil {
		return fmt.Errorf("parse navigation history: %w", err)
	}
	if hist.CurrentIndex <= 0 {
		return fmt.Errorf("cannot go back: no previous page in history")
	}
	prev := hist.Entries[hist.CurrentIndex-1]
	navParams := mustMarshal(struct {
		EntryID int `json:"entryId"`
	}{prev.ID})
	resp2, err := c.Call(ctx, "Page.navigateToHistoryEntry", navParams)
	if err != nil {
		return fmt.Errorf("navigateToHistoryEntry: %w", err)
	}
	if resp2 != nil && resp2.Error != nil {
		return fmt.Errorf("navigateToHistoryEntry error: %v", resp2.Error)
	}
	return nil
}

// GoForward navigates to the next page in history.
// Aligned with Codex navigate_tab_forward.
func GoForward(ctx context.Context, c CDPClient) error {
	params := mustMarshal(struct{}{})
	resp, err := c.Call(ctx, "Page.getNavigationHistory", params)
	if err != nil {
		return fmt.Errorf("getNavigationHistory: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("getNavigationHistory error: %v", resp.Error)
	}
	var hist cdp.NavigationHistoryResult
	if err := json.Unmarshal(resp.Result, &hist); err != nil {
		return fmt.Errorf("parse navigation history: %w", err)
	}
	if hist.CurrentIndex >= len(hist.Entries)-1 {
		return fmt.Errorf("cannot go forward: no next page in history")
	}
	next := hist.Entries[hist.CurrentIndex+1]
	navParams := mustMarshal(struct {
		EntryID int `json:"entryId"`
	}{next.ID})
	resp2, err := c.Call(ctx, "Page.navigateToHistoryEntry", navParams)
	if err != nil {
		return fmt.Errorf("navigateToHistoryEntry: %w", err)
	}
	if resp2 != nil && resp2.Error != nil {
		return fmt.Errorf("navigateToHistoryEntry error: %v", resp2.Error)
	}
	return nil
}

// Reload reloads the current page.
// Aligned with Codex navigate_tab_reload.
func Reload(ctx context.Context, c CDPClient) error {
	params := mustMarshal(struct{}{})
	resp, err := c.Call(ctx, "Page.reload", params)
	if err != nil {
		return fmt.Errorf("Page.reload: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("Page.reload error: %v", resp.Error)
	}
	return nil
}

// SetChecked checks or unchecks a checkbox/radio element.
// Aligned with Codex playwright_locator_set_checked.
func SetChecked(ctx context.Context, c CDPClient, backendNodeID int, checked bool) error {
	objectID, err := resolveNodeID(ctx, c, backendNodeID)
	if err != nil {
		return fmt.Errorf("set_checked resolveNode: %w", err)
	}
	params := mustMarshal(struct {
		ObjectID      string `json:"objectId"`
		Function      string `json:"functionDeclaration"`
		Arguments     []any  `json:"arguments,omitempty"`
		ReturnByValue bool   `json:"returnByValue"`
	}{
		ObjectID:      objectID,
		Function:      "function(el, val) { el.checked = val; el.dispatchEvent(new Event('change', {bubbles: true})); }",
		Arguments:     []any{map[string]bool{"value": checked}},
		ReturnByValue: true,
	})
	resp, err := c.Call(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return fmt.Errorf("callFunctionOn: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("callFunctionOn error: %v", resp.Error)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tools Batch 1: query / mouse / keyboard / wait / clipboard
// ---------------------------------------------------------------------------

// HitTestAt returns a CSS-like description of the topmost element at viewport
// (x, y) via document.elementFromPoint, for the raw-coordinate click path
// (browser_click_at) which has no target to compare against. Informational —
// appended to the click result so the agent can self-check whether the click
// landed on the intended element or passed through an overlay (design D2).
//
// Pierces same-origin iframes: when the initial hit is an IFRAME/FRAME with an
// accessible contentDocument, subtracts the frame's offset (getBoundingClientRect
// + clientLeft/Top) and re-resolves elementFromPoint inside the frame document,
// mirroring the walk in hitTargetCheckJS — so actual_hit reports the real inner
// element (e.g. a button inside a 腾讯文档 modal iframe), not the <iframe> itself.
// Cross-origin OOPIF: contentDocument is null/inaccessible → the loop stops and
// the <iframe> element description is returned (same limitation as CheckHitTarget;
// piercing a cross-origin frame needs a dedicated CDP session).
// desc() formatting mirrors the helper inside hitTargetCheckJS.
func HitTestAt(ctx context.Context, c CDPClient, x, y float64) (string, error) {
	expr := fmt.Sprintf(`(function(x, y){
		var doc = document, lx = x, ly = y;
		var hit = doc.elementFromPoint(lx, ly);
		while (hit && (hit.tagName === 'IFRAME' || hit.tagName === 'FRAME') && hit.contentDocument) {
			var r = hit.getBoundingClientRect();
			lx -= r.x + hit.clientLeft;
			ly -= r.y + hit.clientTop;
			doc = hit.contentDocument;
			hit = doc.elementFromPoint(lx, ly);
		}
		if (!hit) return "";
		var s = (hit.tagName || '').toLowerCase();
		if (hit.id) s += '#' + hit.id;
		else if (typeof hit.className === 'string' && hit.className.trim()) s += '.' + hit.className.trim().split(/\s+/).slice(0,2).join('.');
		return s;
	})(%v, %v)`, x, y)
	return evalJS(ctx, c, expr)
}

// evalJS runs Runtime.evaluate and returns the result value as string.
// Handles all CDP return types: string, number, boolean, undefined, object.
func evalJS(ctx context.Context, c CDPClient, expr string) (string, error) {
	params := mustMarshal(struct {
		Expression    string `json:"expression"`
		ReturnByValue bool   `json:"returnByValue"`
	}{expr, true})
	resp, err := c.Call(ctx, "Runtime.evaluate", params)
	if err != nil {
		return "", fmt.Errorf("evalJS: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return "", fmt.Errorf("evalJS error: %v", resp.Error)
	}
	var eval struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Result, &eval); err != nil {
		return "", fmt.Errorf("parse evalJS result: %w", err)
	}
	// Dispatch by CDP type
	switch eval.Result.Type {
	case "string":
		var s string
		if err := json.Unmarshal(eval.Result.Value, &s); err == nil {
			return s, nil
		}
		return string(eval.Result.Value), nil
	case "number":
		// Value is a raw JSON number like 5 or 3.14
		return string(eval.Result.Value), nil
	case "boolean":
		var b bool
		if err := json.Unmarshal(eval.Result.Value, &b); err == nil {
			if b {
				return "true", nil
			}
			return "false", nil
		}
		return string(eval.Result.Value), nil
	case "undefined", "":
		return "", nil
	default:
		// object, function, symbol, bigint — return raw JSON
		return string(eval.Result.Value), nil
	}
}

// IsVisible checks if an element matching a CSS selector is visible.
func IsVisible(ctx context.Context, c CDPClient, selector string) (bool, error) {
	v, err := evalJS(ctx, c, fmt.Sprintf(`(()=>{const e=document.querySelector(%s);return e!==null&&e.offsetParent!==null})()`, strconvQuote(selector)))
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// IsEnabled checks if an element is enabled (not disabled).
func IsEnabled(ctx context.Context, c CDPClient, backendNodeID int) (bool, error) {
	objectID, err := resolveNodeID(ctx, c, backendNodeID)
	if err != nil {
		return false, fmt.Errorf("is_enabled resolveNode: %w", err)
	}
	params := mustMarshal(struct {
		ObjectID      string `json:"objectId"`
		Function      string `json:"functionDeclaration"`
		ReturnByValue bool   `json:"returnByValue"`
	}{objectID, "function(el){return !el.disabled}", true})
	resp, err := c.Call(ctx, "Runtime.callFunctionOn", params)
	if err != nil {
		return false, fmt.Errorf("callFunctionOn: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return false, fmt.Errorf("callFunctionOn error: %v", resp.Error)
	}
	var result cdp.CallFunctionOnResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return false, fmt.Errorf("parse result: %w", err)
	}
	return result.Result.Value == "true", nil
}

// Count counts elements matching a CSS selector.
func Count(ctx context.Context, c CDPClient, selector string) (int, error) {
	v, err := evalJS(ctx, c, fmt.Sprintf(`document.querySelectorAll(%s).length`, strconvQuote(selector)))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ch := range v {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		}
	}
	return n, nil
}

// DoubleClick double-clicks an element.
func DoubleClick(ctx context.Context, c CDPClient, backendNodeID int) error {
	return ClickPoint(ctx, c, backendNodeID, 2)
}

// Hover moves the mouse to an element's center without clicking.
func Hover(ctx context.Context, c CDPClient, backendNodeID int) error {
	x, y, err := ResolvePoint(ctx, c, backendNodeID)
	if err != nil {
		return err
	}
	return dispatchMouse(ctx, c, "mouseMoved", x, y, "none", 0, 0)
}

// PressKey dispatches a named key on the currently focused element.
func PressKey(ctx context.Context, c CDPClient, key string) error {
	return dispatchKey(ctx, c, key)
}

// WaitForURL polls location.href until it matches a substring or timeout.
func WaitForURL(ctx context.Context, c CDPClient, urlPattern string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		href, err := evalJS(ctx, c, "window.location.href")
		if err == nil && containsStr(href, urlPattern) {
			return true, nil
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// WaitForTimeout sleeps for the given duration.
func WaitForTimeout(ctx context.Context, duration time.Duration) error {
	select {
	case <-time.After(duration):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ClipboardWriteText writes text to the clipboard via navigator.clipboard.
func ClipboardWriteText(ctx context.Context, c CDPClient, text string) error {
	_, err := evalJS(ctx, c, fmt.Sprintf(`navigator.clipboard.writeText(%s)`, strconvQuote(text)))
	return err
}

// ClipboardReadText reads text from the clipboard.
func ClipboardReadText(ctx context.Context, c CDPClient) (string, error) {
	return evalJS(ctx, c, `navigator.clipboard.readText()`)
}

// ConsoleLogs reads recent console messages via an injected capture.
func ConsoleLogs(ctx context.Context, c CDPClient, limit int) (string, error) {
	if limit <= 0 {
		limit = 50
	}
	js := fmt.Sprintf(`(()=>{if(!window.__mcpLogs){window.__mcpLogs=[];var o={log:console.log,warn:console.warn,error:console.error,info:console.info};['log','warn','error','info'].forEach(function(l){console[l]=function(){var a=Array.prototype.slice.call(arguments);window.__mcpLogs.push({level:l,msg:a.map(function(x){return typeof x==='object'?JSON.stringify(x):String(x)}).join(' ')});if(window.__mcpLogs.length>200)window.__mcpLogs.shift();o[l].apply(console,a)}})}return JSON.stringify(window.__mcpLogs.slice(-%d))})()`, limit)
	return evalJS(ctx, c, js)
}

// ---------------------------------------------------------------------------
// Tools Batch 2: tab info / drag / file upload / download
// ---------------------------------------------------------------------------

// GetURL returns the current page URL.
func GetURL(ctx context.Context, c CDPClient) (string, error) {
	return evalJS(ctx, c, "window.location.href")
}

// GetTitle returns the current page title.
func GetTitle(ctx context.Context, c CDPClient) (string, error) {
	return evalJS(ctx, c, "document.title")
}

// NewTab opens a new browser tab/window.
func NewTab(ctx context.Context, c CDPClient, url string) error {
	_, err := evalJS(ctx, c, fmt.Sprintf(`window.open(%s)`, strconvQuote(url)))
	return err
}

// CloseTab closes the current tab/window (only works if opened by script).
func CloseTab(ctx context.Context, c CDPClient) error {
	_, err := evalJS(ctx, c, "window.close()")
	return err
}

// Point is a screen coordinate for drag paths.
type Point struct{ X, Y float64 }

// Drag drags along a path of points (mouseMoved sequence).
func Drag(ctx context.Context, c CDPClient, points []Point) error {
	if len(points) == 0 {
		return fmt.Errorf("drag requires at least one point")
	}
	for _, p := range points {
		if err := dispatchMouse(ctx, c, "mouseMoved", p.X, p.Y, "none", 0, 0); err != nil {
			return err
		}
	}
	return nil
}

// FileUpload sets files on a file input element.
func FileUpload(ctx context.Context, c CDPClient, backendNodeID int, files []string) error {
	params := mustMarshal(struct {
		BackendNodeID int      `json:"backendNodeId"`
		Files         []string `json:"files"`
	}{backendNodeID, files})
	resp, err := c.Call(ctx, "DOM.setFileInputFiles", params)
	if err != nil {
		return fmt.Errorf("setFileInputFiles: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("setFileInputFiles error: %v", resp.Error)
	}
	return nil
}

// DownloadMedia fetches a URL and returns base64-encoded content.
func DownloadMedia(ctx context.Context, c CDPClient, url string) (string, error) {
	js := fmt.Sprintf(`(async()=>{const r=await fetch(%s);const b=await r.blob();return new Promise(function(resolve){const reader=new FileReader();reader.onloadend=function(){resolve(reader.result)};reader.readAsDataURL(b)})})()`, strconvQuote(url))
	return evalJS(ctx, c, js)
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: CUA (Coordinate-based interaction — Tier 3)
// Aligned with Codex cua_click/cua_move/cua_scroll/cua_drag/cua_type/cua_keypress
// ---------------------------------------------------------------------------

// ClickAt clicks at viewport coordinates (x, y).
// Aligned with Codex cua.clickPoint: mouseMoved → mousePressed → mouseReleased.
// For Canvas/WebGL pages where no DOM elements exist.
func ClickAt(ctx context.Context, c CDPClient, x, y float64, clickCount int) error {
	// 1. Move mouse to (x, y)
	if err := dispatchMouse(ctx, c, "mouseMoved", x, y, "none", 0, 0); err != nil {
		return fmt.Errorf("click_at move: %w", err)
	}
	// 2. press + release
	for i := 1; i <= clickCount; i++ {
		if err := dispatchMouse(ctx, c, "mousePressed", x, y, "left", 1, i); err != nil {
			return fmt.Errorf("click_at press: %w", err)
		}
		if err := dispatchMouse(ctx, c, "mouseReleased", x, y, "left", 0, i); err != nil {
			return fmt.Errorf("click_at release: %w", err)
		}
	}
	return nil
}

// DoubleClickAt double-clicks at viewport coordinates.
// Aligned with Codex cua_double_click.
func DoubleClickAt(ctx context.Context, c CDPClient, x, y float64) error {
	return ClickAt(ctx, c, x, y, 2)
}

// MoveMouse moves the mouse to viewport coordinates without clicking.
// Aligned with Codex cua_move: Input.dispatchMouseEvent mouseMoved.
func MoveMouse(ctx context.Context, c CDPClient, x, y float64) error {
	return dispatchMouse(ctx, c, "mouseMoved", x, y, "none", 0, 0)
}

// ScrollAt scrolls at a specific viewport coordinate.
// Aligned with Codex cua_scroll: Input.dispatchMouseEvent type=mouseWheel at (x,y).
func ScrollAt(ctx context.Context, c CDPClient, x, y float64, deltaX, deltaY int) error {
	params := mustMarshal(struct {
		Type   string  `json:"type"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		DeltaX int     `json:"deltaX"`
		DeltaY int     `json:"deltaY"`
	}{"mouseWheel", x, y, deltaX, deltaY})
	resp, err := c.Call(ctx, "Input.dispatchMouseEvent", params)
	if err != nil {
		return fmt.Errorf("scroll_at: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("scroll_at error: %v", resp.Error)
	}
	return nil
}

// DragPath drags along a path of viewport coordinates.
// Aligned with Codex cua_drag: mouseMoved to first point → mousePressed →
// mouseMoved along path → mouseReleased at last point.
func DragPath(ctx context.Context, c CDPClient, path []Point) error {
	if len(path) == 0 {
		return fmt.Errorf("drag requires a non-empty path")
	}
	// 1. Move to start
	start := path[0]
	if err := dispatchMouse(ctx, c, "mouseMoved", start.X, start.Y, "none", 0, 0); err != nil {
		return fmt.Errorf("drag move to start: %w", err)
	}
	// 2. Press at start
	if err := dispatchMouse(ctx, c, "mousePressed", start.X, start.Y, "left", 1, 1); err != nil {
		return fmt.Errorf("drag press: %w", err)
	}
	// 3. Move through intermediate points (button still held: buttons=1)
	for _, p := range path[1:] {
		if err := dispatchMouse(ctx, c, "mouseMoved", p.X, p.Y, "left", 1, 0); err != nil {
			return fmt.Errorf("drag move: %w", err)
		}
	}
	// 4. Release at end
	end := path[len(path)-1]
	if err := dispatchMouse(ctx, c, "mouseReleased", end.X, end.Y, "left", 0, 1); err != nil {
		return fmt.Errorf("drag release: %w", err)
	}
	return nil
}

// TypeAt types text at the current focus position (coordinate-based).
// Aligned with Codex cua_type: prefers Input.insertText, falls back to per-char
// Input.dispatchKeyEvent. Does NOT focus a node — call ClickAt first.
func TypeAt(ctx context.Context, c CDPClient, text string) error {
	// Preferred path: Input.insertText (single CDP call, triggers input event)
	if err := insertText(ctx, c, text); err != nil {
		// Fallback: per-char dispatchKeyEvent
		for _, r := range text {
			if err := dispatchChar(ctx, c, string(r)); err != nil {
				return fmt.Errorf("type_at char %q: %w", r, err)
			}
		}
	}
	return nil
}

// PressKeyCombo dispatches a key combination (e.g., ["Control","a"] for Ctrl+A).
// Aligned with Codex cua_keypress / KeyboardDispatcher:
//  1. Hold modifier keys (keyDown)
//  2. Press and release the final key
//  3. Release modifier keys in reverse order
func PressKeyCombo(ctx context.Context, c CDPClient, keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("press_key requires at least one key")
	}

	// CDP modifier tracking: Alt=1, Ctrl=2, Meta=4, Shift=8
	modifierMask := 0
	modifiers := make([]string, 0, len(keys)-1)

	// Identify modifiers (all keys except the last)
	for _, k := range keys[:len(keys)-1] {
		if isModifier(k) {
			modifiers = append(modifiers, k)
			modifierMask |= modifierBit(k)
		}
	}

	// 1. Hold modifier keys (keyDown)
	for _, mod := range modifiers {
		if err := dispatchKeyWithModifiers(ctx, c, mod, "keyDown", modifierMask); err != nil {
			return fmt.Errorf("press_key hold %s: %w", mod, err)
		}
	}

	// 2. Press and release the final key
	finalKey := keys[len(keys)-1]
	if err := dispatchKeyWithModifiers(ctx, c, finalKey, "keyDown", modifierMask); err != nil {
		return fmt.Errorf("press_key down %s: %w", finalKey, err)
	}
	if err := dispatchKeyWithModifiers(ctx, c, finalKey, "keyUp", modifierMask); err != nil {
		return fmt.Errorf("press_key up %s: %w", finalKey, err)
	}

	// 3. Release modifier keys in reverse order
	for i := len(modifiers) - 1; i >= 0; i-- {
		if err := dispatchKeyWithModifiers(ctx, c, modifiers[i], "keyUp", modifierMask); err != nil {
			return fmt.Errorf("press_key release %s: %w", modifiers[i], err)
		}
	}

	return nil
}

// isModifier returns true if the key is a modifier key.
func isModifier(key string) bool {
	switch key {
	case "Alt", "Control", "Meta", "Shift":
		return true
	}
	return false
}

// modifierBit returns the CDP modifier bit for a modifier key.
func modifierBit(key string) int {
	switch key {
	case "Alt":
		return 1
	case "Control":
		return 2
	case "Meta":
		return 4
	case "Shift":
		return 8
	}
	return 0
}

// dispatchKeyWithModifiers sends a keyDown or keyUp event with CDP modifiers field.
// Aligned with Codex KeyboardDispatcher.callInputDispatch.
func dispatchKeyWithModifiers(ctx context.Context, c CDPClient, key, eventType string, modifiers int) error {
	code := keyToCode(key)
	vkCode := keyToVKCode(key)
	text := ""
	// Only include text for keyDown of printable keys when no modifiers (except Shift)
	if eventType == "keyDown" && modifiers == 0 || (modifiers == 8 && len(key) == 1) {
		if len(key) == 1 {
			text = key
		}
	}

	params := mustMarshal(struct {
		Type                  string `json:"type"`
		Key                   string `json:"key"`
		Code                  string `json:"code,omitempty"`
		Text                  string `json:"text,omitempty"`
		WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode,omitempty"`
		Modifiers             int    `json:"modifiers"`
	}{eventType, key, code, text, vkCode, modifiers})

	resp, err := c.Call(ctx, "Input.dispatchKeyEvent", params)
	if err != nil {
		return fmt.Errorf("dispatchKeyEvent %s %s: %w", eventType, key, err)
	}
	if resp != nil && resp.Error != nil {
		return fmt.Errorf("dispatchKeyEvent %s %s error: %v", eventType, key, resp.Error)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: dom_cua (DOM→Coordinate — Tier 2)
// ---------------------------------------------------------------------------

// ClickNode resolves a nodeId to viewport coordinates and clicks there.
// Aligned with Codex dom_cua_click: getBackendNodeViewportPoint → clickPoint.
// This is the same as ClickPoint but exposed as a distinct tool for the
// three-tier degradation model (Tier 2: DOM→coordinate resolution).
func ClickNode(ctx context.Context, c CDPClient, backendNodeID int, clickCount int) error {
	return ClickPoint(ctx, c, backendNodeID, clickCount)
}

// GetVisibleDOM returns a filtered DOM snapshot with only interactable elements
// in the viewport, with node_id attributes for the dom_cua interaction tier.
// Aligned with Codex dom_cua_get_visible_dom:
//  1. Page.getLayoutMetrics → viewport bounds
//  2. DOM.getDocument(depth=-1, pierce=true) → full DOM including iframes/shadow DOMs
//  3. Filter: backendNodeId > 0 AND in viewport AND interactable tag/attribute
//  4. Output: "node_id=123 tag=input type=text placeholder=Search" per line
func GetVisibleDOM(ctx context.Context, c CDPClient) (string, error) {
	// 1. Get viewport bounds
	layoutParams := mustMarshal(struct{}{})
	layoutResp, err := c.Call(ctx, "Page.getLayoutMetrics", layoutParams)
	if err != nil {
		return "", fmt.Errorf("getLayoutMetrics: %w", err)
	}
	if layoutResp != nil && layoutResp.Error != nil {
		return "", fmt.Errorf("getLayoutMetrics error: %v", layoutResp.Error)
	}
	var layout struct {
		CSSVisualViewport *struct {
			PageX        float64 `json:"pageX"`
			PageY        float64 `json:"pageY"`
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssVisualViewport"`
	}
	if err := json.Unmarshal(layoutResp.Result, &layout); err != nil {
		return "", fmt.Errorf("parse layout: %w", err)
	}
	if layout.CSSVisualViewport == nil {
		return "", fmt.Errorf("no viewport info")
	}
	vp := layout.CSSVisualViewport
	viewportTop := vp.PageY
	viewportBottom := vp.PageY + vp.ClientHeight
	viewportLeft := vp.PageX
	viewportRight := vp.PageX + vp.ClientWidth

	// 2. Get full DOM document
	domParams := mustMarshal(struct {
		Depth  int  `json:"depth"`
		Pierce bool `json:"pierce"`
	}{-1, true})
	domResp, err := c.Call(ctx, "DOM.getDocument", domParams)
	if err != nil {
		return "", fmt.Errorf("getDocument: %w", err)
	}
	if domResp != nil && domResp.Error != nil {
		return "", fmt.Errorf("getDocument error: %v", domResp.Error)
	}
	var domDoc struct {
		Root *visibleDOMNode `json:"root"`
	}
	if err := json.Unmarshal(domResp.Result, &domDoc); err != nil {
		return "", fmt.Errorf("parse document: %w", err)
	}

	// 3. Walk DOM tree and collect interactable elements
	var lines []string
	collectVisibleDOM(domDoc.Root, &lines, c, ctx,
		viewportTop, viewportBottom, viewportLeft, viewportRight)

	result := ""
	for _, l := range lines {
		result += l + "\n"
	}
	return result, nil
}

// visibleDOMNode represents a DOM node for visible DOM traversal.
type visibleDOMNode struct {
	NodeID          int                `json:"nodeId"`
	BackendNodeID   int                `json:"backendNodeId"`
	NodeType        int                `json:"nodeType"`
	NodeName        string             `json:"nodeName"`
	LocalName       string             `json:"localName"`
	NodeValue       string             `json:"nodeValue"`
	Attributes      []string           `json:"attributes"`
	Children        []*visibleDOMNode  `json:"children"`
	ShadowRoots     []*visibleDOMNode  `json:"shadowRoots"`
	ContentDocument *visibleDOMNode    `json:"contentDocument"`
}

// interactableTags is the set of HTML tags considered interactable.
// Aligned with Codex Z4 = new Set(["a","button","details","input","option","select","summary","textarea"])
var interactableTags = map[string]bool{
	"a": true, "button": true, "details": true, "input": true,
	"option": true, "select": true, "summary": true, "textarea": true,
}

// interactableAttrs is the set of attributes that make any element interactable.
// Aligned with Codex X4 = ["aria-label","contenteditable","href","name","placeholder","role","title","type","value"]
var interactableAttrs = map[string]bool{
	"aria-label": true, "contenteditable": true, "href": true, "name": true,
	"placeholder": true, "role": true, "title": true, "type": true, "value": true,
	"onclick": true, "tabindex": true,
}

// nodeInViewport mirrors Codex `rU` (doc/reference/extracted/11_dom_cua_get_visible_dom.js):
// it resolves the element's box via DOM.getBoxModel({backendNodeId}) and returns true only
// when the box is at least partially within the viewport. Returns false (skip the node, do
// NOT abort the snapshot) when:
//   - the CDP call errors or returns a protocol error (Codex `catch{return null}`),
//   - the border quad is missing/short,
//   - any coordinate is non-finite (NaN/Inf),
//   - the box is zero-area (maxX<=minX || maxY<=minY),
//   - the box is fully outside the viewport (maxX<=vpLeft || minX>=vpRight ||
//     maxY<=vpTop || minY>=vpBottom).
// Partial intersection (not fully outside) is kept, matching Codex rU semantics.
func nodeInViewport(ctx context.Context, c CDPClient, backendNodeID int,
	vpTop, vpBottom, vpLeft, vpRight float64) bool {
	params := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resp, err := c.Call(ctx, "DOM.getBoxModel", params)
	// Codex `catch{return null}` — errors/detached/hidden nodes are skipped, not fatal.
	if err != nil || resp == nil || resp.Error != nil {
		return false
	}
	var bm cdp.BoxModelResponse
	if err := json.Unmarshal(resp.Result, &bm); err != nil {
		return false
	}
	b := bm.Model.Border
	if len(b) < 8 {
		return false
	}
	minX := min(b[0], b[2], b[4], b[6])
	maxX := max(b[0], b[2], b[4], b[6])
	minY := min(b[1], b[3], b[5], b[7])
	maxY := max(b[1], b[3], b[5], b[7])
	if !isFinite(minX) || !isFinite(maxX) || !isFinite(minY) || !isFinite(maxY) {
		return false
	}
	if maxX <= minX || maxY <= minY { // zero-area (Codex: D<=x || w<=S)
		return false
	}
	if maxX <= vpLeft || minX >= vpRight || maxY <= vpTop || minY >= vpBottom { // fully outside
		return false
	}
	return true
}

// isFinite reports whether v is neither NaN nor +/-Inf.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// collectVisibleDOM recursively walks the DOM tree, collecting interactable elements
// that are in the viewport.
func collectVisibleDOM(node *visibleDOMNode, lines *[]string, c CDPClient, ctx context.Context,
	vpTop, vpBottom, vpLeft, vpRight float64) {
	if node == nil {
		return
	}

	// Skip non-element nodes and nodes without backendNodeId
	if node.NodeType != 1 || node.BackendNodeID <= 0 {
		for _, child := range node.Children {
			collectVisibleDOM(child, lines, c, ctx, vpTop, vpBottom, vpLeft, vpRight)
		}
		return
	}

	// Parse attributes into a map for easier lookup
	attrs := make(map[string]string, len(node.Attributes)/2)
	for i := 0; i+1 < len(node.Attributes); i += 2 {
		attrs[node.Attributes[i]] = node.Attributes[i+1]
	}

	// Skip hidden elements
	if attrs["aria-hidden"] == "true" || attrs["hidden"] != "" {
		for _, child := range node.Children {
			collectVisibleDOM(child, lines, c, ctx, vpTop, vpBottom, vpLeft, vpRight)
		}
		return
	}

	tag := node.LocalName
	isInteractable := interactableTags[tag] ||
		attrs["contenteditable"] != "" ||
		attrs["href"] != "" ||
		attrs["onclick"] != "" ||
		attrs["role"] != "" ||
		attrs["tabindex"] != ""

	// Check if any interactable attribute is present
	if !isInteractable {
		for attr := range interactableAttrs {
			if attrs[attr] != "" {
				isInteractable = true
				break
			}
		}
	}

	if isInteractable {
		// Codex rU viewport check: skip nodes whose box is fully outside the
		// viewport, zero-area, non-finite, or whose getBoxModel errors. Do NOT
		// abort the snapshot on error (Codex `catch{return null}`). Children are
		// still recursed below regardless of this node's visibility.
		if nodeInViewport(ctx, c, node.BackendNodeID, vpTop, vpBottom, vpLeft, vpRight) {
			line := fmt.Sprintf("node_id=%d tag=%s", node.BackendNodeID, tag)
			// Add key attributes
			attrOrder := []string{"aria-label", "data-testid", "type", "placeholder", "name", "value", "role", "href", "title"}
			for _, a := range attrOrder {
				if v, ok := attrs[a]; ok && v != "" {
					line += fmt.Sprintf(" %s=%s", a, v)
				}
			}
			// Add boolean attributes
			for _, a := range []string{"checked", "disabled", "readonly", "required", "selected"} {
				if attrs[a] != "" {
					line += fmt.Sprintf(" %s=true", a)
				}
			}
			// DM (2026-06-23): accessible name so the AI can locate nodes
			// whose label lives in a child #text node. Keeps the visible-DOM
			// path consistent with the full-filter path (snapshot.AccessibleName).
			if name := accessibleNameVisible(node, attrs); name != "" {
				line += fmt.Sprintf(" text=%q", name)
			}
			*lines = append(*lines, line)
		}
	}

	// Recurse into children, shadow roots, and content documents
	for _, child := range node.Children {
		collectVisibleDOM(child, lines, c, ctx, vpTop, vpBottom, vpLeft, vpRight)
	}
	for _, sr := range node.ShadowRoots {
		collectVisibleDOM(sr, lines, c, ctx, vpTop, vpBottom, vpLeft, vpRight)
	}
	if node.ContentDocument != nil {
		collectVisibleDOM(node.ContentDocument, lines, c, ctx, vpTop, vpBottom, vpLeft, vpRight)
	}
}

// accessibleNameVisibleMax bounds the accessible name length for the
// visible-DOM path (mirrors snapshot.accessibleNameMax).
const accessibleNameVisibleMax = 80

// accessibleNameVisible returns the accessible name for a visibleDOMNode,
// mirroring snapshot.AccessibleName's priority (Codex dom_cua_get_visible_dom):
// aria-label > aria-name > title > subtree #text nodeValue (textContent).
// attrs is the pre-parsed attribute map (caller already builds it). (DM.)
func accessibleNameVisible(node *visibleDOMNode, attrs map[string]string) string {
	if v := strings.TrimSpace(attrs["aria-label"]); v != "" {
		return truncateAccessibleName(v)
	}
	if v := strings.TrimSpace(attrs["aria-name"]); v != "" {
		return truncateAccessibleName(v)
	}
	if v := strings.TrimSpace(attrs["title"]); v != "" {
		return truncateAccessibleName(v)
	}
	if t := collectVisibleText(node); strings.TrimSpace(t) != "" {
		return truncateAccessibleName(t)
	}
	return ""
}

// collectVisibleText concatenates all descendant #text nodeValue of a
// visibleDOMNode (textContent). Same-origin iframe content (ContentDocument,
// populated by DOM.getDocument pierce:true) is included.
func collectVisibleText(node *visibleDOMNode) string {
	if node == nil {
		return ""
	}
	var b strings.Builder
	if node.NodeType == 3 { // Node.TEXT_NODE
		b.WriteString(node.NodeValue)
	}
	for _, c := range node.Children {
		b.WriteString(collectVisibleText(c))
	}
	if node.ContentDocument != nil {
		b.WriteString(collectVisibleText(node.ContentDocument))
	}
	return b.String()
}

// truncateAccessibleName collapses internal whitespace runs and truncates by
// rune count with a trailing ellipsis. Mirrors snapshot.truncateText so the two
// DOM paths render names identically.
func truncateAccessibleName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
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
	if len(r) > accessibleNameVisibleMax {
		return string(r[:accessibleNameVisibleMax]) + "…"
	}
	return collapsed
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: Enhanced JS Evaluation + Raw CDP (Tier 4)
// ---------------------------------------------------------------------------

// EvaluateJS evaluates JavaScript in the page and returns the result.
// Aligned with Codex Runtime.evaluate with awaitPromise + returnByValue.
// Returns (resultJSON, error) where resultJSON is the JSON-serialized return value.
//
// DR② (2026-06-24, Codex-aligned): Codex wraps some eval expressions in
// `(() => ...)` IIFEs. Raw Runtime.evaluate already supports multi-statement
// programs (it returns the completion value of the last statement), so the
// ONLY malformed-JS failure mode is a top-level `return` (LLM writes
// `if(x){...} return y`) which throws SyntaxError. We retry the expression
// wrapped in `(()=>{ <expr> })()` on SyntaxError — non-regressive: bare
// expressions and statement-blocks-without-return keep working via the raw
// path; only the return-at-top-level case gets rescued. Evidence: test7/8
// ~11 "JS exception: Uncaught" errors, all top-level return / statement form.
//
// DR③ (2026-06-24, Codex-aligned): returnByValue:true cannot serialize a live
// DOM element (e.g. LLM returns `document.querySelector(...)`), failing with
// CDP -32000 "Object reference chain is too long" (test7 line 385, test8).
// Codex's object paths use returnByValue:false to fetch a RemoteObject. We
// retry with returnByValue:false and surface the RemoteObject description
// (e.g. "button.create-create-item") + a hint to JSON.stringify if the LLM
// needs the element's properties — instead of crashing.
func EvaluateJS(ctx context.Context, c CDPClient, expression string, awaitPromise bool) (string, error) {
	// Pass 1: raw expression, returnByValue:true (primitives + JSON-serializable).
	out, err := evaluateJSOnce(ctx, c, expression, awaitPromise, true)
	if err != nil {
		return "", err
	}
	if out.syntaxError {
		// Pass 2 (DR②): rescue top-level `return` / statement form via IIFE wrap.
		wrapped := "(() => { " + expression + " })()"
		out2, err2 := evaluateJSOnce(ctx, c, wrapped, awaitPromise, true)
		if err2 != nil {
			return "", err2
		}
		if out2.domReturn {
			return evaluateJSAsRemote(ctx, c, wrapped, awaitPromise)
		}
		return out2.value, nil
	}
	if out.domReturn {
		// Pass 3 (DR③): live DOM element — retry with returnByValue:false.
		return evaluateJSAsRemote(ctx, c, expression, awaitPromise)
	}
	return out.value, nil
}

// guessedCreateURLPattern matches the `/new`-style document-creation URL
// fragments the DK rule prohibits in browser_navigate / browser_new_tab. DR①
// (2026-06-24) extends the prohibition into browser_evaluate_js: the test7/8
// transcripts show the LLM bypassing the navigate guard by evaluating
// `fetch('https://docs.qq.com/api/v1/sheet/create')` and
// `location.href = '.../sheet/new'` — hitting non-existent endpoints and
// getting stuck. This mirrors the DK rule so evaluate_js cannot be used as a
// loophole to guess creation URLs.
var guessedCreateURLPattern = []string{
	"/sheet/new",
	"/doc/new",
	"/slide/new",
	"/form/new",
	"/sheet/create",
	"/doc/create",
	"/api/v1/sheet/",
	"/api/v1/doc/",
}

// detectGuessedCreateURL returns a non-empty guidance string if expression
// references a guessed document-creation URL (the DK prohibition). It checks
// both string literals and bare URL fragments so `fetch('/api/v1/sheet/create')`,
// `location.href='.../sheet/new'`, and `window.open('/doc/new')` are all caught.
// Returns "" when the expression is clean.
func detectGuessedCreateURL(expression string) string {
	low := strings.ToLower(expression)
	for _, frag := range guessedCreateURLPattern {
		if strings.Contains(low, frag) {
			return fmt.Sprintf(
				"refusing: expression references guessed creation URL %q. "+
					"Do NOT guess /new-style or /api/v1/.../create URLs — sites reject no-referrer direct access "+
					"(they return 404/error and get you stuck). To create a document, click the page UI element "+
					"(e.g. the template card) step by step; after the click opens a new tab, follow it with "+
					"browser_list_tabs + browser_switch_tab.",
				frag)
		}
	}
	return ""
}

// evaluateJSAsRemote runs the expression with returnByValue:false and returns
// a helpful description of the resulting RemoteObject (DR③). Used when
// returnByValue:true cannot serialize the result (live DOM element).
func evaluateJSAsRemote(ctx context.Context, c CDPClient, expression string, awaitPromise bool) (string, error) {
	params := mustMarshal(struct {
		Expression    string `json:"expression"`
		ReturnByValue bool   `json:"returnByValue"`
		AwaitPromise  bool   `json:"awaitPromise"`
	}{expression, false, awaitPromise})
	resp, err := c.Call(ctx, "Runtime.evaluate", params)
	if err != nil {
		return "", fmt.Errorf("evaluateJS: %w", err)
	}
	if resp != nil && resp.Error != nil {
		return "", fmt.Errorf("evaluateJS error: %v", resp.Error)
	}
	var eval struct {
		Result struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			ClassName    string `json:"className"`
			Value        json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(resp.Result, &eval); err != nil {
		return "", fmt.Errorf("parse evaluateJS result: %w", err)
	}
	if eval.ExceptionDetails != nil {
		return "", fmt.Errorf("JS exception: %s", eval.ExceptionDetails.Text)
	}
	desc := eval.Result.Description
	if desc == "" {
		desc = eval.Result.Type
	}
	return fmt.Sprintf("%q (DOM/object reference — returnByValue:false; JSON.stringify the value in your expression if you need its properties)", desc), nil
}

// evaluateJSOnce runs Runtime.evaluate once and classifies the outcome.
type evalOutcome struct {
	value       string
	syntaxError bool // true iff the expression failed to parse (top-level return etc.)
	domReturn   bool // true iff returnByValue:true could not serialize (CDP -32000)
}

func evaluateJSOnce(ctx context.Context, c CDPClient, expression string, awaitPromise, returnByValue bool) (evalOutcome, error) {
	params := mustMarshal(struct {
		Expression    string `json:"expression"`
		ReturnByValue bool   `json:"returnByValue"`
		AwaitPromise  bool   `json:"awaitPromise"`
	}{expression, returnByValue, awaitPromise})
	resp, err := c.Call(ctx, "Runtime.evaluate", params)
	if err != nil {
		return evalOutcome{}, fmt.Errorf("evaluateJS: %w", err)
	}
	if resp != nil && resp.Error != nil {
		// DR③: -32000 "Object reference chain is too long" = live DOM element
		// returned with returnByValue:true. Signal the caller to retry as Remote.
		if resp.Error.Code == -32000 || strings.Contains(fmt.Sprintf("%v", resp.Error), "Object reference chain is too long") {
			return evalOutcome{domReturn: true}, nil
		}
		return evalOutcome{}, fmt.Errorf("evaluateJS error: %v", resp.Error)
	}
	var eval struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(resp.Result, &eval); err != nil {
		return evalOutcome{}, fmt.Errorf("parse evaluateJS result: %w", err)
	}
	if eval.ExceptionDetails != nil {
		// SyntaxError (e.g. illegal return statement) is rescuable by IIFE-wrap;
		// other runtime exceptions are real errors.
		if strings.Contains(eval.ExceptionDetails.Text, "SyntaxError") {
			return evalOutcome{syntaxError: true}, nil
		}
		return evalOutcome{}, fmt.Errorf("JS exception: %s", eval.ExceptionDetails.Text)
	}
	return evalOutcome{value: string(eval.Result.Value)}, nil
}

// JSClick clicks an element by resolving its backendNodeId to a DOM element
// via Runtime.evaluate, then calling .click(). This is the Tier 4 fallback for
// React synthetic events — CDP Input.dispatchMouseEvent cannot trigger React's
// onClick handlers, but a DOM .click() call does.
//
// Resolution chain:
//  1. DOM.resolveNode(backendNodeId) → objectId
//  2. Runtime.callFunctionOn(objectId, "function(){ this.click(); }")
//
// If resolveNode fails (e.g. stale node), falls back to a CSS selector approach:
//  3. DOM.describeNode(backendNodeId) → get tag + attributes → build selector
//  4. Runtime.evaluate("document.querySelector(selector).click()")
func JSClick(ctx context.Context, c CDPClient, backendNodeID int) error {
	// Approach 1: DOM.resolveNode → Runtime.callFunctionOn
	resolveParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resolveResp, err := c.Call(ctx, "DOM.resolveNode", resolveParams)
	if err == nil && resolveResp != nil && resolveResp.Error == nil {
		var resolve struct {
			Object struct {
				ObjectID string `json:"objectId"`
				Type     string `json:"type"`
			} `json:"object"`
		}
		if json.Unmarshal(resolveResp.Result, &resolve) == nil && resolve.Object.ObjectID != "" {
			// Dispatch a FULL pointer/mouse event sequence, not just .click().
			// Many React components (Tencent Docs "空白文档" template card is the
			// real-machine case) bind onMouseDown / onPointerDown, not onClick;
			// a bare this.click() only fires a synthetic click event and leaves
			// those handlers cold — the card appears unclickable. Puppeteer and
			// Playwright both dispatch the full sequence; this mirrors them.
			// Returns true if the element was found and the sequence dispatched.
			clickFn := `function(){
				var el = this;
				var opts = { bubbles: true, cancelable: true, view: window, button: 0 };
				var fire = function(type, ctor){
					try { el.dispatchEvent(new (ctor||MouseEvent)(type, opts)); }
					catch(e){ var ev = new Event(type, opts); el.dispatchEvent(ev); }
				};
				if (typeof PointerEvent !== 'undefined') {
					fire('pointerdown', PointerEvent);
					fire('pointerup', PointerEvent);
				}
				fire('mousedown', MouseEvent);
				fire('mouseup', MouseEvent);
				fire('click', MouseEvent);
				return true;
			}`
			callParams := mustMarshal(struct {
				ObjectID            string `json:"objectId"`
				FunctionDeclaration string `json:"functionDeclaration"`
				ReturnByValue       bool   `json:"returnByValue"`
			}{resolve.Object.ObjectID, clickFn, true})
			callResp, callErr := c.Call(ctx, "Runtime.callFunctionOn", callParams)
			if callErr == nil && callResp != nil && callResp.Error == nil {
				return nil
			}
			// callFunctionOn failed — fall through to CSS selector approach
		}
	}

	// Approach 2: Build a CSS selector from DOM.describeNode and use document.querySelector
	describeParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	describeResp, descErr := c.Call(ctx, "DOM.describeNode", describeParams)
	if descErr != nil {
		return fmt.Errorf("js_click: resolveNode and describeNode both failed: %w", descErr)
	}
	if describeResp != nil && describeResp.Error != nil {
		return fmt.Errorf("js_click: resolveNode failed and describeNode error: %v", describeResp.Error)
	}

	var describe struct {
		Node struct {
			NodeName   string   `json:"nodeName"`
			LocalName  string   `json:"localName"`
			Attributes []string `json:"attributes"`
		} `json:"node"`
	}
	if err := json.Unmarshal(describeResp.Result, &describe); err != nil || describe.Node.LocalName == "" {
		return fmt.Errorf("js_click: could not parse describeNode result")
	}

	// Build a simple CSS selector
	tag := describe.Node.LocalName
	selector := tag
	attrs := make(map[string]string, len(describe.Node.Attributes)/2)
	for i := 0; i+1 < len(describe.Node.Attributes); i += 2 {
		attrs[describe.Node.Attributes[i]] = describe.Node.Attributes[i+1]
	}
	// Prefer data-testid, then id, then class
	if v, ok := attrs["data-testid"]; ok {
		selector += fmt.Sprintf(`[data-testid="%s"]`, v)
	} else if v, ok := attrs["id"]; ok && v != "" {
		selector += "#" + v
	} else if v, ok := attrs["class"]; ok && v != "" {
		// Use first class
		classes := strings.Fields(v)
		if len(classes) > 0 {
			selector += "." + classes[0]
		}
	}

	// Use Runtime.evaluate with document.querySelector. Dispatch the full
	// pointer/mouse sequence (same rationale as Approach 1 — see above).
	expr := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if(!el){ return false; }
		var opts = { bubbles: true, cancelable: true, view: window, button: 0 };
		var fire = function(type, ctor){
			try { el.dispatchEvent(new (ctor||MouseEvent)(type, opts)); }
			catch(e){ var ev = new Event(type, opts); el.dispatchEvent(ev); }
		};
		if (typeof PointerEvent !== 'undefined') {
			fire('pointerdown', PointerEvent);
			fire('pointerup', PointerEvent);
		}
		fire('mousedown', MouseEvent);
		fire('mouseup', MouseEvent);
		fire('click', MouseEvent);
		return true;
	})()`, selector)
	result, evalErr := EvaluateJS(ctx, c, expr, false)
	if evalErr != nil {
		return fmt.Errorf("js_click: evaluate selector %q failed: %w", selector, evalErr)
	}
	if result != "true" {
		return fmt.Errorf("js_click: element not found with selector %q", selector)
	}
	return nil
}

// JSFill fills an input element by resolving its backendNodeId to a DOM element
// via Runtime.evaluate, then setting its value and dispatching input/change events.
// This is the Tier 4 fallback for React controlled inputs — CDP Input.insertText
// may not trigger React's onChange handlers, but setting .value + dispatching
// native events does.
//
// Resolution: Same as JSClick — DOM.resolveNode → Runtime.callFunctionOn.
func JSFill(ctx context.Context, c CDPClient, backendNodeID int, value string) error {
	// Approach 1: DOM.resolveNode → Runtime.callFunctionOn
	resolveParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	resolveResp, err := c.Call(ctx, "DOM.resolveNode", resolveParams)
	if err == nil && resolveResp != nil && resolveResp.Error == nil {
		var resolve struct {
			Object struct {
				ObjectID string `json:"objectId"`
				Type     string `json:"type"`
			} `json:"object"`
		}
		if json.Unmarshal(resolveResp.Result, &resolve) == nil && resolve.Object.ObjectID != "" {
			// Set value and dispatch events on the resolved object
			fn := fmt.Sprintf(`function(){ this.focus(); this.value = %q; this.dispatchEvent(new Event('input', {bubbles:true})); this.dispatchEvent(new Event('change', {bubbles:true})); return true; }`, value)
			callParams := mustMarshal(struct {
				ObjectID           string `json:"objectId"`
				FunctionDeclaration string `json:"functionDeclaration"`
				ReturnByValue      bool   `json:"returnByValue"`
			}{resolve.Object.ObjectID, fn, true})
			callResp, callErr := c.Call(ctx, "Runtime.callFunctionOn", callParams)
			if callErr == nil && callResp != nil && callResp.Error == nil {
				return nil
			}
		}
	}

	// Approach 2: Build CSS selector and use Runtime.evaluate (same as JSClick fallback)
	describeParams := mustMarshal(struct {
		BackendNodeID int `json:"backendNodeId"`
	}{backendNodeID})
	describeResp, descErr := c.Call(ctx, "DOM.describeNode", describeParams)
	if descErr != nil {
		return fmt.Errorf("js_fill: resolveNode and describeNode both failed: %w", descErr)
	}
	if describeResp != nil && describeResp.Error != nil {
		return fmt.Errorf("js_fill: resolveNode failed and describeNode error: %v", describeResp.Error)
	}

	var describe struct {
		Node struct {
			NodeName   string   `json:"nodeName"`
			LocalName  string   `json:"localName"`
			Attributes []string `json:"attributes"`
		} `json:"node"`
	}
	if err := json.Unmarshal(describeResp.Result, &describe); err != nil || describe.Node.LocalName == "" {
		return fmt.Errorf("js_fill: could not parse describeNode result")
	}

	tag := describe.Node.LocalName
	selector := tag
	attrs := make(map[string]string, len(describe.Node.Attributes)/2)
	for i := 0; i+1 < len(describe.Node.Attributes); i += 2 {
		attrs[describe.Node.Attributes[i]] = describe.Node.Attributes[i+1]
	}
	if v, ok := attrs["data-testid"]; ok {
		selector += fmt.Sprintf(`[data-testid="%s"]`, v)
	} else if v, ok := attrs["id"]; ok && v != "" {
		selector += "#" + v
	} else if v, ok := attrs["name"]; ok && v != "" {
		selector += fmt.Sprintf(`[name="%s"]`, v)
	} else if v, ok := attrs["placeholder"]; ok && v != "" {
		selector += fmt.Sprintf(`[placeholder="%s"]`, v)
	} else if v, ok := attrs["class"]; ok && v != "" {
		classes := strings.Fields(v)
		if len(classes) > 0 {
			selector += "." + classes[0]
		}
	}

	expr := fmt.Sprintf(`(function(){ var el = document.querySelector(%q); if(el){ el.focus(); el.value = %q; el.dispatchEvent(new Event('input', {bubbles:true})); el.dispatchEvent(new Event('change', {bubbles:true})); return true; } return false; })()`, selector, value)
	result, evalErr := EvaluateJS(ctx, c, expr, false)
	if evalErr != nil {
		return fmt.Errorf("js_fill: evaluate selector %q failed: %w", selector, evalErr)
	}
	if result != "true" {
		return fmt.Errorf("js_fill: element not found with selector %q", selector)
	}
	return nil
}

// resolveNodeIDFromRef resolves an optional ref to a backendNodeId via the
// RefMap (with slow-path re-resolution), else falls back to the nodeId param.
// ref wins if both are present. Returns the backendNodeId to feed into
// ResolvePoint/ClickPoint. Mirrors axtree design RFC §9.
func resolveNodeIDFromRef(ctx context.Context, c CDPClient, req mcptypes.CallToolRequest) (int, error) {
	if ref, ok := optionalString(req, "ref"); ok && ref != "" {
		bid, err := axtree.DefaultRefMap.ResolveRef(ctx, c, ref)
		if err != nil {
			return 0, err
		}
		return int(bid), nil
	}
	if nodeID, ok := optionalInt(req, "nodeId"); ok {
		return nodeID, nil
	}
	return 0, fmt.Errorf("provide either ref or nodeId")
}

// optionalString reads an optional string param, returning (value, ok).
func optionalString(req mcptypes.CallToolRequest, key string) (string, bool) {
	args := req.GetArguments()
	if args == nil {
		return "", false
	}
	val, ok := args[key]
	if !ok {
		return "", false
	}
	s, _ := val.(string)
	return s, s != ""
}

// optionalInt reads an optional int param, returning (value, ok).
func optionalInt(req mcptypes.CallToolRequest, key string) (int, bool) {
	args := req.GetArguments()
	if args == nil {
		return 0, false
	}
	val, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := val.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// CDPCall sends a raw CDP command and returns the result as JSON.
// Aligned with Codex tab_cdp_call: direct CDP passthrough for advanced use.
func CDPCall(ctx context.Context, c CDPClient, method string, params json.RawMessage) (string, error) {
	resp, err := c.Call(ctx, method, params)
	if err != nil {
		return "", fmt.Errorf("cdp_call %s: %w", method, err)
	}
	if resp != nil && resp.Error != nil {
		errJSON, _ := json.Marshal(resp.Error)
		return "", fmt.Errorf("cdp_call %s error: %s", method, string(errJSON))
	}
	if resp.Result == nil {
		return "{}", nil
	}
	return string(resp.Result), nil
}
