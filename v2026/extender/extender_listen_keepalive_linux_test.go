package extender

import "syscall"

// Linux exposes each TCP keepalive option in syscall.
func extenderTcpKeepAliveOptions() [3]int {
	return [3]int{syscall.TCP_KEEPIDLE, syscall.TCP_KEEPINTVL, syscall.TCP_KEEPCNT}
}
