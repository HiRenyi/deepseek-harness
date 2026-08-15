package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInjectCC_NewConfig creates a fresh config when none exists.
func TestInjectCC_NewConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := injectCC(path); err != nil {
		t.Fatalf("injectCC: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(mustRead(t, path), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	srv, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %T", cfg["mcpServers"])
	}
	bm, ok := srv["browser-mcp"].(map[string]any)
	if !ok {
		t.Fatalf("browser-mcp missing: %+v", srv)
	}
	if bm["type"] != "streamable-http" || bm["url"] != mcpServerURL {
		t.Fatalf("browser-mcp entry wrong: %+v", bm)
	}
}

// TestInjectCC_PreservesOtherServers merges into an existing config without
// clobbering the user's other MCP servers.
func TestInjectCC_PreservesOtherServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	original := map[string]any{
		"mcpServers": map[string]any{
			"user-other": map[string]any{"type": "stdio", "command": "echo"},
		},
		"otherField": 42,
	}
	writeJSON(t, path, original)

	if err := injectCC(path); err != nil {
		t.Fatalf("injectCC: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(mustRead(t, path), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	srv := cfg["mcpServers"].(map[string]any)
	if _, ok := srv["user-other"]; !ok {
		t.Fatalf("user-other server was clobbered: %+v", srv)
	}
	if _, ok := srv["browser-mcp"]; !ok {
		t.Fatalf("browser-mcp not added")
	}
	if cfg["otherField"] != float64(42) {
		t.Fatalf("otherField changed: %v", cfg["otherField"])
	}

	// Backup must exist and match the original.
	bak := mustRead(t, path+".browser-mcp.bak")
	var bakCfg map[string]any
	if err := json.Unmarshal(bak, &bakCfg); err != nil {
		t.Fatalf("parse backup: %v", err)
	}
	if _, ok := bakCfg["mcpServers"].(map[string]any)["user-other"]; !ok {
		t.Fatalf("backup lost user-other")
	}
}

// TestInjectCC_Idempotent re-running updates the entry in place without
// duplicating or corrupting.
func TestInjectCC_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := injectCC(path); err != nil { t.Fatalf("first: %v", err) }
	if err := injectCC(path); err != nil { t.Fatalf("second: %v", err) }
	var cfg map[string]any
	if err := json.Unmarshal(mustRead(t, path), &cfg); err != nil { t.Fatalf("parse: %v", err) }
	srv := cfg["mcpServers"].(map[string]any)
	if len(srv) != 1 {
		t.Fatalf("expected 1 server after re-inject, got %d: %+v", len(srv), srv)
	}
	if _, ok := srv["browser-mcp"]; !ok { t.Fatalf("browser-mcp missing") }
}

// TestInjectCC_AddsPermissionsAllow merges permissions.allow with the
// browser-mcp allowlist entry, preserving other servers and other permissions
// keys. (W2.3a CC)
func TestInjectCC_AddsPermissionsAllow(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	_ = os.WriteFile(cfg, []byte(`{"mcpServers":{"other":{"url":"x"}}}`), 0600)
	if err := injectCC(cfg); err != nil { t.Fatal(err) }
	got, _ := os.ReadFile(cfg)
	var v map[string]any
	_ = json.Unmarshal(got, &v)
	perm, _ := v["permissions"].(map[string]any)
	allow, _ := perm["allow"].([]any)
	found := false
	for _, a := range allow {
		if s, ok := a.(string); ok && s == "mcp__browser-mcp__*" { found = true }
	}
	if !found { t.Error("permissions.allow missing mcp__browser-mcp__*") }
	if srv := v["mcpServers"].(map[string]any); srv["other"] == nil {
		t.Error("other server dropped")
	}
}

// TestInjectCC_PermissionsAllowIdempotent re-running must not duplicate the
// allowlist entry, and must preserve user's existing allow entries + other
// permissions keys (deny/ask). (W2.3a CC)
func TestInjectCC_PermissionsAllowIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	original := map[string]any{
		"mcpServers": map[string]any{"user": map[string]any{"url": "u"}},
		"permissions": map[string]any{
			"allow": []any{"Bash(ls)", "mcp__browser-mcp__*"},
			"deny":  []any{"Bash(rm:*)"},
			"ask":   []any{"WebFetch"},
		},
	}
	writeJSON(t, cfg, original)
	if err := injectCC(cfg); err != nil { t.Fatalf("first: %v", err) }
	if err := injectCC(cfg); err != nil { t.Fatalf("second: %v", err) }
	var v map[string]any
	if err := json.Unmarshal(mustRead(t, cfg), &v); err != nil { t.Fatalf("parse: %v", err) }
	perm := v["permissions"].(map[string]any)
	allow := perm["allow"].([]any)
	count := 0
	for _, a := range allow {
		if s, ok := a.(string); ok && s == "mcp__browser-mcp__*" { count++ }
	}
	if count != 1 { t.Errorf("expected 1 allowlist entry, got %d", count) }
	// user's existing entries preserved
	hasBash := false
	for _, a := range allow {
		if s, ok := a.(string); ok && s == "Bash(ls)" { hasBash = true }
	}
	if !hasBash { t.Error("user's Bash(ls) allow entry dropped") }
	// other permissions keys preserved
	if _, ok := perm["deny"]; !ok { t.Error("permissions.deny dropped") }
	if _, ok := perm["ask"]; !ok { t.Error("permissions.ask dropped") }
}

// TestInjectCC_RefusesBadJSON does not write when the existing config is
// unparseable (refuses to risk the user's config).
func TestInjectCC_RefusesBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := injectCC(path)
	if err == nil { t.Fatal("expected error on bad JSON") }
	// File must be untouched (no backup created, content unchanged).
	got := string(mustRead(t, path))
	if got != "{not valid json" {
		t.Fatalf("config was modified despite bad JSON: %q", got)
	}
}

// TestDetectClients_ReadOnly does not touch any config; just returns a list.
func TestDetectClients_ReadOnly(t *testing.T) {
	cs := detectClients()
	if len(cs) == 0 { t.Fatal("detectClients returned nothing") }
	ids := map[string]bool{}
	for _, c := range cs { ids[c.ID] = true }
	// Kairos was removed from client injection (Kairos users use the GUI host,
	// not a .mcp.json injection target); detectClients no longer lists it.
	for _, want := range []string{"claude-code", "codex", "opencode"} {
		if !ids[want] { t.Errorf("missing client %q", want) }
	}
}

// TestKairosConfigPath_CorrectDir verifies kairosConfigPath resolves under
// AppData/Roaming/KairosAIStudio (Win) / Library/Application Support/KairosAIStudio
// (Mac) / .config/KairosAIStudio (Linux) — NOT the old wrong Local/kairos path.
// (W2.3a Kairos path fix)
func TestKairosConfigPath_CorrectDir(t *testing.T) {
	p, err := kairosConfigPath()
	if err != nil {
		t.Fatalf("kairosConfigPath: %v", err)
	}
	switch runtime.GOOS {
	case "windows":
		if !strings.Contains(p, filepath.Join("AppData", "Roaming", "KairosAIStudio")) {
			t.Errorf("Windows: expected ...AppData/Roaming/KairosAIStudio..., got %s", p)
		}
		if strings.Contains(strings.ToLower(p), strings.ToLower(filepath.Join("appdata", "local", "kairos"))) {
			t.Errorf("Windows: must NOT be AppData/Local/kairos (old wrong path), got %s", p)
		}
	case "darwin":
		if !strings.Contains(p, filepath.Join("Library", "Application Support", "KairosAIStudio")) {
			t.Errorf("Mac: expected Library/Application Support/KairosAIStudio, got %s", p)
		}
	default:
		if !strings.Contains(p, filepath.Join(".config", "KairosAIStudio")) {
			t.Errorf("Linux: expected .config/KairosAIStudio, got %s", p)
		}
	}
	if !strings.HasSuffix(p, "config.json") {
		t.Errorf("expected config.json suffix, got %s", p)
	}
}

// TestInjectKairos_MergesMcpServers merges browser-mcp into an existing Kairos
// config's mcpServers object (CherryStudio-like schema assumption — best-effort
// since the real schema is undocumented). (W2.3a Kairos)
func TestInjectKairos_MergesMcpServers(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	original := map[string]any{
		"mcpServers": map[string]any{
			"user-other": map[string]any{"type": "stdio", "command": "echo"},
		},
		"somePref": true,
	}
	writeJSON(t, cfg, original)
	if err := injectKairos(cfg); err != nil {
		t.Fatalf("injectKairos: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(mustRead(t, cfg), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	srv, ok := v["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing/wrong type: %T", v["mcpServers"])
	}
	bm, ok := srv["browser-mcp"].(map[string]any)
	if !ok {
		t.Fatalf("browser-mcp not added: %+v", srv)
	}
	if bm["type"] != "streamable-http" || bm["url"] != mcpServerURL {
		t.Fatalf("browser-mcp entry wrong: %+v", bm)
	}
	if _, ok := srv["user-other"]; !ok {
		t.Error("user-other server dropped")
	}
	if v["somePref"] != true {
		t.Error("somePref changed")
	}
	// backup must exist
	bak := mustRead(t, cfg+".browser-mcp.bak")
	var bakCfg map[string]any
	if err := json.Unmarshal(bak, &bakCfg); err != nil {
		t.Fatalf("parse backup: %v", err)
	}
	if _, ok := bakCfg["mcpServers"].(map[string]any)["user-other"]; !ok {
		t.Error("backup lost user-other")
	}
}

// TestInjectKairos_Idempotent re-running must not duplicate the browser-mcp entry.
func TestInjectKairos_Idempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeJSON(t, cfg, map[string]any{"mcpServers": map[string]any{}})
	if err := injectKairos(cfg); err != nil { t.Fatalf("first: %v", err) }
	if err := injectKairos(cfg); err != nil { t.Fatalf("second: %v", err) }
	var v map[string]any
	if err := json.Unmarshal(mustRead(t, cfg), &v); err != nil { t.Fatalf("parse: %v", err) }
	srv := v["mcpServers"].(map[string]any)
	if len(srv) != 1 {
		t.Fatalf("expected 1 server after re-inject, got %d: %+v", len(srv), srv)
	}
	if _, ok := srv["browser-mcp"]; !ok {
		t.Fatal("browser-mcp missing")
	}
}

// TestInjectKairos_RefusesBadJSON does not write when the existing config is
// unparseable (schema uncertain — never clobber). (W2.3a Kairos)
func TestInjectKairos_RefusesBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte("{not valid json"), 0600)
	err := injectKairos(cfg)
	if err == nil { t.Fatal("expected error on bad JSON") }
	got := string(mustRead(t, cfg))
	if got != "{not valid json" {
		t.Fatalf("config modified despite bad JSON: %q", got)
	}
}

// TestInjectKairos_MissingFileReturnsNote: when the config file doesn't exist,
// injectKairos returns an error guiding the user to add the server in-app
// (rather than guessing at an unknown schema from scratch). (W2.3a Kairos)
func TestInjectKairos_MissingFileReturnsNote(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json") // does not exist
	err := injectKairos(cfg)
	if err == nil { t.Fatal("expected error when config missing") }
	if !strings.Contains(err.Error(), mcpServerURL) {
		t.Errorf("error should guide user to the MCP URL, got: %v", err)
	}
}

// TestInjectKairos_AbsentMcpServersAborts: the real Kairos config.json holds
// only app settings (clientId/theme/gitBash) with NO mcpServers key — its MCP
// config lives in electron IndexedDB. injectKairos must NOT guess-create the
// mcpServers key (would pollute a file Kairos never reads); it aborts with a
// guiding note + leaves the file untouched + writes no backup. (W2.3a Kairos fix.)
func TestInjectKairos_AbsentMcpServersAborts(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeJSON(t, cfg, map[string]any{"clientId": "abc", "theme": "system", "gitBashPath": "/usr/bin/bash"})
	before := mustRead(t, cfg)
	err := injectKairos(cfg)
	if err == nil {
		t.Fatal("expected abort when mcpServers absent")
	}
	if !strings.Contains(err.Error(), mcpServerURL) {
		t.Errorf("error should guide to the MCP URL, got: %v", err)
	}
	after := mustRead(t, cfg)
	if string(before) != string(after) {
		t.Errorf("config modified despite abort:\nbefore: %s\nafter: %s", before, after)
	}
	if _, err := os.Stat(cfg + ".browser-mcp.bak"); err == nil {
		t.Error("backup created despite abort (should only back up right before a real write)")
	}
}

// TestInjectOpenCode_MergesMCP merges browser-mcp into an existing OpenCode
// config's mcp map, preserving other servers. (W2.3a OpenCode)
func TestInjectOpenCode_MergesMCP(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte(`{"mcp":{"other":{"type":"stdio","command":"x"}},"theme":"dark"}`), 0600)
	if err := injectOpenCode(cfg); err != nil {
		t.Fatalf("injectOpenCode: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(mustRead(t, cfg), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	mcp, ok := v["mcp"].(map[string]any)
	if !ok { t.Fatalf("mcp missing/wrong type: %T", v["mcp"]) }
	bm, ok := mcp["browser-mcp"].(map[string]any)
	if !ok { t.Fatalf("browser-mcp not added: %+v", mcp) }
	if bm["type"] != "streamable-http" || bm["url"] != mcpServerURL {
		t.Fatalf("browser-mcp entry wrong: %+v", bm)
	}
	if _, ok := mcp["other"]; !ok { t.Error("other server dropped") }
	if v["theme"] != "dark" { t.Error("theme changed") }
	// backup
	if _, err := os.Stat(cfg + ".browser-mcp.bak"); err != nil {
		t.Error("backup not created")
	}
}

// TestInjectOpenCode_Idempotent re-running must not duplicate the entry.
func TestInjectOpenCode_Idempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte(`{"mcp":{}}`), 0600)
	if err := injectOpenCode(cfg); err != nil { t.Fatalf("first: %v", err) }
	if err := injectOpenCode(cfg); err != nil { t.Fatalf("second: %v", err) }
	var v map[string]any
	if err := json.Unmarshal(mustRead(t, cfg), &v); err != nil { t.Fatalf("parse: %v", err) }
	mcp := v["mcp"].(map[string]any)
	if len(mcp) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(mcp), mcp)
	}
}

// TestInjectOpenCode_RefusesBadJSON does not write when config is unparseable.
func TestInjectOpenCode_RefusesBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte("not json"), 0600)
	if err := injectOpenCode(cfg); err == nil {
		t.Fatal("expected error on bad JSON")
	}
	got := string(mustRead(t, cfg))
	if got != "not json" {
		t.Fatalf("config modified despite bad JSON: %q", got)
	}
}

// TestInjectOpenCode_NewConfigCreatesMCP: when the file is absent, OpenCode
// (unlike Kairos) creates a fresh config — OpenCode's schema is documented and
// stable ({mcp: {name: {type,url}}}), so creating from scratch is safe.
func TestInjectOpenCode_NewConfigCreatesMCP(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := injectOpenCode(cfg); err != nil {
		t.Fatalf("injectOpenCode on new config: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(mustRead(t, cfg), &v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	bm, ok := v["mcp"].(map[string]any)["browser-mcp"].(map[string]any)
	if !ok { t.Fatal("browser-mcp not created") }
	if bm["url"] != mcpServerURL { t.Errorf("url wrong: %v", bm["url"]) }
}

// TestInjectCodex_WritesTomlServer appends a [mcp_servers.browser-mcp] block
// into an existing Codex config.toml, preserving the user's existing servers.
// Append-only (no TOML parse dep). (W2.3a Codex)
func TestInjectCodex_WritesTomlServer(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	_ = os.WriteFile(cfg, []byte("[mcp_servers.existing]\nurl = \"other\"\n"), 0600)
	if err := injectCodex(cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	if !strings.Contains(string(got), "[mcp_servers.browser-mcp]") {
		t.Error("browser-mcp server block missing")
	}
	if !strings.Contains(string(got), "other") {
		t.Error("existing server dropped")
	}
	// url is the correct per-server field for a Codex remote HTTP MCP server.
	if !strings.Contains(string(got), `url = "http://127.0.0.1:58080/mcp"`) {
		t.Error("per-server url field missing")
	}
	// approval_policy is a Codex TOP-LEVEL key, not per-server — must NOT appear
	// inside [mcp_servers.browser-mcp]. (W2.3a Codex fix.)
	if strings.Contains(string(got), "approval_policy") {
		t.Error("approval_policy leaked into per-server block (should be top-level only)")
	}
}

// TestInjectCodex_Idempotent re-running must not append a duplicate block.
func TestInjectCodex_Idempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	_ = os.WriteFile(cfg, []byte("[mcp_servers.existing]\nurl = \"other\"\n"), 0600)
	if err := injectCodex(cfg); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := injectCodex(cfg); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if strings.Count(string(got), "[mcp_servers.browser-mcp]") != 1 {
		t.Errorf("expected exactly 1 browser-mcp block, got %d", strings.Count(string(got), "[mcp_servers.browser-mcp]"))
	}
}

// TestInjectCodex_NewConfig appends to a non-existent file (creates it).
func TestInjectCodex_NewConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := injectCodex(cfg); err != nil {
		t.Fatalf("injectCodex: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if !strings.Contains(string(got), "[mcp_servers.browser-mcp]") {
		t.Error("browser-mcp server block missing for new config")
	}
}

// TestInjectCodex_BackupCreated creates a .browser-mcp.bak before writing,
// only when a non-empty file pre-existed.
func TestInjectCodex_BackupCreated(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	orig := []byte("[mcp_servers.existing]\nurl = \"other\"\n")
	_ = os.WriteFile(cfg, orig, 0600)
	if err := injectCodex(cfg); err != nil {
		t.Fatalf("injectCodex: %v", err)
	}
	bak, err := os.ReadFile(cfg + ".browser-mcp.bak")
	if err != nil {
		t.Fatalf("backup not created: %v", err)
	}
	if string(bak) != string(orig) {
		t.Errorf("backup content mismatch: got %q want %q", bak, orig)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil { t.Fatalf("read %s: %v", p, err) }
	return b
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil { t.Fatalf("marshal: %v", err) }
	if err := os.WriteFile(p, b, 0600); err != nil { t.Fatalf("write: %v", err) }
}
