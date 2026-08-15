package native

import (
	"bytes"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	original := []byte(`{"jsonrpc":"2.0","method":"DOM.getDocument","id":1}`)

	if err := WriteMessage(&buf, original); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("roundtrip mismatch\ngot:  %s\nwant: %s", got, original)
	}
}

func TestEmptyMessage(t *testing.T) {
	var buf bytes.Buffer

	if err := WriteMessage(&buf, []byte{}); err != nil {
		t.Fatalf("WriteMessage empty: %v", err)
	}

	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty message, got %d bytes", len(got))
	}
}

func TestMultipleMessages(t *testing.T) {
	var buf bytes.Buffer
	messages := [][]byte{
		[]byte(`{"id":1}`),
		[]byte(`{"id":2,"result":{}}`),
		[]byte(`{"jsonrpc":"2.0","method":"DOM.getBoxModel","params":{"backendNodeId":42}}`),
	}

	for _, msg := range messages {
		if err := WriteMessage(&buf, msg); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	for i, expected := range messages {
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage #%d: %v", i+1, err)
		}
		if string(got) != string(expected) {
			t.Errorf("message %d mismatch\ngot:  %s\nwant: %s", i+1, got, expected)
		}
	}
}

func TestLargeMessage(t *testing.T) {
	var buf bytes.Buffer
	// 64KB payload to verify no truncation
	original := make([]byte, 64*1024)
	for i := range original {
		original[i] = byte(i % 256)
	}

	if err := WriteMessage(&buf, original); err != nil {
		t.Fatalf("WriteMessage large: %v", err)
	}

	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage large: %v", err)
	}
	if len(got) != len(original) {
		t.Fatalf("large message length mismatch: got %d, want %d", len(got), len(original))
	}
	for i := range original {
		if got[i] != original[i] {
			t.Fatalf("large message byte %d mismatch: got %d, want %d", i, got[i], original[i])
		}
	}
}
