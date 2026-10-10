//go:build windows

package connect

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows sockets use overlapped I/O, not Unix nonblocking mode. Let
// RawConn.Read wait with its zero-byte read before a synchronous peek so
// idle sockets still honor the net.Conn deadline and Close.
const udpSocketPeekRequiresWait = true

func peekUdpSocket(fd SocketHandle, buffer []byte) (int, error) {
	n, _, err := windows.Recvfrom(windows.Handle(fd), buffer, windows.MSG_PEEK)
	switch err {
	case windows.WSAEWOULDBLOCK:
		return 0, syscall.EWOULDBLOCK
	case windows.WSAEMSGSIZE:
		// MSG_PEEK keeps the full datagram queued, even when the receive
		// buffer only fits a prefix. Charge that bounded read length.
		return len(buffer), nil
	default:
		return n, err
	}
}
