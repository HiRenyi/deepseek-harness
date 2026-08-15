package axtree

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// captureLog swaps log.Output to a buffer; returns the buffer and a restore.
func captureLog() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&buf)
	return &buf, func() { log.SetOutput(saved); _ = os.Stderr }
}

func TestRefMap_LookupMissLogged(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	m := newRefMap()
	_, ok := m.Lookup("e999")
	if ok {
		t.Fatal("expected miss")
	}
	if !strings.Contains(buf.String(), "refmap: lookup miss ref=e999") {
		t.Fatalf("lookup miss not logged: %q", buf.String())
	}
}

func TestRefMap_AssignInteractiveLogged(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	m := newRefMap()
	id := m.Assign("button", "OK", -1, "main")
	if !strings.Contains(buf.String(), "refmap: assign ref="+id+" role=button name=\"OK\" nth=-1 frame=main") {
		t.Fatalf("assign interactive not logged: %q", buf.String())
	}
}

func TestRefMap_AssignContentRoleNotLogged(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	m := newRefMap()
	m.Assign("heading", "Title", -1, "main") // content role, not logged
	if strings.Contains(buf.String(), "refmap: assign") {
		t.Fatalf("content role assign should not be logged: %q", buf.String())
	}
}

func TestRefMap_AssignReuseNotLogged(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	m := newRefMap()
	id := m.Assign("button", "OK", -1, "")
	buf.Reset() // clear the initial assign log
	m.Assign("button", "OK", -1, "") // reuse, same identity
	if buf.String() != "" {
		t.Fatalf("reuse assign should not be logged: %q", buf.String())
	}
	_ = id
}

func TestRefMap_PruneRemovedLogged(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	m := newRefMap()
	a := m.Assign("button", "A", -1, "")
	b := m.Assign("button", "B", -1, "")
	buf.Reset()
	m.Prune(map[string]bool{a: true}) // removes b, keeps a
	if !strings.Contains(buf.String(), "refmap: prune removed 1 refs:") {
		t.Fatalf("prune not logged: %q", buf.String())
	}
	if !strings.Contains(buf.String(), b) {
		t.Fatalf("prune list missing removed ref %s: %q", b, buf.String())
	}
}

func TestRefMap_PruneNothingRemovedNotLogged(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	m := newRefMap()
	a := m.Assign("button", "A", -1, "")
	buf.Reset()
	m.Prune(map[string]bool{a: true}) // removes nothing
	if strings.Contains(buf.String(), "refmap: prune") {
		t.Fatalf("empty prune should not be logged: %q", buf.String())
	}
}
