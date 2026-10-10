// Provider egress TTL/hop-limit mirroring and its bounded, NAT-owned counters.
// Shadow mode reads the native socket value without changing it. Mirror mode
// applies the first packet's value before connect where the dialer permits it;
// an opaque host dial gets the post-connect subset. Application bytes and the
// client-facing emulated stack are untouched.
package connect

import (
	"context"
	"net"
	"sync/atomic"
	"syscall"
)

// Provider defaults mirror the client. Empty and unknown values retain native
// behavior for callers constructing settings themselves.
type EgressTtlMode string

const (
	EgressTtlModeNative EgressTtlMode = "native"
	EgressTtlModeShadow EgressTtlMode = "shadow"
	EgressTtlModeMirror EgressTtlMode = "mirror"
)

// Cumulative local diagnostics, never published as another client's protocol
// diagnostics. Flow counts describe first packets; socket counts can be larger
// when a dial retries. Reads are concurrent-safe, not an atomic multi-field view.
type EgressRealismStats struct {
	TcpFlowCount             uint64
	UdpFlowCount             uint64
	ShadowFlowCount          uint64
	MirrorFlowCount          uint64
	InvalidTtlFlowCount      uint64
	PreConnectSocketCount    uint64
	PostConnectSocketCount   uint64
	SocketUnavailableCount   uint64
	TtlMatchCount            uint64
	TtlMismatchCount         uint64
	TtlReadErrorCount        uint64
	TtlApplyCount            uint64
	TtlApplyErrorCount       uint64
	KeepAliveApplyErrorCount uint64
}

// Shared across the owning NAT's protocols, families, and dispatch shards.
type egressRealismCounters struct {
	tcpFlowCount             atomic.Uint64
	udpFlowCount             atomic.Uint64
	shadowFlowCount          atomic.Uint64
	mirrorFlowCount          atomic.Uint64
	invalidTtlFlowCount      atomic.Uint64
	preConnectSocketCount    atomic.Uint64
	postConnectSocketCount   atomic.Uint64
	socketUnavailableCount   atomic.Uint64
	ttlMatchCount            atomic.Uint64
	ttlMismatchCount         atomic.Uint64
	ttlReadErrorCount        atomic.Uint64
	ttlApplyCount            atomic.Uint64
	ttlApplyErrorCount       atomic.Uint64
	keepAliveApplyErrorCount atomic.Uint64
}

// Returns this NAT's bounded aggregate counters without retaining flow details.
func (self *LocalUserNat) EgressRealismStats() EgressRealismStats {
	c := &self.egressRealism
	return EgressRealismStats{
		TcpFlowCount:             c.tcpFlowCount.Load(),
		UdpFlowCount:             c.udpFlowCount.Load(),
		ShadowFlowCount:          c.shadowFlowCount.Load(),
		MirrorFlowCount:          c.mirrorFlowCount.Load(),
		InvalidTtlFlowCount:      c.invalidTtlFlowCount.Load(),
		PreConnectSocketCount:    c.preConnectSocketCount.Load(),
		PostConnectSocketCount:   c.postConnectSocketCount.Load(),
		SocketUnavailableCount:   c.socketUnavailableCount.Load(),
		TtlMatchCount:            c.ttlMatchCount.Load(),
		TtlMismatchCount:         c.ttlMismatchCount.Load(),
		TtlReadErrorCount:        c.ttlReadErrorCount.Load(),
		TtlApplyCount:            c.ttlApplyCount.Load(),
		TtlApplyErrorCount:       c.ttlApplyErrorCount.Load(),
		KeepAliveApplyErrorCount: c.keepAliveApplyErrorCount.Load(),
	}
}

// Immutable after the first SYN/datagram, before any socket worker starts.
type providerEgressTarget struct {
	mode     EgressTtlMode
	ttl      uint8
	counters *egressRealismCounters
	// Tests refuse the option on this flow alone, without changing global state.
	setTtlForTest func(uintptr, bool, int) error
}

// Zero cannot be applied as a TTL; other values are preserved, including low
// limits used by probes. Native mode has neither observations nor option calls.
func newProviderEgressTarget(mode EgressTtlMode, ttl uint8, protocol ipProtocolNumber, counters *egressRealismCounters) providerEgressTarget {
	if mode != EgressTtlModeShadow && mode != EgressTtlModeMirror {
		return providerEgressTarget{}
	}
	if counters != nil {
		if protocol == ipProtocolNumberTcp {
			counters.tcpFlowCount.Add(1)
		} else {
			counters.udpFlowCount.Add(1)
		}
		if mode == EgressTtlModeShadow {
			counters.shadowFlowCount.Add(1)
		} else {
			counters.mirrorFlowCount.Add(1)
		}
		if ttl == 0 {
			counters.invalidTtlFlowCount.Add(1)
		}
	}
	return providerEgressTarget{mode: mode, ttl: ttl, counters: counters}
}

// Per dial, so a fallback socket is observed too. Context-preserving host
// wrappers reach the pre-connect hook; merely having a custom dial is not proof
// that the hook was bypassed.
type providerEgressDial struct {
	target          *providerEgressTarget
	controlObserved atomic.Bool
}

type providerEgressDialKey struct{}

// Carries only provider flow state. Ordinary client-strategy dials have no
// target and retain their existing socket behavior.
func (self *providerEgressTarget) dialContext(ctx context.Context) (context.Context, *providerEgressDial) {
	if self.ttl == 0 || (self.mode != EgressTtlModeShadow && self.mode != EgressTtlModeMirror) {
		return ctx, nil
	}
	dial := &providerEgressDial{target: self}
	return context.WithValue(ctx, providerEgressDialKey{}, dial), dial
}

// Chains after the existing buffer and interface controls. Go ignores Control
// when ControlContext exists, so this callback explicitly retains the old one.
func providerEgressDialer(dialer *net.Dialer) *net.Dialer {
	previousContext := dialer.ControlContext
	dialer.ControlContext = func(ctx context.Context, network, address string, raw syscall.RawConn) error {
		if previousContext != nil {
			if err := previousContext(ctx, network, address, raw); err != nil {
				return err
			}
		} else if dialer.Control != nil {
			// Honor callers replacing Control on a returned NetDialer too.
			if err := dialer.Control(network, address, raw); err != nil {
				return err
			}
		}
		if dial, ok := ctx.Value(providerEgressDialKey{}).(*providerEgressDial); ok {
			dial.controlObserved.Store(true)
			if c := dial.target.counters; c != nil {
				c.preConnectSocketCount.Add(1)
			}
			dial.target.apply(raw, network == "tcp6" || network == "udp6")
		}
		return nil
	}
	return dialer
}

// The TCP SYN has already left on this path; UDP still has sent no datagram.
// Unsupported/opaque socket wrappers are counted and keep forwarding normally.
func (self *providerEgressDial) afterConnect(conn net.Conn) {
	if self == nil || self.controlObserved.Load() {
		return
	}
	c := self.target.counters
	if c != nil {
		c.postConnectSocketCount.Add(1)
	}
	socket, ok := conn.(syscall.Conn)
	if !ok {
		if c != nil {
			c.socketUnavailableCount.Add(1)
		}
		return
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		if c != nil {
			c.socketUnavailableCount.Add(1)
		}
		return
	}
	var ip net.IP
	switch addr := conn.LocalAddr().(type) {
	case *net.TCPAddr:
		ip = addr.IP
	case *net.UDPAddr:
		ip = addr.IP
	default:
		if c != nil {
			c.socketUnavailableCount.Add(1)
		}
		return
	}
	self.target.apply(raw, ip.To4() == nil)
}

// Observes the actual socket default before applying anything. Failed reads,
// writes, and raw-descriptor access are diagnostics, never dial failures.
func (self *providerEgressTarget) apply(raw syscall.RawConn, ipv6 bool) {
	c := self.counters
	if err := raw.Control(func(fd uintptr) {
		nativeTtl, err := providerSocketTtl(fd, ipv6)
		if c != nil {
			if err != nil || nativeTtl <= 0 {
				c.ttlReadErrorCount.Add(1)
			} else if nativeTtl == int(self.ttl) {
				c.ttlMatchCount.Add(1)
			} else {
				c.ttlMismatchCount.Add(1)
			}
		}
		if self.mode == EgressTtlModeMirror {
			setTtl := setProviderSocketTtl
			if self.setTtlForTest != nil {
				setTtl = self.setTtlForTest
			}
			err := setTtl(fd, ipv6, int(self.ttl))
			if c != nil {
				if err != nil {
					c.ttlApplyErrorCount.Add(1)
				} else {
					c.ttlApplyCount.Add(1)
				}
			}
		}
	}); err != nil && c != nil {
		c.socketUnavailableCount.Add(1)
	}
}

// An explicit provider override is independent of the control-plane recovery
// cadence. Nil preserves all existing settings, including custom host dials.
func configureProviderKeepAlive(conn net.Conn, config *net.KeepAliveConfig, counters *egressRealismCounters) {
	if config == nil {
		return
	}
	setter, ok := conn.(interface {
		SetKeepAliveConfig(net.KeepAliveConfig) error
	})
	if !ok {
		if counters != nil {
			counters.keepAliveApplyErrorCount.Add(1)
		}
		return
	}
	if err := setter.SetKeepAliveConfig(*config); err != nil && counters != nil {
		counters.keepAliveApplyErrorCount.Add(1)
	}
}
