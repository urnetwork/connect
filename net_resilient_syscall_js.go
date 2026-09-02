//go:build js

package connect

import (
	"errors"
	"net"
)

type SocketHandle = int

func duplicateSocketHandle(conn *net.TCPConn) (SocketHandle, func(), error) {
	return 0, nil, errors.ErrUnsupported
}

func GetSocketTtl(fd SocketHandle) int {
	// not supported
	return 0
}

// SetSocketTtl reports that the TTL cannot be set. There is no socket option
// surface under js/wasm, and returning nil would claim the low TTL was applied
// when nothing happened, so the reorder technique would look live when it is
// not. Nothing in the resilient path reaches here: GetSocketTtl above returns
// 0, and both reorder branches bail out to a single unmodified write on
// nativeTtl <= 0 before any SetSocketTtl call. The unsupported error is for any
// other caller.
func SetSocketTtl(fd SocketHandle, ttl int) error {
	// not supported
	return errors.ErrUnsupported
}
