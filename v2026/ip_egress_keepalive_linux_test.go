package connect

import "syscall"

// Linux exposes all four options in the standard syscall package.
func providerEgressTestKeepAliveOptions() [4]int {
	return [4]int{syscall.SO_KEEPALIVE, syscall.TCP_KEEPIDLE, syscall.TCP_KEEPINTVL, syscall.TCP_KEEPCNT}
}
