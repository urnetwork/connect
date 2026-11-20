//go:build windows

package connect

import (
	"net"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows answers a dial with no route with its own socket errors, which Go's
// errno values for EHOSTUNREACH and ENETUNREACH do not match.
func TestDialRouteRefusedWindowsSocketErrors(t *testing.T) {
	for _, c := range []struct {
		errno   syscall.Errno
		refused bool
	}{
		{errno: windows.WSAEHOSTUNREACH, refused: true},
		{errno: windows.WSAENETUNREACH, refused: true},
		{errno: windows.WSAECONNREFUSED, refused: false},
		{errno: windows.WSAETIMEDOUT, refused: false},
	} {
		err := &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("connectex", c.errno)}
		if refused := dialRouteRefused(err); refused != c.refused {
			t.Errorf("%d: dialRouteRefused = %t, want %t", c.errno, refused, c.refused)
		}
	}
}
