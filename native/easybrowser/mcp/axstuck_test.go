package mcp

import (
	"strings"
	"testing"
)

// resetAxStuck clears the package-global stuck counter so tests are
// order-independent. (DQ2 test helper.)
func resetAxStuck() {
	axStuck.Lock()
	axStuck.sig = ""
	axStuck.n = 0
	axStuck.Unlock()
}

func TestAxStuckDirectiveSameSignatureTripAtThreshold(t *testing.T) {
	resetAxStuck()
	sig := "url=https://x/|nodes=10|iframes=1"

	if d := axStuckDirective(sig); d != "" {
		t.Fatalf("first snapshot must not trip, got %q", d)
	}
	if d := axStuckDirective(sig); d != "" {
		t.Fatalf("second snapshot (n=1) must not trip at threshold=2, got %q", d)
	}
	d := axStuckDirective(sig) // n=2 -> trip
	if d == "" {
		t.Fatalf("third unchanged snapshot (n=2) must trip hard-switch directive")
	}
	if !strings.Contains(d, "browser_snapshot_visible") {
		t.Errorf("directive should name the DOM switch tool: %q", d)
	}
	if !strings.Contains(d, "no structural change") {
		t.Errorf("directive should explain the stuck condition: %q", d)
	}
}

func TestAxStuckDirectiveChangedSignatureResets(t *testing.T) {
	resetAxStuck()
	a := "url=https://x/|nodes=10|iframes=1"
	b := "url=https://y/|nodes=40|iframes=0" // different page / popup opened

	axStuckDirective(a) // n=0
	axStuckDirective(a) // n=1
	if d := axStuckDirective(b); d != "" {
		t.Fatalf("changed signature must reset and not trip, got %q", d)
	}
	// After reset, one more unchanged call must NOT yet trip (n=1).
	if d := axStuckDirective(b); d != "" {
		t.Fatalf("after reset, n=1 must not trip, got %q", d)
	}
}
