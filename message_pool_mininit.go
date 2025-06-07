//go:build mininit
// +build mininit

package connect

import (
	"io"
)

func MessagePoolReadAllWithTag(r io.Reader, tag uint8) ([]byte, error) {
	return io.ReadAll(r)
}

func MessagePoolGetDetailedWithTag(n int, tag uint8) ([]byte, bool) {
	b := make([]byte, n)
	return b, false
}

func MessagePoolReturn(message []byte) bool {
	return false
}

func MessagePoolShareReadOnly(message []byte) []byte {
	return message
}

func MessagePoolCheck(message []byte) (pooled bool, shared bool) {
	return false, false
}
