package axtree

import "testing"

func TestRoleNameTrackerUniqueAndAmbiguous(t *testing.T) {
	tr := newRoleNameTracker()
	// Three "textbox"/"Note" -> all ambiguous, nth 0,1,2.
	p0 := tr.track("textbox", "Note")
	p1 := tr.track("textbox", "Note")
	p2 := tr.track("textbox", "Note")
	got0 := tr.finalize("textbox", "Note", p0)
	got1 := tr.finalize("textbox", "Note", p1)
	got2 := tr.finalize("textbox", "Note", p2)
	if (got0 != 0) || (got1 != 1) || (got2 != 2) {
		t.Errorf("nth ambiguous: got %d,%d,%d want 0,1,2", got0, got1, got2)
	}
	// A unique role/name stays -1.
	pu := tr.track("button", "Submit")
	if got := tr.finalize("button", "Submit", pu); got != -1 {
		t.Errorf("unique nth=%d want -1", got)
	}
}

func TestRoleWhitelists(t *testing.T) {
	if !INTERACTIVE_ROLES["button"] || !INTERACTIVE_ROLES["textbox"] {
		t.Error("expected interactive roles present")
	}
	if !CONTENT_ROLES["heading"] {
		t.Error("expected heading in content roles")
	}
	if INTERACTIVE_ROLES["heading"] {
		t.Error("heading must not be interactive")
	}
}
