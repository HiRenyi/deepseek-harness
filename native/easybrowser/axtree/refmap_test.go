package axtree

import "testing"

func TestRefMapAssignStable(t *testing.T) {
	m := newRefMap()
	r1 := m.Assign("button", "Submit", -1, "")
	r2 := m.Assign("button", "Submit", -1, "")
	if r1 != r2 {
		t.Errorf("stable reuse: r1=%q r2=%q want equal", r1, r2)
	}
	r3 := m.Assign("button", "Cancel", -1, "")
	if r3 == r1 {
		t.Errorf("different name should get new ref: r3=%q", r3)
	}
}

func TestRefMapNthDisambiguation(t *testing.T) {
	m := newRefMap()
	// Two "textbox" "Note" — nth 0 and 1 — distinct refs.
	a := m.Assign("textbox", "Note", 0, "")
	b := m.Assign("textbox", "Note", 1, "")
	if a == b {
		t.Errorf("nth 0/1 must differ: a=%q b=%q", a, b)
	}
	// Re-assign same (role,name,nth) reuses.
	if got := m.Assign("textbox", "Note", 0, ""); got != a {
		t.Errorf("nth reuse: got %q want %q", got, a)
	}
}

func TestRefMapPrune(t *testing.T) {
	m := newRefMap()
	a := m.Assign("button", "A", -1, "")
	m.Assign("button", "B", -1, "")
	m.Prune(map[string]bool{a: true})
	if _, ok := m.Lookup("e2"); ok {
		t.Error("e2 should have been pruned")
	}
	if _, ok := m.Lookup(a); !ok {
		t.Error("a should survive prune")
	}
}

func TestParseRef(t *testing.T) {
	cases := map[string]string{"e3": "e3", "@e3": "e3", "ref=e3": "e3", " e3 ": "e3"}
	for in, want := range cases {
		if got := parseRef(in); got != want {
			t.Errorf("parseRef(%q)=%q want %q", in, got, want)
		}
	}
}
