//go:build windows

package connect

import (
	"syscall"
)

type SocketHandle = syscall.Handle

func GetSocketTtl(fd SocketHandle) int {
	nativeTtl, _ := syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL)
	return nativeTtl
}

// SetSocketTtl sets the outgoing TTL on the socket. The error is returned
// rather than discarded so callers can tell a rejected TTL from an applied
// one. Windows accepts IP_TTL=0 where Linux rejects it, so the discarded
// error also hid a real behaviour difference between the two platforms.
func SetSocketTtl(fd SocketHandle, ttl int) error {
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
}
