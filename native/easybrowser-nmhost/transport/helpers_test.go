package transport

import (
	"regexp"
	"testing"
)

var nmPipeNameRe = regexp.MustCompile(`^\\\\\.\\pipe\\browser-mcp-[0-9a-f]{8}$`)

func TestDeterministicPipeName_Stable(t *testing.T) {
	a := DeterministicPipeName(`C:\Users\x\AppData\Local\browser-mcp`)
	b := DeterministicPipeName(`C:\Users\x\AppData\Local\browser-mcp`)
	if a != b {
		t.Fatalf("same dataDir must yield same name: %q vs %q", a, b)
	}
}

func TestDeterministicPipeName_DifferentDataDir(t *testing.T) {
	a := DeterministicPipeName(`C:\dirA`)
	b := DeterministicPipeName(`C:\dirB`)
	if a == b {
		t.Fatalf("different dataDir must yield different name: both %q", a)
	}
}

func TestDeterministicPipeName_Format(t *testing.T) {
	name := DeterministicPipeName(`C:\Users\x\AppData\Local\browser-mcp`)
	if !nmPipeNameRe.MatchString(name) {
		t.Fatalf("name %q does not match expected format", name)
	}
}
