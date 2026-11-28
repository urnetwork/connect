// Exact local replay supersession remains separate from shutdown, unrelated
// flow changes and partially written Transfer groups.
package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Each owner veto stays strict. The latest native preparation can invalidate
// a local replay even when the native group also carries application bytes.
func TestNativeAckRetrySupersessionRequiresLatestNativeOfferOnLiveOwners(t *testing.T) {
	for _, name := range []string{
		"two pure feedback groups", "mixed native group", "source canceled", "flow canceled",
		"selected client canceled", "source reset", "selection replaced", "source replaced",
		"epoch changed", "inbound evidence lost", "final native serial", "remote close after native offer",
		"remote close before newer native offer",
	} {
		f := newNativeAckRetryOwnerFixture(t, 4)
		group := f.prepare(t, f.packet)
		attempt := *group.nativeAckRetry
		attempt.replay = true
		if err := attempt.replayDisposition(f.source); err != nil {
			t.Fatalf("%s: unchanged original replay refused: %v", name, err)
		}
		source := f.source
		update := f.update
		func() {
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			group.prepareNativeAckRetryWithLock(update)
			// A homogeneous group also supersedes the retained singleton.
			group.packets = append(group.packets, group.packets[0])
			group.prepareNativeAckRetryWithLock(update)
			switch name {
			case "source canceled":
				f.cancelSource()
			case "flow canceled":
				update.cancel()
			case "selected client canceled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				attempt.selection.client.ctx = ctx
			case "source reset":
				update.sourceReset.Store(true)
			case "selection replaced":
				update.client.Store(&multiClientChannel{ctx: context.Background()})
			case "source replaced":
				source = &SendSequence{ctx: context.Background()}
			case "epoch changed":
				update.sequenceAdmissionEpoch++
			case "inbound evidence lost":
				update.receivedInbound.Store(false)
			case "final native serial":
				update.nativeAckSerial = ^uint64(0)
			case "mixed native group":
				packet := reraceTcpPacket(group.ipPath, tcpFlagAck, 100, 502, []byte("data"))
				path, payload, err := ParseIpPathWithPayload(packet)
				if err != nil {
					t.Fatal(err)
				}
				mixed := parsedPacketGroup{packets: []parsedPacket{group.packets[0], {packet: packet, ipPath: path, payload: payload}}}
				mixed.prepareNativeAckRetryWithLock(update)
			case "remote close after native offer":
				update.observeTcpControlWithLock(tcpControlObservation{valid: true, fin: true}, true)
			case "remote close before newer native offer":
				update.observeTcpControlWithLock(tcpControlObservation{valid: true, fin: true}, true)
				group.prepareNativeAckRetryWithLock(update)
			}
		}()
		err := attempt.replayDisposition(source)
		if err == nil || !errors.Is(err, errNativeAckRetrySuperseded) || !sendPackFailedUnwritten(err) ||
			IsSendPackNativeAckRetrySuperseded(err) != (name == "two pure feedback groups" || name == "mixed native group") {
			t.Errorf("%s: changed original veto or assigned unsupported proof: %v", name, err)
		}
	}
}

// A string, generic unwritten marker, wrapped copy or partially successful
// group cannot establish the exact singleton producer's whole-Pack evidence.
func TestSendPackNativeAckRetrySupersessionRejectsAmbiguousErrors(t *testing.T) {
	for _, err := range []error{
		nil, context.Canceled, errNativeAckRetrySuperseded,
		errors.New(errNativeAckRetrySuperseded.Error()),
		newUnwrittenSendPackError(errNativeAckRetrySuperseded),
		newUnwrittenSendPackError(errNativeSynOpeningSuperseded),
		newUnwrittenSendPackError(context.Canceled),
		errors.Join(errSendPackNativeAckRetrySuperseded),
		errors.Join(errSendPackNativeAckRetrySuperseded, context.Canceled),
		fmt.Errorf("wrapped: %w", errSendPackNativeAckRetrySuperseded),
	} {
		if IsSendPackNativeAckRetrySuperseded(err) {
			t.Errorf("ambiguous replay disposition qualified: %T %v", err, err)
		}
	}
}

// A native DATA offer can supersede a local control replay. Its own refusal
// at the full Transfer boundary still has both original admission errors.
func TestNativeDataRefusalCannotInheritAckReplaySupersession(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		refusals := make(chan SendPackLifecycleObservation, 4)
		f := newNativeBatchCreditFixture(t, func(settings *ClientSettings) {
			previous := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				previous(event)
				if event.MessageType == protocol.MessageType_IpIpPacketToProvider && event.Err != nil {
					select {
					case refusals <- event:
					default:
						t.Error("unexpected extra native DATA refusal")
					}
				}
			}
		})
		accepted := []bool{true}
		if count := f.base.parent.SendNativePacketBatchWithResults(f.base.source, protocol.ProvideMode_Network,
			f.ownPackets(f.tcp), 5*time.Second, accepted); count != 0 || accepted[0] {
			t.Fatalf("full DATA admission returned %d %v", count, accepted)
		}
		if len(refusals) != 2 || f.base.parent.NativeAdmissionRefusalCount() != 1 {
			t.Fatalf("DATA refusal lost its raw phases or public input count: phases=%d refusals=%d", len(refusals), f.base.parent.NativeAdmissionRefusalCount())
		}
		first, terminal := <-refusals, <-refusals
		if first.Phase != SendPackLifecyclePhaseFirstRouteWrite || terminal.Phase != SendPackLifecyclePhaseTerminal || first.Token != terminal.Token {
			t.Fatal("refused DATA lost the identity of its own original")
		}
		for _, event := range []SendPackLifecycleObservation{first, terminal} {
			var admission *SendPackAdmissionError
			if !errors.As(event.Err, &admission) || !errors.Is(event.Err, ErrSendPackNotAdmitted) ||
				admission.NativeReadyPureTcpAck || admission.RecoveredByOwner || IsSendPackNativeAckRetrySuperseded(event.Err) {
				t.Fatalf("original DATA refusal acquired unrelated ACK evidence: %v", event.Err)
			}
		}
		f.releasePeerCredit()
	})
}
