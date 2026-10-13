package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func dialFailureTestPath(protocol IpProtocol, version int) *IpPath {
	if protocol == IpProtocolTcp {
		// carries Protocol=tcp plus a non-zero sequence number, so the round
		// trip exercises the tcp embed the same way the provider does.
		return icmpTcpTestPath(version)
	}
	return udpTestPath(version)
}

// dialTimeoutError is the error a dialer returns when its timeout expires:
// "dial tcp ...: i/o timeout". The dial runs on a context whose deadline has
// passed, so it fails before any socket is opened.
func dialTimeoutError(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:9")
	if conn != nil {
		conn.Close()
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("a dial past its deadline returned %v, want a timeout", err)
	}
	return err
}

// classifyDialFailure is the single home of the dial-failure errno logic: both
// ip.go dial sites and this test call it, so the test never restates the
// classification. A dial that timed out gets nothing, on tcp and udp: its
// signal would land 15s late. ECONNREFUSED on tcp is the one case that becomes
// a RST (the destination refused); every other immediate failure -- and all of
// udp, ECONNREFUSED included -- is capacity-class and gets an icmp unreachable,
// with unrecognized errors defaulting to capacity deliberately.
func TestClassifyDialFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// wantTcp is the action for a tcp path; udp is unreachable for every
		// failure that is not a timeout.
		wantTcp dialFailureAction
	}{
		{"econnrefused", syscall.ECONNREFUSED, dialFailureRst},
		// real net dials wrap the errno (*net.OpError -> *os.SyscallError ->
		// syscall.Errno); errors.Is must still see through it.
		{"econnrefused wrapped", fmt.Errorf("dial tcp 93.184.216.34:443: connect: %w", syscall.ECONNREFUSED), dialFailureRst},
		{"enetunreach", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}, dialFailureUnreachable},
		{"ehostunreach", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, dialFailureUnreachable},
		{"emfile", syscall.EMFILE, dialFailureUnreachable},
		{"eaddrnotavail", syscall.EADDRNOTAVAIL, dialFailureUnreachable},
		{"generic", errors.New("some non-syscall dial failure"), dialFailureUnreachable},
		// timeouts: the dialer's own, its socket deadline, the kernel's
		{"dial timeout", dialTimeoutError(t), dialFailureNone},
		{"context deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), dialFailureNone},
		{"socket deadline", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, dialFailureNone},
		{"etimedout", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ETIMEDOUT)}, dialFailureNone},
	}

	for _, protocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
		for _, version := range []int{4, 6} {
			for _, c := range cases {
				label := fmt.Sprintf("%s/v%d/%s", protocol, version, c.name)

				ipPath := dialFailureTestPath(protocol, version)

				want := c.wantTcp
				if protocol == IpProtocolUdp && want == dialFailureRst {
					// udp "dial" never yields a meaningful refusal at connect
					// time, so ECONNREFUSED is capacity-class there.
					want = dialFailureUnreachable
				}

				action, packet := classifyDialFailure(ipPath, c.err)
				if action != want {
					t.Errorf("%s: action = %d, want %d", label, action, want)
					continue
				}

				switch action {
				case dialFailureUnreachable:
					if packet == nil {
						t.Errorf("%s: unreachable action returned no packet", label)
						continue
					}
					// the built signal must round-trip back to the original
					// egress tuple, or the client intercept cannot match the
					// flow it refers to.
					parsed, ok := ipParseIcmpUnreachable(packet)
					if !ok {
						t.Errorf("%s: built unreachable did not parse back", label)
						continue
					}
					if parsed.Protocol != protocol {
						t.Errorf("%s: round-trip protocol = %v, want %v", label, parsed.Protocol, protocol)
					}
					var sameKey bool
					switch version {
					case 4:
						sameKey = parsed.ToIp4Path() == ipPath.ToIp4Path()
					case 6:
						sameKey = parsed.ToIp6Path() == ipPath.ToIp6Path()
					}
					if !sameKey {
						t.Errorf("%s: round-trip flow-map key mismatch: got %s->%s want %s->%s",
							label,
							parsed.SourceHostPort(), parsed.DestinationHostPort(),
							ipPath.SourceHostPort(), ipPath.DestinationHostPort())
					}
				case dialFailureRst, dialFailureNone:
					if packet != nil {
						t.Errorf("%s: action %d must not carry a packet, got %d bytes", label, action, len(packet))
					}
				default:
					t.Errorf("%s: unexpected action %d", label, action)
				}
			}
		}
	}
}

// A nil path and an unsupported protocol both classify to none with no packet,
// so the dial sites emit nothing rather than panicking on a malformed flow.
func TestClassifyDialFailureNoSignal(t *testing.T) {
	if action, packet := classifyDialFailure(nil, syscall.ECONNREFUSED); action != dialFailureNone || packet != nil {
		t.Errorf("nil path: action = %d, packet = %v, want none/nil", action, packet)
	}
	for _, ipVersion := range testIpVersions {
		unknown := &IpPath{Version: ipVersion, Protocol: IpProtocolUnknown}
		if action, packet := classifyDialFailure(unknown, errors.New("x")); action != dialFailureNone || packet != nil {
			t.Errorf("v%d unknown protocol: action = %d, packet = %v, want none/nil", ipVersion, action, packet)
		}
	}
}
