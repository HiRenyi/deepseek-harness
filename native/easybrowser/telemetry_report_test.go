package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/browser-mcp/bridge/profile"
)

// pingProbe captures the install POST body, URL path, and a call counter.
type pingProbe struct {
	mu    sync.Mutex
	path  string
	body  map[string]any
	calls int
}

func newPingProbe(t *testing.T) (*httptest.Server, *pingProbe) {
	t.Helper()
	p := &pingProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.path = r.URL.Path
		p.calls++
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p.body)
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

// configureTelemetryEnv wires the update source at srv + isolates data dir.
// As of hardcode-update-base-url, env BROWSER_MCP_UPDATE_BASE is gone — the
// base is set via profile.UpdateBase directly (test-only, same mechanism as
// the ldflag bake). update_source stays env-configurable (backend selector,
// not a URL override).
func configureTelemetryEnv(t *testing.T, base, channel string) {
	t.Helper()
	t.Setenv("BROWSER_MCP_DATA_DIR", t.TempDir())
	t.Setenv("BROWSER_MCP_UPDATE_SOURCE", "http")
	oldBase := profile.UpdateBase
	profile.UpdateBase = base
	t.Cleanup(func() { profile.UpdateBase = oldBase })
	if channel != "" {
		t.Setenv("BROWSER_MCP_UPDATE_CHANNEL", channel)
	} else {
		t.Setenv("BROWSER_MCP_UPDATE_CHANNEL", "")
	}
	resetUpdateSource()
}

// TestTelemetryInstallURL pins the install-URL derivation: drop the trailing
// slug+channel pair from base, keep the mount prefix (so a /tool-hub/ sub-path
// mount does NOT 404 — the regression that host-root stripping caused).
func TestTelemetryInstallURL(t *testing.T) {
	cases := map[string]string{
		// Sub-path-mounted tool-hub (the bug-fix case): keep /tool-hub prefix.
		"http://10.249.19.32/tool-hub/browser-mcp/stable": "http://10.249.19.32/tool-hub/api/telemetry/install",
		"http://host/tool-hub/browser-mcp/sit":             "http://host/tool-hub/api/telemetry/install",
		// Root-mounted tool-hub (old sit layout): slug+channel are the whole path
		// → strip to host root (same as the legacy host-root behavior).
		"http://127.0.0.1:58090/browser-mcp/stable": "http://127.0.0.1:58090/api/telemetry/install",
		"http://10.0.0.1:8080/browser-mcp/sit":      "http://10.0.0.1:8080/api/telemetry/install",
		// Trailing slash tolerated.
		"http://host/browser-mcp/stable/": "http://host/api/telemetry/install",
		// gitee: no telemetry endpoint → 404 silent (acceptable).
		"https://gitee.com/renvip/browser-mcp-release": "https://gitee.com/api/telemetry/install",
		// Root-only base.
		"http://host/": "http://host/api/telemetry/install",
	}
	for base, want := range cases {
		if got := telemetryInstallURL(base); got != want {
			t.Errorf("telemetryInstallURL(%q) = %q, want %q", base, got, want)
		}
	}
	// Fallbacks for non-URL input.
	if got := telemetryInstallURL("not-a-url"); got != "not-a-url/api/telemetry/install" {
		t.Errorf("telemetryInstallURL(%q) = %q, want fallback %q", "not-a-url", got, "not-a-url/api/telemetry/install")
	}
	if got := telemetryInstallURL("/rel/path/"); got != "/rel/path/api/telemetry/install" {
		t.Errorf("telemetryInstallURL(%q) = %q, want fallback %q", "/rel/path/", got, "/rel/path/api/telemetry/install")
	}
}

// TestReportInstallPing_SendsVersionAndChannel asserts the install POST carries
// client_version + channel (when channel configured) and NO client_id.
func TestReportInstallPing_SendsVersionAndChannel(t *testing.T) {
	srv, p := newPingProbe(t)
	configureTelemetryEnv(t, srv.URL, "stable")

	if ok := reportInstallPing(); !ok {
		t.Fatalf("reportInstallPing returned false, want true (2xx)")
	}
	if p.path != "/api/telemetry/install" {
		t.Fatalf("POST path = %q, want /api/telemetry/install", p.path)
	}
	if p.body["client_version"] != version {
		t.Fatalf("client_version = %v, want %q (version var)", p.body["client_version"], version)
	}
	if p.body["channel"] != "stable" {
		t.Fatalf("channel = %v, want stable", p.body["channel"])
	}
	if _, ok := p.body["client_id"]; ok {
		t.Fatalf("client_id present in payload = %v, want absent (UUID removed, server uses IP)", p.body["client_id"])
	}
}

// TestReportInstallPing_NoChannelOmitted asserts that with channel unset, the
// field is omitted from the JSON body (server side channel=null).
func TestReportInstallPing_NoChannelOmitted(t *testing.T) {
	srv, p := newPingProbe(t)
	configureTelemetryEnv(t, srv.URL, "")

	if ok := reportInstallPing(); !ok {
		t.Fatalf("reportInstallPing returned false, want true")
	}
	if p.body["client_version"] != version {
		t.Fatalf("client_version = %v, want %q", p.body["client_version"], version)
	}
	if _, ok := p.body["channel"]; ok {
		t.Fatalf("channel present = %v, want absent (omitempty when unset)", p.body["channel"])
	}
}

// TestReportInstallPing_UnreachableSilent points at a closed port — must return
// false + log without panicking (install report failure never blocks startup).
func TestReportInstallPing_UnreachableSilent(t *testing.T) {
	configureTelemetryEnv(t, "http://127.0.0.1:1", "stable")
	startupPingTimeout = 100 * time.Millisecond
	startupPingClient = &http.Client{Timeout: startupPingTimeout}
	if ok := reportInstallPing(); ok {
		t.Fatalf("reportInstallPing returned true on unreachable server, want false")
	}
}

// TestReportInstallPing_PreservesMountPrefix is the DV-caught 404 regression
// guard: base carrying a /tool-hub/ mount prefix must POST to
// /tool-hub/api/telemetry/install, NOT the root /api/telemetry/install.
func TestReportInstallPing_PreservesMountPrefix(t *testing.T) {
	srv, p := newPingProbe(t)
	configureTelemetryEnv(t, srv.URL+"/tool-hub/browser-mcp/stable", "stable")
	if ok := reportInstallPing(); !ok {
		t.Fatalf("reportInstallPing returned false, want true")
	}
	if p.path != "/tool-hub/api/telemetry/install" {
		t.Fatalf("POST path = %q, want /tool-hub/api/telemetry/install (preserved /tool-hub mount)", p.path)
	}
}

// TestReportInstallIfNewVersion_MarkerLogic pins the trigger semantics:
//   - first run (no marker): sends exactly one install + writes marker
//   - ordinary restart (marker == version): skips (no second POST)
//   - version change: sends again
//   - HTTP failure: marker NOT written → retries next startup
func TestReportInstallIfNewVersion_MarkerLogic(t *testing.T) {
	srv, p := newPingProbe(t)
	configureTelemetryEnv(t, srv.URL, "stable")
	startupPingTimeout = 2 * time.Second
	startupPingClient = &http.Client{Timeout: startupPingTimeout}

	dataDir := os.Getenv("BROWSER_MCP_DATA_DIR")
	marker := filepath.Join(dataDir, "install_reported_version")

	// 1. First run: no marker → sends + writes marker.
	origVersion := version
	version = "v1.0.0"
	reportInstallIfNewVersion()
	if p.calls != 1 {
		t.Fatalf("first run: calls = %d, want 1", p.calls)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("first run: marker not written: %v", err)
	}
	if strings.TrimSpace(string(got)) != "v1.0.0" {
		t.Fatalf("first run: marker = %q, want v1.0.0", string(got))
	}

	// 2. Ordinary restart (same version): marker == version → skip.
	reportInstallIfNewVersion()
	if p.calls != 1 {
		t.Fatalf("restart same version: calls = %d, want 1 (skip)", p.calls)
	}

	// 3. Version change (e.g. self-update landed new build): marker=old ≠ new → sends again.
	version = "v1.0.1"
	reportInstallIfNewVersion()
	if p.calls != 2 {
		t.Fatalf("version change: calls = %d, want 2", p.calls)
	}
	got, err = os.ReadFile(marker)
	if err != nil {
		t.Fatalf("version change: marker not rewritten: %v", err)
	}
	if strings.TrimSpace(string(got)) != "v1.0.1" {
		t.Fatalf("version change: marker = %q, want v1.0.1", string(got))
	}

	// 4. HTTP failure (point at closed port): marker NOT updated → would retry next time.
	version = "v1.0.2"
	configureTelemetryEnv(t, "http://127.0.0.1:1", "stable")
	startupPingTimeout = 100 * time.Millisecond
	startupPingClient = &http.Client{Timeout: startupPingTimeout}
	reportInstallIfNewVersion()
	if p.calls != 2 {
		t.Fatalf("failure: calls = %d, want 2 (no new POST on unreachable)", p.calls)
	}
	got, err = os.ReadFile(marker)
	if err != nil {
		t.Fatalf("failure: marker vanished: %v", err)
	}
	if strings.TrimSpace(string(got)) != "v1.0.1" {
		t.Fatalf("failure: marker = %q, want v1.0.1 (not rewritten on failure)", string(got))
	}

	version = origVersion
}
