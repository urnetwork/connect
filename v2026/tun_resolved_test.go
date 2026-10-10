// Explicitly resolved probe sockets retain ordinary tun family and race policy.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
)

// A supplied v6 answer cannot bypass an IPv4-only tun's route constraint.
func TestResolvedTunStreamPreservesFamilyPolicy(t *testing.T) {
	var calls atomic.Int32
	_, err := dialResolvedTunStream(context.Background(), "tcp", "sample.example:443", []netip.Addr{netip.MustParseAddr("2001:db8::1")}, false,
		func(context.Context, string, netip.AddrPort) (net.Conn, error) {
			calls.Add(1)
			return nil, errors.New("unexpected socket")
		})
	if !errors.Is(err, syscall.EAFNOSUPPORT) || calls.Load() != 0 {
		t.Fatalf("family policy bypass: calls=%d err=%v", calls.Load(), err)
	}
}

// Failed addresses fall through without resolving again or losing the hostname.
func TestResolvedTunStreamUsesSuppliedAnswers(t *testing.T) {
	first := netip.MustParseAddr("192.0.2.10")
	second := netip.MustParseAddr("192.0.2.11")
	var calls atomic.Int32
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn, err := dialResolvedTunStream(context.Background(), "tcp4", "sample.example:443", []netip.Addr{first, second}, false,
		func(_ context.Context, host string, endpoint netip.AddrPort) (net.Conn, error) {
			calls.Add(1)
			if host != "sample.example" || endpoint.Port() != 443 {
				return nil, errors.New("lost original target")
			}
			if endpoint.Addr() == first {
				return nil, errors.New("synthetic first-path failure")
			}
			if endpoint.Addr() != second {
				return nil, errors.New("unexpected answer")
			}
			return left, nil
		})
	if err != nil || conn != left || calls.Load() != 2 {
		t.Fatalf("resolved race failed: calls=%d err=%v", calls.Load(), err)
	}
}
