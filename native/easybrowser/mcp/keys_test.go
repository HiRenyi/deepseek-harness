package mcp

import "testing"

func TestKeyToCode(t *testing.T) {
	cases := map[string]string{
		"Enter":     "Enter",
		"Tab":       "Tab",
		"Escape":    "Escape",
		"Backspace": "Backspace",
		"Delete":    "Delete",
		"ArrowUp":   "ArrowUp",
		"Home":      "Home",
		"End":       "End",
		"Control":   "ControlLeft",
		"Unknown":   "",
	}
	for key, want := range cases {
		if got := keyToCode(key); got != want {
			t.Errorf("keyToCode(%q)=%q, want %q", key, got, want)
		}
	}
}

func TestKeyToVKCode(t *testing.T) {
	cases := map[string]int{
		"Enter":     13,
		"Tab":       9,
		"Escape":    27,
		"Backspace": 8,
		"Delete":    46,
		"ArrowUp":   38,
		"ArrowDown": 40,
		"Home":      36,
		"End":       35,
		"Control":   17,
		"Shift":     16,
		"Alt":       18,
		"Unknown":   0,
	}
	for key, want := range cases {
		if got := keyToVKCode(key); got != want {
			t.Errorf("keyToVKCode(%q)=%d, want %d", key, got, want)
		}
	}
}

func TestIsNamedKey(t *testing.T) {
	if !isNamedKey("Enter") {
		t.Error("isNamedKey(Enter) = false, want true")
	}
	if isNamedKey("a") {
		t.Error("isNamedKey(a) = true, want false (printable char)")
	}
	if isNamedKey("Unknown") {
		t.Error("isNamedKey(Unknown) = true, want false")
	}
}
