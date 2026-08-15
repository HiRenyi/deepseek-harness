package axtree

import (
	"context"
	"encoding/json"

	"github.com/browser-mcp/bridge/jsonrpc"
)

// CDPClient is the axtree-local CDP abstraction. It mirrors mcp.CDPClient's
// shape (structural typing): mcp's concrete client satisfies this directly, so
// no adapter wrapper is needed. Kept local to avoid an mcp->axtree->mcp cycle.
type CDPClient interface {
	// Call sends a CDP method with params and returns the JSON-RPC response.
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Response, error)
}
