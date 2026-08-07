//go:build unix

package connect

import (
	"syscall"
)

type SocketHandle = int

func GetSocketTtl(fd SocketHandle) int {
	nativeTtl, _ := syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL)
	return nativeTtl
}

// SetSocketTtl sets the outgoing TTL on the socket. The error is returned
// rather than discarded because IP_TTL only accepts 1-255: Linux fails a 0
// with EINVAL, so a swallowed error let the reorder technique silently
// degrade to plain fragmentation. Callers decide whether a failure is fatal
// (see resilientLowTtl and the TTL helpers in net_resilient.go).
func SetSocketTtl(fd SocketHandle, ttl int) error {
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
}
