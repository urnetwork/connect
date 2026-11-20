// The inner tun's explicit ipv4-only policy is independent of packet/link
// sizes and never changes another owner's default dual-stack behavior.
package connect

import (
	"syscall"
	"testing"
)

// Removing only the constructor's explicit policy check enables ipv6 on a
// disabled stack at the protocol minimum, reproducing the missing owner guard.
func TestTunExplicitIpv4OnlyPolicyIndependentOfMtu(t *testing.T) {
	for _, mtu := range []int{900, 1280, 1500} {
		for _, disableIpv6 := range []bool{false, true} {
			settings := DefaultTunSettings()
			settings.Log = NewNoopLogger()
			settings.Mtu, settings.DisableIpv6 = mtu, disableIpv6
			tun, err := CreateTun(t.Context(), settings)
			if err != nil {
				t.Fatal(err)
			}
			wantIpv6 := !disableIpv6 && tunIpv6MinimumMtu <= mtu
			if tun.Ipv6Enabled() != wantIpv6 || settings.Mtu != mtu {
				_ = tun.Close()
				t.Fatalf("mtu=%d disable_ipv6=%t: enabled=%t, want %t; settings mtu=%d", mtu, disableIpv6, tun.Ipv6Enabled(), wantIpv6, settings.Mtu)
			}
			addresses := tun.LocalAddresses()
			wantAddresses := 1
			if wantIpv6 {
				wantAddresses++
			}
			if len(addresses) != wantAddresses || !addresses[0].Is4() || wantIpv6 && !addresses[1].Is6() {
				_ = tun.Close()
				t.Fatalf("mtu=%d disable_ipv6=%t: unexpected local address families %v", mtu, disableIpv6, addresses)
			}
			if !wantIpv6 {
				for _, network := range []string{"tcp6", "udp6"} {
					conn, err := tun.DialContext(t.Context(), network, "[2001:db8::25]:443")
					if conn != nil {
						_ = conn.Close()
					}
					if conn != nil || err != syscall.EAFNOSUPPORT {
						_ = tun.Close()
						t.Fatalf("disabled ipv6 dial %s at mtu=%d: conn=%v error=%v", network, mtu, conn, err)
					}
				}
			}
			if err := tun.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
