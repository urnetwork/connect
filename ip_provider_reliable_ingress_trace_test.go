package connect

import (
	"fmt"
	"hash/crc64"
	"testing"
	"testing/synctest"
)

func TestProviderReliableTcpRejectionTraceNilAllocation(t *testing.T) {
	settings := DefaultReceiveBufferSettings()
	owner := &providerReliablePacket{peer: Peer{delivery: &receiveDeliveryReceipt{
		queue: &receiveDeliveryQueue{sequence: &ReceiveSequence{receiveBufferSettings: settings}},
	}}}
	if allocations := testing.AllocsPerRun(100, func() { owner.traceFinalRejection(nil, errReliableIngressNoFlow) }); allocations != 0 {
		t.Fatalf("disabled rejection trace allocated %g objects", allocations)
	}
}

func TestProviderReliableTcpRejectionTraceIdentity(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, flags := range []byte{tcpFlagAck, tcpFlagFin | tcpFlagAck} {
			t.Run(fmt.Sprintf("ipv%d/flags-%x", version, flags), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					observer, observations := progressTraceTestObserver()
					sequence := f.runningReceiveSequence(func(settings *ReceiveBufferSettings) { settings.ProgressObserver = observer })
					path := *f.path
					path.SourcePort++
					id := NewId()
					payload := []byte("synthetic refused bytes are never captured")
					packet := ipOosTcpPacketSequence(&path, flags, 101, payload)
					checksum := crc64.Checksum(packet, transferProgressChecksumTable())
					reliableIngressPackForPath(t, f, sequence, &path, 0, id, flags, payload)
					synctest.Wait()
					events := h1TraceStage(takeProgressTraceEvents(observations), "provider_tcp_rejected")
					if !ackLineageTraceEnabled {
						if len(events) != 0 {
							t.Fatal("ordinary build emitted a diagnostic rejection event")
						}
						return
					}
					if len(events) != 1 {
						t.Fatalf("final refusal events=%d, want exactly one", len(events))
					}
					event := events[0]
					if event.ClientId != f.client.ClientId() || event.PeerId != f.source.SourceId ||
						event.SequenceId != sequence.sequenceId || event.MessageId != id || event.SequenceNumber != 0 ||
						event.ByteCount != len(packet) || event.WireHash != checksum || event.ErrorKind != "no_flow" ||
						event.Outcome != fmt.Sprintf("ip=%d flags=%02x payload=%d", version, flags, len(payload)) ||
						event.Success || event.AtUnixNano == 0 {
						t.Fatalf("exact raw-packet/receipt metadata mismatch: %+v", event)
					}
					if present, _ := reliableIngressCumulativeHead(sequence); present {
						t.Fatal("observation changed rejected payload into a successful ACK")
					}
				})
			})
		}
	}
}
