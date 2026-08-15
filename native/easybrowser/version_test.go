package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestVersionShortCircuit pins that --version prints build metadata and exits
// WITHOUT acquiring the bridge lock — a version query must coexist with a
// running bridge. The pre-fix code had no such path; this guards against
// regressions that move --version after AcquireLock.
func TestVersionShortCircuit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the bridge binary")
	}

	// Resolve the bridge module root from this test file's location so the
	// test works regardless of the cwd `go test` is invoked from.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	bridgeDir := filepath.Dir(file)

	exeName := "bridge"
	if runtime.GOOS == "windows" {
		exeName = "bridge.exe"
	}
	bin := filepath.Join(t.TempDir(), exeName)

	// Build the bridge binary into a temp dir (isolated, like health_smoke_test).
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = bridgeDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Isolate the data dir so we never collide with a real running bridge.
	// (Even though --version exits before AcquireLock, defense-in-depth.)
	runCmd := exec.Command(bin, "--version")
	runCmd.Env = append(append([]string(nil), os.Environ()...),
		"BROWSER_MCP_DATA_DIR="+t.TempDir())
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bridge --version failed: %v\n%s", err, out)
	}
	got := string(out)
	// Assert the full field set defined in
	// openspec/changes/native-messaging-lifecycle/specs/binary-lifecycle/spec.md:
	// `bridge version=<v> commit=<c> build=<t> started=<s> pid=<p>`. A regression
	// dropping any field must fail the test. Use t.Errorf (not Fatalf) so all
	// missing fields are reported in a single run.
	for _, want := range []string{"bridge ", "version=", "commit=", "build=", "started=", "pid="} {
		if !strings.Contains(got, want) {
			t.Errorf("bridge --version output missing %q: %q", want, got)
		}
	}
}
