package connect

import "syscall"

// Darwin's interval/count constants match net/tcpsockopt_darwin.go; syscall
// does not expose them on every architecture supported by the Go toolchain.
func providerEgressTestKeepAliveOptions() [4]int {
	return [4]int{syscall.SO_KEEPALIVE, syscall.TCP_KEEPALIVE, 0x101, 0x102}
}
