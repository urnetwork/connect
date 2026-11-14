//go:build linux && !android

// The Linux egress binding: the egress mark (SetEgressMark) as SO_MARK.
//
// The tag carries `!android` for the reason egress_resolver_linux.go's does: Go
// builds `_linux.go` files for GOOS=android too, and Android keeps its own
// self-exclusion (VpnService.protect), so it keeps egress_other.go's no-op.
package connect

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// applyEgressInterface marks a Linux socket with the egress mark. Its callers set
// it at creation, before connect() and the first send, which is where the
// kernel routes a socket and so the only point a mark steers it: a mark set on a
// socket the kernel already routed moves its later packets and keeps a source
// address taken from the tunnel (egress_resolver_linux.go's egressMarkControl
// says the same). A policy rule that sends every unmarked socket into a tunnel
// this process provides, `not fwmark <mark> lookup <table>` as ur tun installs
// it under exits, then passes the socket by, and it keeps the host's own route.
//
// The interface indexes are Windows' and macOS' binding and stay inert here, as
// they were before this file: SO_BINDTOIFINDEX would follow one interface, and
// needs CAP_NET_RAW besides (QD-800).
//
// Setting the mark needs CAP_NET_ADMIN, or CAP_NET_RAW on a kernel that admits
// it. A socket that cannot carry the mark would follow the tunnel's route back
// into this process, so the error travels back through egressControl to the
// dial, which fails and retries, as egress_windows.go's pin does.
func applyEgressInterface(fd uintptr, _ uint32, _ uint32, mark uint32) error {
	if mark == 0 {
		return nil
	}
	if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark)); err != nil {
		return fmt.Errorf(
			"connect: could not mark the socket with the egress mark %#x: %w; the socket would follow the tunnel's route",
			mark,
			err,
		)
	}
	return nil
}
