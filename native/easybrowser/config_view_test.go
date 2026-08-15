package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setTempHome redirects os.UserHomeDir() (via USERPROFILE on Windows, HOME on
// *nix) to a temp dir so detectClients() resolves config paths under it.
// Returns the temp dir and a cleanup func.
func setTempHome(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	var key string
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	} else {
		key = "HOME"
	}
	old, had := os.LookupEnv(key)
	os.Setenv(key, dir)
	return dir, func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	}
}

// claudeConfigPath returns the ~/.claude.json path under the given home dir.
func claudeConfigPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".claude.json")
}

func doJSON(t *testing.T, method, url string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	configViewHandler(rec, req)
	return rec
}

func doRollback(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/api/config-rollback", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	configRollbackHandler(rec, req)
	return rec
}

func TestConfigView_UnknownClient404(t *testing.T) {
	home, cleanup := setTempHome(t)
	defer cleanup()
	_ = home
	rec := doJSON(t, http.MethodGet, "/api/config-view?client=nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown client, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != false {
		t.Fatalf("expected ok=false, got %v", resp["ok"])
	}
}

func TestConfigView_FileAbsentExistsFalse(t *testing.T) {
	home, cleanup := setTempHome(t)
	defer cleanup()
	// Claude Code config path is ~/.claude.json; don't create it.
	rec := doJSON(t, http.MethodGet, "/api/config-view?client=claude-code", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("expected ok=true, got %v", resp["ok"])
	}
	if resp["exists"] != false {
		t.Fatalf("expected exists=false, got %v", resp["exists"])
	}
	if resp["path"] != claudeConfigPath(t, home) {
		t.Fatalf("expected path=%s, got %v", claudeConfigPath(t, home), resp["path"])
	}
	if resp["marker"] != "browser-mcp" {
		t.Fatalf("expected marker=browser-mcp, got %v", resp["marker"])
	}
}

func TestConfigView_FilePresentReturnsContent(t *testing.T) {
	home, cleanup := setTempHome(t)
	defer cleanup()
	path := claudeConfigPath(t, home)
	content := `{"mcpServers":{"browser-mcp":{"type":"streamable-http","url":"http://127.0.0.1:58080/mcp"}}}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, http.MethodGet, "/api/config-view?client=claude-code", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("expected ok=true, got %v", resp["ok"])
	}
	if resp["exists"] != true {
		t.Fatalf("expected exists=true, got %v", resp["exists"])
	}
	if resp["content"] != content {
		t.Fatalf("content mismatch:\nwant %s\ngot  %v", content, resp["content"])
	}
	if resp["marker"] != "browser-mcp" {
		t.Fatalf("expected marker=browser-mcp, got %v", resp["marker"])
	}
}

func TestConfigView_MethodGuard(t *testing.T) {
	_, cleanup := setTempHome(t)
	defer cleanup()
	rec := doJSON(t, http.MethodPost, "/api/config-view?client=claude-code", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", rec.Code)
	}
}

func TestConfigRollback_NoBackup404(t *testing.T) {
	_, cleanup := setTempHome(t)
	defer cleanup()
	rec := doRollback(t, `{"client":"claude-code"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for no backup, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != false {
		t.Fatalf("expected ok=false, got %v", resp["ok"])
	}
	if resp["error"] != "no backup" {
		t.Fatalf("expected error=no backup, got %v", resp["error"])
	}
}

func TestConfigRollback_RestoresBak(t *testing.T) {
	home, cleanup := setTempHome(t)
	defer cleanup()
	path := claudeConfigPath(t, home)
	bak := path + ".browser-mcp.bak"

	original := `{"mcpServers":{"other":{"url":"x"}}}`
	injected := `{"mcpServers":{"browser-mcp":{"type":"streamable-http","url":"http://127.0.0.1:58080/mcp"},"other":{"url":"x"}}}`
	if err := os.WriteFile(bak, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(injected), 0600); err != nil {
		t.Fatal(err)
	}

	rec := doRollback(t, `{"client":"claude-code"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("expected ok=true, got %v", resp["ok"])
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("config not restored:\nwant %s\ngot  %s", original, string(got))
	}
	// backup file should remain (we do not delete it)
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("backup should remain after rollback: %v", err)
	}
}

func TestConfigRollback_UnknownClient404(t *testing.T) {
	_, cleanup := setTempHome(t)
	defer cleanup()
	rec := doRollback(t, `{"client":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestConfigRollback_BadJSON400(t *testing.T) {
	_, cleanup := setTempHome(t)
	defer cleanup()
	rec := doRollback(t, `not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
