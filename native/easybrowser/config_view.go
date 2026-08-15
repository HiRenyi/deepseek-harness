package main

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/browser-mcp/bridge/profile"
)

// P4.3: config-view + config-rollback endpoints. The dashboard reads a client's
// raw config file (so the user can see what the browser-mcp entry looks like
// in context) and rolls back an injection by restoring the .browser-mcp.bak
// backup that injectCC/injectCodex/injectKairos/injectOpenCode wrote.
// (W2.3a view/rollback)

// configViewHandler: GET /api/config-view?client=<id>
//
// Returns the client's config file raw content plus the marker the GUI should
// highlight ("browser-mcp"). Response shapes:
//   - client known + file present → {ok:true, exists:true, path, content, marker}
//   - client known + file absent  → {ok:true, exists:false, path} (GUI shows "no config yet")
//   - client unknown              → 404 {ok:false, error:"unknown client"}
//
// `marker` is always returned when ok:true so the GUI can highlight the
// browser-mcp entry substring regardless of whether the file currently exists.
func configViewHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "GET required"})
		return
	}

	clientID := r.URL.Query().Get("client")
	byID := map[string]clientInfo{}
	for _, c := range detectClients() {
		byID[c.ID] = c
	}
	c, ok := byID[clientID]
	if !ok || c.ConfigPath == "" {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown client"})
		return
	}

	resp := map[string]any{
		"ok":     true,
		"path":   c.ConfigPath,
		"marker": mcpServerName, // "browser-mcp" — GUI highlights this substring
	}
	content, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		// File absent (or unreadable): report exists:false rather than 500 —
		// the GUI uses this to show "no config yet" instead of an error.
		resp["exists"] = false
		json.NewEncoder(w).Encode(resp)
		return
	}
	resp["exists"] = true
	resp["content"] = string(content)
	json.NewEncoder(w).Encode(resp)
}

// configRollbackHandler: POST /api/config-rollback {"client":"<id>"}
//
// Restores the client's config from its .browser-mcp.bak backup (the file
// inject* wrote before the first modification). The backup is copied verbatim
// over the live config path, reverting to the pre-injection state.
//   - no backup present → 404 {ok:false, error:"no backup"}
//   - copy fails        → 500 {ok:false, error:...}
//   - success           → {ok:true, path, backup}
//
// We do NOT delete the backup after restore: the user may want to roll back
// again, and leaving the .bak is the conservative choice (matches the inject*
// functions, which also leave the .bak in place across re-injections).
func configRollbackHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "POST required"})
		return
	}

	var req struct {
		Client string `json:"client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad JSON: " + err.Error()})
		return
	}

	byID := map[string]clientInfo{}
	for _, c := range detectClients() {
		byID[c.ID] = c
	}
	c, ok := byID[req.Client]
	if !ok || c.ConfigPath == "" {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown client"})
		return
	}

	bak := c.ConfigPath + profile.BackupSuffix
	data, err := os.ReadFile(bak)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no backup", "backup": bak})
		return
	}
	if err := os.WriteFile(c.ConfigPath, data, 0600); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "backup": bak})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"path":   c.ConfigPath,
		"backup": bak,
	})
}
