package mcp

// keyMap maps named keys (aligned with Codex cua_keypress key names and the
// UI Events key spec) to their CDP `code` and Windows virtual-key code.
// These are used by dispatchKey for named/special keys.
var keyMap = map[string]struct {
	code   string
	vkCode int
}{
	"Enter":      {"Enter", 13},
	"Tab":        {"Tab", 9},
	"Escape":     {"Escape", 27},
	"Backspace":  {"Backspace", 8},
	"Delete":     {"Delete", 46},
	"ArrowUp":    {"ArrowUp", 38},
	"ArrowDown":  {"ArrowDown", 40},
	"ArrowLeft":  {"ArrowLeft", 37},
	"ArrowRight": {"ArrowRight", 39},
	"Home":       {"Home", 36},
	"End":        {"End", 35},
	"PageUp":     {"PageUp", 33},
	"PageDown":   {"PageDown", 34},
	"Control":    {"ControlLeft", 17},
	"Shift":      {"ShiftLeft", 16},
	"Alt":        {"AltLeft", 18},
	"Meta":       {"MetaLeft", 91},
}

func keyToCode(key string) string {
	if k, ok := keyMap[key]; ok {
		return k.code
	}
	return ""
}

func keyToVKCode(key string) int {
	if k, ok := keyMap[key]; ok {
		return k.vkCode
	}
	return 0
}

// isNamedKey reports whether key is a special/named key (not a printable char).
func isNamedKey(key string) bool {
	_, ok := keyMap[key]
	return ok
}
