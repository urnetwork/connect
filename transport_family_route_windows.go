//go:build windows

package connect

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Reports whether a socket call failed because the routing has no route to
// the destination. Windows answers with its own socket errors
// (WSAEHOSTUNREACH, WSAENETUNREACH), which the errno values Go invents for
// EHOSTUNREACH and ENETUNREACH on Windows do not match, so both forms count.
// See dialRouteRefused.
func isNoRouteErrno(errno syscall.Errno) bool {
	return errno == windows.WSAEHOSTUNREACH ||
		errno == windows.WSAENETUNREACH ||
		errno == syscall.EHOSTUNREACH ||
		errno == syscall.ENETUNREACH
}
