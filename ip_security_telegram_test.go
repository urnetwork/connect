package connect

import (
	"context"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func telegramTestIp(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func TestTelegramCallReflectorRanges(t *testing.T) {
	addressCount := uint64(0)
	var previousHi uint32
	detector := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	for i, r := range telegramCallReflectorIpv4Ranges {
		if r.hi < r.lo {
			t.Fatalf("range %d is inverted: %08x-%08x", i, r.lo, r.hi)
		}
		if 0 < i && uint64(r.lo) <= uint64(previousHi)+1 {
			t.Fatalf("range %d overlaps, is unsorted, or should be merged", i)
		}
		for endpoint := r.lo; endpoint <= r.hi; endpoint++ {
			// The reflector snapshot and reputation feeds update independently.
			// Matching the exception never overrides an IP reputation drop.
			want, wantReason := cfaaAllow, SecurityPolicyReasonCfaaAllow
			if cfaaBlockedIp4(endpoint) {
				want, wantReason = cfaaDrop, SecurityPolicyReasonCfaaDropIp
			}
			for port := 596; port <= 599; port++ {
				for _, transport := range []IpProtocol{IpProtocolUdp, IpProtocolTcp} {
					if !isTelegramCallReflector(telegramTestIp(endpoint), port, transport, 4) {
						t.Errorf("published endpoint %s:%d/%s did not match", telegramTestIp(endpoint), port, transport)
					}
					if got, reason := detector.inspectReason(telegramTestIp(endpoint), port, transport, 4); got != want || reason != wantReason {
						t.Errorf("snapshot endpoint %s:%d/%s = %s/%v, want %s/%v", telegramTestIp(endpoint), port, transport, cfaaVerdictName(got), reason, cfaaVerdictName(want), wantReason)
					}
				}
			}
		}
		addressCount += uint64(r.hi-r.lo) + 1
		previousHi = r.hi
	}
	if addressCount != 154 {
		t.Fatalf("reflector ranges contain %d addresses, want published snapshot count 154", addressCount)
	}
}

func TestTelegramCallReflectorRejectsNearMisses(t *testing.T) {
	tests := []struct {
		name      string
		ip        string
		port      int
		protocol  IpProtocol
		ipVersion int
	}{
		{"hole 9.11", "91.108.9.11", 596, IpProtocolUdp, 4},
		{"hole 9.18", "91.108.9.18", 597, IpProtocolTcp, 4},
		{"v12 fallback wrong port", "91.108.9.38", 598, IpProtocolTcp, 4},
		{"v12 fallback wrong transport", "91.108.9.38", 595, IpProtocolUdp, 4},
		{"before 13 range", "91.108.13.1", 599, IpProtocolTcp, 4},
		{"after 17 range", "91.108.17.59", 596, IpProtocolUdp, 4},
		{"broader telegram address", "149.154.167.51", 596, IpProtocolUdp, 4},
		{"port below", "91.108.13.2", 595, IpProtocolUdp, 4},
		{"port above", "91.108.13.2", 600, IpProtocolTcp, 4},
		{"wrong protocol", "91.108.13.2", 596, IpProtocolIcmp, 4},
		{"wrong ip version", "91.108.13.2", 596, IpProtocolUdp, 6},
		{"ipv6", "2001:db8::1", 596, IpProtocolUdp, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isTelegramCallReflector(net.ParseIP(tt.ip), tt.port, tt.protocol, tt.ipVersion) {
				t.Fatal("near miss matched Telegram call exception")
			}
		})
	}
}

func TestTelegramCallV12TcpFallback(t *testing.T) {
	ip := net.ParseIP("91.108.9.38")
	if !isTelegramCallReflector(ip, 595, IpProtocolTcp, 4) {
		t.Fatal("official protocol-v12 TCP fallback did not match")
	}
	want, wantReason := cfaaAllow, SecurityPolicyReasonCfaaAllow
	if cfaaBlockedIp4(telegramCallV12TcpFallbackIpv4) {
		want, wantReason = cfaaDrop, SecurityPolicyReasonCfaaDropIp
	}
	detector := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	if got, reason := detector.inspectReason(ip, 595, IpProtocolTcp, 4); got != want || reason != wantReason {
		t.Fatalf("protocol-v12 TCP fallback = %s/%v, want %s/%v", cfaaVerdictName(got), reason, cfaaVerdictName(want), wantReason)
	}
}

func TestCfaaTelegramCallException(t *testing.T) {
	for _, allowCalls := range []bool{true, false} {
		settings := DefaultCfaaSecurityPolicySettings()
		settings.AllowTelegramCalls = allowCalls
		detector := newCfaaDetector(settings)
		for _, addressRange := range telegramCallReflectorIpv4Ranges {
			for endpoint := addressRange.lo; endpoint <= addressRange.hi; endpoint++ {
				want, wantReason := cfaaAllow, SecurityPolicyReasonCfaaAllow
				if cfaaBlockedIp4(endpoint) {
					want, wantReason = cfaaDrop, SecurityPolicyReasonCfaaDropIp
				} else if !allowCalls {
					want, wantReason = cfaaDrop, SecurityPolicyReasonCfaaDropPort
				}
				for port := 596; port <= 599; port++ {
					for _, transport := range []IpProtocol{IpProtocolUdp, IpProtocolTcp} {
						if got, reason := detector.inspectReason(telegramTestIp(endpoint), port, transport, 4); got != want || reason != wantReason {
							t.Errorf("calls=%t Telegram %s:%d/%s = %s/%v, want %s/%v", allowCalls, telegramTestIp(endpoint), port, transport, cfaaVerdictName(got), reason, cfaaVerdictName(want), wantReason)
						}
					}
				}
			}
		}
		want, wantReason := cfaaAllow, SecurityPolicyReasonCfaaAllow
		if cfaaBlockedIp4(telegramCallV12TcpFallbackIpv4) {
			want, wantReason = cfaaDrop, SecurityPolicyReasonCfaaDropIp
		} else if !allowCalls {
			want, wantReason = cfaaDrop, SecurityPolicyReasonCfaaDropPort
		}
		if got, reason := detector.inspectReason(telegramTestIp(telegramCallV12TcpFallbackIpv4), 595, IpProtocolTcp, 4); got != want || reason != wantReason {
			t.Errorf("calls=%t protocol-v12 fallback = %s/%v, want %s/%v", allowCalls, cfaaVerdictName(got), reason, cfaaVerdictName(want), wantReason)
		}
	}

	detector := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	if got := detector.inspect(net.ParseIP("91.108.13.1"), 596, IpProtocolUdp, 4); got != cfaaDrop {
		t.Fatalf("non-reflector privileged endpoint = %s, want drop", cfaaVerdictName(got))
	}
}

func TestSecurityPolicyAllowsTelegramCallReflectors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := DefaultSecurityPolicy(ctx)

	for _, addressRange := range telegramCallReflectorIpv4Ranges {
		for endpoint := addressRange.lo; endpoint <= addressRange.hi; endpoint++ {
			want := SecurityPolicyResultAllow
			if cfaaBlockedIp4(endpoint) {
				want = SecurityPolicyResultDrop
			}
			for port := 596; port <= 599; port++ {
				for _, transport := range []IpProtocol{IpProtocolUdp, IpProtocolTcp} {
					egress := dmcaPath(transport, 41000, port, transport == IpProtocolTcp)
					egress.DestinationIp = telegramTestIp(endpoint)
					result, err := policy.InspectEgress(protocol.ProvideMode_Public, egress, encryptedPayload(512))
					if err != nil || result != want {
						t.Fatalf("Telegram %s:%d egress over %s = (%v, %v), want %v", egress.DestinationIp, port, transport, result, err, want)
					}
					result, err = policy.InspectIngress(protocol.ProvideMode_Public, egress.Reverse(), encryptedPayload(512))
					if err != nil || result != want {
						t.Fatalf("Telegram %s:%d ingress over %s = (%v, %v), want %v", egress.DestinationIp, port, transport, result, err, want)
					}
				}
			}
		}
	}

	fallback := dmcaPath(IpProtocolTcp, 41002, 595, true)
	fallback.DestinationIp = net.ParseIP("91.108.9.38")
	result, err := policy.InspectEgress(protocol.ProvideMode_Public, fallback, encryptedPayload(512))
	want := SecurityPolicyResultAllow
	if cfaaBlockedIp4(telegramCallV12TcpFallbackIpv4) {
		want = SecurityPolicyResultDrop
	}
	if err != nil || result != want {
		t.Fatalf("Telegram protocol-v12 fallback egress = (%v, %v), want %v", result, err, want)
	}
	result, err = policy.InspectIngress(protocol.ProvideMode_Public, fallback.Reverse(), encryptedPayload(512))
	if err != nil || result != want {
		t.Fatalf("Telegram protocol-v12 fallback ingress = (%v, %v), want %v", result, err, want)
	}
}

func TestSecurityPolicyCanDisableTelegramCallException(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfaa := DefaultCfaaSecurityPolicySettings()
	cfaa.AllowTelegramCalls = false
	policy := NewSecurityPolicy(
		ctx,
		cfaa,
		DefaultDmcaSecurityPolicySettings(),
		DefaultWebStandardSettings(),
		DefaultSecurityPolicyStatsCollector(),
	)
	for _, addressRange := range telegramCallReflectorIpv4Ranges {
		for endpoint := addressRange.lo; endpoint <= addressRange.hi; endpoint++ {
			for port := 596; port <= 599; port++ {
				for _, transport := range []IpProtocol{IpProtocolUdp, IpProtocolTcp} {
					path := dmcaPath(transport, 41001, port, transport == IpProtocolTcp)
					path.DestinationIp = telegramTestIp(endpoint)
					result, err := policy.InspectEgress(protocol.ProvideMode_Public, path, encryptedPayload(512))
					if err != nil || result != SecurityPolicyResultDrop {
						t.Fatalf("disabled Telegram %s:%d/%s = (%v, %v), want drop", path.DestinationIp, port, transport, result, err)
					}
				}
			}
		}
	}
}
