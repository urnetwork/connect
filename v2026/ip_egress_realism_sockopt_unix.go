//go:build unix

package connect

import "syscall"

// Reads the actual socket family's TTL/hop limit without changing it.
func providerSocketTtl(fd uintptr, ipv6 bool) (int, error) {
	if ipv6 {
		return syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS)
	}
	return syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL)
}

// These ordinary unicast options do not need raw-socket privileges.
func setProviderSocketTtl(fd uintptr, ipv6 bool, ttl int) error {
	if ipv6 {
		return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
	}
	return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
}
