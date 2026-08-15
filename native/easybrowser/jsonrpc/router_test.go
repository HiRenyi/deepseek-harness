package jsonrpc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestSendReceive(t *testing.T) {
	var mu sync.Mutex
	var sent []byte

	sender := func(data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent = make([]byte, len(data))
		copy(sent, data)
		return nil
	}

	router := NewRouter(sender, 5*time.Second)

	// Send request in background, then deliver response
	done := make(chan *Response, 1)
	go func() {
		resp, err := router.SendRequest(context.Background(), "test.method", json.RawMessage(`{"key":"val"}`))
		if err != nil {
			t.Errorf("SendRequest error: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	// Wait for request to be sent
	time.Sleep(50 * time.Millisecond)

	// Parse the sent request to get the ID
	mu.Lock()
	var req Request
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	mu.Unlock()

	if req.Method != "test.method" {
		t.Errorf("expected method test.method, got %s", req.Method)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %s", req.JSONRPC)
	}

	// Deliver response
	respData, _ := json.Marshal(Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  json.RawMessage(`{"answer":42}`),
	})
	router.HandleResponse(respData)

	// Check result
	resp := <-done
	if resp == nil {
		t.Fatal("got nil response")
	}
	if string(resp.Result) != `{"answer":42}` {
		t.Errorf("unexpected result: %s", string(resp.Result))
	}
}

func TestTimeout(t *testing.T) {
	// Sender that never delivers a response
	sender := func(data []byte) error { return nil }
	router := NewRouter(sender, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := router.SendRequest(ctx, "slow.method", nil)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestErrorResponse(t *testing.T) {
	var mu sync.Mutex
	var sent []byte

	sender := func(data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent = make([]byte, len(data))
		copy(sent, data)
		return nil
	}

	router := NewRouter(sender, 5*time.Second)

	done := make(chan *Response, 1)
	go func() {
		resp, err := router.SendRequest(context.Background(), "fail.method", nil)
		if err != nil {
			t.Errorf("SendRequest error: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	var req Request
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	mu.Unlock()

	// Deliver error response
	respData, _ := json.Marshal(Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &ResponseError{
			Code:    -32000,
			Message: "Server error",
		},
	})
	router.HandleResponse(respData)

	resp := <-done
	if resp == nil {
		t.Fatal("got nil response")
	}
	if resp.Error == nil {
		t.Fatal("expected error in response")
	}
	if resp.Error.Code != -32000 {
		t.Errorf("expected error code -32000, got %d", resp.Error.Code)
	}
	if resp.Error.Message != "Server error" {
		t.Errorf("expected error message 'Server error', got %s", resp.Error.Message)
	}
}

func TestHandleResponseUnknownID(t *testing.T) {
	sender := func(data []byte) error { return nil }
	router := NewRouter(sender, 5*time.Second)

	// Should not panic on unknown ID
	respData, _ := json.Marshal(Response{
		JSONRPC: "2.0",
		ID:      "nonexistent",
		Result:  json.RawMessage(`{}`),
	})
	router.HandleResponse(respData) // no panic
}

func TestHandleResponseInvalidJSON(t *testing.T) {
	sender := func(data []byte) error { return nil }
	router := NewRouter(sender, 5*time.Second)

	// Should not panic on invalid JSON
	router.HandleResponse([]byte(`not json at all`)) // no panic
}
