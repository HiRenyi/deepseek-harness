package axtree

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

// RefEntry maps a stable ref ("e3") to its CDP backendNodeId + identity used
// for slow-path re-resolution. Mirrors agent-browser element.rs RefEntry.
type RefEntry struct {
	BackendNodeID int64
	Role, Name    string
	Nth           int // -1 = unique
	FrameID       string
}

// RefMap is a process-level ref store (single-user, single-bridge). ref IDs are
// monotonically increasing and stable across snapshots: matching
// (role,name,nth,frame) reuses the prior ref ID so the LLM can keep referring
// to the same element. Concurrency-safe (mcp-go handlers may run concurrently).
type RefMap struct {
	mu      sync.Mutex
	entries map[string]*RefEntry // "e1" -> entry
	next    int                  // next ref id
}

// DefaultRefMap is the process singleton.
var DefaultRefMap = newRefMap()

func newRefMap() *RefMap {
	return &RefMap{entries: make(map[string]*RefEntry), next: 1}
}

// Assign reuses or allocates a ref for one tree node, returning its ref id.
// Reuses an existing entry with identical (role,name,nth,frame) identity so ref
// IDs stay stable across snapshots; otherwise allocates the next id.
func (m *RefMap) Assign(role, name string, nth int, frame string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reuse path: scan for an existing entry with identical identity. This is
	// O(n) but n is small (per-snapshot ref count). Keeps ref IDs stable across
	// snapshots without a second index.
	for id, e := range m.entries {
		if e.Role == role && e.Name == name && e.Nth == nth && e.FrameID == frame {
			return id
		}
	}
	id := fmt.Sprintf("e%d", m.next)
	m.next++
	m.entries[id] = &RefEntry{Role: role, Name: name, Nth: nth, FrameID: frame}
	// Log only NEW (non-reuse) INTERACTIVE refs — content roles and reuse are
	// too high-frequency to log per-snapshot. Interactive refs are the
	// click/fill targets the LLM refers to, so their assignment is the
	// diagnostic signal for "unknown ref" / stale-ref post-mortems.
	if INTERACTIVE_ROLES[role] {
		log.Printf("refmap: assign ref=%s role=%s name=%q nth=%d frame=%s", id, role, name, nth, frame)
	}
	return id
}

// SetBackend records the resolved backendNodeId for a ref (after assignRefs).
func (m *RefMap) SetBackend(refID string, backendNodeID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[refID]; ok {
		e.BackendNodeID = backendNodeID
	}
}

// Lookup returns the entry for a ref id (read-only copy). ok=false if unknown.
// Logs the miss (low-frequency, only on stale/unknown ref) — the single most
// useful signal for diagnosing "unknown ref e72" failures post-mortem.
func (m *RefMap) Lookup(refID string) (RefEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[refID]
	if !ok {
		log.Printf("refmap: lookup miss ref=%s", refID)
		return RefEntry{}, false
	}
	return *e, true
}

// UpdateBackend replaces a ref's backendNodeId (used by the slow path).
func (m *RefMap) UpdateBackend(refID string, backendNodeID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[refID]; ok {
		e.BackendNodeID = backendNodeID
	}
}

// Prune removes refs not in keep (called at end of each snapshot so stale
// elements don't linger).
func (m *RefMap) Prune(keep map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var removed []string
	for id := range m.entries {
		if !keep[id] {
			delete(m.entries, id)
			removed = append(removed, id)
		}
	}
	// Only log when something was actually removed; an empty prune (page
	// unchanged) would otherwise spam. Cap the rendered list at 20 refs so a
	// navigation that drops 100 refs stays readable — the count still tells
	// the user how many went away.
	if len(removed) > 0 {
		list := strings.Join(removed, " ")
		if len(removed) > 20 {
			list = strings.Join(removed[:20], " ") + fmt.Sprintf(" ... +%d more", len(removed)-20)
		}
		log.Printf("refmap: prune removed %d refs: [%s]", len(removed), list)
	}
}

// parseRef accepts "@e3", "ref=e3", "e3" and returns canonical "e3".
func parseRef(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimPrefix(s, "ref=")
	return s
}

// ResolveRef resolves a ref to a live backendNodeId. Fast path: the cached
// backendNodeId is probed via DOM.getBoxModel; if it resolves, return it.
// Slow path: on stale backendNodeId, re-query Accessibility.getFullAXTree and
// match by role+name+nth, update RefMap, and retry once. Mirrors agent-browser
// resolve_element_center (element.rs).
func (m *RefMap) ResolveRef(ctx context.Context, c CDPClient, ref string) (int64, error) {
	refID := parseRef(ref)
	entry, ok := m.Lookup(refID) // Lookup logs the miss on !ok
	if !ok {
		return 0, fmt.Errorf("unknown ref %q — this ref is not in the current snapshot. "+
			"Re-run browser_snapshot_ax (or browser_snapshot / browser_snapshot_visible for the DOM path) "+
			"to get a fresh ref, then retry the click/fill. Stale refs happen when the page re-rendered "+
			"or navigated since your last snapshot.", refID)
	}
	// Fast path: probe cached backendNodeId.
	if entry.BackendNodeID != 0 && boxModelAlive(ctx, c, entry.BackendNodeID) {
		return entry.BackendNodeID, nil
	}
	// Slow path: re-query AX tree by identity.
	fresh, err := findNodeIDByRoleName(ctx, c, entry.Role, entry.Name, entry.Nth, entry.FrameID)
	if err != nil {
		return 0, fmt.Errorf("ref %q (%s %q) is stale — the element closed, re-rendered, or scrolled away "+
			"(common after a menu/popup closes or a navigation). Re-run browser_snapshot_ax to get a fresh ref "+
			"and retry; do NOT loop on the stale ref.", refID, entry.Role, entry.Name)
	}
	m.UpdateBackend(refID, fresh)
	return fresh, nil
}

// boxModelAlive returns true if getBoxModel succeeds for the backendNodeId.
func boxModelAlive(ctx context.Context, c CDPClient, bid int64) bool {
	p := mustMarshalRaw(struct {
		BackendNodeID int64 `json:"backendNodeId"`
	}{bid})
	resp, err := c.Call(ctx, "DOM.getBoxModel", p)
	if err != nil || resp == nil || resp.Error != nil {
		return false
	}
	return true
}

// findNodeIDByRoleName re-queries the AX tree and returns the backendNodeId of
// the nth node matching role+name (within frame if specified). DL: when frame
// is a child frame, getFullAXTree must be queried WITH that frameId — the
// default (no-param) call returns only the main frame, so child-frame refs
// would never re-resolve. Mirrors agent-browser resolve_ax_session: same-origin
// child frame -> parent session + {frameId}.
func findNodeIDByRoleName(ctx context.Context, c CDPClient, role, name string, nth int, frame string) (int64, error) {
	_, _ = c.Call(ctx, "Accessibility.enable", mustMarshalRaw(struct{}{}))
	var params any
	if frame != "" {
		params = struct {
			FrameID string `json:"frameId"`
		}{frame}
	} else {
		params = struct{}{}
	}
	resp, err := c.Call(ctx, "Accessibility.getFullAXTree", mustMarshalRaw(params))
	if err != nil || resp == nil || resp.Error != nil {
		return 0, fmt.Errorf("re-query AX: %w", err)
	}
	var out struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return 0, err
	}
	target := nth
	if target < 0 {
		target = 0
	}
	seen := 0
	for _, n := range out.Nodes {
		if n.Ignored {
			continue
		}
		if frame != "" && n.FrameID != frame {
			continue
		}
		if extractAXString(n.Role) == role && extractAXString(n.Name) == name {
			if seen == target {
				return n.BackendDOMNodeID, nil
			}
			seen++
		}
	}
	return 0, fmt.Errorf("no AX match for %s %q", role, name)
}
