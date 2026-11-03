//go:build windows

package connect

import (
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/windows"
)

type SocketHandle = syscall.Handle

// duplicateSocketHandle retains a second handle to the connection's socket for
// the complete TTL choreography without calling TCPConn.File. On Windows File
// first detaches the socket from the runtime's I/O completion port, for good,
// because the duplicate it returns may do I/O of its own. After that every read
// and write on the net.Conn parks an OS thread in an infinite wait, read and
// write deadlines no longer fire, and Close waits on a parked read until the
// peer sends or hangs up. The duplicate here only ever carries socket options,
// never I/O, so it is the in-process WSADuplicateSocket that File makes, minus
// the detach, and the connection stays on the poller.
func duplicateSocketHandle(conn *net.TCPConn) (SocketHandle, func(), error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0, nil, err
	}
	var duplicate windows.Handle
	var duplicateErr error
	if err := rawConn.Control(func(fd uintptr) {
		var info windows.WSAProtocolInfo
		duplicateErr = windows.WSADuplicateSocket(windows.Handle(fd), windows.GetCurrentProcessId(), &info)
		if duplicateErr != nil {
			return
		}
		duplicate, duplicateErr = windows.WSASocket(-1, -1, -1, &info, 0, windows.WSA_FLAG_OVERLAPPED|windows.WSA_FLAG_NO_HANDLE_INHERIT)
	}); err != nil {
		return 0, nil, err
	}
	if duplicateErr != nil {
		return 0, nil, duplicateErr
	}
	return SocketHandle(duplicate), func() { _ = windows.Closesocket(duplicate) }, nil
}

// socketTtlOption selects the socket option that controls the outgoing hop
// count for THIS socket's address family. See the unix file for why IP_TTL
// alone silently disabled the reorder technique on IPv6 sockets.
func socketTtlOption(fd SocketHandle) (level int, opt int) {
	if sockaddr, err := syscall.Getsockname(fd); err == nil {
		if in6, ok := sockaddr.(*syscall.SockaddrInet6); ok {
			if !netip.AddrFrom16(in6.Addr).Is4In6() {
				return syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS
			}
		}
	}
	return syscall.IPPROTO_IP, syscall.IP_TTL
}

// GetSocketTtl reads the outgoing hop count. See the unix file for the
// contract: 0 means unreadable, and a negative value is the kernel-default
// sentinel that SetSocketTtl accepts back.
func GetSocketTtl(fd SocketHandle) int {
	level, opt := socketTtlOption(fd)
	nativeTtl, err := syscall.GetsockoptInt(fd, level, opt)
	if err != nil {
		return socketTtlUnreadable
	}
	return nativeTtl
}

// SetSocketTtl sets the outgoing TTL (IPv6: hop limit) on the socket. The
// error is returned rather than discarded so callers can tell a rejected TTL
// from an applied one. Windows accepts IP_TTL=0 where Linux rejects it, so the
// discarded error also hid a real behaviour difference between the two
// platforms.
func SetSocketTtl(fd SocketHandle, ttl int) error {
	level, opt := socketTtlOption(fd)
	return syscall.SetsockoptInt(fd, level, opt, ttl)
}
