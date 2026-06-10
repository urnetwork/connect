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

func SetSocketTtl(fd SocketHandle, ttl int) {
	syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
}
