package connect

import (
	"errors"
	"net"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The listener saves the received IP/TCP SYN without raw sockets or packet
// capture privileges. The received header distinguishes pre-connect mirroring
// from a post-connect setter that can only change later packets.
func TestProviderEgressSavedSynCarriesClientTtl(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, reliable := range []bool{false, true} {
			for _, opaque := range []bool{false, true} {
				settings := DefaultLocalUserNatSettingsWithBufferSize(2)
				providerEgressTestBaseline(&settings.TcpBufferSettings.ConnectSettings, 91)
				if opaque {
					host := &net.Dialer{Control: settings.TcpBufferSettings.DialControl}
					settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: host.DialContext}
				}
				f := newProviderEgressTestFlow(t, version, IpProtocolTcp, settings, 123, reliable, func(listener *net.TCPListener) {
					raw, err := listener.SyscallConn()
					if err != nil {
						t.Fatal(err)
					}
					var optionErr error
					err = raw.Control(func(fd uintptr) { optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_SAVE_SYN, 1) })
					if errors.Is(optionErr, unix.ENOPROTOOPT) || errors.Is(optionErr, unix.EINVAL) {
						t.Skipf("kernel has no saved SYN support: %v", optionErr)
					}
					if err != nil || optionErr != nil {
						t.Fatalf("save SYN: raw=%v option=%v", err, optionErr)
					}
				})
				raw, err := f.peer.(*net.TCPConn).SyscallConn()
				if err != nil {
					t.Fatal(err)
				}
				var saved [256]byte
				length := uint32(len(saved))
				var optionErr unix.Errno
				err = raw.Control(func(fd uintptr) {
					// GetsockoptString trims at the first NUL in this binary
					// packet, so use the syscall's byte-buffer interface.
					_, _, optionErr = unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, unix.TCP_SAVED_SYN,
						uintptr(unsafe.Pointer(&saved[0])), uintptr(unsafe.Pointer(&length)), 0)
				})
				if err != nil || optionErr != 0 {
					t.Fatalf("read saved SYN: raw=%v option=%v", err, optionErr)
				}
				if length < 40 || int(saved[0]>>4) != version {
					t.Fatalf("saved SYN missing ipv%d header: length=%d bytes=%x", version, length, saved[:min(length, 40)])
				}
				index := 8
				if version == 6 {
					index = 7
				}
				want := byte(123)
				if opaque {
					want = 91
				}
				if saved[index] != want {
					t.Fatalf("ipv%d reliable=%t opaque=%t SYN TTL=%d want=%d", version, reliable, opaque, saved[index], want)
				}
				if got := providerEgressTestSocketTtl(t, f.socket, version); got != 123 {
					t.Fatalf("post-connect TTL=%d want123", got)
				}
			}
		}
	}
}
