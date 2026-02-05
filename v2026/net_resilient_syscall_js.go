//go:build js

package connect

type SocketHandle = int

func GetSocketTtl(fd SocketHandle) int {
	// not supported
	return 0
}

func SetSocketTtl(fd SocketHandle, ttl int) {
	// not supported
}
