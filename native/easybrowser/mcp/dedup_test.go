package mcp

import (
	"strings"
	"testing"
)

func TestTruncateSnapshotUnderLimit(t *testing.T) {
	in := "line1\nline2\nline3"
	got := truncateSnapshot(in)
	if got != in {
		t.Errorf("under-limit input should pass through unchanged, got: %q", got)
	}
}

func TestTruncateSnapshotOverLines(t *testing.T) {
	var lines []string
	for i := 0; i < maxSnapshotLines+50; i++ {
		lines = append(lines, "l"+itoa(i))
	}
	in := strings.Join(lines, "\n")
	got := truncateSnapshot(in)
	if !strings.Contains(got, "(truncated") {
		t.Fatalf("expected truncation footer, got end: %q", got[len(got)-80:])
	}
	if !strings.Contains(got, "50 more lines") {
		t.Errorf("footer should report omitted line count, got end: %q", got[len(got)-120:])
	}
	// Should NOT contain lines beyond the cap.
	if strings.Contains(got, "l"+itoa(maxSnapshotLines+5)) {
		t.Errorf("truncated content should not include post-cap lines")
	}
}

func TestTruncateSnapshotOverBytes(t *testing.T) {
	// One giant line exceeding the byte cap.
	big := strings.Repeat("x", maxSnapshotBytes+5000)
	got := truncateSnapshot(big)
	if !strings.Contains(got, "(truncated") {
		t.Errorf("expected byte-truncation footer")
	}
	if !strings.Contains(got, "more bytes") {
		t.Errorf("footer should report omitted byte count, got end: %q", got[len(got)-100:])
	}
}

func TestDedupUnchangedDOM(t *testing.T) {
	dedupReset("dom")
	sig := "url=http://x/|node=5"
	unchanged, msg := dedupUnchanged("dom", sig, 5)
	if unchanged {
		t.Fatalf("first call must not be unchanged: %s", msg)
	}
	unchanged2, msg2 := dedupUnchanged("dom", sig, 5)
	if !unchanged2 {
		t.Fatalf("second identical call must be unchanged: %s", msg2)
	}
	if !strings.Contains(msg2, "unchanged") || !strings.Contains(msg2, "hash=") {
		t.Errorf("unchanged message missing fields: %s", msg2)
	}
	if !strings.Contains(msg2, "force:true") {
		t.Errorf("unchanged message should mention force:true escape hatch: %s", msg2)
	}
}

func TestDedupChangedResetsBaseline(t *testing.T) {
	dedupReset("ax")
	dedupUnchanged("ax", "sig-A", 3)
	// Different signature -> full, baseline updated.
	unchanged, _ := dedupUnchanged("ax", "sig-B", 3)
	if unchanged {
		t.Fatalf("changed signature must return full, not unchanged")
	}
	// Now sig-B is baseline; repeating it is unchanged.
	unchanged2, _ := dedupUnchanged("ax", "sig-B", 3)
	if !unchanged2 {
		t.Fatalf("repeating the new baseline must be unchanged")
	}
}

func TestDedupDOMAndAXAreIndependent(t *testing.T) {
	dedupReset("dom")
	dedupReset("ax")
	dedupUnchanged("dom", "shared-sig", 1)
	// Same signature on AX slot must NOT be treated as unchanged (different slot).
	unchanged, _ := dedupUnchanged("ax", "shared-sig", 1)
	if unchanged {
		t.Fatalf("dom and ax baselines must be independent")
	}
}

// itoa avoids importing strconv just for one test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
