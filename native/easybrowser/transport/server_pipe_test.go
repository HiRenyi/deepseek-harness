//go:build windows

package transport

import (
	"regexp"
	"testing"
)

var pipeNameRe = regexp.MustCompile(`^\\\\\.\\pipe\\browser-mcp-[0-9a-f]{8}$`)

func TestDeterministicPipeName_Stable(t *testing.T) {
	a := deterministicPipeName(`C:\Users\x\AppData\Local\browser-mcp`)
	b := deterministicPipeName(`C:\Users\x\AppData\Local\browser-mcp`)
	if a != b {
		t.Fatalf("same dataDir must yield same name: %q vs %q", a, b)
	}
}

func TestDeterministicPipeName_DifferentDataDir(t *testing.T) {
	a := deterministicPipeName(`C:\dirA`)
	b := deterministicPipeName(`C:\dirB`)
	if a == b {
		t.Fatalf("different dataDir must yield different name: both %q", a)
	}
}

func TestDeterministicPipeName_Format(t *testing.T) {
	name := deterministicPipeName(`C:\Users\x\AppData\Local\browser-mcp`)
	if !pipeNameRe.MatchString(name) {
		t.Fatalf("name %q does not match expected format", name)
	}
}

func TestNewPipeServer_UsesDeterministicName(t *testing.T) {
	t.Setenv("BROWSER_MCP_DATA_DIR", `C:\test-isolated-dir`)
	srv := NewPipeServer()
	expected := deterministicPipeName(`C:\test-isolated-dir`)
	if srv.pipeName != expected {
		t.Fatalf("NewPipeServer pipeName=%q, want deterministic %q", srv.pipeName, expected)
	}
}
