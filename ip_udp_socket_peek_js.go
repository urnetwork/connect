//go:build js

package connect

import "errors"

const udpSocketPeekRequiresWait = false

// Browser WASM has no raw UDP socket or MSG_PEEK support.
func peekUdpSocket(fd SocketHandle, buffer []byte) (int, error) {
	return 0, errors.ErrUnsupported
}
