package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/browser-mcp/bridge/profile"
)

// P4: multi-client MCP config injection. The dashboard /api/configure writes
// the browser-mcp MCP server entry (streamable-http @ 127.0.0.1:58080/mcp) into
// each detected client's config, backing up first and merging (not overwriting
// the user's other servers). CC (JSON merge), Codex (append-only TOML), Kairos
// (best-effort JSON merge — schema uncertain), and OpenCode (JSON merge into
// the `mcp` map) are all implemented. (W2.3a)

// mcpServerName/mcpServerURL are profile-driven (test vs prod coexistence):
// the server entry key + port differ per profile so two coexisting installs
// hold separate slots in a client's config. Vars (not consts) because they
// reference the ldflag-baked profile package vars.
var mcpServerName = profile.McpServerName
var mcpServerURL = "http://127.0.0.1:" + profile.Port + "/mcp"

// clientInfo describes one detectable MCP client on this machine.
type clientInfo struct {
	ID          string `json:"id"` // "claude-code" | "codex" | "kairos" | "opencode"
	Label       string `json:"label"`
	Installed   bool   `json:"installed"`    // CLI in PATH OR config file exists
	ConfigPath  string `json:"config_path"`  // resolved config file (may not exist yet)
	Injectable  bool   `json:"injectable"`   // can this client be auto-injected now?
	Note        string `json:"note,omitempty"`
}

// homeConfig resolves a config path under the user's home dir (cross-platform).
func homeConfig(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// inPath reports whether a CLI is on PATH (cross-platform).
func inPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// detectClients scans the machine for known MCP clients (P4.1).
func detectClients() []clientInfo {
	var out []clientInfo

	// Claude Code: `claude` CLI in PATH OR ~/.claude.json exists.
	ccPath, _ := homeConfig(".claude.json")
	ccInstalled := inPath("claude") || fileExists(ccPath)
	out = append(out, clientInfo{
		ID:         "claude-code",
		Label:      "Claude Code",
		Installed:  ccInstalled,
		ConfigPath: ccPath,
		Injectable: true,
	})

	// Codex: `codex` CLI in PATH OR ~/.codex/config.toml exists.
	codexPath, _ := homeConfig(".codex", "config.toml")
	codexInstalled := inPath("codex") || fileExists(codexPath)
	out = append(out, clientInfo{
		ID:         "codex",
		Label:      "Codex",
		Installed:  codexInstalled,
		ConfigPath: codexPath,
		Injectable: true, // append-only TOML injection (W2.3a)
	})

	// Kairos AI Studio 已从注入列表移除（Electron 应用，配置在应用内 IndexedDB，
	// 无法外部注入；GUI 不再展示）。如需恢复见 git 历史。

	// OpenCode: `opencode` CLI in PATH OR ~/.config/opencode/config.json exists
	// OR opencode.json in cwd. Injectable when installed — OpenCode's schema is
	// documented and stable ({mcp:{name:{type,url}}}), so creating the config
	// from scratch is safe. (W2.3a OpenCode)
	ocPath, _ := opencodeConfigPath()
	ocInstalled := inPath("opencode") || fileExists(ocPath) || fileExists("opencode.json")
	ocNote := ""
	if !ocInstalled {
		ocNote = "安装 OpenCode 后可自动注入，或手动在 ~/.config/opencode/config.json 添加 mcp.browser-mcp"
	}
	out = append(out, clientInfo{
		ID:         "opencode",
		Label:      "OpenCode",
		Installed:  ocInstalled,
		ConfigPath: ocPath,
		Injectable: ocInstalled,
		Note:       ocNote,
	})

	return out
}

// kairosConfigDir returns the Kairos AI Studio config directory (cross-platform).
// Win: %APPDATA%\Roaming\KairosAIStudio; Mac: ~/Library/Application Support/KairosAIStudio;
// Linux: ~/.config/KairosAIStudio. (design §F2.3 — corrected from the earlier
// AppData/Local/kairos guess which was wrong.) (W2.3a Kairos path fix)
func kairosConfigDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		return homeConfig("AppData", "Roaming", "KairosAIStudio")
	case "darwin":
		return homeConfig("Library", "Application Support", "KairosAIStudio")
	default:
		return homeConfig(".config", "KairosAIStudio")
	}
}

// kairosConfigPath returns the Kairos config file path. SCHEMA UNCERTAIN
// (design §7 risk): the exact filename/structure is not publicly documented;
// v1 assumes config.json under the KairosAIStudio dir (CherryStudio reference).
// Best-effort only — injectKairos never clobbers and aborts on parse failure.
func kairosConfigPath() (string, error) {
	d, err := kairosConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// kairosDetected is a best-effort check for the Kairos desktop app: we look for
// the KairosAIStudio config dir (created on first app run). Absence is not proof
// of absence. (W2.3a — path corrected from Local/kairos to Roaming/KairosAIStudio.)
func kairosDetected() bool {
	d, err := kairosConfigDir()
	if err != nil {
		return false
	}
	return fileExists(d)
}

// opencodeConfigPath returns the OpenCode config file path
// (~/.config/opencode/config.json). OpenCode also reads opencode.json in cwd,
// but injection targets the stable home config. (W2.3a OpenCode)
func opencodeConfigPath() (string, error) {
	return homeConfig(".config", "opencode", "config.json")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// injectCC merges the browser-mcp server into ~/.claude.json's mcpServers,
// backing up the original first. Idempotent: re-running updates the entry in
// place and preserves all other servers. (P4.2)
func injectCC(configPath string) error {
	existing, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	var cfg map[string]any
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return fmt.Errorf("parse %s (backup saved? no — refusing to write): %w", configPath, err)
		}
	} else {
		cfg = map[string]any{}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// Backup before any write (only if a real file existed).
	if len(existing) > 0 {
		bak := configPath + profile.BackupSuffix
		if err := os.WriteFile(bak, existing, 0600); err != nil {
			return fmt.Errorf("backup %s: %w", bak, err)
		}
	}

	// Merge mcpServers.browser-mcp = {type:streamable-http, url:...}.
	serversAny, ok := cfg["mcpServers"]
	if !ok || serversAny == nil {
		cfg["mcpServers"] = map[string]any{}
		serversAny = cfg["mcpServers"]
	}
	servers, ok := serversAny.(map[string]any)
	if !ok {
		return fmt.Errorf("mcpServers is not an object in %s; aborting (manual check needed)", configPath)
	}
	servers[mcpServerName] = map[string]any{
		"type": "streamable-http",
		"url":  mcpServerURL,
	}
	cfg["mcpServers"] = servers

	// Merge permissions.allow with the browser-mcp allowlist entry
	// (mcp__browser-mcp__*). Idempotent: don't duplicate if already present.
	// Preserves all other permissions keys (deny/ask/etc.) and the user's
	// existing allow entries. (W2.3a CC)
	permAny, ok := cfg["permissions"]
	if !ok || permAny == nil {
		cfg["permissions"] = map[string]any{}
		permAny = cfg["permissions"]
	}
	perm, ok := permAny.(map[string]any)
	if !ok {
		return fmt.Errorf("permissions is not an object in %s; aborting (manual check needed)", configPath)
	}
	allowAny, _ := perm["allow"].([]any)
	allow := append([]any{}, allowAny...)
	has := false
	for _, a := range allow {
		if s, ok := a.(string); ok && s == "mcp__browser-mcp__*" {
			has = true
		}
	}
	if !has {
		allow = append(allow, "mcp__browser-mcp__*")
	}
	perm["allow"] = allow
	cfg["permissions"] = perm

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// injectCodex appends a [mcp_servers.browser-mcp] block into the Codex
// config.toml. Append-only (no TOML parse dep — BurntSushi/toml can't
// round-trip preserve comments/spacing reliably; append is simpler + safer):
// the user's existing file is preserved verbatim and the block is appended.
// Idempotent: if [mcp_servers.browser-mcp] is already present, return nil.
// Backs up the original to <path>.browser-mcp.bak before the first write
// (only if a non-empty file pre-existed). (W2.3a Codex)
func injectCodex(configPath string) error {
	existing, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	// Idempotent: already injected — don't append a duplicate block.
	if strings.Contains(string(existing), "[mcp_servers.browser-mcp]") {
		return nil
	}

	// Backup before write (only if a real, non-empty file existed).
	if len(existing) > 0 {
		bak := configPath + profile.BackupSuffix
		if err := os.WriteFile(bak, existing, 0600); err != nil {
			return fmt.Errorf("backup %s: %w", bak, err)
		}
	}

	// Codex MCP servers are stdio (command+args) by default, but Codex CLI v2
	// also supports remote HTTP/SSE servers via a per-server `url` field.
	// approval_policy is a Codex TOP-LEVEL key, NOT a per-server field — putting
	// it inside [mcp_servers.browser-mcp] would be a bogus ignored key, so it is
	// intentionally omitted. (W2.3a Codex fix — was wrongly injecting approval_policy.)
	block := fmt.Sprintf("\n[mcp_servers.browser-mcp]\nurl = %q\n", mcpServerURL)
	out := append([]byte{}, existing...)
	out = append(out, block...)

	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// injectKairos performs a BEST-EFFORT JSON merge of browser-mcp into the Kairos
// config. SCHEMA UNCERTAIN (design §7 risk): Kairos's config schema is not
// publicly documented; this assumes a CherryStudio-like `mcpServers` object at
// the root ({mcpServers: {name: {type, url, ...}}}). Behavior:
//   - config file missing → return an error guiding the user to add the server
//     in-app (we never guess-create a config for an unknown schema);
//   - JSON parse fails → abort (never clobber the user's config);
//   - mcpServers present but not an object → abort cleanly (schema differs from
//     the assumption — don't risk corrupting);
//   - otherwise merge browser-mcp into mcpServers (idempotent overwrite of the
//     browser-mcp entry, preserving all other servers + root fields).
//
// Backs up the original to <path>.browser-mcp.bak before the first write.
// (W2.3a Kairos)
func injectKairos(configPath string) error {
	existing, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("Kairos 配置不存在：%s（请在 Kairos 应用内添加 MCP server：URL %s，streamable-http）", configPath, mcpServerURL)
		}
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	var cfg map[string]any
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return fmt.Errorf("parse %s (refusing to write — schema uncertain): %w", configPath, err)
		}
	} else {
		cfg = map[string]any{}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// Verify the schema matches the CherryStudio-like assumption BEFORE touching
	// anything: mcpServers must already exist as an object. Kairos's real
	// config.json (app settings: clientId/theme/gitBash) has NO mcpServers — its
	// MCP config lives in electron IndexedDB, not this file. Guess-creating
	// mcpServers here would pollute a file Kairos never reads. (W2.3a Kairos fix.)
	serversAny, ok := cfg["mcpServers"]
	if !ok || serversAny == nil {
		return fmt.Errorf("config %s 无 mcpServers 键（Kairos MCP 配置在 electron IndexedDB，非 config.json）——请在 Kairos 应用内添加 MCP server：URL %s（streamable-http）", configPath, mcpServerURL)
	}
	servers, ok := serversAny.(map[string]any)
	if !ok {
		// Schema differs from the CherryStudio-like assumption — abort cleanly.
		return fmt.Errorf("mcpServers is not an object in %s; schema differs from best-effort assumption — aborting (请在 Kairos 应用内手动添加 MCP server：URL %s)", configPath, mcpServerURL)
	}

	// Backup before write (only if a real, non-empty file existed).
	if len(existing) > 0 {
		bak := configPath + profile.BackupSuffix
		if err := os.WriteFile(bak, existing, 0600); err != nil {
			return fmt.Errorf("backup %s: %w", bak, err)
		}
	}

	servers[mcpServerName] = map[string]any{
		"type": "streamable-http",
		"url":  mcpServerURL,
	}
	cfg["mcpServers"] = servers

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// injectOpenCode merges browser-mcp into the OpenCode config's `mcp` map.
// OpenCode's schema is documented and stable ({mcp: {name: {type, url}}, ...}),
// so creating the config from scratch is safe (unlike Kairos). Idempotent:
// re-running overwrites the browser-mcp entry in place, preserving all other
// servers and root fields. Refuses to write on JSON parse failure (never
// clobber). Backs up the original to <path>.browser-mcp.bak before the first
// write to an existing file. (W2.3a OpenCode)
func injectOpenCode(configPath string) error {
	existing, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	var cfg map[string]any
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return fmt.Errorf("parse %s (refusing to write): %w", configPath, err)
		}
	} else {
		cfg = map[string]any{}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// Backup before write (only if a real, non-empty file existed).
	if len(existing) > 0 {
		bak := configPath + profile.BackupSuffix
		if err := os.WriteFile(bak, existing, 0600); err != nil {
			return fmt.Errorf("backup %s: %w", bak, err)
		}
	}

	// Merge mcp.browser-mcp = {type:streamable-http, url:...}.
	mcpAny, ok := cfg["mcp"]
	if !ok || mcpAny == nil {
		cfg["mcp"] = map[string]any{}
		mcpAny = cfg["mcp"]
	}
	mcp, ok := mcpAny.(map[string]any)
	if !ok {
		return fmt.Errorf("mcp is not an object in %s; aborting (manual check needed)", configPath)
	}
	mcp[mcpServerName] = map[string]any{
		"type": "streamable-http",
		"url":  mcpServerURL,
	}
	cfg["mcp"] = mcp

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// --- HTTP handlers -----------------------------------------------------------

func detectClientsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	clients := detectClients()
	json.NewEncoder(w).Encode(map[string]any{"clients": clients, "mcp_url": mcpServerURL})
}

type configureReq struct {
	Clients []string `json:"clients"`
}

func configureHandler(w http.ResponseWriter, r *http.Request) {
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
	var req configureReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad JSON: " + err.Error()})
		return
	}

	type result struct {
		Client string `json:"client"`
		OK     bool   `json:"ok"`
		Error  string `json:"error,omitempty"`
		Note   string `json:"note,omitempty"`
	}
	var results []result
	byID := map[string]clientInfo{}
	for _, c := range detectClients() {
		byID[c.ID] = c
	}
	for _, id := range req.Clients {
		c, ok := byID[id]
		if !ok {
			results = append(results, result{Client: id, OK: false, Error: "unknown client"})
			continue
		}
		if !c.Installed {
			results = append(results, result{Client: id, OK: false, Error: "client not installed"})
			continue
		}
		switch id {
		case "claude-code":
			if err := injectCC(c.ConfigPath); err != nil {
				results = append(results, result{Client: id, OK: false, Error: err.Error()})
			} else {
				results = append(results, result{Client: id, OK: true, Note: "已写入 " + c.ConfigPath + "（原文件备份为 " + profile.BackupSuffix + "）"})
			}
		case "codex":
			if err := injectCodex(c.ConfigPath); err != nil {
				results = append(results, result{Client: id, OK: false, Error: err.Error()})
			} else {
				results = append(results, result{Client: id, OK: true, Note: "已写入 " + c.ConfigPath + "（原文件备份为 " + profile.BackupSuffix + "）"})
			}
		case "kairos":
			// Kairos MCP config is in electron IndexedDB, not config.json — file
			// injection is infeasible. Guide the user to add the server in-app.
			// (Injectable is false in detectClients, so the GUI won't pre-check this
			// row, but if the user manually selects it, return the note rather than
			// polluting config.json. W2.3a Kairos fix.)
			results = append(results, result{Client: id, OK: false, Note: c.Note})
		case "opencode":
			if err := injectOpenCode(c.ConfigPath); err != nil {
				results = append(results, result{Client: id, OK: false, Error: err.Error()})
			} else {
				results = append(results, result{Client: id, OK: true, Note: "已写入 " + c.ConfigPath + "（原文件备份为 " + profile.BackupSuffix + "）"})
			}
		default:
			results = append(results, result{Client: id, OK: false, Note: c.Note})
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"results": results})
}
