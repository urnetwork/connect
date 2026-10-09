//go:build !unix && !windows

package connect

import "errors"

var errProviderSocketTtlUnsupported = errors.New("provider socket TTL is unavailable on this platform")

// Platforms without native socket options retain their original behavior.
func providerSocketTtl(fd uintptr, ipv6 bool) (int, error) {
	return 0, errProviderSocketTtlUnsupported
}

// The caller counts unsupported options and continues the flow.
func setProviderSocketTtl(fd uintptr, ipv6 bool, ttl int) error {
	return errProviderSocketTtlUnsupported
}
