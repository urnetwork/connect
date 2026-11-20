// Selective acknowledgement retains a real physical prefix. Its disposition
// must remain independent of a later, never-materialized original suffix.
package connect

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Uses the public native batch and real route/ack owners. Only the second
// contract update fails at the existing source seam; all timers are unchanged.
func TestTcpGroupDispositionSelectivePrefixKeepsProofAndRetriesSuffix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		clientId, destination := NewId(), NewId()
		firstWritten, allowFirst := make(chan struct{}), make(chan struct{})
		secondContract, allowFailure := make(chan struct{}), make(chan struct{})
		sequenceDone := make(chan struct{})
		var firstWriteOnce, releaseFirstOnce, releaseFailureOnce, doneOnce sync.Once
		releaseFirst := func() { releaseFirstOnce.Do(func() { close(allowFirst) }) }
		releaseFailure := func() { releaseFailureOnce.Do(func() { close(allowFailure) }) }
		var contractUpdates, appliedAcks, wireAttempts, started atomic.Int64
		events := make(chan SendPackLifecycleObservation, 16)
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.PrewarmOpeningContract = false
		settings.SendBufferSettings.forceContractFailureForTest = func(id sendSequenceId) bool {
			if id.Destination != destination || contractUpdates.Add(1) != 2 {
				return false
			}
			close(secondContract)
			select {
			case <-allowFailure:
			case <-ctx.Done():
			}
			return true
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, sequence uint64) {
			if id.Destination == destination && sequence == 0 {
				// A later successful retry opens another sequence at zero.
				firstWriteOnce.Do(func() {
					close(firstWritten)
					select {
					case <-allowFirst:
					case <-ctx.Done():
					}
				})
			}
		}
		settings.SendBufferSettings.afterAckSendItemForTest = func(id sendSequenceId, sequence uint64) {
			if id.Destination == destination && sequence == 0 {
				appliedAcks.Add(1)
			}
		}
		settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				doneOnce.Do(func() { close(sequenceDone) })
			}
		}
		settings.SendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
			wireAttempts.Add(1)
		}
		settings.SendBufferSettings.SendPackLifecycleObserver = func(observation SendPackLifecycleObservation) {
			if observation.DestinationId == destination {
				if observation.Phase == SendPackLifecyclePhaseStarted {
					started.Add(1)
				}
				events <- observation
			}
		}
		client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
		client.ContractManager().AddNoContractPeer(destination)
		route := make(Route, 8)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{route})
		var originals, witnesses [][]byte
		batchOffered := false
		releaseWitnesses := func() {
			for index, witness := range witnesses {
				if !MessagePoolReturn(witness) {
					t.Errorf("original packet %d still has a production owner", index)
				}
			}
			witnesses = nil
		}
		defer func() {
			cancel()
			releaseFirst()
			releaseFailure()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("join partial-group sender: %v", err)
			}
			if !batchOffered {
				for _, packet := range originals {
					MessagePoolReturn(packet)
				}
			}
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
			releaseWitnesses()
		}()
		parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		parent.settings.TcpCollapsePrevention = true
		parent.settings.TcpCollapseMaxHold = 0
		parent.settings.DialFailureRerace = false
		selected := newPacketTransferTestChannel()
		selected.ctx, selected.client = ctx, client
		selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
		update.client.Store(selected)
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(192, 0, 2, 50),
			DestinationIp:   net.IPv4(198, 51, 100, 60),
			SourcePort:      42000,
			DestinationPort: 443,
		}
		if sendPackBatchMaxFrames != 2 || !selected.ipPacketTransferAckRequired(path) {
			t.Fatal("fixture requires the unchanged two-frame bound and reliable TCP admission")
		}
		var templates [3][]byte
		for index := range templates {
			templates[index] = ipOosTcpPacketSequence(path, tcpFlagAck, uint32(100+10*index), bytes.Repeat([]byte{byte(index + 1)}, 10))
			originals = append(originals, MessagePoolCopy(templates[index]))
		}
		witnesses = groupTestPacketWitnesses(t, originals)
		source := SourceId(NewId())
		startTime := time.Now()
		// Batch admission consumes all inputs, including any rejected member.
		batchOffered = true
		if count := parent.SendPacketBatch(source, protocol.ProvideMode_Network, originals, 0); count != 3 || started.Load() != 1 {
			t.Fatalf("native batch admitted %d/3 packets in %d original Packs", count, started.Load())
		}
		waitGate := func(gate <-chan struct{}, name string) {
			t.Helper()
			synctest.Wait()
			select {
			case <-gate:
			default:
				t.Fatalf("owner did not reach %s at the original virtual instant", name)
			}
			if !time.Now().Equal(startTime) {
				t.Fatalf("%s advanced virtual time by %s", name, time.Since(startTime))
			}
		}
		waitGate(firstWritten, "first physical chunk")
		if contractUpdates.Load() != 1 || wireAttempts.Load() != 1 || appliedAcks.Load() != 0 || len(route) != 1 {
			t.Fatal("first owner barrier did not isolate one accepted physical chunk")
		}
		prefixWire := <-route
		defer MessagePoolReturn(prefixWire)
		prefixPack := decodeSendPackLifecycleWirePack(t, prefixWire)
		if prefixPack.Nack || len(prefixPack.Frames) != 2 {
			t.Fatal("physical prefix is not exactly two reliable original frames")
		}
		for index, frame := range prefixPack.Frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider || !bytes.Equal(frame.MessageBytes, templates[index]) {
				t.Fatalf("prefix physical frame %d differs from the original native packet", index)
			}
		}
		{
			if !client.sendBuffer.Ack(destination, &protocol.Ack{MessageId: prefixPack.MessageId, SequenceId: prefixPack.SequenceId, Selective: true}, 0) {
				t.Fatal("actual prefix peer acknowledgement was not admitted")
			}
			synctest.Wait()
		}
		releaseFirst()
		waitGate(secondContract, "unwritten suffix contract update")
		wantPrefixAcks := int64(0)
		if contractUpdates.Load() != 2 || appliedAcks.Load() != wantPrefixAcks || wireAttempts.Load() != 1 || len(route) != 0 {
			t.Fatal("suffix failure was not ordered after the requested prefix disposition")
		}
		client.sendBuffer.mutex.Lock()
		sequence := client.sendBuffer.sendSequences[sendSequenceId{Destination: destination, EncryptionRole: sequenceTlsRoleClient}]
		client.sendBuffer.mutex.Unlock()
		if sequence == nil || len(sequence.sendItems) != 1 || !sequence.sendItems[0].selectiveAcked ||
			sequence.sendItems[0].messageId != RequireIdFromBytes(prefixPack.MessageId) {
			t.Fatal("selective peer acknowledgement did not retain the exact physical prefix at the source barrier")
		}
		releaseFailure()
		waitGate(sequenceDone, "original sequence retirement")
		if ctx.Err() != nil || update.client.Load() != selected || wireAttempts.Load() != 1 || len(route) != 0 || len(events) != 3 {
			t.Fatal("source failure changed the provider, wrote the suffix or failed to settle the original group")
		}
		var originalToken uint64
		for _, phase := range []SendPackLifecyclePhase{SendPackLifecyclePhaseStarted, SendPackLifecyclePhaseFirstRouteWrite, SendPackLifecyclePhaseTerminal} {
			observation := <-events
			if phase == SendPackLifecyclePhaseStarted {
				originalToken = observation.Token
			}
			if observation.Phase != phase || observation.Token != originalToken || observation.ClientId != clientId ||
				observation.DestinationId != destination || !observation.AckRequired {
				t.Fatalf("original lifecycle identity/order changed: %+v", observation)
			}
			if phase == SendPackLifecyclePhaseStarted {
				if observation.Err != nil {
					t.Fatal("original admission began with an error")
				}
			} else if observation.Err == nil || !strings.Contains(observation.Err.Error(), "No contract") {
				t.Fatalf("original group lost its contract failure: %+v", observation)
			}
			if phase == SendPackLifecyclePhaseTerminal &&
				!strings.Contains(observation.Err.Error(), "Send sequence closed.") {
				t.Fatal("terminal disposition confused acknowledged and retained physical prefixes")
			}
		}
		if _, hardError := selected.WindowStats(); hardError == nil || !strings.Contains(hardError.Error(), "No contract") {
			t.Fatal("fixture lost the existing provider-error policy")
		}
		// These owners must settle before retry or cancellation, not because of it.
		releaseWitnesses()
		checkPrefixCollapsed := func(wantStarts, wantWrites int64) {
			t.Helper()
			for index := range 2 {
				packet := MessagePoolCopy(templates[index])
				if parent.SendPacket(source, protocol.ProvideMode_Network, packet, 0) {
					t.Fatalf("written prefix packet %d lost its public collapse proof", index)
				}
				MessagePoolReturn(packet)
			}
			synctest.Wait()
			if started.Load() != wantStarts || wireAttempts.Load() != wantWrites || len(route) != 0 || len(events) != 0 ||
				update.client.Load() != selected || !time.Now().Equal(startTime) {
				t.Fatal("collapsed prefix created an admission/write or changed owner/time")
			}
		}
		checkPrefixCollapsed(1, 1)
		retryPacket := MessagePoolCopy(templates[2])
		retryWitness := MessagePoolShareReadOnly(retryPacket)
		defer func() {
			if retryWitness != nil {
				MessagePoolReturn(retryWitness)
			}
		}()
		if !parent.SendPacket(source, protocol.ProvideMode_Network, retryPacket, 0) {
			MessagePoolReturn(retryPacket)
			t.Fatalf("unwritten logical-group suffix was collapsed on its identical public retry: prefix_selective=true starts=%d writes=%d same_provider=%t elapsed=%s",
				started.Load(), wireAttempts.Load(), update.client.Load() == selected, time.Since(startTime))
		}
		synctest.Wait()
		if started.Load() != 2 || wireAttempts.Load() != 2 || contractUpdates.Load() != 3 || len(route) != 1 || len(events) != 2 ||
			update.client.Load() != selected || !time.Now().Equal(startTime) {
			t.Fatal("public suffix retry did not promptly write once through the same provider")
		}
		retryWire := <-route
		defer MessagePoolReturn(retryWire)
		retryPack := decodeSendPackLifecycleWirePack(t, retryWire)
		if retryPack.Nack || len(retryPack.Frames) != 1 || bytes.Equal(retryPack.SequenceId, prefixPack.SequenceId) ||
			retryPack.Frames[0].MessageType != protocol.MessageType_IpIpPacketToProvider || !bytes.Equal(retryPack.Frames[0].MessageBytes, templates[2]) {
			t.Fatal("retry physical Pack is not exactly the byte-identical suffix on a fresh Transfer sequence")
		}
		var retryToken uint64
		for _, phase := range []SendPackLifecyclePhase{SendPackLifecyclePhaseStarted, SendPackLifecyclePhaseFirstRouteWrite} {
			observation := <-events
			if phase == SendPackLifecyclePhaseStarted {
				retryToken = observation.Token
			}
			if observation.Phase != phase || observation.Token != retryToken || retryToken == originalToken || observation.Err != nil ||
				observation.ClientId != clientId || observation.DestinationId != destination || !observation.AckRequired {
				t.Fatalf("suffix retry lost its distinct reliable lifecycle: %+v", observation)
			}
		}
		if !client.sendBuffer.Ack(destination, &protocol.Ack{MessageId: retryPack.MessageId, SequenceId: retryPack.SequenceId}, 0) {
			t.Fatal("actual suffix retry peer acknowledgement was not admitted")
		}
		synctest.Wait()
		if len(events) != 1 || appliedAcks.Load() != wantPrefixAcks+1 || ctx.Err() != nil {
			t.Fatal("suffix retry did not settle under its own peer acknowledgement")
		}
		terminal := <-events
		if terminal.Phase != SendPackLifecyclePhaseTerminal || terminal.Token != retryToken || terminal.Err != nil ||
			terminal.ClientId != clientId || terminal.DestinationId != destination || !terminal.AckRequired {
			t.Fatalf("suffix retry terminal was not one exact successful peer acknowledgement: %+v", terminal)
		}
		if !MessagePoolReturn(retryWitness) {
			retryWitness = nil
			t.Fatal("peer-acknowledged suffix retry retained its original packet owner")
		}
		retryWitness = nil
		checkPrefixCollapsed(2, 2)
		t.Logf("partial-group public recovery: prefix_selective=true prefix_frames=2 suffix_retry_frames=1 same_provider=true elapsed=%s",
			time.Since(startTime))
	})
}
