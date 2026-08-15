//go:build !windows

package transport

import (
	"log"
	"os"
)

// NewServer creates the default transport server for Linux/Mac.
// It respects the BROWSER_MCP_TRANSPORT environment variable:
//   - "unix": Unix Socket only (no fallback)
//   - "tcp":  TCP only (skip unix socket)
//   - "auto" or unset: try Unix Socket, fallback to TCP on failure
func NewServer() Server {
	transport := os.Getenv("BROWSER_MCP_TRANSPORT")
	switch transport {
	case "unix":
		return NewUnixServer()
	case "tcp":
		return NewTCPServer("127.0.0.1:0")
	case "auto", "":
		return NewFallbackServer([]ServerFactory{
			func() Server { return NewUnixServer() },
			func() Server { return NewTCPServer("127.0.0.1:0") },
		})
	default:
		log.Printf("WARNING: unknown BROWSER_MCP_TRANSPORT=%q, using auto with fallback", transport)
		return NewFallbackServer([]ServerFactory{
			func() Server { return NewUnixServer() },
			func() Server { return NewTCPServer("127.0.0.1:0") },
		})
	}
}
