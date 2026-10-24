// The real batch entry must follow a generic route replacement instead of
// keeping the prior multi-client's exact-flow callback alive.
package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Both possible senders consume exact pooled owners, so the causal assertion
// reports route identity without leaking or over-returning on OLD or FIX.
func TestIpMuxGenericReplacementClearsOldGroupRoute(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	selected := newPacketTransferTestChannel()
	selected.ctx, selected.settings = parent.ctx, parent.settings
	update.client.Store(selected)
	oldCalls, customCalls := 0, 0
	selected.sendGroupForTest = func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
		oldCalls++
		for _, member := range group.packets {
			MessagePoolReturn(member.packet)
		}
		return true, nil
	}
	mux := &UpgradeMux{mux: &IpMux{}}
	mux.SetUpstreamBatchClient(parent)
	mux.SetUpstream(func(_ TransferPath, _ protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
		customCalls++
		if timeout != 5*time.Second {
			t.Error("generic batch replacement changed the explicit budget")
		}
		MessagePoolReturn(packet)
		return true
	})
	packets := [][]byte{MessagePoolCopy(groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil))}
	witnesses := groupTestPacketWitnesses(t, packets)
	count := mux.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 5*time.Second)
	requireGroupTestWitnessesReleased(t, packets, witnesses)
	if count != 1 || customCalls != 1 || oldCalls != 0 {
		t.Fatalf("IP_MUX_GENERIC_REPLACEMENT_USED_STALE_GROUP_ROUTE: count=%d custom=%d old_multi=%d", count, customCalls, oldCalls)
	}
}

// Known multi reinstall restores both optional capabilities; generic direct
// replacement clears them while exact owners keep their original ACK policy.
func TestIpMuxKnownMultiReinstallPublishesAllSenders(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	selected := newPacketTransferTestChannel()
	selected.ctx, selected.settings = parent.ctx, parent.settings
	update.client.Store(selected)
	var budgets []time.Duration
	selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
		budgets = append(budgets, timeout)
		if !ack {
			t.Error("route publication changed reliable TCP ACK policy")
		}
		for _, member := range group.packets {
			MessagePoolReturn(member.packet)
		}
		return true, nil
	}
	mux := &UpgradeMux{mux: &IpMux{}}
	customCalls := 0
	custom := func(_ TransferPath, _ protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
		customCalls++
		if timeout != 5*time.Second {
			t.Error("replacement custom route lost its explicit budget")
		}
		MessagePoolReturn(packet)
		return true
	}
	batch := func() {
		packets := [][]byte{MessagePoolCopy(groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil))}
		witnesses := groupTestPacketWitnesses(t, packets)
		count := mux.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 5*time.Second)
		requireGroupTestWitnessesReleased(t, packets, witnesses)
		if count != 1 {
			t.Fatal("replacement batch did not consume its exact original")
		}
	}
	pump := func() {
		packets := [][]byte{MessagePoolCopy(groupRecoveryPacket(101, 0, 64, tcpFlagSyn, nil))}
		witnesses := groupTestPacketWitnesses(t, packets)
		success := mux.mux.getPumpUpstream()(SourceId(NewId()), protocol.ProvideMode_Network, packets[0], 5*time.Second)
		if !success {
			MessagePoolReturn(packets[0])
		}
		requireGroupTestWitnessesReleased(t, packets, witnesses)
		if !success {
			t.Fatal("replacement pump did not admit its exact original")
		}
	}
	mux.SetUpstream(custom)
	mux.SetUpstreamBatchClient(parent)
	batch()
	pump()
	if customCalls != 0 || len(budgets) != 2 || budgets[0] != 5*time.Second || budgets[1] != 0 {
		t.Fatalf("known multi publication mixed route capabilities: custom=%d budgets=%v", customCalls, budgets)
	}
	// This lower-level setter also owns a complete IpMux route replacement.
	mux.mux.SetUpstream(custom)
	batch()
	pump()
	if customCalls != 2 || len(budgets) != 2 {
		t.Fatalf("generic replacement retained old capabilities: custom=%d old_calls=%d", customCalls, len(budgets))
	}
}
