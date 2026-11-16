//go:build linux

package connect

import (
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SO_MEMINFO returns nine u32 counters for a socket; the last is sk_drops,
// which UDP advances for every datagram it could not queue because the
// receive buffer was full.
const soMemInfo = 0x37
const skMemInfoDropsIndex = 8

// Datagrams the kernel dropped at this socket's receive buffer, read from
// the socket itself. Zero when the socket exposes no descriptor or the
// kernel predates SO_MEMINFO.
func udpSocketReceiveDropCount(socket net.Conn) uint64 {
	rawConn, ok := socketRawConn(socket)
	if !ok {
		return 0
	}
	var counters [9]uint32
	var errno unix.Errno
	controlErr := rawConn.Control(func(fd uintptr) {
		length := uint32(unsafe.Sizeof(counters))
		// The frozen syscall package does not expose SYS_GETSOCKOPT on every
		// Linux architecture (notably 386); x/sys/unix does.
		_, _, errno = unix.Syscall6(
			unix.SYS_GETSOCKOPT,
			fd,
			unix.SOL_SOCKET,
			soMemInfo,
			uintptr(unsafe.Pointer(&counters[0])),
			uintptr(unsafe.Pointer(&length)),
			0,
		)
	})
	if controlErr != nil || errno != 0 {
		return 0
	}
	return uint64(counters[skMemInfoDropsIndex])
}
