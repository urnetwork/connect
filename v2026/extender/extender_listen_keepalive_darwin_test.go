package extender

import "syscall"

// Darwin exposes the idle constant; interval/count match net's Darwin options.
func extenderTcpKeepAliveOptions() [3]int {
	return [3]int{syscall.TCP_KEEPALIVE, 0x101, 0x102}
}
