//go:build windows

package transport

import (
	"log"
	"os"
)

// NewServer creates the default transport server for Windows.
// It respects the BROWSER_MCP_TRANSPORT environment variable:
//   - "pipe": Named Pipe only (no fallback)
//   - "tcp":  TCP only (skip pipe)
//   - "auto" or unset: try Named Pipe, fallback to TCP on failure
func NewServer() Server {
	transport := os.Getenv("BROWSER_MCP_TRANSPORT")
	switch transport {
	case "pipe":
		return NewPipeServer()
	case "tcp":
		return NewTCPServer("127.0.0.1:0")
	case "auto", "":
		return NewFallbackServer([]ServerFactory{
			func() Server { return NewPipeServer() },
			func() Server { return NewTCPServer("127.0.0.1:0") },
		})
	default:
		log.Printf("WARNING: unknown BROWSER_MCP_TRANSPORT=%q, using auto with fallback", transport)
		return NewFallbackServer([]ServerFactory{
			func() Server { return NewPipeServer() },
			func() Server { return NewTCPServer("127.0.0.1:0") },
		})
	}
}
