package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sender is a function that sends raw JSON bytes over the transport.
type Sender func([]byte) error

// Request represents a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response represents a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError represents a JSON-RPC 2.0 error object.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// pendingEntry tracks an outstanding request awaiting a response.
type pendingEntry struct {
	ch  chan *Response
	ctx context.Context
}

// Router sends JSON-RPC requests and dispatches responses.
// It also supports server-side method handlers for incoming requests
// (used for config.set_cdp_permission from the extension).
type Router struct {
	sender    Sender
	timeout   time.Duration
	pending   sync.Map // map[string]*pendingEntry
	handlers  map[string]func(json.RawMessage) (any, error)
	handlersMu sync.RWMutex
}

// NewRouter creates a router that sends requests via sender with the given timeout.
func NewRouter(sender Sender, timeout time.Duration) *Router {
	return &Router{
		sender:   sender,
		timeout:  timeout,
		handlers: make(map[string]func(json.RawMessage) (any, error)),
	}
}

// RegisterMethod registers a handler for an incoming JSON-RPC method.
// Used for methods that the extension calls on the bridge (e.g. config.set_cdp_permission).
func (r *Router) RegisterMethod(method string, handler func(json.RawMessage) (any, error)) {
	r.handlersMu.Lock()
	defer r.handlersMu.Unlock()
	r.handlers[method] = handler
}

// SendRequest sends a JSON-RPC request and waits for the response.
func (r *Router) SendRequest(ctx context.Context, method string, params json.RawMessage) (*Response, error) {
	id := uuid.New().String()
	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Register pending before sending
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	entry := &pendingEntry{ch: make(chan *Response, 1), ctx: ctx}
	r.pending.Store(id, entry)
	defer r.pending.Delete(id)

	if err := r.sender(data); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	select {
	case resp := <-entry.ch:
		return resp, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("request %s timed out: %w", method, ctx.Err())
	}
}

// HandleResponse processes an incoming JSON-RPC response and routes it to the waiting caller.
func (r *Router) HandleResponse(data []byte) {
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}

	if resp.ID == "" {
		return
	}

	val, ok := r.pending.Load(resp.ID)
	if !ok {
		return
	}

	entry := val.(*pendingEntry)
	select {
	case entry.ch <- &resp:
	default:
		// Channel full, response already received or caller timed out
	}
}
