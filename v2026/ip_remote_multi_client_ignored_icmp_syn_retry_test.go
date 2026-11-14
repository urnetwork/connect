// Ignored provider packets cannot authorize retained syn dispatch. These
// controls leave the production dispatcher unheld through rejection and retry.
package connect

import (
	"testing"
	"testing/synctest"
)

// Each rejection class preserves the exact source owner until a genuine
// signal authorizes automatic continuation, without another source syn.
func TestDialFailureIgnoredIcmpPreservesUnheldSynRetryAuthority(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	cases := []reraceIcmpCase{
		{name: "unrelated flow", intercepted: 1, packet: func(t *testing.T, path *IpPath) []byte {
			unrelated := *path
			unrelated.SourcePort++
			return reraceProviderSignal(t, &unrelated)
		}},
		{name: "foreign outer address", packet: func(t *testing.T, path *IpPath) []byte {
			packet := reraceProviderSignal(t, path)
			packet[dialFailureIcmpOffset(packet)-1] ^= 0x55
			return resealDialFailureSignal(packet)
		}},
		{name: "malformed header", packet: func(t *testing.T, path *IpPath) []byte {
			packet := reraceProviderSignal(t, path)
			packet[0] = 0x55
			return packet
		}},
		{name: "other family", packet: reraceCrossFamilySignal},
		{name: "truncated embed", packet: func(t *testing.T, path *IpPath) []byte {
			packet := reraceProviderSignal(t, path)
			return packet[:len(packet)-1]
		}},
		{name: "other type", packet: func(t *testing.T, path *IpPath) []byte {
			icmpType := byte(11)
			if path.Version == 6 {
				icmpType = 3
			}
			return reraceIcmpRetype(reraceProviderSignal(t, path), icmpType, 0)
		}},
		{name: "other code", packet: func(t *testing.T, path *IpPath) []byte {
			packet := reraceProviderSignal(t, path)
			code := byte(0)
			if path.Version == 6 {
				code = 1
			}
			return reraceIcmpRetype(packet, packet[dialFailureIcmpOffset(packet)], code)
		}},
	}
	for _, version := range []int{4, 6} {
		for _, c := range cases {
			synctest.Test(t, func(t *testing.T) {
				harness := newReraceHarness(t, []reraceExitSpec{
					{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
					{name: "b", tier: 1},
				}, nil)
				path := reraceFlow(version, 1, 48001)
				if !harness.syn(path) || harness.boundTo(path) != harness.channel("a") {
					t.Fatalf("ipv%d %s: initial SYN did not bind to A", version, c.name)
				}
				update := harness.update(path)
				owner := synRetryControlOwner(t, update)
				harness.inject("a", c.packet(t, path))
				if harness.update(path) != update || harness.boundTo(path) != harness.channel("a") ||
					update.receivedInbound.Load() || update.sourceReset.Load() || synRetryControlOwner(t, update) != owner {
					t.Fatalf("ipv%d %s: ignored packet changed flow or retained SYN authority", version, c.name)
				}
				metrics := harness.metrics()
				if metrics.FlowsReraced != 0 || metrics.DialFailuresIntercepted != uint64(c.intercepted) ||
					harness.channel("a").dialFailureCount() != 0 || len(harness.exit("b").syns(path)) != 0 || len(harness.delivered(path)) != 0 {
					t.Fatalf("ipv%d %s: ignored packet authorized a retry or delivery", version, c.name)
				}

				harness.inject("a", reraceProviderSignal(t, path))
				if harness.update(path) != update || harness.boundTo(path) != harness.channel("b") {
					t.Fatalf("ipv%d %s: genuine signal did not continue the existing flow on B", version, c.name)
				}
				if _, ok := harness.synAck(path); !ok || len(harness.sentSyns(path)) != 1 || len(harness.exit("b").syns(path)) != 1 {
					t.Fatalf("ipv%d %s: continuation needed another source SYN or duplicated B's SYN", version, c.name)
				}
				metrics = harness.metrics()
				if metrics.FlowsReraced != 1 || metrics.DialFailuresIntercepted != uint64(c.intercepted+1) ||
					harness.channel("a").dialFailureCount() != 1 || harness.channel("b").dialFailureCount() != 0 {
					t.Fatalf("ipv%d %s: genuine signal did not authorize exactly one retry", version, c.name)
				}
				synRetryControlReleased(t, owner)
			})
		}
	}
}
