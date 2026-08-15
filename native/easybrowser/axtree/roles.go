package axtree

// INTERACTIVE_ROLES always get a ref (aligns snapshot.rs INTERACTIVE_ROLES).
var INTERACTIVE_ROLES = map[string]bool{
	"button": true, "link": true, "textbox": true, "checkbox": true,
	"radio": true, "combobox": true, "listbox": true, "menuitem": true,
	"menuitemcheckbox": true, "menuitemradio": true, "option": true,
	"searchbox": true, "slider": true, "spinbutton": true, "switch": true,
	"tab": true, "treeitem": true, "Iframe": true,
}

// CONTENT_ROLES get a ref only when they have a non-empty name.
var CONTENT_ROLES = map[string]bool{
	"heading": true, "cell": true, "gridcell": true, "columnheader": true,
	"rowheader": true, "listitem": true, "article": true, "region": true,
	"main": true, "navigation": true,
}

// roleNameTracker counts (role,name) occurrences to assign nth disambiguation.
// Mirrors agent-browser RoleNameTracker.
type roleNameTracker struct {
	counts map[string]int // "role\x00name" -> seen count
}

func newRoleNameTracker() *roleNameTracker {
	return &roleNameTracker{counts: map[string]int{}}
}

func key4(role, name string) string { return role + "\x00" + name }

// track records a hit and returns the provisional nth to assign: the member's
// 0-based position within its (role,name) key. finalize later discards it
// (-> -1) when the key ended up unique.
func (t *roleNameTracker) track(role, name string) int {
	k := key4(role, name)
	t.counts[k]++
	return t.counts[k] - 1 // 0-based position
}

// finalize returns the final nth for a node given its provisional position. It
// keeps the position only when the key is ambiguous (count>1); a unique key
// collapses to -1 (no nth).
func (t *roleNameTracker) finalize(role, name string, provisional int) int {
	if t.counts[key4(role, name)] > 1 {
		return provisional
	}
	return -1
}
