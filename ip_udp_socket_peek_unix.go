//go:build unix

package connect

import "syscall"

const udpSocketPeekRequiresWait = false

// Peek measures the bounded read while leaving the datagram kernel-owned
// until return-read admission succeeds.
func peekUdpSocket(fd SocketHandle, buffer []byte) (int, error) {
	n, _, err := syscall.Recvfrom(fd, buffer, syscall.MSG_PEEK)
	return n, err
}
