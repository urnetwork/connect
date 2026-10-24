// The pump capability must preserve the real sender's ownership, recovery,
// generic budget and replacement rules beside the actual shared-reader root.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Real retained Transfer credit refuses the native pump immediately, while
// the very same mux's ordinary upstream waits and later admits its original.
func TestUpgradeMuxDohPumpNativeAdmissionKeepsGenericBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		mux := &UpgradeMux{mux: &IpMux{}}
		mux.SetUpstreamBatchClient(f.base.parent)
		packet := f.ownPackets(f.tcp)[0]
		started := time.Now()
		if mux.mux.getPumpUpstream()(f.base.source, protocol.ProvideMode_Network, packet, 5*time.Second) {
			t.Fatal("unavailable real Transfer credit admitted a native pump packet")
		}
		MessagePoolReturn(packet)
		if time.Since(started) != 0 || f.base.update.client.Load() != f.base.selected || f.base.selected.endErr != nil {
			t.Fatal("native pump refusal waited or reset its healthy selected owner")
		}
		packet = f.ownPackets(f.tcp)[0]
		done := make(chan bool, 1)
		f.workers.Add(1)
		go func() {
			defer f.workers.Done()
			success := mux.mux.getUpstream()(f.base.source, protocol.ProvideMode_Network, packet, 5*time.Second)
			if !success {
				MessagePoolReturn(packet)
			}
			done <- success
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("ordinary mux upstream inherited native readiness semantics")
		default:
		}
		f.releasePeerCredit()
		synctest.Wait()
		select {
		case success := <-done:
			if !success {
				t.Fatal("ordinary mux upstream lost the original after real credit release")
			}
		default:
			t.Fatal("ordinary mux upstream did not finish after real credit release")
		}
		f.finishPackets(f.tcp)
	})
}

// Generic replacement clears the native capability under the existing lock;
// reinstalling the known multi restores it. Snapshots are invoked outside it.
func TestUpgradeMuxDohPumpGenericReplacementKeepsCustomBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	mux := &UpgradeMux{mux: &IpMux{}}
	calls := 0
	custom := func(_ TransferPath, _ protocol.ProvideMode, _ []byte, timeout time.Duration) bool {
		calls++
		if timeout != 5*time.Second {
			t.Error("custom pump upstream lost its exact explicit budget")
		}
		// A callback may replace its route. Invocation while holding the
		// snapshot lock would deadlock this ordinary reentrant mutation.
		mux.SetUpstream(nil)
		return false
	}
	for _, replaceDirectly := range []bool{false, true} {
		mux.SetUpstreamBatchClient(parent)
		if replaceDirectly {
			mux.mux.SetUpstream(custom)
		} else {
			mux.SetUpstream(custom)
		}
		packet := MessagePoolCopy(groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil))
		if mux.mux.getPumpUpstream()(SourceId(NewId()), protocol.ProvideMode_Network, packet, 5*time.Second) {
			t.Fatal("refusing custom pump upstream transferred ownership")
		}
		MessagePoolReturn(packet)
		if mux.mux.getPumpUpstream() != nil {
			t.Fatal("generic route replacement retained an old native pump capability")
		}
	}
	if calls != 2 {
		t.Fatal("generic pump replacement still invoked the old multi")
	}
	mux.SetUpstreamBatchClient(parent)
	packet := MessagePoolCopy(groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil))
	if mux.mux.getPumpUpstream()(SourceId(NewId()), protocol.ProvideMode_Network, packet, 5*time.Second) {
		t.Fatal("empty known multi admitted an unavailable pump packet")
	}
	MessagePoolReturn(packet)
	if calls != 2 {
		t.Fatal("known multi reinstall kept the obsolete custom pump callback")
	}
}

// An unestablished encryption generation is readiness, preserving the caller
// owner and reliable ACK policy. Refusal must not commit TCP collapse state.
func TestUpgradeMuxDohPumpEncryptionReadinessKeepsRetryOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		parent.settings.TcpCollapsePrevention, parent.settings.TcpCollapseMaxHold = true, 0
		selected := newPacketTransferTestChannel()
		selected.ctx, selected.settings = parent.ctx, parent.settings
		update.client.Store(selected)
		ready, calls, resets := false, 0, 0
		parent.SetReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) { resets++ })
		selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			calls++
			if timeout != 0 || !ack {
				t.Error("native pump changed ready-only admission or reliable TCP ACK policy")
			}
			if !ready {
				return false, ErrEncryptionRequiredNotEstablished
			}
			for _, member := range group.packets {
				MessagePoolReturn(member.packet)
			}
			return true, nil
		}
		mux := &UpgradeMux{mux: &IpMux{}}
		mux.SetUpstreamBatchClient(parent)
		offer := func() bool {
			packets := [][]byte{MessagePoolCopy(groupRecoveryPacket(101, 500, 64, tcpFlagAck, []byte{0x42}))}
			witnesses := groupTestPacketWitnesses(t, packets)
			success := mux.mux.getPumpUpstream()(SourceId(NewId()), protocol.ProvideMode_Network, packets[0], 5*time.Second)
			if !success {
				MessagePoolReturn(packets[0])
			}
			requireGroupTestWitnessesReleased(t, packets, witnesses)
			return success
		}
		started := time.Now()
		if offer() || calls != 1 || resets != 0 || time.Since(started) != 0 || update.client.Load() != selected || selected.endErr != nil {
			t.Fatal("encryption readiness waited, changed custody or reset a healthy pump route")
		}
		ready = true
		if !offer() || calls != 2 || resets != 0 {
			t.Fatal("previous native pump refusal consumed retry or collapse authority")
		}
	})
}
