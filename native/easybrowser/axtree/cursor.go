package axtree

import (
	"context"
	"encoding/json"
	"fmt"
)

// cursorJS is the single Runtime.evaluate payload that finds cursor-interactive
// elements (pointer/onclick/tabindex/contenteditable), skipping native
// interactive tags and ARIA interactive roles (those are already covered by the
// role whitelist). Ported from agent-browser snapshot.rs:609.
const cursorJS = `(() => {
  const NATIVE = new Set(['A','BUTTON','INPUT','SELECT','TEXTAREA','DETAILS','SUMMARY']);
  const ARIA = new Set(['button','link','checkbox','radio','combobox','listbox','menuitem','option','searchbox','slider','spinbutton','switch','tab','treeitem','textbox']);
  const els = document.querySelectorAll('*');
  const out = [];
  let idx = 0;
  const inheritedPointer = (el) => {
    let p = el.parentElement;
    while (p) { if (getComputedStyle(p).cursor === 'pointer' && p.style.cursor) return true; p = p.parentElement; }
    return false;
  };
  const findLabelFor = (el) => {
    if (el.id && document.querySelector('label[for="' + CSS.escape(el.id) + '"]')) return true;
    return !!el.closest('label');
  };
  for (const el of els) {
    if (NATIVE.has(el.tagName)) continue;
    const role = el.getAttribute('role');
    if (role && ARIA.has(role.toLowerCase())) continue;
    const rect = el.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) continue;
    if (el.hidden || el.getAttribute('aria-hidden') === 'true') continue;
    const cs = getComputedStyle(el);
    const hasPointer = cs.cursor === 'pointer';
    const hasOnClick = !!el.onclick || el.hasAttribute('onclick');
    const hasTabIndex = el.tabIndex >= 0;
    const editable = el.isContentEditable;
    if (!hasPointer && !hasOnClick && !hasTabIndex && !editable) continue;
    if (hasPointer && !el.style.cursor && inheritedPointer(el)) continue;
    let hiddenKind = '', hiddenChecked = '';
    if (el.tagName === 'INPUT' && cs.display === 'none' &&
        (el.type === 'radio' || el.type === 'checkbox') && findLabelFor(el)) {
      hiddenKind = el.type;
      hiddenChecked = el.checked ? 'true' : 'false';
    }
    el.setAttribute('data-__ab-ci', String(idx));
    out.push({i: idx, kind: hiddenKind, checked: hiddenChecked});
    idx++;
  }
  return JSON.stringify(out);
})()`

type cursorHit struct {
	I       int    `json:"i"`
	Kind    string `json:"kind"`
	Checked string `json:"checked"`
}

// findCursorInteractiveElements runs cursorJS once, then resolves each tagged
// element's backendNodeId via DOM.performSearch + DOM.describeNode. Returns
// map[backendNodeId]CursorInfo. Best-effort: failed lookups are skipped.
func findCursorInteractiveElements(ctx context.Context, c CDPClient) (map[int64]CursorInfo, error) {
	evalParams := mustMarshalRaw(struct {
		Expression string `json:"expression"`
		ReturnBy   bool   `json:"returnByValue"`
	}{cursorJS, true})
	resp, err := c.Call(ctx, "Runtime.evaluate", evalParams)
	if err != nil {
		return nil, fmt.Errorf("cursor JS evaluate: %w", err)
	}
	if resp == nil || resp.Error != nil {
		return nil, fmt.Errorf("cursor JS error: %v", resp)
	}
	var eval struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Result, &eval); err != nil {
		return nil, fmt.Errorf("parse cursor eval: %w", err)
	}
	var hits []cursorHit
	if err := json.Unmarshal([]byte(eval.Result.Value), &hits); err != nil {
		return nil, fmt.Errorf("parse cursor hits: %w", err)
	}
	out := map[int64]CursorInfo{}
	for _, h := range hits {
		bid, err := describeByDataAttr(ctx, c, h.I)
		if err != nil || bid == 0 {
			continue // tolerate partial; cursor is a best-effort supplement
		}
		out[bid] = CursorInfo{HiddenInputKind: h.Kind, HiddenInputChecked: h.Checked}
	}
	return out, nil
}

// describeByDataAttr resolves the backendNodeId of the element tagged
// data-__ab-ci=<idx> via the standard CDP search→results→describe chain:
// DOM.performSearch → DOM.getSearchResults → DOM.describeNode(nodeId).
//
// The previous implementation passed searchId/nodeIndex directly to
// DOM.describeNode, but that method only accepts nodeId/backendNodeId/
// objectId — so every cursor lookup failed with "Either nodeId, backendNodeId
// or objectId is not provided" (observed 99.6% failure rate in real-machine
// events.jsonl). The searchId/nodeIndex fields belong to the now-removed
// DOM.getSearchResults-based flow that this function restores.
func describeByDataAttr(ctx context.Context, c CDPClient, idx int) (int64, error) {
	searchParams := mustMarshalRaw(struct {
		Query string `json:"query"`
	}{fmt.Sprintf(`[data-__ab-ci="%d"]`, idx)})
	sresp, err := c.Call(ctx, "DOM.performSearch", searchParams)
	if err != nil || sresp == nil || sresp.Error != nil {
		return 0, fmt.Errorf("performSearch: %v", err)
	}
	var sr struct {
		SearchID  string `json:"searchId"`
		ResultCnt int    `json:"resultCount"`
	}
	if err := json.Unmarshal(sresp.Result, &sr); err != nil {
		return 0, err
	}
	if sr.ResultCnt == 0 {
		return 0, nil
	}

	// getSearchResults: resolve the searchId to concrete nodeIds. CDP returns
	// {nodeIds: [...]} for the [fromIndex, toIndex) range.
	gsrParams := mustMarshalRaw(struct {
		SearchID  string `json:"searchId"`
		FromIndex int    `json:"fromIndex"`
		ToIndex   int    `json:"toIndex"`
	}{sr.SearchID, 0, 1})
	gresp, err := c.Call(ctx, "DOM.getSearchResults", gsrParams)
	if err != nil || gresp == nil || gresp.Error != nil {
		return 0, fmt.Errorf("getSearchResults: %v", err)
	}
	var gr struct {
		NodeIDs []int64 `json:"nodeIds"`
	}
	if err := json.Unmarshal(gresp.Result, &gr); err != nil {
		return 0, err
	}
	if len(gr.NodeIDs) == 0 {
		return 0, nil
	}

	// describeNode(nodeId): now CDP accepts the nodeId and returns the node
	// (with backendNodeId) we need for the cursor map key + ref assignment.
	descParams := mustMarshalRaw(struct {
		NodeID int64 `json:"nodeId"`
	}{gr.NodeIDs[0]})
	dresp, err := c.Call(ctx, "DOM.describeNode", descParams)
	if err != nil || dresp == nil || dresp.Error != nil {
		return 0, fmt.Errorf("describeNode: %v", err)
	}
	// CDP DOM.describeNode returns {node: {backendNodeId, ...}} — the
	// backendNodeId lives one level down under "node".
	var dn struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := json.Unmarshal(dresp.Result, &dn); err != nil {
		return 0, err
	}
	return dn.Node.BackendNodeID, nil
}

// promoteHiddenInputs rewrites a TreeNode whose role is LabelText/generic but
// whose backendNodeId matches a cursor hit with a hidden radio/checkbox into a
// proper radio/checkbox role with checked state. Mirrors snapshot.rs
// promote_hidden_inputs.
func promoteHiddenInputs(tree []TreeNode, cursor map[int64]CursorInfo) {
	for i := range tree {
		tn := &tree[i]
		if tn.Role != "LabelText" && tn.Role != "generic" {
			continue
		}
		ci, ok := cursor[tn.BackendNodeID]
		if !ok || ci.HiddenInputKind == "" {
			continue
		}
		tn.Role = ci.HiddenInputKind // "radio" | "checkbox"
		if ci.HiddenInputChecked != "" {
			c := ci.HiddenInputChecked
			tn.Checked = &c
		}
	}
}

// mustMarshalRaw is the axtree-local marshal helper (mirrors mcp.mustMarshal).
func mustMarshalRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustMarshalRaw: %v", err))
	}
	return b
}
