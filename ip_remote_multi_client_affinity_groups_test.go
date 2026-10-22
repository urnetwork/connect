// Public native batching preserves ownership while separating TCP generations.
// These roots isolate grouping; the real tuple transaction has separate roots.
package connect

import (
	"slices"
	"testing"
	"time"
)

// Preserve package layering without importing this package's protocol child.
// This is the protocol's established network relationship value.
const affinityRenewalProvideModeNetwork = 1

func TestAffinityRenewalNativeGroupingPreservesOrdinaryCoalescing(t *testing.T) {
	for _, packets := range [][][]byte{
		{groupRecoveryPacket(100, 500, 64, tcpFlagAck, []byte{1}), groupRecoveryPacket(101, 500, 64, tcpFlagAck, []byte{2})},
		{groupRecoveryPacket(100, 0, 64, tcpFlagSyn, []byte{1}), groupRecoveryPacket(102, 500, 64, tcpFlagAck, []byte{2}), groupRecoveryPacket(100, 0, 64, tcpFlagSyn, []byte{1})},
		{ipOosUdpPacket(udpTestPath(4), []byte{1}), ipOosUdpPacket(udpTestPath(4), []byte{2})},
	} {
		indexes := map[*ipPacketGroup][]int{}
		groups, rejected := groupIpPacketsBoundedIndexed(packets, 0, 0, indexes)
		if len(rejected) != 0 || len(groups) != 1 {
			t.Fatal("ordinary grouping fixture failed")
		}
		original := groups[0]
		got := splitAffinitySynGroups(groups, indexes)
		if len(got) != 1 || got[0] != original || &got[0] != &groups[0] || len(indexes) != 1 {
			t.Fatal("ordinary data or same-ISN retries allocated/split a generation group")
		}
	}
}

func TestAffinityRenewalNativeGroupingSplitsResetThenSameIsn(t *testing.T) {
	packets := [][]byte{
		groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil),
		groupRecoveryPacket(101, 500, 64, tcpFlagRst, nil),
		groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil),
		groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil),
	}
	indexes := map[*ipPacketGroup][]int{}
	groups, rejected := groupIpPacketsBoundedIndexed(packets, 0, 0, indexes)
	if len(rejected) != 0 {
		t.Fatal("reset fixture did not parse")
	}
	got := splitAffinitySynGroups(groups, indexes)
	if len(got) != 2 || !slices.Equal(indexes[got[0]], []int{0, 1}) || !slices.Equal(indexes[got[1]], []int{2, 3}) {
		t.Fatal("reset did not separate a new identical-ISN connection from its closed prefix")
	}
}

// Real public parsing, security, grouping, indexing and pool ownership; only
// placement and final Transfer admission are seams in this grouping root.
func testAffinityRenewalNativeMixedBatch(t *testing.T, prefixFlags byte, refuse int, cancelAfterPrefix, mux bool) {
	t.Helper()
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	client := newPacketTransferTestChannel()
	client.ctx, client.settings = parent.ctx, parent.settings
	var updates []*multiClientChannelUpdate
	defer func() {
		for _, update := range updates {
			update.Close()
		}
	}()
	parent.sendClientPathForTest = func(path *IpPath, _ flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
		update := newMultiClientChannelUpdate(parent.ctx, path)
		update.client.Store(client)
		updates = append(updates, update)
		callback(update, client)
	}
	var lengths []int
	var firstSequences []uint32
	client.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
		index := len(lengths)
		lengths = append(lengths, len(group.packets))
		firstSequences = append(firstSequences, group.packets[0].ipPath.SequenceNumber)
		if !ack || timeout != 0 {
			t.Error("generation split changed TCP ACK reliability or zero-wait admission")
		}
		if index == 1 && len(group.packets[0].payload) != 3 {
			t.Error("generation split lost TCP Fast Open payload")
		}
		if cancelAfterPrefix && index == 0 {
			parent.cancel()
		}
		if index == refuse {
			return false, nil
		}
		for _, packet := range group.packets {
			MessagePoolReturn(packet.packet)
		}
		return true, nil
	}
	packets := [][]byte{
		MessagePoolCopy(groupRecoveryPacket(101, 500, 64, prefixFlags, []byte{1})),
		MessagePoolCopy(groupRecoveryPacket(0xfffffffe, 0, 64, tcpFlagSyn, []byte{2, 3, 4})),
		MessagePoolCopy(groupRecoveryPacket(2, 500, 64, tcpFlagAck, []byte{5})),
		MessagePoolCopy(groupRecoveryPacket(0xfffffffe, 0, 64, tcpFlagSyn, []byte{2, 3, 4})),
		MessagePoolCopy(groupRecoveryPacket(700, 0, 64, tcpFlagSyn, nil)),
		MessagePoolCopy(groupRecoveryPacket(701, 500, 64, tcpFlagAck, []byte{6})),
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	accepted := make([]bool, len(packets))
	count := 0
	if mux {
		stage := &IpMux{upstream: parent.SendPacket, upstreamGroupSend: parent.sendPacketGroup}
		count = stage.SendPacketBatch(SourceId(NewId()), affinityRenewalProvideModeNetwork, packets, 0)
	} else {
		count = parent.SendPacketBatchWithResults(SourceId(NewId()), affinityRenewalProvideModeNetwork, packets, 0, accepted)
	}
	want := []bool{refuse != 0, refuse != 1, refuse != 1, refuse != 1, refuse != 2, refuse != 2}
	if cancelAfterPrefix {
		clear(want[1:])
		if !slices.Equal(lengths, []int{1}) {
			t.Fatalf("canceled suffix reached admission: %v", lengths)
		}
	} else if !slices.Equal(lengths, []int{1, 3, 2}) || !slices.Equal(firstSequences, []uint32{101, 0xfffffffe, 700}) {
		t.Fatalf("generation coalescing/order changed: lengths=%v sequences=%v", lengths, firstSequences)
	}
	wantCount := 0
	for _, value := range want {
		if value {
			wantCount++
		}
	}
	if count != wantCount || !mux && !slices.Equal(accepted, want) {
		t.Fatalf("prefix flags=%d refusal=%d cancel=%v results=%v count=%d want=%v count=%d",
			prefixFlags, refuse, cancelAfterPrefix, accepted, count, want, wantCount)
	}
}

func TestAffinityRenewalNativeBatchMixedGenerationIndexedOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, flags := range []byte{tcpFlagAck, tcpFlagAck | tcpFlagFin, tcpFlagRst} {
		for _, refused := range []int{-1, 0, 1, 2} {
			testAffinityRenewalNativeMixedBatch(t, flags, refused, false, false)
		}
	}
}

func TestAffinityRenewalNativeBatchCancellationKeepsPrefixOutcome(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, refused := range []int{-1, 0} {
		testAffinityRenewalNativeMixedBatch(t, tcpFlagAck, refused, true, false)
	}
}

func TestAffinityRenewalMuxBatchMixedGenerationOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, flags := range []byte{tcpFlagAck, tcpFlagAck | tcpFlagFin, tcpFlagRst} {
		for _, refused := range []int{-1, 0, 1, 2} {
			testAffinityRenewalNativeMixedBatch(t, flags, refused, false, true)
		}
	}
}

func TestAffinityRenewalMuxBatchCancellationKeepsPrefixOutcome(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, refused := range []int{-1, 0} {
		testAffinityRenewalNativeMixedBatch(t, tcpFlagAck, refused, true, true)
	}
}
