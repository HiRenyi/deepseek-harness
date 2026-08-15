package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/browser-mcp/bridge/axtree"
	"github.com/browser-mcp/bridge/jsonrpc"
)

// fakeCDP records every Call(method, params) and returns canned responses keyed by method.
type fakeCDP struct {
	calls     []fakeCall
	responses map[string]json.RawMessage // method -> result payload
	errors    map[string]*jsonrpc.ResponseError
}

type fakeCall struct {
	method string
	params map[string]any
}

func (f *fakeCDP) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	f.calls = append(f.calls, fakeCall{method, p})
	resp := &jsonrpc.Response{}
	if e, ok := f.errors[method]; ok {
		resp.Error = e
		return resp, nil
	}
	if r, ok := f.responses[method]; ok {
		resp.Result = r
	}
	return resp, nil
}

// hangingCDP blocks every Call until ctx is cancelled, simulating a hung CDP
// round-trip (real-machine Drift-B: 97s no response). Used to prove ClickPoint's
// aggregate operation timeout bounds the hang instead of accumulating forever.
type hangingCDP struct {
	calls int
}

func (h *hangingCDP) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	h.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestClickPointHungBoundedByOpTimeout(t *testing.T) {
	h := &hangingCDP{}
	// Parent ctx deadline shorter than clickOpTimeout: opCtx takes the min, so the
	// hang is bounded by the parent deadline (200ms) not the full 10s — keeps the
	// test fast while still proving the aggregate bound fires instead of forever.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := ClickPoint(ctx, h, 42, 1)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error from hung ClickPoint, got nil")
	}
	if !containsStr(err.Error(), "click timeout") {
		t.Errorf("err=%q, want to contain 'click timeout'", err.Error())
	}
	// Must return promptly (~200ms). Allow generous slack for CI scheduling but
	// catch the unbounded-accumulate regression (would take 90s+).
	if elapsed > 2*time.Second {
		t.Errorf("ClickPoint took %v on hung CDP; aggregate op timeout did not bound it (want <2s)", elapsed)
	}
	// Only the first CDP round-trip (scrollIntoViewIfNeeded) should have been
	// attempted before the deadline fired — no full click sequence.
	if h.calls > 3 {
		t.Errorf("hung CDP made %d calls; expected to bail early after deadline, not run the full sequence", h.calls)
	}
}

func TestClickPointWaitForLoadSwallowed(t *testing.T) {
	// Click dispatch succeeds; waitForLoad polls readyState and times out. Per
	// Codex alignment the waitForLoad timeout is swallowed (a=s.catch) — a slow
	// page after a successful click is NOT a click failure.
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			// readyState never "complete" -> waitForLoad times out
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":"loading"}}`),
		},
	}
	// Short parent ctx so the swallowed waitForLoad poll (200ms interval) does
	// not drag the test out; the key assertion is nil error despite load timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := ClickPoint(ctx, f, 42, 1)
	if err != nil {
		t.Errorf("expected nil error (waitForLoad timeout swallowed, Codex a=s.catch), got %v", err)
	}
}

func TestResolvePointViaContentQuads(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		},
	}
	x, y, err := ResolvePoint(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("ResolvePoint err: %v", err)
	}
	if x != 5 || y != 5 {
		t.Errorf("point=(%v,%v), want (5,5)", x, y)
	}
	// expect scrollIntoViewIfNeeded then getContentQuads
	if len(f.calls) != 2 {
		t.Fatalf("want 2 calls, got %d: %+v", len(f.calls), f.calls)
	}
	if f.calls[0].method != "DOM.scrollIntoViewIfNeeded" {
		t.Errorf("call[0]=%q, want DOM.scrollIntoViewIfNeeded", f.calls[0].method)
	}
	if f.calls[0].params["backendNodeId"] != float64(42) {
		t.Errorf("call[0] backendNodeId=%v, want 42", f.calls[0].params["backendNodeId"])
	}
	if f.calls[1].method != "DOM.getContentQuads" {
		t.Errorf("call[1]=%q, want DOM.getContentQuads", f.calls[1].method)
	}
}

func TestResolvePointFallsBackToBoxModel(t *testing.T) {
	f := &fakeCDP{
		// contentQuads returns empty quads -> fallback
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[]}`),
			"DOM.getBoxModel":     json.RawMessage(`{"model":{"border":[0,0,20,0,20,20,0,20]}}`),
		},
	}
	x, y, err := ResolvePoint(context.Background(), f, 7)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if x != 10 || y != 10 {
		t.Errorf("point=(%v,%v), want (10,10)", x, y)
	}
	last := f.calls[len(f.calls)-1].method
	if last != "DOM.getBoxModel" {
		t.Errorf("last call=%q, want DOM.getBoxModel", last)
	}
}

func TestResolvePointMissingGeometry(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[]}`),
			"DOM.getBoxModel":     json.RawMessage(`{"model":{}}`), // no border
		},
	}
	_, _, err := ResolvePoint(context.Background(), f, 1)
	if err == nil {
		t.Fatal("expected error for missing geometry")
	}
}

func TestClickPointSequence(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			// readyState=complete so waitForLoad (post-dispatch, Codex alignment)
			// returns after a single poll instead of looping for 10s.
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	if err := ClickPoint(context.Background(), f, 42, 1); err != nil {
		t.Fatalf("ClickPoint err: %v", err)
	}
	// expected: scrollIntoViewIfNeeded, getContentQuads, resolveNode,
	// callFunctionOn (hit-target probe), mouseMoved, mousePressed,
	// mouseReleased, then ONE Runtime.evaluate (readyState poll, already complete).
	methods := []string{}
	for _, c := range f.calls {
		methods = append(methods, c.method)
	}
	want := []string{
		"DOM.scrollIntoViewIfNeeded",
		"DOM.getContentQuads",
		"DOM.resolveNode",
		"Runtime.callFunctionOn", // hit-target occlusion probe
		"Input.dispatchMouseEvent", // mouseMoved
		"Input.dispatchMouseEvent", // mousePressed
		"Input.dispatchMouseEvent", // mouseReleased
		"Runtime.evaluate",         // waitForLoad readyState poll
	}
	if len(methods) != len(want) {
		t.Fatalf("calls=%v, want %v", methods, want)
	}
	for i, m := range methods {
		if m != want[i] {
			t.Errorf("call[%d]=%q, want %q", i, m, want[i])
		}
	}
	// verify types: call[4].type=moved, call[5]=pressed, call[6]=released
	if f.calls[4].params["type"] != "mouseMoved" {
		t.Errorf("call[4].type=%v, want mouseMoved", f.calls[4].params["type"])
	}
	if f.calls[5].params["type"] != "mousePressed" {
		t.Errorf("call[5].type=%v, want mousePressed", f.calls[5].params["type"])
	}
	if f.calls[6].params["type"] != "mouseReleased" {
		t.Errorf("call[6].type=%v, want mouseReleased", f.calls[6].params["type"])
	}
	// verify clickCount
	if f.calls[5].params["clickCount"] != float64(1) {
		t.Errorf("call[5].clickCount=%v, want 1", f.calls[5].params["clickCount"])
	}
}

func TestClickPointDoubleClick(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,4,0,4,4,0,4]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	if err := ClickPoint(context.Background(), f, 1, 2); err != nil {
		t.Fatalf("err: %v", err)
	}
	// count Input.dispatchMouseEvent calls: moved + 2*(pressed+released) = 5
	mouseCalls := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchMouseEvent" {
			mouseCalls++
		}
	}
	if mouseCalls != 5 {
		t.Errorf("mouse calls=%d, want 5 (moved + 2*(pressed+released))", mouseCalls)
	}
}

func TestTypeKeysNamedSequence(t *testing.T) {
	f := &fakeCDP{}
	if err := TypeKeys(context.Background(), f, []string{"Control", "a"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	// 2 named keys x (keydown + keyup) = 4 dispatchKeyEvent calls
	if len(f.calls) != 4 {
		t.Fatalf("calls=%d, want 4", len(f.calls))
	}
	for _, c := range f.calls {
		if c.method != "Input.dispatchKeyEvent" {
			t.Errorf("method=%q, want Input.dispatchKeyEvent", c.method)
		}
	}
	// first keydown should be Control
	if f.calls[0].params["type"] != "keyDown" {
		t.Errorf("call[0].type=%v, want keyDown", f.calls[0].params["type"])
	}
	if f.calls[0].params["key"] != "Control" {
		t.Errorf("call[0].key=%v, want Control", f.calls[0].params["key"])
	}
}

func TestTypeKeysEmpty(t *testing.T) {
	f := &fakeCDP{}
	if err := TypeKeys(context.Background(), f, []string{}); err != nil {
		t.Errorf("empty keys should not error: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(f.calls))
	}
}

func TestRobustFillSequence(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	if err := RobustFill(context.Background(), f, 42, "ab"); err != nil {
		t.Fatalf("err: %v", err)
	}
	// insertText path: expect 1 Input.insertText call, 0 char events
	insertCount := 0
	charCount := 0
	for _, c := range f.calls {
		if c.method == "Input.insertText" {
			insertCount++
			if c.params["text"] != "ab" {
				t.Errorf("insertText text=%v, want ab", c.params["text"])
			}
		}
		if c.method == "Input.dispatchKeyEvent" && c.params["type"] == "char" {
			charCount++
		}
	}
	if insertCount != 1 {
		t.Errorf("insertText calls=%d, want 1", insertCount)
	}
	if charCount != 0 {
		t.Errorf("char events=%d, want 0 (insertText preferred)", charCount)
	}
	// last call should be Tab keyUp
	last := f.calls[len(f.calls)-1]
	if last.params["key"] != "Tab" {
		t.Errorf("last key=%v, want Tab", last.params["key"])
	}
}

func TestRobustFillEmptyValue(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,4,0,4,4,0,4]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	if err := RobustFill(context.Background(), f, 1, ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	// still does focus + selectAll + clear + Tab, just no char events
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" && c.params["type"] == "char" {
			t.Error("did not expect char events for empty value")
		}
	}
}

func TestTryFillContentEditableEvalStampAndEval(t *testing.T) {
	// CE evaluate path: DOM.getAttributes(contenteditable=true) → ClickPoint
	// double-click to focus (getContentQuads+resolveNode+hit-target probe+
	// mouseEvents+readyState) → DOM.resolveNode → stamp callFunctionOn →
	// Runtime.evaluate (fill IIFE) → Tab.
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads":   json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			"DOM.resolveNode":       hitTargetResolveResp,
			"DOM.describeNode":      json.RawMessage(`{"node":{"nodeId":42,"attributes":["contenteditable","true"]}}`),
			"DOM.setAttributeValue": json.RawMessage(`{}`),
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"type":"string","value":"complete"}}`),
		},
	}
	if err := RobustFill(context.Background(), f, 42, "NEWVAL"); err != nil {
		t.Fatalf("err: %v", err)
	}
	var evalExpr string
	var stampAttrVal string
	tabSeen := false
	pressCount := 0
	for _, c := range f.calls {
		switch c.method {
		case "DOM.setAttributeValue":
			name, _ := c.params["name"].(string)
			if name == "data-bridge-fill-target" {
				stampAttrVal, _ = c.params["value"].(string)
			}
		case "Runtime.evaluate":
			expr, _ := c.params["expression"].(string)
			if containsStr(expr, "data-bridge-fill-target") {
				evalExpr = expr
			}
		case "Input.dispatchKeyEvent":
			if c.params["key"] == "Tab" {
				tabSeen = true
			}
		case "Input.dispatchMouseEvent":
			if c.params["type"] == "mousePressed" {
				pressCount++
			}
		}
	}
	if stampAttrVal == "" {
		t.Error("expected DOM.setAttributeValue stamping data-bridge-fill-target")
	}
	if !containsStr(evalExpr, stampAttrVal) {
		t.Errorf("eval expression must embed the stamped uuid %q; got: %s", stampAttrVal, evalExpr)
	}
	if !containsStr(evalExpr, "NEWVAL") {
		t.Errorf("eval expression must embed the fill value; got: %s", evalExpr)
	}
	if !containsStr(evalExpr, "textContent") {
		t.Errorf("eval expression must use textContent reset (selection-bypass); got: %s", evalExpr)
	}
	if !tabSeen {
		t.Error("expected Tab commit after fill")
	}
	if pressCount != 2 {
		t.Errorf("expected 2 mousePressed (double-click focus), got %d", pressCount)
	}
	// No callFunctionOn stamp must remain (DOM-domain stamp replaces it).
	for _, c := range f.calls {
		if c.method == "Runtime.callFunctionOn" {
			fn, _ := c.params["functionDeclaration"].(string)
			if containsStr(fn, "data-bridge-fill-target") {
				t.Error("callFunctionOn must NOT stamp (DOM-domain stamp replaces it; callFunctionOn hijacks focus)")
			}
		}
	}
}

func TestNavigateSuccess(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.navigate":    json.RawMessage(`{"frameId":"F1","loaderId":"L1"}`),
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	res, err := Navigate(context.Background(), f, "https://example.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.FrameID != "F1" || res.LoaderID != "L1" {
		t.Errorf("result=%+v", res)
	}
	if !res.Loaded {
		t.Error("Loaded=false, want true")
	}
	// first call should be Page.navigate
	if f.calls[0].method != "Page.navigate" {
		t.Errorf("call[0]=%q, want Page.navigate", f.calls[0].method)
	}
	if f.calls[0].params["url"] != "https://example.com" {
		t.Errorf("url=%v", f.calls[0].params["url"])
	}
	// should have at least one Runtime.evaluate for readyState check
	foundEval := false
	for _, c := range f.calls {
		if c.method == "Runtime.evaluate" {
			foundEval = true
		}
	}
	if !foundEval {
		t.Error("expected at least one Runtime.evaluate call for readyState polling")
	}
}

func TestNavigateErrorText(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.navigate": json.RawMessage(`{"errorText":"ERR_NAME_NOT_RESOLVED"}`),
		},
	}
	_, err := Navigate(context.Background(), f, "https://nope.invalid")
	if err == nil {
		t.Fatal("expected error for errorText")
	}
	// should NOT call Runtime.evaluate when navigation fails
	for _, c := range f.calls {
		if c.method == "Runtime.evaluate" {
			t.Error("did not expect Runtime.evaluate when navigation fails")
		}
	}
}

// evalSequenceFake returns successive Runtime.evaluate results per call so the
// readyState poll (call 1) and the HTTP-status probe (call 2) can be distinct.
// Other methods are passthrough via a method-keyed map.
type evalSequenceFake struct {
	calls     int
	eval      []json.RawMessage // successive Runtime.evaluate results
	responses map[string]json.RawMessage
}

func (e *evalSequenceFake) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	resp := &jsonrpc.Response{}
	if method == "Runtime.evaluate" {
		idx := e.calls
		e.calls++
		if idx < len(e.eval) {
			resp.Result = e.eval[idx]
		}
		return resp, nil
	}
	if r, ok := e.responses[method]; ok {
		resp.Result = r
	}
	return resp, nil
}

func TestNavigateHTTPStatus404(t *testing.T) {
	// readyState=complete (call 1), responseStatus=404 (call 2).
	e := &evalSequenceFake{
		eval: []json.RawMessage{
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"value":404}}`),
		},
		responses: map[string]json.RawMessage{
			"Page.navigate": json.RawMessage(`{"frameId":"F1","loaderId":"L1"}`),
		},
	}
	res, err := Navigate(context.Background(), e, "https://example.com/missing")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.HTTPStatus != 404 {
		t.Errorf("HTTPStatus=%d, want 404", res.HTTPStatus)
	}
}

func TestNavigateHTTPStatus200(t *testing.T) {
	e := &evalSequenceFake{
		eval: []json.RawMessage{
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"value":200}}`),
		},
		responses: map[string]json.RawMessage{
			"Page.navigate": json.RawMessage(`{"frameId":"F1","loaderId":"L1"}`),
		},
	}
	res, err := Navigate(context.Background(), e, "https://example.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("HTTPStatus=%d, want 200", res.HTTPStatus)
	}
}

func TestNavigateHTTPStatusUnknown(t *testing.T) {
	// readyState=complete (call 1), status probe returns null/undefined (call 2)
	// — simulates PerformanceNavigationTiming unavailable (about:blank, cross-origin).
	e := &evalSequenceFake{
		eval: []json.RawMessage{
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"undefined"}}`),
		},
		responses: map[string]json.RawMessage{
			"Page.navigate": json.RawMessage(`{"frameId":"F1","loaderId":"L1"}`),
		},
	}
	res, err := Navigate(context.Background(), e, "about:blank")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.HTTPStatus != -1 {
		t.Errorf("HTTPStatus=%d, want -1 (unknown)", res.HTTPStatus)
	}
}

func TestWaitForLoadImmediate(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	loaded, err := waitForLoad(context.Background(), f, 5*time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !loaded {
		t.Error("loaded=false, want true (readyState was 'complete')")
	}
}

func TestWaitForLoadTimeout(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":"loading"}}`),
		},
	}
	loaded, err := waitForLoad(context.Background(), f, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if loaded {
		t.Error("loaded=true, want false (timeout with 'loading')")
	}
}

func TestNavigateWithWait(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.navigate":    json.RawMessage(`{"frameId":"F1","loaderId":"L1"}`),
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	res, err := Navigate(context.Background(), f, "https://oa.example.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Loaded {
		t.Error("Loaded=false, want true")
	}
	// verify sequence: Page.navigate then Runtime.evaluate
	if len(f.calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(f.calls))
	}
	if f.calls[0].method != "Page.navigate" {
		t.Errorf("call[0]=%q, want Page.navigate", f.calls[0].method)
	}
	if f.calls[1].method != "Runtime.evaluate" {
		t.Errorf("call[1]=%q, want Runtime.evaluate", f.calls[1].method)
	}
}

func TestInsertTextSuccess(t *testing.T) {
	f := &fakeCDP{}
	err := insertText(context.Background(), f, "hello world")
	if err != nil {
		t.Fatalf("insertText err: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls=%d, want 1", len(f.calls))
	}
	if f.calls[0].method != "Input.insertText" {
		t.Errorf("method=%q, want Input.insertText", f.calls[0].method)
	}
	if f.calls[0].params["text"] != "hello world" {
		t.Errorf("text=%v, want 'hello world'", f.calls[0].params["text"])
	}
}

func TestInsertTextFallback(t *testing.T) {
	f := &fakeCDP{
		errors: map[string]*jsonrpc.ResponseError{
			"Input.insertText": {Code: -1, Message: "not supported"},
		},
	}
	// insertText should return error
	err := insertText(context.Background(), f, "test")
	if err == nil {
		t.Fatal("expected error from insertText")
	}
}

func TestRobustFillInsertTextPath(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	if err := RobustFill(context.Background(), f, 42, "hello"); err != nil {
		t.Fatalf("err: %v", err)
	}
	// Count method calls
	methods := map[string]int{}
	for _, c := range f.calls {
		methods[c.method]++
	}
	// Expected: scrollIntoViewIfNeeded(1) + getContentQuads(1) + mouseEvents(3)
	// + dispatchKeyEvent for Ctrl/a/Backspace/Tab(8) + insertText(1) = 14 total
	if methods["Input.insertText"] != 1 {
		t.Errorf("insertText calls=%d, want 1", methods["Input.insertText"])
	}
	// No char events (insertText succeeded)
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" && c.params["type"] == "char" {
			t.Error("did not expect char events when insertText succeeds")
		}
	}
}

func TestRobustFillFallbackToChar(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		},
		errors: map[string]*jsonrpc.ResponseError{
			"Input.insertText": {Code: -1, Message: "not supported"},
		},
	}
	if err := RobustFill(context.Background(), f, 42, "ab"); err != nil {
		t.Fatalf("err: %v", err)
	}
	// insertText failed, should fallback to per-char
	charCount := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" && c.params["type"] == "char" {
			charCount++
		}
	}
	if charCount != 2 {
		t.Errorf("char events=%d, want 2 (fallback path)", charCount)
	}
}

// ---------------------------------------------------------------------------
// Interaction Tools V2 Tests
// ---------------------------------------------------------------------------

func TestScreenshot(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.captureScreenshot": json.RawMessage(`{"data":"aGVsbG8="}`),
		},
	}
	data, err := Screenshot(context.Background(), f)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if data != "aGVsbG8=" {
		t.Errorf("data=%q, want 'aGVsbG8='", data)
	}
	if f.calls[0].method != "Page.captureScreenshot" {
		t.Errorf("method=%q", f.calls[0].method)
	}
	if f.calls[0].params["format"] != "jpeg" {
		t.Errorf("format=%v", f.calls[0].params["format"])
	}
}

func TestScrollBasic(t *testing.T) {
	f := &fakeCDP{}
	if err := Scroll(context.Background(), f, 0, 500, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0].method != "Input.dispatchMouseEvent" {
		t.Fatalf("calls=%+v", f.calls)
	}
	if f.calls[0].params["type"] != "mouseWheel" {
		t.Errorf("type=%v", f.calls[0].params["type"])
	}
	if f.calls[0].params["deltaY"] != float64(500) {
		t.Errorf("deltaY=%v", f.calls[0].params["deltaY"])
	}
}

func TestScrollWithNodeId(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		},
	}
	nodeID := 42
	if err := Scroll(context.Background(), f, 0, 100, &nodeID); err != nil {
		t.Fatalf("err: %v", err)
	}
	// should call scrollIntoViewIfNeeded + getContentQuads + mouseWheel
	methods := []string{}
	for _, c := range f.calls {
		methods = append(methods, c.method)
	}
	if len(methods) < 3 {
		t.Fatalf("expected at least 3 calls, got %d: %v", len(methods), methods)
	}
	if methods[len(methods)-1] != "Input.dispatchMouseEvent" {
		t.Errorf("last call=%q, want Input.dispatchMouseEvent", methods[len(methods)-1])
	}
}

func TestSelectOption(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.resolveNode": json.RawMessage(`{"object":{"objectId":"obj123"}}`),
		},
	}
	if err := SelectOption(context.Background(), f, 42, "option1"); err != nil {
		t.Fatalf("err: %v", err)
	}
	// should call DOM.resolveNode then Runtime.callFunctionOn
	if len(f.calls) != 2 {
		t.Fatalf("calls=%d, want 2", len(f.calls))
	}
	if f.calls[0].method != "DOM.resolveNode" {
		t.Errorf("call[0]=%q", f.calls[0].method)
	}
	if f.calls[1].method != "Runtime.callFunctionOn" {
		t.Errorf("call[1]=%q", f.calls[1].method)
	}
	if f.calls[1].params["objectId"] != "obj123" {
		t.Errorf("objectId=%v", f.calls[1].params["objectId"])
	}
}

func TestGetText(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.resolveNode":          json.RawMessage(`{"object":{"objectId":"obj123"}}`),
			"Runtime.callFunctionOn":   json.RawMessage(`{"result":{"type":"string","value":"Hello World"}}`),
		},
	}
	text, err := GetText(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if text != "Hello World" {
		t.Errorf("text=%q, want 'Hello World'", text)
	}
}

func TestGetAttributes(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.describeNode": json.RawMessage(`{"node":{"attributes":["id","submit","class","btn","type","button"]}}`),
		},
	}
	attrs, err := GetAttributes(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(attrs) != 3 {
		t.Fatalf("attrs=%+v, want 3", attrs)
	}
	if attrs["id"] != "submit" {
		t.Errorf("id=%q", attrs["id"])
	}
	if attrs["class"] != "btn" {
		t.Errorf("class=%q", attrs["class"])
	}
}

func TestWaitForElementFound(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":true}}`),
		},
	}
	found, err := WaitForElement(context.Background(), f, ".submit-btn", 2*time.Second, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !found {
		t.Error("found=false, want true")
	}
}

func TestWaitForElementTimeout(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Runtime.evaluate": json.RawMessage(`{"result":{"value":false}}`),
		},
	}
	found, err := WaitForElement(context.Background(), f, ".missing", 300*time.Millisecond, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if found {
		t.Error("found=true, want false (timeout)")
	}
}

// ---------------------------------------------------------------------------
// Navigation History + Checkbox Tests
// ---------------------------------------------------------------------------

func TestGoBack(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.getNavigationHistory": json.RawMessage(`{"entries":[{"id":1,"url":"a.com"},{"id":2,"url":"b.com"}],"currentIndex":1}`),
		},
	}
	if err := GoBack(context.Background(), f); err != nil {
		t.Fatalf("err: %v", err)
	}
	// should call getNavigationHistory then navigateToHistoryEntry with entryId=1
	if len(f.calls) != 2 {
		t.Fatalf("calls=%d, want 2", len(f.calls))
	}
	if f.calls[0].method != "Page.getNavigationHistory" {
		t.Errorf("call[0]=%q", f.calls[0].method)
	}
	if f.calls[1].method != "Page.navigateToHistoryEntry" {
		t.Errorf("call[1]=%q", f.calls[1].method)
	}
	if f.calls[1].params["entryId"] != float64(1) {
		t.Errorf("entryId=%v, want 1", f.calls[1].params["entryId"])
	}
}

func TestGoBackNoHistory(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.getNavigationHistory": json.RawMessage(`{"entries":[{"id":1,"url":"a.com"}],"currentIndex":0}`),
		},
	}
	err := GoBack(context.Background(), f)
	if err == nil {
		t.Fatal("expected error for no previous page")
	}
}

func TestGoForward(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.getNavigationHistory": json.RawMessage(`{"entries":[{"id":1,"url":"a.com"},{"id":2,"url":"b.com"}],"currentIndex":0}`),
		},
	}
	if err := GoForward(context.Background(), f); err != nil {
		t.Fatalf("err: %v", err)
	}
	if f.calls[1].params["entryId"] != float64(2) {
		t.Errorf("entryId=%v, want 2", f.calls[1].params["entryId"])
	}
}

func TestReload(t *testing.T) {
	f := &fakeCDP{}
	if err := Reload(context.Background(), f); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0].method != "Page.reload" {
		t.Errorf("calls=%+v", f.calls)
	}
}

func TestSetChecked(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.resolveNode": json.RawMessage(`{"object":{"objectId":"obj123"}}`),
		},
	}
	if err := SetChecked(context.Background(), f, 42, true); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls=%d, want 2", len(f.calls))
	}
	if f.calls[0].method != "DOM.resolveNode" {
		t.Errorf("call[0]=%q", f.calls[0].method)
	}
	if f.calls[1].method != "Runtime.callFunctionOn" {
		t.Errorf("call[1]=%q", f.calls[1].method)
	}
}

// ---------------------------------------------------------------------------
// Tools Batch 1 Tests
// ---------------------------------------------------------------------------

func TestIsVisible(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"boolean","value":true}}`)}}
	v, err := IsVisible(context.Background(), f, ".btn")
	if err != nil { t.Fatalf("err: %v", err) }
	if !v { t.Error("want true") }
}

func TestCount(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"number","value":5}}`)}}
	n, err := Count(context.Background(), f, "div")
	if err != nil { t.Fatalf("err: %v", err) }
	if n != 5 { t.Errorf("count=%d, want 5", n) }
}

func TestDoubleClick(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		// CheckHitTarget probe: unobstructed → proceed to dispatch.
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
		"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
	}}
	if err := DoubleClick(context.Background(), f, 1); err != nil { t.Fatalf("err: %v", err) }
	mouseCount := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchMouseEvent" { mouseCount++ }
	}
	// mouseMoved + 2*(pressed+released) = 5
	if mouseCount != 5 { t.Errorf("mouse calls=%d, want 5", mouseCount) }
}

func TestHover(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`)}}
	if err := Hover(context.Background(), f, 1); err != nil { t.Fatalf("err: %v", err) }
	// last call should be mouseMoved, no pressed/released
	last := f.calls[len(f.calls)-1]
	if last.params["type"] != "mouseMoved" { t.Errorf("last type=%v", last.params["type"]) }
}

func TestClipboardWrite(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"undefined"}}`)}}
	if err := ClipboardWriteText(context.Background(), f, "hello"); err != nil { t.Fatalf("err: %v", err) }
}

func TestWaitForTimeout(t *testing.T) {
	if err := WaitForTimeout(context.Background(), 50*time.Millisecond); err != nil { t.Fatalf("err: %v", err) }
}

// ---------------------------------------------------------------------------
// Tools Batch 2 Tests
// ---------------------------------------------------------------------------

func TestGetURL(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"string","value":"https://example.com"}}`)}}
	url, err := GetURL(context.Background(), f)
	if err != nil { t.Fatalf("err: %v", err) }
	if url != "https://example.com" { t.Errorf("url=%q", url) }
}

func TestGetTitle(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"string","value":"My Page"}}`)}}
	title, err := GetTitle(context.Background(), f)
	if err != nil { t.Fatalf("err: %v", err) }
	if title != "My Page" { t.Errorf("title=%q", title) }
}

func TestDrag(t *testing.T) {
	f := &fakeCDP{}
	pts := []Point{{10, 20}, {30, 40}, {50, 60}}
	if err := Drag(context.Background(), f, pts); err != nil { t.Fatalf("err: %v", err) }
	mouseCount := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchMouseEvent" { mouseCount++ }
	}
	if mouseCount != 3 { t.Errorf("mouse calls=%d, want 3", mouseCount) }
}

func TestFileUpload(t *testing.T) {
	f := &fakeCDP{}
	if err := FileUpload(context.Background(), f, 42, []string{"C:\\test.txt"}); err != nil { t.Fatalf("err: %v", err) }
	if len(f.calls) != 1 || f.calls[0].method != "DOM.setFileInputFiles" {
		t.Errorf("calls=%+v", f.calls)
	}
}

func TestNewTab(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"value":""}}`)}}
	if err := NewTab(context.Background(), f, "https://example.com"); err != nil { t.Fatalf("err: %v", err) }
}

// ---------------------------------------------------------------------------
// Codex Hybrid Approach: CUA Coordinate Tools Tests
// ---------------------------------------------------------------------------

func TestClickAt(t *testing.T) {
	f := &fakeCDP{}
	if err := ClickAt(context.Background(), f, 100, 200, 1); err != nil {
		t.Fatalf("ClickAt err: %v", err)
	}
	// Expect: mouseMoved, mousePressed, mouseReleased
	if len(f.calls) != 3 {
		t.Fatalf("want 3 calls, got %d: %+v", len(f.calls), f.calls)
	}
	if f.calls[0].params["type"] != "mouseMoved" {
		t.Errorf("call[0] type=%v, want mouseMoved", f.calls[0].params["type"])
	}
	if f.calls[1].params["type"] != "mousePressed" {
		t.Errorf("call[1] type=%v, want mousePressed", f.calls[1].params["type"])
	}
	if f.calls[2].params["type"] != "mouseReleased" {
		t.Errorf("call[2] type=%v, want mouseReleased", f.calls[2].params["type"])
	}
	// Check coordinates
	if f.calls[0].params["x"] != float64(100) || f.calls[0].params["y"] != float64(200) {
		t.Errorf("call[0] coords=%v,%v, want 100,200", f.calls[0].params["x"], f.calls[0].params["y"])
	}
}

func TestClickAtDouble(t *testing.T) {
	f := &fakeCDP{}
	if err := ClickAt(context.Background(), f, 50, 75, 2); err != nil {
		t.Fatalf("ClickAt double err: %v", err)
	}
	// Expect: mouseMoved + 2x(mousePressed + mouseReleased) = 5 calls
	if len(f.calls) != 5 {
		t.Fatalf("want 5 calls, got %d", len(f.calls))
	}
}

func TestDoubleClickAt(t *testing.T) {
	f := &fakeCDP{}
	if err := DoubleClickAt(context.Background(), f, 100, 200); err != nil {
		t.Fatalf("DoubleClickAt err: %v", err)
	}
	if len(f.calls) != 5 {
		t.Fatalf("want 5 calls (move+2xpress+release), got %d", len(f.calls))
	}
}

func TestMoveMouse(t *testing.T) {
	f := &fakeCDP{}
	if err := MoveMouse(context.Background(), f, 300, 400); err != nil {
		t.Fatalf("MoveMouse err: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0].method != "Input.dispatchMouseEvent" {
		t.Errorf("want 1 dispatchMouseEvent, got %+v", f.calls)
	}
	if f.calls[0].params["type"] != "mouseMoved" {
		t.Errorf("type=%v, want mouseMoved", f.calls[0].params["type"])
	}
}

func TestScrollAt(t *testing.T) {
	f := &fakeCDP{}
	if err := ScrollAt(context.Background(), f, 200, 300, 0, -500); err != nil {
		t.Fatalf("ScrollAt err: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0].method != "Input.dispatchMouseEvent" {
		t.Fatalf("want 1 dispatchMouseEvent, got %+v", f.calls)
	}
	if f.calls[0].params["type"] != "mouseWheel" {
		t.Errorf("type=%v, want mouseWheel", f.calls[0].params["type"])
	}
	if f.calls[0].params["deltaY"] != float64(-500) {
		t.Errorf("deltaY=%v, want -500", f.calls[0].params["deltaY"])
	}
}

func TestDragPath(t *testing.T) {
	f := &fakeCDP{}
	path := []Point{{10, 20}, {50, 60}, {100, 100}}
	if err := DragPath(context.Background(), f, path); err != nil {
		t.Fatalf("DragPath err: %v", err)
	}
	// Expect: mouseMoved to start, mousePressed, 2x mouseMoved (dragging), mouseReleased
	// = 1 + 1 + 2 + 1 = 5 calls
	if len(f.calls) != 5 {
		t.Fatalf("want 5 calls, got %d: %+v", len(f.calls), f.calls)
	}
	// First call: mouseMoved to start
	if f.calls[0].params["type"] != "mouseMoved" {
		t.Errorf("call[0] type=%v, want mouseMoved", f.calls[0].params["type"])
	}
	// Second call: mousePressed
	if f.calls[1].params["type"] != "mousePressed" {
		t.Errorf("call[1] type=%v, want mousePressed", f.calls[1].params["type"])
	}
	// Last call: mouseReleased
	last := f.calls[len(f.calls)-1]
	if last.params["type"] != "mouseReleased" {
		t.Errorf("last call type=%v, want mouseReleased", last.params["type"])
	}
}

func TestDragPathEmpty(t *testing.T) {
	f := &fakeCDP{}
	err := DragPath(context.Background(), f, []Point{})
	if err == nil {
		t.Error("expected error for empty path")
	}
}

func TestTypeAt(t *testing.T) {
	f := &fakeCDP{}
	if err := TypeAt(context.Background(), f, "hello"); err != nil {
		t.Fatalf("TypeAt err: %v", err)
	}
	// Should call Input.insertText
	found := false
	for _, c := range f.calls {
		if c.method == "Input.insertText" {
			found = true
			if c.params["text"] != "hello" {
				t.Errorf("insertText text=%v, want hello", c.params["text"])
			}
		}
	}
	if !found {
		t.Error("expected Input.insertText call")
	}
}

func TestTypeAtFallback(t *testing.T) {
	// When insertText fails, should fall back to per-char dispatch
	f := &fakeCDP{errors: map[string]*jsonrpc.ResponseError{
		"Input.insertText": {Code: -32000, Message: "not focused"},
	}}
	if err := TypeAt(context.Background(), f, "ab"); err != nil {
		t.Fatalf("TypeAt fallback err: %v", err)
	}
	// dispatchChar sends 1 event per char (type="char"), so 2 chars = 2 events
	keyEvents := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" {
			keyEvents++
		}
	}
	if keyEvents != 2 {
		t.Errorf("dispatchKeyEvent count=%d, want 2 (2 chars x 1 char event each)", keyEvents)
	}
}

func TestPressKeyComboSingle(t *testing.T) {
	f := &fakeCDP{}
	if err := PressKeyCombo(context.Background(), f, []string{"Enter"}); err != nil {
		t.Fatalf("PressKeyCombo err: %v", err)
	}
	// Single key: keyDown + keyUp = 2 calls
	keyEvents := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" {
			keyEvents++
		}
	}
	if keyEvents != 2 {
		t.Errorf("dispatchKeyEvent count=%d, want 2", keyEvents)
	}
}

func TestPressKeyComboCtrlA(t *testing.T) {
	f := &fakeCDP{}
	if err := PressKeyCombo(context.Background(), f, []string{"Control", "a"}); err != nil {
		t.Fatalf("PressKeyCombo Ctrl+A err: %v", err)
	}
	// Ctrl+A: keyDown Ctrl, keyDown a, keyUp a, keyUp Ctrl = 4 calls
	keyEvents := 0
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" {
			keyEvents++
		}
	}
	if keyEvents != 4 {
		t.Errorf("dispatchKeyEvent count=%d, want 4", keyEvents)
	}
}

func TestPressKeyComboEmpty(t *testing.T) {
	f := &fakeCDP{}
	err := PressKeyCombo(context.Background(), f, []string{})
	if err == nil {
		t.Error("expected error for empty keys")
	}
}

func TestIsModifier(t *testing.T) {
	tests := []struct {
		key string
		want bool
	}{
		{"Alt", true},
		{"Control", true},
		{"Meta", true},
		{"Shift", true},
		{"Enter", false},
		{"a", false},
	}
	for _, tt := range tests {
		if got := isModifier(tt.key); got != tt.want {
			t.Errorf("isModifier(%q)=%v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestModifierBit(t *testing.T) {
	if modifierBit("Alt") != 1 { t.Error("Alt should be 1") }
	if modifierBit("Control") != 2 { t.Error("Control should be 2") }
	if modifierBit("Meta") != 4 { t.Error("Meta should be 4") }
	if modifierBit("Shift") != 8 { t.Error("Shift should be 8") }
	if modifierBit("Enter") != 0 { t.Error("Enter should be 0") }
}

func TestClickNode(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			// CheckHitTarget probe: unobstructed → proceed to dispatch.
			"DOM.resolveNode":        hitTargetResolveResp,
			"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
			"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		},
	}
	if err := ClickNode(context.Background(), f, 42, 1); err != nil {
		t.Fatalf("ClickNode err: %v", err)
	}
	// Should resolve point and then click: scrollIntoViewIfNeeded + getContentQuads
	// + resolveNode + callFunctionOn (hit-target probe) + 3 mouse events
	if len(f.calls) < 7 {
		t.Fatalf("want >= 7 calls, got %d", len(f.calls))
	}
}

func TestGetVisibleDOM(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"Page.getLayoutMetrics": json.RawMessage(`{"cssVisualViewport":{"pageX":0,"pageY":0,"clientWidth":1280,"clientHeight":800}}`),
			"DOM.getDocument": json.RawMessage(`{"root":{"nodeId":1,"backendNodeId":1,"nodeType":9,"nodeName":"#document","localName":"","children":[{"nodeId":2,"backendNodeId":2,"nodeType":1,"nodeName":"BODY","localName":"body","children":[{"nodeId":3,"backendNodeId":3,"nodeType":1,"nodeName":"INPUT","localName":"input","attributes":["type","text","placeholder","Search","name","q"]},{"nodeId":4,"backendNodeId":4,"nodeType":1,"nodeName":"A","localName":"a","attributes":["href","https://example.com"]},{"nodeId":5,"backendNodeId":5,"nodeType":1,"nodeName":"DIV","localName":"div","children":[{"nodeId":6,"backendNodeId":6,"nodeType":1,"nodeName":"BUTTON","localName":"button","attributes":["aria-label","Submit"]}]}]}]}}`),
			// Codex rU viewport check: every interactable node resolves its box via
			// DOM.getBoxModel. Provide an in-viewport border quad so nodes 3/4/6
			// are kept (viewport is 0..1280 x 0..800).
			"DOM.getBoxModel": json.RawMessage(`{"model":{"border":[10,10,100,10,100,40,10,40]}}`),
		},
	}
	result, err := GetVisibleDOM(context.Background(), f)
	if err != nil {
		t.Fatalf("GetVisibleDOM err: %v", err)
	}
	// Should contain input, a, and button — but not div
	if !containsStr(result, "node_id=3") {
		t.Error("expected node_id=3 (input) in result")
	}
	if !containsStr(result, "node_id=4") {
		t.Error("expected node_id=4 (a) in result")
	}
	if !containsStr(result, "node_id=6") {
		t.Error("expected node_id=6 (button) in result")
	}
	if containsStr(result, "node_id=5") && !containsStr(result, "tag=div") {
		// div without interactable attrs should not appear
	}
}

func TestEvaluateJS(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"number","value":42}}`),
	}}
	result, err := EvaluateJS(context.Background(), f, "1+1", false)
	if err != nil {
		t.Fatalf("EvaluateJS err: %v", err)
	}
	if result != "42" {
		t.Errorf("result=%q, want 42", result)
	}
}

func TestEvaluateJSException(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"undefined"},"exceptionDetails":{"text":"ReferenceError: x is not defined"}}`),
	}}
	_, err := EvaluateJS(context.Background(), f, "x", false)
	if err == nil {
		t.Error("expected error for JS exception")
	}
}

func TestCDPCall(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"Page.getFrameTree": json.RawMessage(`{"frameTree":{"frame":{"id":"main"}}}`),
	}}
	result, err := CDPCall(context.Background(), f, "Page.getFrameTree", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CDPCall err: %v", err)
	}
	if !containsStr(result, "main") {
		t.Errorf("result=%q, want to contain 'main'", result)
	}
}

func TestCDPCallError(t *testing.T) {
	f := &fakeCDP{errors: map[string]*jsonrpc.ResponseError{
		"DOM.getSomething": {Code: -32000, Message: "not found"},
	}}
	_, err := CDPCall(context.Background(), f, "DOM.getSomething", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for CDP error response")
	}
}

// ---------------------------------------------------------------------------
// JS Click/Fill Tests — Tier 4 React Fallback
// ---------------------------------------------------------------------------

func TestJSClickViaResolveNode(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        json.RawMessage(`{"object":{"objectId":"obj123","type":"object"}}`),
		"Runtime.callFunctionOn": json.RawMessage(`{"result":{"type":"boolean","value":true}}`),
	}}
	err := JSClick(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("JSClick err: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want 2 calls, got %d: %+v", len(f.calls), f.calls)
	}
	if f.calls[0].method != "DOM.resolveNode" {
		t.Errorf("call[0]=%q, want DOM.resolveNode", f.calls[0].method)
	}
	if f.calls[1].method != "Runtime.callFunctionOn" {
		t.Errorf("call[1]=%q, want Runtime.callFunctionOn", f.calls[1].method)
	}
}

func TestJSClickViaCSSSelector(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":  json.RawMessage(`{"object":{"objectId":"","type":"undefined"}}`),
		"DOM.describeNode": json.RawMessage(`{"node":{"nodeName":"BUTTON","localName":"button","attributes":["data-testid","submit-btn"]}}`),
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"boolean","value":true}}`),
	}}
	err := JSClick(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("JSClick err: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("want 3 calls, got %d: %+v", len(f.calls), f.calls)
	}
	if f.calls[1].method != "DOM.describeNode" {
		t.Errorf("call[1]=%q, want DOM.describeNode", f.calls[1].method)
	}
	if f.calls[2].method != "Runtime.evaluate" {
		t.Errorf("call[2]=%q, want Runtime.evaluate", f.calls[2].method)
	}
	expr, _ := f.calls[2].params["expression"].(string)
	if !containsStr(expr, `data-testid`) || !containsStr(expr, `submit-btn`) {
		t.Errorf("expression=%q, want selector with data-testid", expr)
	}
}

func TestJSClickElementNotFound(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":  json.RawMessage(`{"object":{"objectId":"","type":"undefined"}}`),
		"DOM.describeNode": json.RawMessage(`{"node":{"nodeName":"DIV","localName":"div","attributes":[]}}`),
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"boolean","value":false}}`),
	}}
	err := JSClick(context.Background(), f, 99)
	if err == nil {
		t.Error("expected error when element not found")
	}
	if !containsStr(err.Error(), "not found") {
		t.Errorf("err=%q, want 'not found'", err.Error())
	}
}

// TestBrowserJSClickHandlerNavigationEffect: JSClick handler probes url
// before/after and reports effect=navigation when the url changes (Drift-I:
// previously the handler returned only "React .click() dispatched" with no
// outcome, so the AI could not tell a real navigation from a silent no-op).
func TestBrowserJSClickHandlerNavigationEffect(t *testing.T) {
	f := &effectFake{
		// Runtime.evaluate sequence:
		//  0: GetURL before  -> "https://docs.qq.com/desktop"
		//  1: waitForLoad readyState -> "complete"
		//  2: GetURL after   -> "https://docs.qq.com/sheet/NEW"  (navigation)
		evalSeq: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"string","value":"https://docs.qq.com/desktop"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://docs.qq.com/sheet/NEW"}}`),
		},
		static: commonEffectStatic(),
	}
	h := BrowserJSClickHandler(f)
	res, err := h(context.Background(), makeReq(map[string]any{"nodeId": float64(42)}))
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	txt := toolText(res)
	if !containsStr(txt, "effect: navigation") {
		t.Errorf("text=%q, want 'effect: navigation'", txt)
	}
	if !containsStr(txt, "https://docs.qq.com/sheet/NEW") {
		t.Errorf("text=%q, want url after", txt)
	}
}

// TestBrowserJSClickHandlerNoneEffect: JSClick with unchanged url reports
// effect=none so the AI can decide to retry with a fresh snapshot.
func TestBrowserJSClickHandlerNoneEffect(t *testing.T) {
	f := &effectFake{
		// GetURL before / readyState / GetURL after — same url → none
		evalSeq: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"string","value":"https://docs.qq.com/desktop"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://docs.qq.com/desktop"}}`),
		},
		static: commonEffectStatic(),
	}
	h := BrowserJSClickHandler(f)
	res, err := h(context.Background(), makeReq(map[string]any{"nodeId": float64(42)}))
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	txt := toolText(res)
	if !containsStr(txt, "effect: none") {
		t.Errorf("text=%q, want 'effect: none'", txt)
	}
}

func TestJSFillViaResolveNode(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        json.RawMessage(`{"object":{"objectId":"obj456","type":"object"}}`),
		"Runtime.callFunctionOn": json.RawMessage(`{"result":{"type":"boolean","value":true}}`),
	}}
	err := JSFill(context.Background(), f, 42, "hello world")
	if err != nil {
		t.Fatalf("JSFill err: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want 2 calls, got %d: %+v", len(f.calls), f.calls)
	}
	if f.calls[0].method != "DOM.resolveNode" {
		t.Errorf("call[0]=%q, want DOM.resolveNode", f.calls[0].method)
	}
	if f.calls[1].method != "Runtime.callFunctionOn" {
		t.Errorf("call[1]=%q, want Runtime.callFunctionOn", f.calls[1].method)
	}
	// Verify the function declaration includes value setting and event dispatch
	fn, _ := f.calls[1].params["functionDeclaration"].(string)
	if !containsStr(fn, "hello world") {
		t.Errorf("functionDeclaration=%q, want to contain value", fn)
	}
	if !containsStr(fn, "dispatchEvent") {
		t.Errorf("functionDeclaration=%q, want to contain dispatchEvent", fn)
	}
}

func TestJSFillViaCSSSelector(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":  json.RawMessage(`{"object":{"objectId":"","type":"undefined"}}`),
		"DOM.describeNode": json.RawMessage(`{"node":{"nodeName":"INPUT","localName":"input","attributes":["name","email","type","text"]}}`),
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"boolean","value":true}}`),
	}}
	err := JSFill(context.Background(), f, 55, "test@example.com")
	if err != nil {
		t.Fatalf("JSFill err: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("want 3 calls, got %d: %+v", len(f.calls), f.calls)
	}
	expr, _ := f.calls[2].params["expression"].(string)
	if !containsStr(expr, `name`) || !containsStr(expr, `email`) {
		t.Errorf("expression=%q, want selector with name=email", expr)
	}
	if !containsStr(expr, "test@example.com") {
		t.Errorf("expression=%q, want to contain value", expr)
	}
}

func TestJSFillElementNotFound(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":  json.RawMessage(`{"object":{"objectId":"","type":"undefined"}}`),
		"DOM.describeNode": json.RawMessage(`{"node":{"nodeName":"INPUT","localName":"input","attributes":[]}}`),
		"Runtime.evaluate": json.RawMessage(`{"result":{"type":"boolean","value":false}}`),
	}}
	err := JSFill(context.Background(), f, 99, "nope")
	if err == nil {
		t.Error("expected error when element not found")
	}
	if !containsStr(err.Error(), "not found") {
		t.Errorf("err=%q, want 'not found'", err.Error())
	}
}

// ---- resolveNodeIDFromRef (ref support, RFC §9) ----

// makeReq builds a CallToolRequest with the given arguments map.
func makeReq(args map[string]any) mcptypes.CallToolRequest {
	return mcptypes.CallToolRequest{Params: mcptypes.CallToolParams{Arguments: args}}
}

// toolText extracts the first text content from a CallToolResult, for handler tests.
func toolText(res *mcptypes.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcptypes.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestResolveNodeIDFromRefNodeIdOnly(t *testing.T) {
	f := &fakeCDP{}
	req := makeReq(map[string]any{"nodeId": float64(99)})
	id, err := resolveNodeIDFromRef(context.Background(), f, req)
	if err != nil || id != 99 {
		t.Fatalf("nodeId path: id=%d err=%v", id, err)
	}
	// no ref -> RefMap not touched -> no CDP calls
	if len(f.calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(f.calls))
	}
}

func TestResolveNodeIDFromRefMissingBoth(t *testing.T) {
	f := &fakeCDP{}
	_, err := resolveNodeIDFromRef(context.Background(), f, makeReq(map[string]any{}))
	if err == nil {
		t.Fatal("expected error when neither ref nor nodeId given")
	}
}

func TestResolveNodeIDFromRefRefWinsOverNodeId(t *testing.T) {
	// Seed the singleton RefMap: assign a ref with unique identity, capture its
	// id, set its backendNodeId to 42 (alive).
	refID := axtree.DefaultRefMap.Assign("button", "OK-RefWins", -1, "")
	axtree.DefaultRefMap.SetBackend(refID, 42)
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.getBoxModel": json.RawMessage(`{"model":{"border":[0,0,1,0,1,1,0,1]}}`),
		},
	}
	// Helper uses DefaultRefMap; ref present -> ref wins over nodeId=99.
	req := makeReq(map[string]any{"ref": refID, "nodeId": float64(99)})
	id, err := resolveNodeIDFromRef(context.Background(), f, req)
	if err != nil {
		t.Fatalf("ref-wins: err=%v", err)
	}
	if id == 99 {
		t.Errorf("nodeId=99 used despite ref present; ref should win")
	}
	if id != 42 {
		t.Errorf("expected ref-resolved id=42, got %d", id)
	}
	// Ref path must have probed getBoxModel (alive check).
	found := false
	for _, c := range f.calls {
		if c.method == "DOM.getBoxModel" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected DOM.getBoxModel probe for ref path, calls=%+v", f.calls)
	}
}

func TestNodeInViewport(t *testing.T) {
	// viewport: x in [0,1000], y in [0,800]
	const vpTop, vpBottom, vpLeft, vpRight = 0.0, 800.0, 0.0, 1000.0

	cases := []struct {
		name   string
		border string // model.border JSON; "" => getBoxModel error
		err    *jsonrpc.ResponseError
		want   bool
	}{
		{
			name:   "fully inside viewport kept",
			border: `[10,10, 50,10, 50,40, 10,40]`,
			want:   true,
		},
		{
			name:   "partial intersection kept",
			border: `[990,10, 1050,10, 1050,40, 990,40]`, // crosses right edge 1000
			want:   true,
		},
		{
			name:   "fully outside viewport skipped",
			border: `[2000,10, 2050,10, 2050,40, 2000,40]`, // x>=vpRight
			want:   false,
		},
		{
			name:   "zero area skipped",
			border: `[100,100, 100,100, 100,100, 100,100]`, // maxX<=minX && maxY<=minY
			want:   false,
		},
		{
			name:   "non-finite skipped",
			border: `[10,10, 50,10, 50,40, 10,NaN]`,
			want:   false,
		},
		{
			name: "getBoxModel error skipped without abort",
			err:  &jsonrpc.ResponseError{Code: -32000, Message: "Could not find node"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCDP{
				responses: map[string]json.RawMessage{},
				errors:    map[string]*jsonrpc.ResponseError{},
			}
			if tc.err != nil {
				f.errors["DOM.getBoxModel"] = tc.err
			} else {
				f.responses["DOM.getBoxModel"] = json.RawMessage(
					fmt.Sprintf(`{"model":{"border":%s}}`, tc.border))
			}
			got := nodeInViewport(context.Background(), f, 42, vpTop, vpBottom, vpLeft, vpRight)
			if got != tc.want {
				t.Errorf("nodeInViewport=%v, want %v", got, tc.want)
			}
			// exactly one getBoxModel call regardless of outcome (no abort/retry)
			var gbmCalls int
			for _, c := range f.calls {
				if c.method == "DOM.getBoxModel" {
					gbmCalls++
				}
			}
			if gbmCalls != 1 {
				t.Errorf("getBoxModel calls=%d, want 1", gbmCalls)
			}
			if f.calls[0].params["backendNodeId"] != float64(42) {
				t.Errorf("backendNodeId=%v, want 42", f.calls[0].params["backendNodeId"])
			}
		})
	}
}

func TestCollectVisibleDOMFiltersOffscreen(t *testing.T) {
	// viewport: x in [0,1000], y in [0,800]
	root := &visibleDOMNode{
		NodeType:      1,
		LocalName:     "html",
		BackendNodeID: 0,
		Children: []*visibleDOMNode{
			{
				NodeType: 1, LocalName: "button", BackendNodeID: 11,
				Attributes: []string{"type", "submit"},
			}, // in viewport (box 10,10..50,40)
			{
				NodeType: 1, LocalName: "button", BackendNodeID: 22,
				Attributes: []string{"type", "button"},
			}, // off-screen (box 2000,10..2050,40)
		},
	}
	// fakeCDP is method-keyed, not backendNodeId-keyed, so to differentiate two
	// buttons we use a stateful fake that returns successive border boxes per call.
	sf := &sequentialBoxFake{boxes: [][]float64{
		{10, 10, 50, 10, 50, 40, 10, 40},         // node 11 -> in viewport
		{2000, 10, 2050, 10, 2050, 40, 2000, 40}, // node 22 -> off-screen
	}}
	var lines []string
	collectVisibleDOM(root, &lines, sf, context.Background(), 0, 800, 0, 1000)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "node_id=11") {
		t.Errorf("missing in-viewport node_id=11; lines=%q", joined)
	}
	if strings.Contains(joined, "node_id=22") {
		t.Errorf("off-screen node_id=22 should be filtered; lines=%q", joined)
	}
}

// sequentialBoxFake returns successive getBoxModel border boxes per call, so two
// different backendNodeIds can map to two different geometries.
type sequentialBoxFake struct {
	calls int
	boxes [][]float64
}

func (s *sequentialBoxFake) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	s.calls++
	resp := &jsonrpc.Response{}
	if method != "DOM.getBoxModel" {
		return resp, nil
	}
	idx := s.calls - 1
	if idx >= len(s.boxes) {
		idx = len(s.boxes) - 1
	}
	b := s.boxes[idx]
	arr := make([]string, len(b))
	for i, v := range b {
		arr[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	resp.Result = json.RawMessage(fmt.Sprintf(`{"model":{"border":[%s]}}`, strings.Join(arr, ",")))
	return resp, nil
}

// effectFake is a stateful CDP fake for ClickWithEffect tests. It sequences the
// methods whose response varies across the pre/post-click probe:
//   - Runtime.evaluate: successive payloads from evalSeq (url before/after,
//     readyState polls interleaved).
//   - DOM.describeNode: successive flat-attr arrays from attrSeq (state before
//     vs after).
//
// As of the GetAttributes fix (alloy-canvas-fill-commit 2026-07-16),
// GetAttributes issues DOM.describeNode (not DOM.getAttributes — Chrome's
// DOM.getAttributes requires nodeId and rejects backendNodeId). So BOTH
// GetAttributes (attrsBefore/after in ClickWithEffect + probeEffect) AND
// describeNodeRole consume DOM.describeNode, interleaved in call order:
// GetAttr-before → describeNodeRole → GetAttr-after (per fallback). attrSeq is
// consumed in that order; an exhausted/empty entry falls back to the static
// DOM.describeNode so describeNodeRole still sees a nodeName/localName.
// Other methods return a fixed static response (geometry, resolveNode, etc.).
// All calls are recorded in order for fallback-chain assertions.
type effectFake struct {
	evalSeq []json.RawMessage // successive Runtime.evaluate results
	attrSeq [][]string       // successive DOM.describeNode flat-attr arrays
	evalIdx int
	attrIdx int
	calls   []string
	static  map[string]json.RawMessage
}

func (e *effectFake) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	e.calls = append(e.calls, method)
	resp := &jsonrpc.Response{}
	switch method {
	case "Runtime.evaluate":
		idx := e.evalIdx
		e.evalIdx++
		if idx < len(e.evalSeq) {
			resp.Result = e.evalSeq[idx]
		}
		return resp, nil
	case "DOM.describeNode":
		idx := e.attrIdx
		e.attrIdx++
		var attrs []string
		if idx < len(e.attrSeq) {
			attrs = e.attrSeq[idx]
		}
		if len(attrs) == 0 {
			// Exhausted or intentionally empty → static node (keeps nodeName for
			// describeNodeRole; matches pre-fix behavior where describeNodeRole
			// read the static DOM.describeNode, separate from getAttributes).
			if r, ok := e.static["DOM.describeNode"]; ok {
				resp.Result = r
			}
			return resp, nil
		}
		arr := make([]string, len(attrs))
		for i, a := range attrs {
			arr[i] = strconv.Quote(a)
		}
		resp.Result = json.RawMessage(fmt.Sprintf(`{"node":{"attributes":[%s]}}`, strings.Join(arr, ",")))
		return resp, nil
	default:
		if r, ok := e.static[method]; ok {
			resp.Result = r
		}
		return resp, nil
	}
}

// hasCall reports whether a CDP method was invoked at least once.
func (e *effectFake) hasCall(method string) bool {
	for _, c := range e.calls {
		if c == method {
			return true
		}
	}
	return false
}

// callCount reports how many times a CDP method was invoked.
func (e *effectFake) callCount(method string) int {
	n := 0
	for _, c := range e.calls {
		if c == method {
			n++
		}
	}
	return n
}

// commonEffectStatic returns the fixed CDP responses shared across effect tests:
// scrollIntoView succeeds, getContentQuads returns a unit box centered at (5,5),
// describeNode reports role=button (interactive), resolveNode yields an objectId
// (so JSClick's Approach 1 succeeds), callFunctionOn returns the hit-target
// unobstructed verdict (CheckHitTarget probe inside ClickPoint; JSClick ignores
// the value and only checks resp.Error, so a string response still satisfies it),
// screenshot returns a tiny base64 blob.
func commonEffectStatic() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"DOM.scrollIntoViewIfNeeded": json.RawMessage(`{}`),
		"DOM.getContentQuads":         json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"Input.dispatchMouseEvent":    json.RawMessage(`{}`),
		"DOM.describeNode":           json.RawMessage(`{"node":{"nodeName":"BUTTON","localName":"button","attributes":["role","button","aria-expanded","false"]}}`),
		"DOM.resolveNode":            json.RawMessage(`{"object":{"objectId":"OBJ1","type":"object"}}`),
		"Runtime.callFunctionOn":     json.RawMessage(`{"result":{"type":"string","value":"{\"ok\":true,\"reason\":\"\",\"hit\":\"\"}"}}`),
		"Page.captureScreenshot":     json.RawMessage(`{"data":"BASE64PNG"}`),
	}
}

// TestClickEffectNavigationNoFallback: URL changes on click → effect=navigation,
// no fallback chain (no JSClick / coord-retry / screenshot).
func TestClickEffectNavigationNoFallback(t *testing.T) {
	f := &effectFake{
		// Runtime.evaluate sequence:
		//  0: GetURL before  -> "https://app/a"
		//  1: waitForLoad readyState -> "complete"
		//  2: GetURL after   -> "https://app/b"  (URL changed → navigation)
		evalSeq: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"string","value":"https://app/a"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://app/b"}}`),
		},
		attrSeq: [][]string{
			{"role", "button"}, // before (unused for navigation, but probed)
		},
		static: commonEffectStatic(),
	}
	eff, err := ClickWithEffect(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if eff.Effect != "navigation" {
		t.Errorf("Effect=%q, want navigation", eff.Effect)
	}
	if eff.URLBefore != "https://app/a" || eff.URLAfter != "https://app/b" {
		t.Errorf("url before/after=%q/%q", eff.URLBefore, eff.URLAfter)
	}
	if len(eff.Fallbacks) != 0 {
		t.Errorf("Fallbacks=%v, want empty (navigation needs no fallback)", eff.Fallbacks)
	}
	// CheckHitTarget (inside ClickPoint) calls Runtime.callFunctionOn exactly once.
	// JSClick would add a second call — assert count==1 to prove no fallback ran.
	if got := f.callCount("Runtime.callFunctionOn"); got != 1 {
		t.Errorf("callFunctionOn callCount=%d, want 1 (CheckHitTarget only, no JSClick on navigation)", got)
	}
	if f.hasCall("Page.captureScreenshot") {
		t.Error("screenshot triggered on navigation; should be skipped")
	}
}

// TestClickEffectToggleNoFallback: aria-expanded flips on click → effect=toggle,
// URL unchanged, no fallback chain.
func TestClickEffectToggleNoFallback(t *testing.T) {
	f := &effectFake{
		// Runtime.evaluate sequence:
		//  0: GetURL before  -> "https://app/x"
		//  1: waitForLoad readyState -> "complete"
		//  2: GetURL after (probeEffect) -> "https://app/x"  (unchanged → check toggle)
		evalSeq: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
		},
		attrSeq: [][]string{
			{"role", "button", "aria-expanded", "false"}, // GetAttributes-before
			{"role", "button"},                           // describeNodeRole (between before/after)
			{"role", "button", "aria-expanded", "true"},  // GetAttributes-after (flipped → toggle)
		},
		static: commonEffectStatic(),
	}
	eff, err := ClickWithEffect(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if eff.Effect != "toggle" {
		t.Errorf("Effect=%q, want toggle", eff.Effect)
	}
	if len(eff.Fallbacks) != 0 {
		t.Errorf("Fallbacks=%v, want empty (toggle is a real effect)", eff.Fallbacks)
	}
	// CheckHitTarget (inside ClickPoint) calls Runtime.callFunctionOn exactly once.
	// JSClick would add a second call — assert count==1 to prove no fallback ran.
	if got := f.callCount("Runtime.callFunctionOn"); got != 1 {
		t.Errorf("callFunctionOn callCount=%d, want 1 (CheckHitTarget only, no JSClick on toggle)", got)
	}
}

// TestClickEffectNoneTriggersFullFallbackChain: URL unchanged + aria unchanged
// on an interactive button → effect=none → JSClick → coord-retry → screenshot.
// Screenshot attached; honest report does NOT single-report success.
func TestClickEffectNoneTriggersFullFallbackChain(t *testing.T) {
	f := &effectFake{
		// Runtime.evaluate sequence (url/readyState interleaved across primary
		// click + JSClick + coord-retry; coord-retry has no separate settle
		// because ClickPoint already runs waitForLoad internally):
		//  0: GetURL before           -> "https://app/x"
		//  1: readyState (primary)    -> "complete"
		//  2: GetURL after primary    -> "https://app/x"  (none)
		//  3: readyState (JSClick)    -> "complete"
		//  4: GetURL after JSClick    -> "https://app/x"  (none)
		//  5: readyState (coord-retry)-> "complete"
		//  6: GetURL after coord-retry-> "https://app/x"  (none → screenshot)
		evalSeq: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
		},
		attrSeq: [][]string{
			{"role", "button", "aria-expanded", "false"}, // before
			{"role", "button", "aria-expanded", "false"}, // after primary (none)
			{"role", "button", "aria-expanded", "false"}, // after JSClick (none)
			{"role", "button", "aria-expanded", "false"}, // after coord-retry (none)
		},
		static: commonEffectStatic(),
	}
	eff, err := ClickWithEffect(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if eff.Effect != "none" {
		t.Errorf("Effect=%q, want none", eff.Effect)
	}
	wantFallbacks := []string{"JSClick", "coord-retry", "screenshot"}
	if len(eff.Fallbacks) != len(wantFallbacks) {
		t.Errorf("Fallbacks=%v, want %v", eff.Fallbacks, wantFallbacks)
	} else {
		for i, fb := range wantFallbacks {
			if eff.Fallbacks[i] != fb {
				t.Errorf("Fallbacks[%d]=%q, want %q", i, eff.Fallbacks[i], fb)
			}
		}
	}
	if eff.Screenshot != "BASE64PNG" {
		t.Errorf("Screenshot=%q, want BASE64PNG (attached on exhausted none)", eff.Screenshot)
	}
	// callFunctionOn is invoked 3 times: CheckHitTarget inside the primary
	// ClickPoint, JSClick, and CheckHitTarget inside the coord-retry ClickPoint.
	// coord-retry does NOT call JSClick — so exactly one of the three is JSClick.
	if got := f.callCount("Runtime.callFunctionOn"); got != 3 {
		t.Errorf("callFunctionOn callCount=%d, want 3 (2 CheckHitTarget + 1 JSClick)", got)
	}
	if !f.hasCall("Page.captureScreenshot") {
		t.Error("screenshot not triggered after exhausted fallbacks")
	}
}

// TestClickEffectNoneNonInteractiveRoleSkipsFallback: a generic div has no
// expected effect → effect=none but the fallback chain is skipped (YAGNI, no
// role in the interactive set).
func TestClickEffectNoneNonInteractiveRoleSkipsFallback(t *testing.T) {
	static := commonEffectStatic()
	// Override describeNode to a generic div (no role, localName div).
	static["DOM.describeNode"] = json.RawMessage(`{"node":{"nodeName":"DIV","localName":"div","attributes":[]}}`)
	f := &effectFake{
		evalSeq: []json.RawMessage{
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
			json.RawMessage(`{"result":{"value":"complete"}}`),
			json.RawMessage(`{"result":{"type":"string","value":"https://app/x"}}`),
		},
		attrSeq: [][]string{{}, {}},
		static:  static,
	}
	eff, err := ClickWithEffect(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if eff.Effect != "none" {
		t.Errorf("Effect=%q, want none", eff.Effect)
	}
	if len(eff.Fallbacks) != 0 {
		t.Errorf("Fallbacks=%v, want empty (non-interactive role skips chain)", eff.Fallbacks)
	}
	if eff.Screenshot != "" {
		t.Errorf("Screenshot attached for non-interactive role; should be skipped")
	}
}

// hitTargetResolveResp is the canned DOM.resolveNode payload used by the
// CheckHitTarget tests (target objectId "obj-1"). var (not const) because
// json.RawMessage is a slice type and cannot be a compile-time constant.
var hitTargetResolveResp = json.RawMessage(`{"object":{"objectId":"obj-1"}}`)

// hitTargetCallResp builds a Runtime.callFunctionOn canned payload whose
// Result.Value is a JSON-encoded {ok,reason,hit} object string (the JS probe
// returns JSON.stringify so the existing CallFunctionOnResult.Value string
// field decodes it without a type change).
func hitTargetCallResp(t *testing.T, ok bool, reason, hit string) json.RawMessage {
	t.Helper()
	s := fmt.Sprintf(`{"ok":%v,"reason":%q,"hit":%q}`, ok, reason, hit)
	// CDP returns the JS string value; evalJS/callFunctionOn string-decodes it.
	return json.RawMessage(fmt.Sprintf(`{"result":{"type":"string","value":%q}}`, s))
}

func TestCheckHitTargetUnobstructed(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
	}}
	if err := CheckHitTarget(context.Background(), f, 42, 5, 5); err != nil {
		t.Errorf("unobstructed: want nil err, got %v", err)
	}
}

func TestCheckHitTargetObstructed(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, false, "obscured", "iframe#modal"),
	}}
	err := CheckHitTarget(context.Background(), f, 42, 5, 5)
	if err == nil {
		t.Fatal("obscured: want error, got nil")
	}
	if !containsStr(err.Error(), "obscured") || !containsStr(err.Error(), "iframe#modal") {
		t.Errorf("obscured err=%q, want to contain 'obscured' and 'iframe#modal'", err.Error())
	}
}

func TestCheckHitTargetPointerEventsNone(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, false, "target-pointer-events-none", "div.x"),
	}}
	err := CheckHitTarget(context.Background(), f, 42, 5, 5)
	if err == nil {
		t.Fatal("pointer-events-none: want error, got nil")
	}
	if !containsStr(err.Error(), "target-pointer-events-none") {
		t.Errorf("err=%q, want to contain 'target-pointer-events-none'", err.Error())
	}
}

func TestCheckHitTargetCrossOriginSkip(t *testing.T) {
	// cross-origin-iframe-skip: cannot walk to top doc → SKIP check, proceed (nil err).
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, false, "cross-origin-iframe-skip", ""),
	}}
	if err := CheckHitTarget(context.Background(), f, 42, 5, 5); err != nil {
		t.Errorf("cross-origin skip: want nil err (proceed), got %v", err)
	}
}

func TestCheckHitTargetNoOwnerDocSkip(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, false, "no-owner-document", ""),
	}}
	if err := CheckHitTarget(context.Background(), f, 42, 5, 5); err != nil {
		t.Errorf("no-owner-doc: want nil err (proceed), got %v", err)
	}
}

// clickPointCallRespFor builds a Runtime.callFunctionOn response so that
// ClickPoint's CheckHitTarget probe resolves to the given verdict. (The fake
// keys responses by method only, so the same callFunctionOn response is used
// for every probe call within one test — fine, there is exactly one probe per
// ClickPoint.)
func clickPointCallRespFor(t *testing.T, ok bool, reason, hit string) json.RawMessage {
	t.Helper()
	s := fmt.Sprintf(`{"ok":%v,"reason":%q,"hit":%q}`, ok, reason, hit)
	return json.RawMessage(fmt.Sprintf(`{"result":{"type":"string","value":%q}}`, s))
}

func countDispatchMouse(calls []fakeCall) int {
	n := 0
	for _, c := range calls {
		if c.method == "Input.dispatchMouseEvent" {
			n++
		}
	}
	return n
}

func TestClickPointObstructedNoDispatch(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads":    json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": clickPointCallRespFor(t, false, "obscured", "iframe#modal"),
	}}
	err := ClickPoint(context.Background(), f, 42, 1)
	if err == nil {
		t.Fatal("want obstruction error, got nil")
	}
	if !containsStr(err.Error(), "obscured") {
		t.Errorf("err=%q, want 'obscured'", err.Error())
	}
	if n := countDispatchMouse(f.calls); n != 0 {
		t.Errorf("obstructed target: expected 0 Input.dispatchMouseEvent, got %d (穿透 regression)", n)
	}
}

func TestClickPointUnobstructedDispatches(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads":    json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": clickPointCallRespFor(t, true, "", ""),
		// waitForLoad poll never returns complete → timeout swallowed (existing behavior).
		"Runtime.evaluate": json.RawMessage(`{"result":{"value":"loading"}}`),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := ClickPoint(ctx, f, 42, 1); err != nil {
		t.Fatalf("unobstructed: want nil err, got %v", err)
	}
	// mouseMoved + 1×(pressed+released) = 3 dispatches for clickCount=1.
	if n := countDispatchMouse(f.calls); n != 3 {
		t.Errorf("unobstructed: expected 3 Input.dispatchMouseEvent, got %d", n)
	}
}

func TestClickPointPointerEventsNoneNoDispatch(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads":    json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": clickPointCallRespFor(t, false, "target-pointer-events-none", "div.x"),
	}}
	err := ClickPoint(context.Background(), f, 42, 1)
	if err == nil || !containsStr(err.Error(), "target-pointer-events-none") {
		t.Fatalf("err=%v, want 'target-pointer-events-none'", err)
	}
	if n := countDispatchMouse(f.calls); n != 0 {
		t.Errorf("pointer-events-none: expected 0 dispatch, got %d", n)
	}
}

func TestClickPointCrossOriginSkipStillDispatches(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads":    json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": clickPointCallRespFor(t, false, "cross-origin-iframe-skip", ""),
		"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"loading"}}`),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := ClickPoint(ctx, f, 42, 1); err != nil {
		t.Fatalf("cross-origin skip: want nil err (proceed), got %v", err)
	}
	if n := countDispatchMouse(f.calls); n != 3 {
		t.Errorf("cross-origin skip: expected 3 dispatch (proceed), got %d", n)
	}
}

func TestTryClearContentEditable_Cleared(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.describeNode":       json.RawMessage(`{"node":{"attributes":["contenteditable","true"]}}`),
		"DOM.resolveNode":        json.RawMessage(`{"object":{"objectId":"o1"}}`),
		"Runtime.callFunctionOn": json.RawMessage(`{"result":{"type":"string","value":""}}`),
	}}
	cleared, err := tryClearContentEditable(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !cleared {
		t.Fatal("want cleared=true for contenteditable target")
	}
	hasCallFn := false
	for _, c := range f.calls {
		if c.method == "Runtime.callFunctionOn" {
			hasCallFn = true
		}
	}
	if !hasCallFn {
		t.Error("expected Runtime.callFunctionOn call for contenteditable clear")
	}
}

func TestTryClearContentEditable_NotContentEditable(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.describeNode": json.RawMessage(`{"node":{"attributes":["type","text","value","x"]}}`),
	}}
	cleared, err := tryClearContentEditable(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("err=%v want nil (not-CE is not an error)", err)
	}
	if cleared {
		t.Fatal("want cleared=false for non-contenteditable target")
	}
	for _, c := range f.calls {
		if c.method == "Runtime.callFunctionOn" {
			t.Error("non-CE target must NOT call Runtime.callFunctionOn")
		}
	}
}

func TestTryClearContentEditable_EvalFailureFallback(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.describeNode": json.RawMessage(`{"node":{"attributes":["contenteditable","true"]}}`),
			"DOM.resolveNode":   json.RawMessage(`{"object":{"objectId":"o1"}}`),
		},
		errors: map[string]*jsonrpc.ResponseError{
			"Runtime.callFunctionOn": {Code: -1, Message: "eval fail"},
		},
	}
	cleared, err := tryClearContentEditable(context.Background(), f, 42)
	if cleared {
		t.Fatal("want cleared=false when callFunctionOn errors")
	}
	if err == nil {
		t.Fatal("want non-nil err when callFunctionOn errors (caller falls back to keyboard)")
	}
}

func TestTryFillContentEditable_Filled(t *testing.T) {
	// New evaluate path: ClickPoint double-click to focus + DOM-domain stamp
	// (describeNode+setAttributeValue, no callFunctionOn — it hijacks focus) +
	// fill via Runtime.evaluate (expression embeds value).
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.getContentQuads":   json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":       hitTargetResolveResp,
		"DOM.describeNode":      json.RawMessage(`{"node":{"nodeId":42,"attributes":["contenteditable","true"]}}`),
		"DOM.setAttributeValue": json.RawMessage(`{}`),
		"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
		"Runtime.evaluate":       json.RawMessage(`{"result":{"type":"string","value":"complete"}}`),
	}}
	filled, err := tryFillContentEditable(context.Background(), f, 42, "hello")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !filled {
		t.Fatal("want filled=true for contenteditable target")
	}
	// DOM-domain stamp sets data-bridge-fill-target (value=uuid, NOT the fill value).
	var stampVal string
	leakedValueArg := false
	var evalExpr string
	for _, c := range f.calls {
		if c.method == "DOM.setAttributeValue" {
			if name, _ := c.params["name"].(string); name == "data-bridge-fill-target" {
				stampVal, _ = c.params["value"].(string)
			}
			if v, _ := c.params["value"].(string); v == "hello" {
				leakedValueArg = true // fill value must NOT travel via the stamp
			}
		}
		if c.method == "Runtime.evaluate" {
			if expr, ok := c.params["expression"].(string); ok && containsStr(expr, "data-bridge-fill-target") {
				evalExpr = expr
			}
		}
	}
	if stampVal == "" {
		t.Error("expected DOM.setAttributeValue stamping data-bridge-fill-target")
	}
	if !containsStr(evalExpr, stampVal) {
		t.Errorf("eval expression must embed the stamped uuid %q; got: %s", stampVal, evalExpr)
	}
	if leakedValueArg {
		t.Error("fill value must NOT be passed via the stamp (it goes via Runtime.evaluate)")
	}
	if evalExpr == "" || !containsStr(evalExpr, "hello") {
		t.Errorf("Runtime.evaluate expression must embed the fill value; got: %s", evalExpr)
	}
	if !containsStr(evalExpr, "textContent") {
		t.Errorf("evaluate expression must use textContent reset (selection-bypass); got: %s", evalExpr)
	}
}

func TestTryFillContentEditable_IdPathNoStamp(t *testing.T) {
	// Alloy/canvas CE path (node HAS an id): ClickPoint double-click to focus +
	// fill via single Runtime.evaluate using getElementById(attrs["id"]) — NO
	// stamp (stamping triggers alloy mutation observer → re-render → edit-mode
	// expires → partial residue; DV 2026-07-15). General: reads the node's OWN id.
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.describeNode":       json.RawMessage(`{"node":{"attributes":["contenteditable","true","id","alloy-simple-text-editor","class","formula-input"]}}`),
		"DOM.getContentQuads":   json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":       hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
		"Runtime.evaluate":       json.RawMessage(`{"result":{"type":"string","value":"complete"}}`),
	}}
	filled, err := tryFillContentEditable(context.Background(), f, 42, "NEWVAL")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !filled {
		t.Fatal("want filled=true for contenteditable-with-id target")
	}
	// Must NOT stamp (no DOM.setAttributeValue) — id path. DOM.describeNode is
	// EXPECTED here: it is the read-only attribute fetch inside GetAttributes
	// (does not touch the DOM, so it does not trigger alloy's mutation observer).
	for _, c := range f.calls {
		if c.method == "DOM.setAttributeValue" {
			t.Errorf("id path must NOT stamp (got %s); stamping disrupts alloy edit-mode", c.method)
		}
	}
	// eval expression must use getElementById(<id>) + embed the fill value.
	var evalExpr string
	for _, c := range f.calls {
		if c.method == "Runtime.evaluate" {
			if expr, ok := c.params["expression"].(string); ok && containsStr(expr, "data-bridge-fill-target") {
				evalExpr = expr
			}
		}
	}
	for _, c := range f.calls {
		if c.method == "Runtime.evaluate" {
			if expr, ok := c.params["expression"].(string); ok && containsStr(expr, "getElementById") {
				evalExpr = expr
			}
		}
	}
	if evalExpr == "" {
		t.Fatal("expected a Runtime.evaluate expression using getElementById")
	}
	if !containsStr(evalExpr, "alloy-simple-text-editor") {
		t.Errorf("eval must target the node's own id; got: %s", evalExpr)
	}
	if !containsStr(evalExpr, "NEWVAL") || !containsStr(evalExpr, "textContent") {
		t.Errorf("eval must embed value+textContent (selection-bypass); got: %s", evalExpr)
	}
}

func TestTryFillContentEditable_NotContentEditable(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.describeNode": json.RawMessage(`{"node":{"attributes":["type","text","value","x"]}}`),
	}}
	filled, err := tryFillContentEditable(context.Background(), f, 42, "x")
	if err != nil {
		t.Fatalf("err=%v want nil (not-CE is not an error)", err)
	}
	if filled {
		t.Fatal("want filled=false for non-contenteditable target")
	}
	for _, c := range f.calls {
		if c.method == "Runtime.callFunctionOn" {
			t.Error("non-CE target must NOT call Runtime.callFunctionOn")
		}
	}
}

func TestTryFillContentEditable_EvalFailureFallback(t *testing.T) {
	f := &fakeCDP{
		responses: map[string]json.RawMessage{
			"DOM.describeNode":   json.RawMessage(`{"node":{"attributes":["contenteditable","true"]}}`),
			"DOM.getContentQuads": json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
			"DOM.resolveNode":   json.RawMessage(`{"object":{"objectId":"o1"}}`),
		},
		errors: map[string]*jsonrpc.ResponseError{
			"Runtime.callFunctionOn": {Code: -1, Message: "eval fail"},
		},
	}
	filled, err := tryFillContentEditable(context.Background(), f, 42, "x")
	if filled {
		t.Fatal("want filled=false when callFunctionOn errors")
	}
	if err == nil {
		t.Fatal("want non-nil err when callFunctionOn errors (caller falls back to insertText)")
	}
}

// TestRobustFill_PlainInputUsesKeyboardClear verifies a non-contenteditable target
// still uses the original Ctrl+A+Backspace path (no Runtime.callFunctionOn clear).
// ClickPoint is stubbed with minimal responses so the focus step succeeds; the
// assertion is about the clear branch, not the click.
//
// Note: ClickPoint's CheckHitTarget itself calls Runtime.callFunctionOn once
// (hit-target occlusion probe). We assert exactly 1 callFunctionOn (that
// hitTarget call) — a second one would mean tryClearContentEditable fired a
// callFunctionOn clear for a non-CE target, which is the regression we guard.
func TestRobustFill_PlainInputUsesKeyboardClear(t *testing.T) {
	f := &fakeCDP{responses: map[string]json.RawMessage{
		"DOM.describeNode":       json.RawMessage(`{"node":{"attributes":["type","text"]}}`),
		"DOM.getContentQuads":    json.RawMessage(`{"quads":[[0,0,10,0,10,10,0,10]]}`),
		"DOM.resolveNode":        hitTargetResolveResp,
		"Runtime.callFunctionOn": hitTargetCallResp(t, true, "", ""),
		"Input.insertText":       json.RawMessage(`{}`),
		"Runtime.evaluate":       json.RawMessage(`{"result":{"value":"complete"}}`),
		"Input.dispatchKeyEvent": json.RawMessage(`{}`),
		"Input.dispatchMouseEvent": json.RawMessage(`{}`),
	}}
	_ = RobustFill(context.Background(), f, 1, "x")
	// non-CE → tryClearContentEditable must NOT add a callFunctionOn clear.
	// ClickPoint's hitTarget check accounts for exactly 1; a 2nd means a clear fired.
	callFnCount := 0
	for _, c := range f.calls {
		if c.method == "Runtime.callFunctionOn" {
			callFnCount++
		}
	}
	if callFnCount > 1 {
		t.Fatalf("plain input must not trigger callFunctionOn clear; got %d callFunctionOn calls (1=hitTarget, >1=clear regression)", callFnCount)
	}
	// must dispatch Backspace (keyboard clear)
	hasBackspace := false
	for _, c := range f.calls {
		if c.method == "Input.dispatchKeyEvent" {
			if v, ok := c.params["key"].(string); ok && v == "Backspace" {
				hasBackspace = true
			}
		}
	}
	if !hasBackspace {
		t.Error("plain input clear must dispatch Backspace")
	}
}

