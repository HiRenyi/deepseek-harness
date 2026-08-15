package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// TestBrowserListTabsOutputMarksEnrolled verifies the list_tabs output appends
// [controlled] / [unmanaged—adopt first] per the enrolled field returned by the
// extension, and that the `*` active marker is independent of enrolled (the
// window.open'd blank-sheet tab is active but unmanaged — the exact trap from
// 测试11.md: AI saw `*` and assumed controlled, then select/switch errored with
// "尚未关联").
func TestBrowserListTabsOutputMarksEnrolled(t *testing.T) {
	s := newCannedRouter()
	// Tab 42: active + enrolled (controlled, foreground).
	// Tab 99: active=false + NOT enrolled (unmanaged — the window.open'd blank-sheet tab).
	s.byMethod["tabs.list"] = json.RawMessage(`[
		{"id":42,"index":0,"url":"https://docs.qq.com/","title":"docs","active":true,"status":"complete","enrolled":true},
		{"id":99,"index":1,"url":"https://docs.qq.com/sheet/X","title":"空白表格","active":false,"status":"complete","enrolled":false}
	]`)
	handler := BrowserListTabsHandler(s.router)

	res, err := handler(context.Background(), mcptypes.CallToolRequest{})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	txt := toolResultText(res)

	// controlled + active tab: `*` marker + [controlled]
	if !strings.Contains(txt, "* [0] id=42") {
		t.Errorf("active controlled tab 42 missing `* [0]` marker:\n%s", txt)
	}
	if !strings.Contains(txt, "[controlled]") {
		t.Errorf("enrolled tab missing [controlled] marker:\n%s", txt)
	}

	// unmanaged tab: no `*` + [unmanaged—adopt first]
	if !strings.Contains(txt, "  [1] id=99") {
		t.Errorf("unmanaged tab 99 missing `  [1]` marker (no active `*`):\n%s", txt)
	}
	if !strings.Contains(txt, "[unmanaged—adopt first]") {
		t.Errorf("unenrolled tab missing [unmanaged—adopt first] marker:\n%s", txt)
	}

	// active marker must be independent of enrolled: tab 99 is unmanaged, must NOT have `*`
	if strings.Contains(txt, "* [1]") {
		t.Errorf("unmanaged tab 99 must not have active `*` marker (enrolled is independent of active):\n%s", txt)
	}
}
