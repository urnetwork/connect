//go:build windows

package connect

import (
	"syscall"
)

type SocketHandle = syscall.Handle
