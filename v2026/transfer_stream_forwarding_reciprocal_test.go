// The full TUN returns reliable IP data on its own signed contract and
// native sequence over the same authenticated stream.
package connect

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// The provider's existing reply policy retains force-stream and its server
// sequence role. The signed stream path still exists without a local list of
// intermediaries, including before its platform StreamOpen is applied.
func TestStreamForwardingProviderReplyOffersPaidStreamWithoutIntermediaries(t *testing.T) {
	now := time.Now()
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	contract := testStreamForwardingContractForPath(t, now, path)
	key := providerReplyTransferKey(
		TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer},
		protocol.ProvideMode_Network,
	)
	opts := providerReturnIpTransferOptions(TransferOptions{}, protocol.ProvideMode_Network, key, IpProtocolTcp)
	if !opts.Ack || !opts.ForceStream || opts.CompanionContract {
		t.Fatal("provider return no longer uses the ordinary reliable stream sequence")
	}
	if key.EncryptionRole != protocol.SequenceRole_SequenceRoleServer {
		t.Fatal("provider return changed its existing native sequence role")
	}
	sender := &SendSequence{
		ctx: t.Context(), client: testStreamForwardingWireClient(t.Context(), path.SourceId),
		forceStream: opts.ForceStream, companionContract: opts.CompanionContract,
		encryptionRole: sequenceTlsRoleServer, destination: path.DestinationId, sequenceId: NewId(),
	}
	introductionId := NewId()
	offer := sender.streamForwardingIntroduction(contract, introductionId, 1, now)
	if offer == nil || offer.grant.path != path || offer.grant.contractId != contract.contractId ||
		offer.grant.sequenceId != sender.sequenceId || offer.grant.introductionId != introductionId {
		t.Fatal("provider reverse stream contract produced no exact forwarding introduction")
	}
	// An already classified adjacent stream does not add the optional offer.
	direct := &StreamSequence{ctx: t.Context(), destinationId: &path.DestinationId}
	sender.client.streamManager.streamBuffer.streamSequencesByStreamId = map[Id]*StreamSequence{path.StreamId: direct}
	sender.forwardingOffer.Store(nil)
	if sender.streamForwardingIntroduction(contract, NewId(), 2, now) != nil {
		t.Fatal("known direct provider return emitted a reciprocal offer")
	}
}

// One physical stream owner can carry its original DATA, native ACK, and one
// separately verified provider return. Earlier and unrelated reverse writes
// keep their ordinary route. Protection still uses the original replay owner.
func TestStreamForwardingApplicationReciprocalPaidWorkKeepsOnePhysicalOwner(t *testing.T) {
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	primary := testStreamForwardingBindingEnrollment(t, path)
	reciprocal := testStreamForwardingBindingEnrollment(t, path.Reverse())
	device := testStreamForwardingBindingSequence(t, primary, NewId())
	defer device.Cancel()
	primaryAtProvider := primary
	primaryAtProvider.destination = true
	provider := testStreamForwardingBindingSequence(t, primaryAtProvider, NewId())
	defer provider.Cancel()
	if !device.bindStreamForwarding(primary) || !provider.bindStreamForwarding(primaryAtProvider) {
		t.Fatal("primary application owners did not bind")
	}
	deviceOwner, providerOwner := device.forwarding.association.Load(), provider.forwarding.association.Load()
	deviceWriter, deviceForwarded, deviceOrdinary := streamForwardingScopeTestWriter(t, deviceOwner)
	providerWriter, providerForwarded, providerOrdinary := streamForwardingScopeTestWriter(t, providerOwner)

	write := func(
		writer *MultiRouteSelector,
		forwarded, ordinary Route,
		name string,
		frame *protocol.TransferFrame,
		contractId *Id,
		expect TransportType,
	) []byte {
		t.Helper()
		plain := marshalStreamForwardingTestFrame(t, frame)
		wire := MessagePoolCopy(plain)
		ok, disposition, err := writer.writeDetailedWithRoutePolicyGeneration(
			t.Context(), wire, 0, TransportTypeP2p, false, 0, false, nil,
			streamForwardingWrite{inner: plain, contractId: contractId},
		)
		if !ok || err != nil {
			MessagePoolReturn(wire)
			t.Fatalf("%s did not find its route: %v", name, err)
		}
		requireStreamForwardingScopeRoute(t, name, forwarded, ordinary, wire, expect)
		if disposition.transportType != expect {
			t.Fatalf("%s recorded %s, want %s", name, disposition.transportType, expect)
		}
		return plain
	}
	sendPrimary := func() {
		write(deviceWriter, deviceForwarded, deviceOrdinary, "original DATA",
			streamForwardingTestPack(deviceOwner, []byte("original committed payload"), false),
			&primary.grant.contractId, TransportTypeP2p)
	}
	returnData := &protocol.TransferFrame{
		TransferPath: providerOwner.sendPath.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId: NewId().Bytes(), SequenceId: reciprocal.grant.sequenceId.Bytes(), ForceStream: true,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider,
				MessageBytes: []byte("synthetic provider return"), Raw: true}},
		},
	}
	sendPrimary()
	write(providerWriter, providerForwarded, providerOrdinary, "unverified returned DATA",
		returnData, &reciprocal.grant.contractId, TransportTypeH1)

	providerBuffer := &StreamBuffer{ctx: t.Context(), streamSequencesByStreamId: map[Id]*StreamSequence{path.StreamId: provider}}
	deviceBuffer := &StreamBuffer{ctx: t.Context(), streamSequencesByStreamId: map[Id]*StreamSequence{path.StreamId: device}}
	if !providerBuffer.enrollStreamForwarding(reciprocal) {
		t.Fatal("live forward scope rejected the independently verified reciprocal grant")
	}
	reciprocal.destination = true
	if !deviceBuffer.enrollStreamForwarding(reciprocal) ||
		provider.forwarding.association.Load() != providerOwner || device.forwarding.association.Load() != deviceOwner {
		t.Fatal("reciprocal proof replaced the authenticated physical stream owner")
	}

	ack := &protocol.TransferFrame{TransferPath: deviceOwner.sendPath.ToProtobuf(), Ack: &protocol.Ack{
		MessageId: returnData.Pack.MessageId, SequenceId: returnData.Pack.SequenceId,
		StreamForwardingAcceptance: make([]byte, streamForwardingAnswerByteCount),
	}}
	write(deviceWriter, deviceForwarded, deviceOrdinary, "reciprocal platform acceptance", ack, nil, TransportTypeH1)
	plain := write(providerWriter, providerForwarded, providerOrdinary, "verified returned DATA",
		returnData, &reciprocal.grant.contractId, TransportTypeP2p)
	protected, ok := providerOwner.seal(plain, 65536)
	if !ok {
		t.Fatal("physical writer rejected the authorized provider return")
	}
	defer MessagePoolReturn(protected)
	opened, class, ok := deviceOwner.open(protected, 65536)
	if !ok || class != streamForwardingPack || !bytes.Equal(opened, plain) {
		t.Fatal("device did not authenticate the exact reciprocal application DATA")
	}
	if _, _, ok := deviceOwner.open(protected, 65536); ok {
		t.Fatal("physical replay accepted the same reciprocal DATA")
	}
	cipher := newFrameCodecTestSequenceCipher(t)
	wrapped, err := cipher.SealOuterFrame(providerOwner.sendPath, plain, protocol.SequenceRole_SequenceRoleServer, false)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(wrapped)
	shared := MessagePoolShareReadOnly(wrapped)
	ok, _, err = providerWriter.writeDetailedWithRoutePolicyGeneration(t.Context(), shared, 0, TransportTypeP2p, false, 0, false, nil,
		streamForwardingWrite{inner: plain, contractId: &reciprocal.grant.contractId})
	if !ok || err != nil {
		MessagePoolReturn(shared)
		t.Fatalf("scoped encrypted return missed the actual selector: %v", err)
	}
	requireStreamForwardingScopeRoute(t, "verified encrypted return", providerForwarded, providerOrdinary, shared, TransportTypeP2p)
	protectedWrap, ok := providerOwner.seal(wrapped, 65536)
	if !ok {
		t.Fatal("physical send rejected the authorized encrypted return")
	}
	defer MessagePoolReturn(protectedWrap)
	wrapBytes, class, ok := deviceOwner.open(protectedWrap, 65536)
	if !ok || class != streamForwardingWrap {
		t.Fatal("device did not authenticate the reciprocal encrypted outer frame")
	}
	var wrap protocol.TransferFrame
	if err := proto.Unmarshal(wrapBytes, &wrap); err != nil {
		t.Fatal(err)
	}
	inner, err := cipher.Open(wrap.EncryptedTransferFrame)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(inner)
	if class, exact := deviceOwner.classifyFrame(inner, deviceOwner.receivePath, nil); class != streamForwardingPack ||
		exact == nil || exact.grant != reciprocal.grant || !bytes.Equal(inner, plain) {
		t.Fatal("decryption lost the exact reciprocal native scope")
	}

	ack.Ack.StreamForwardingAcceptance = nil
	write(deviceWriter, deviceForwarded, deviceOrdinary, "matching return native ACK", ack, nil, TransportTypeP2p)
	unrelated := NewId()
	write(providerWriter, providerForwarded, providerOrdinary, "unrelated return grant",
		returnData, &unrelated, TransportTypeH1)
	returnData.Pack.SequenceId = NewId().Bytes()
	write(providerWriter, providerForwarded, providerOrdinary, "unrelated return sequence",
		returnData, &reciprocal.grant.contractId, TransportTypeH1)
	sendPrimary()
}

// A delivered reverse grant can be verified before the original grant.
// Neither endpoint publishes physical readiness from the reverse proof alone.
func TestStreamForwardingReciprocalProofCanPrecedePrimary(t *testing.T) {
	ctx, now := t.Context(), time.Now()
	forwardPath := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	path := forwardPath.Reverse()
	contract := testStreamForwardingContractForPath(t, now, path)
	providerClient := testStreamForwardingWireClient(ctx, path.SourceId)
	deviceClient := testStreamForwardingWireClient(ctx, path.DestinationId)
	sequenceId, messageId := NewId(), NewId()
	sender := &SendSequence{ctx: ctx, client: providerClient, destination: path.DestinationId,
		sequenceId: sequenceId, forceStream: true, encryptionRole: sequenceTlsRoleServer}
	receiver := &ReceiveSequence{ctx: ctx, client: deviceClient, sequenceId: sequenceId,
		source: TransferPath{SourceId: path.SourceId}}
	offer := sender.streamForwardingIntroduction(contract, messageId, 1, now)
	if offer == nil {
		t.Fatal("provider did not offer the first full reciprocal grant")
	}
	providerPlatform := newPlatformReceiveOwner(path.SourceId, ctx.Done(), nil, true)
	devicePlatform := newPlatformReceiveOwner(path.DestinationId, ctx.Done(), nil, true)
	item := &receiveItem{
		transferItem: transferItem{messageId: messageId, sequenceNumber: 1},
		ack:          true,
		decodedOwner: &decodedPackOwner{receivePack: ReceivePack{
			carrier: &receivePackCarrier{provenance: devicePlatform.provenance(platformReceiveH1)},
		}},
	}
	receiver.noteStreamForwardingIntroduction(item, contract, offer.wire[:])
	receipt := sequenceAck{messageId: messageId, sequenceNumber: 1}
	acceptance := receiver.streamForwardingDeliveryAcceptance(receipt)
	if len(acceptance) != streamForwardingAnswerByteCount {
		t.Fatal("verified delivery did not carry the reciprocal acceptance")
	}
	ack := receiveAckMessage{messageId: messageId, sequenceId: sequenceId}
	ackFrame := &protocol.Ack{StreamForwardingAcceptance: acceptance}
	sender.acceptStreamForwardingAck(ack, 1, true,
		streamForwardingIngress(path.Reverse().LocalMask(), nil, ackFrame))
	if offer.published {
		t.Fatal("reciprocal sender enrolled without a verified platform receipt")
	}
	carrier := &receivePackCarrier{provenance: providerPlatform.provenance(platformReceiveH3Datagram)}
	sender.acceptStreamForwardingAck(ack, 0, false, streamForwardingIngress(path.Reverse().LocalMask(), carrier, ackFrame))
	if !offer.published {
		t.Fatal("exact repeated native delivery did not complete reciprocal enrollment")
	}

	primary := testStreamForwardingBindingEnrollment(t, forwardPath)
	for _, side := range []struct {
		client      *Client
		destination bool
	}{
		{client: deviceClient, destination: false},
		{client: providerClient, destination: true},
	} {
		primary.destination = side.destination
		buffer := side.client.streamManager.streamBuffer
		if len(buffer.pendingForwardingEnrollments) != 0 ||
			buffer.pendingReciprocalEnrollments[path.StreamId].grant != offer.grant {
			t.Fatal("early reverse proof acquired a primary grant slot")
		}
		sequence := testStreamForwardingBindingSequence(t, primary, NewId())
		defer sequence.Cancel()
		if sequence.forwarding.readiness.activeKeys() != nil || !buffer.bindForwardingPrimary(sequence, primary) {
			t.Fatal("only the later original grant can install physical readiness")
		}
		association := sequence.forwarding.association.Load()
		reverse := association.reciprocal.Load()
		if association.grant != primary.grant || reverse == nil || reverse.grant != offer.grant {
			t.Fatal("arrival order changed primary keys or lost the exact reverse grant")
		}
	}
}

// Reciprocal retirement must leave existing physical send and replay state in
// place. A new verified grant can bind only after its own predecessor ends.
func TestStreamForwardingReciprocalRetirementPreservesPhysicalReplay(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		for _, retireByExpiry := range []bool{false, true} {
			path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
			old := testStreamForwardingBindingEnrollment(t, path.Reverse())
			oldContext, cancelNative := context.WithCancel(old.ctx)
			defer cancelNative()
			old.ctx = oldContext
			// The physical owner has a later signed cutoff.
			time.Sleep(time.Second)
			primary := testStreamForwardingBindingEnrollment(t, path)
			primary.destination = true
			sequence := testStreamForwardingBindingSequence(t, primary, NewId())
			defer sequence.Cancel()
			if !sequence.bindStreamForwarding(primary) {
				t.Fatal("primary grant did not bind")
			}
			association := sequence.forwarding.association.Load()
			buffer := &StreamBuffer{ctx: t.Context(), streamSequencesByStreamId: map[Id]*StreamSequence{path.StreamId: sequence}}
			if !buffer.enrollStreamForwarding(old) {
				t.Fatal("initial reciprocal grant did not bind")
			}
			fresh := testStreamForwardingBindingEnrollment(t, path.Reverse())
			if buffer.enrollStreamForwarding(fresh) {
				t.Fatal("new reciprocal grant displaced a live predecessor")
			}
			oldData := streamForwardingTestPack(association, []byte("retiring reply"), true)
			oldData.Pack.SequenceId = old.grant.sequenceId.Bytes()
			oldData.Pack.ContractId = old.grant.contractId.Bytes()
			plain := marshalStreamForwardingTestFrame(t, oldData)
			protected, ok := association.seal(plain, 65536)
			if !ok {
				t.Fatal("live reciprocal grant did not use the physical writer")
			}
			MessagePoolReturn(protected)
			initialCounter := association.send.sendCounter
			initialReceive := association.receive.replayWindow
			if retireByExpiry {
				time.Sleep(time.Until(time.UnixMilli(old.grant.expirationTimeUnixMillis)))
			} else {
				cancelNative()
			}
			if sealed, ok := association.seal(plain, 65536); ok {
				MessagePoolReturn(sealed)
				t.Fatalf("old reciprocal frame retained physical authority with expiry=%t", retireByExpiry)
			}
			if !buffer.enrollStreamForwarding(fresh) || sequence.forwarding.association.Load() != association ||
				association.send.sendCounter != initialCounter || association.receive.replayWindow != initialReceive {
				t.Fatalf("new reciprocal proof reset the live physical owner with expiry=%t", retireByExpiry)
			}
			oldData.Pack.SequenceId = fresh.grant.sequenceId.Bytes()
			oldData.Pack.ContractId = fresh.grant.contractId.Bytes()
			protected, ok = association.seal(marshalStreamForwardingTestFrame(t, oldData), 65536)
			if !ok {
				t.Fatal("fresh exact reciprocal grant could not send")
			}
			MessagePoolReturn(protected)
			if association.send.sendCounter != initialCounter+1 {
				t.Fatal("replacement grant did not continue the original send counter")
			}
		}
	})
}
