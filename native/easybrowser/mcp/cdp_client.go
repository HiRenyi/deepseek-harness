package mcp

import (
	"context"
	"encoding/json"

	"github.com/browser-mcp/bridge/jsonrpc"
)

// CDPClient is a thin abstraction over CDP method calls. It mirrors Codex's
// t.cdp.call(tabId, method, params) pattern: callers depend on this interface,
// not the concrete transport, so interaction logic is unit-testable with a fake.
type CDPClient interface {
	// Call sends a CDP method with params and returns the JSON-RPC response.
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error)
}

// routerCDPClient adapts an existing *jsonrpc.Router (via the package-level
// sendCDP helper) to the CDPClient interface. It always targets the active tab
// (tabId=nil), matching the MVP snapshot behavior.
type routerCDPClient struct {
	router *jsonrpc.Router
}

// NewRouterCDPClient wraps a Router as a CDPClient.
func NewRouterCDPClient(router *jsonrpc.Router) CDPClient {
	return &routerCDPClient{router: router}
}

func (c *routerCDPClient) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error) {
	return sendCDP(ctx, c.router, method, params)
}
