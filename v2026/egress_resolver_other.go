//go:build !windows

package connect

import "net"

// Off Windows the OS resolver is already tunnel-aware (macOS network
// extensions and Android VpnService route the extension's own lookups
// correctly), so control dials keep the platform resolver: nil means
// net.Dialer uses net.DefaultResolver, exactly as before.
func egressResolver() *net.Resolver {
	return nil
}
