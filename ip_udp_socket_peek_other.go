//go:build !unix

package connect

import (
	"errors"
)

// Windows and js/wasm have no portable non-consuming datagram peek
// (syscall.MSG_PEEK is unix only, and js has no sockets). Return reads there
// skip the peek and charge a full read buffer, the same bounded pre-read
// charge that non-syscall connections use. The shared readiness poller that
// also peeks has no backend on these platforms (ip_udp_socket_poller_unsupported.go).
const udpSocketPeekSupported = false

// Unreachable while udpSocketPeekSupported is false; reports unsupported for
// any other caller rather than claiming a datagram length.
func peekUdpSocket(fd SocketHandle, buffer []byte) (int, error) {
	return 0, errors.ErrUnsupported
}
