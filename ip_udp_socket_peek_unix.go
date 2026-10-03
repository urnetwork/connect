//go:build unix

package connect

import (
	"syscall"
)

// Unix sockets can report the next datagram's length without consuming it,
// so a return read is charged its actual size before the read takes it.
const udpSocketPeekSupported = true

// Borrows `buffer` for the call. Returns the next datagram's length (bounded by
// the buffer) and leaves the datagram queued in the kernel.
func peekUdpSocket(fd SocketHandle, buffer []byte) (int, error) {
	n, _, err := syscall.Recvfrom(fd, buffer, syscall.MSG_PEEK)
	return n, err
}
