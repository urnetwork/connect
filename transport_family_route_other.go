//go:build !windows

package connect

import (
	"syscall"
)

// isNoRouteErrno reports whether a socket call failed because the routing has
// no route to the destination: to the host (EHOSTUNREACH) or to its network
// (ENETUNREACH). See dialRouteRefused.
func isNoRouteErrno(errno syscall.Errno) bool {
	return errno == syscall.EHOSTUNREACH || errno == syscall.ENETUNREACH
}
