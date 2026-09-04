//go:build unix

package connect

import (
	"net"
	"syscall"
)

type SocketHandle = int

// duplicateSocketHandle retains a socket handle for the complete TTL
// choreography without calling TCPConn.File().Fd(). File().Fd() deliberately
// clears O_NONBLOCK on Unix; duplicated descriptors share that status flag, so
// it also turns the original net.Conn into a blocking socket and can deadlock
// HTTP/2 Close behind a raw read. Dup leaves the shared status flags unchanged.
func duplicateSocketHandle(conn *net.TCPConn) (SocketHandle, func(), error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return -1, nil, err
	}
	duplicate := -1
	var duplicateErr error
	if err := rawConn.Control(func(fd uintptr) {
		duplicate, duplicateErr = syscall.Dup(int(fd))
		if duplicateErr == nil {
			syscall.CloseOnExec(duplicate)
		}
	}); err != nil {
		return -1, nil, err
	}
	if duplicateErr != nil {
		return -1, nil, duplicateErr
	}
	return duplicate, func() { _ = syscall.Close(duplicate) }, nil
}

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
