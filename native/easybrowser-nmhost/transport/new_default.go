//go:build !windows

package transport

// NewClient creates a transport client based on the address info.
// On Linux/Mac, this routes to Unix Socket for "unix" network type.
func NewClient(addrInfo AddrInfo) Client {
	switch addrInfo.Network {
	case "unix":
		return NewUnixClient(addrInfo)
	case "tcp":
		return NewTCPClient(addrInfo.Address)
	default:
		// Fallback to unix on Linux/Mac
		return NewUnixClient(addrInfo)
	}
}
