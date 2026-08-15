//go:build windows

package transport

// NewClient creates a transport client based on the address info.
// On Windows, this routes to Named Pipe for "pipe" network type.
func NewClient(addrInfo AddrInfo) Client {
	switch addrInfo.Network {
	case "pipe":
		return NewPipeClient(addrInfo)
	case "tcp":
		return NewTCPClient(addrInfo.Address)
	default:
		// Fallback to pipe on Windows
		return NewPipeClient(addrInfo)
	}
}
