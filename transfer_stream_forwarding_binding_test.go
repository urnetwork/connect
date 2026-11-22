// Verifies that route matching preserves an ordinary adjacent writer through
// the legacy control shape while refusing every distant alias until enrollment.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The old missing field and the explicit direct classification preserve the
// same established adjacent route. Neither can lend it to a different client.
func TestStreamForwardingLegacyAndDirectAliasesStayAdjacent(t *testing.T) {
	ctx := t.Context()
	adjacentId, distantId, streamId := NewId(), NewId(), NewId()
	for _, classification := range []*bool{nil, proto.Bool(false)} {
		openWire, err := proto.Marshal(&protocol.StreamOpen{
			SourceId: adjacentId.Bytes(), StreamId: streamId.Bytes(), PathHasIntermediaries: classification,
		})
		if err != nil {
			t.Fatal(err)
		}
		var open protocol.StreamOpen
		if err := proto.Unmarshal(openWire, &open); err != nil {
			t.Fatal(err)
		}
		if open.GetPathHasIntermediaries() || (open.PathHasIntermediaries == nil) != (classification == nil) {
			t.Fatal("legacy or direct stream became a forwarded endpoint")
		}
		routes := NewRouteManager(ctx, "synthetic-legacy-stream")
		adjacent := routes.OpenMultiRouteWriter(DestinationId(adjacentId))
		distant := routes.OpenMultiRouteWriter(DestinationId(distantId))
		scopedDistant := routes.OpenMultiRouteWriter(TransferPath{DestinationId: distantId, StreamId: streamId})
		removeAlias := routes.AddWriterDestinationAlias(DestinationId(distantId), StreamId(streamId))
		transport := &P2pSendTransport{
			transportId: NewId(), ctx: ctx, peerId: adjacentId, streamId: streamId,
			endToEndReadinessRequired: true, endpointDestinationRequired: true,
		}
		transport.endToEndReady.Store(true)
		route := make(Route, 1)
		routes.UpdateTransport(transport, []Route{route})
		if got := adjacent.GetActiveRoutes(); len(got) != 1 || got[0] != route {
			t.Fatal("legacy or direct topology lost the authenticated adjacent route")
		}
		if len(distant.GetActiveRoutes()) != 0 || len(scopedDistant.GetActiveRoutes()) != 0 {
			t.Fatal("a distant writer admitted the legacy adjacent route")
		}
		routes.RemoveTransport(transport)
		removeAlias()
		routes.CloseMultiRouteWriter(scopedDistant)
		routes.CloseMultiRouteWriter(distant)
		routes.CloseMultiRouteWriter(adjacent)
	}
}

// Supplies a valid enrollment created by the ordinary grant and endpoint
// negotiation used in the platform receipt tests.
func testStreamForwardingBindingEnrollment(t *testing.T, path TransferPath) streamForwardingEnrollment {
	t.Helper()
	now := time.Now()
	contract := testStreamForwardingContractForPath(t, now, path)
	offer := newStreamForwardingOffer(contract, NewId(), NewId(), now)
	if offer == nil {
		t.Fatal("synthetic signed grant did not produce its offer")
	}
	ctx := t.Context()
	owner := newPlatformReceiveOwner(contract.path.DestinationId, ctx.Done(), nil, true)
	enrollment := streamForwardingEnrollment{ctx: ctx}
	if !acceptStreamForwardingOffer(contract, contract.path.LocalMask(), offer.grant.sequenceId,
		offer.grant.introductionId, now, owner.provenance(platformReceiveH1), offer.wire[:], func(answer streamForwardingAnswer) bool {
			enrollment.grant, enrollment.keys = answer.grant, answer.keys
			return true
		}) {
		t.Fatal("synthetic platform receipt did not authenticate its grant")
	}
	return enrollment
}

// Supplies only the exact adjacent hop and local client from StreamOpen.
func testStreamForwardingBindingSequence(t *testing.T, enrollment streamForwardingEnrollment, adjacentId Id) *StreamSequence {
	t.Helper()
	localId, remoteId := enrollment.grant.path.SourceId, enrollment.grant.path.DestinationId
	var sourceId, destinationId *Id
	if enrollment.destination {
		localId, remoteId = remoteId, localId
	}
	if remoteId.LessThan(localId) {
		sourceId = &adjacentId
	} else {
		destinationId = &adjacentId
	}
	manager := &StreamManager{client: &Client{clientId: localId}}
	sequence := NewStreamSequence(t.Context(), manager, sourceId, destinationId, enrollment.grant.path.StreamId, nil)
	sequence.forwarding = &streamForwardingBinding{readiness: newP2pStreamReadiness(sequence.ctx)}
	return sequence
}

// The platform publishes one canonical hop order for both signed directions.
// A proof only binds its exact local StreamOpen on that canonical side.
func TestStreamForwardingBindingRequiresStreamOpenDirection(t *testing.T) {
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	for _, directedPath := range []TransferPath{path, path.Reverse()} {
		enrollment := testStreamForwardingBindingEnrollment(t, directedPath)
		for _, destination := range []bool{false, true} {
			enrollment.destination = destination
			sequence := testStreamForwardingBindingSequence(t, enrollment, NewId())
			defer sequence.Cancel()
			sequence.sourceId, sequence.destinationId = sequence.destinationId, sequence.sourceId
			if sequence.bindStreamForwarding(enrollment) || sequence.forwarding.association.Load() != nil {
				t.Fatalf("grant installed on the wrong StreamOpen side with destination role %t", destination)
			}
			sequence.sourceId, sequence.destinationId = sequence.destinationId, sequence.sourceId
			if !sequence.bindStreamForwarding(enrollment) || sequence.forwarding.readiness.activeKeys() == nil {
				t.Fatalf("matching grant failed to install on the canonical StreamOpen side with destination role %t", destination)
			}
		}
	}
}

// A real promotion from the probe worker remains live until the signed
// boundary, where StreamSequence retires its writer without a probe tick.
func TestStreamForwardingBindingRetiresAtSignedGrantExpiry(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
		enrollment := testStreamForwardingBindingEnrollment(t, path)
		adjacentId := NewId()
		sequence := testStreamForwardingBindingSequence(t, enrollment, adjacentId)
		defer sequence.Cancel()
		if !sequence.bindStreamForwarding(enrollment) {
			t.Fatal("verified grant could not be bound")
		}
		association := sequence.forwarding.association.Load()
		streamId := enrollment.grant.path.StreamId
		endpoint := newP2pForwardProbeTestEndpoint(t, streamId, enrollment.grant.path.DestinationId, false)
		endpoint.probe.forwardReadiness = sequence.forwarding.readiness
		go endpoint.probe.run()
		defer endpoint.close()
		retired := make(chan struct{})
		go func() {
			defer close(retired)
			sequence.waitForRetirement()
		}()
		synctest.Wait()
		request := takeP2pForwardProbeControl(t, endpoint.requests, "origin challenge")
		defer MessagePoolReturn(request)
		_, _, _, nonce := decodeP2pStreamProbe(request)
		peer := association.readinessKeys
		peer.sendKey, peer.receiveKey = peer.receiveKey, peer.sendKey
		response := encodeP2pReadinessProbe(&peer, p2pStreamProbeResponseType, streamId, nonce)
		defer MessagePoolReturn(response)
		deliverP2pForwardProbeControl(t, endpoint.probe, response)
		synctest.Wait()
		if len(endpoint.writer.GetActiveRoutes()) != 1 {
			t.Fatal("authenticated challenge response did not promote the writer")
		}

		deadline := time.UnixMilli(enrollment.grant.expirationTimeUnixMillis)
		time.Sleep(time.Until(deadline) - time.Millisecond)
		synctest.Wait()
		if !association.live() || len(endpoint.writer.GetActiveRoutes()) != 1 {
			t.Fatal("the signed grant retired early")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		select {
		case <-retired:
		default:
			t.Fatal("the signed grant deadline did not retire its stream")
		}
		if association.live() || sequence.forwarding.readiness.activeKeys() != nil ||
			len(endpoint.writer.GetActiveRoutes()) != 0 {
			t.Fatal("the expired grant retained its readiness or published writer")
		}

		// Teardown may still be joining the old physical generation. A
		// fresh proof must be available to the next authorized StreamOpen.
		buffer := &StreamBuffer{
			ctx: t.Context(), streamSequencesByStreamId: map[Id]*StreamSequence{streamId: sequence},
		}
		fresh := testStreamForwardingBindingEnrollment(t, path)
		if !buffer.enrollStreamForwarding(fresh) {
			t.Fatal("the indexed retired generation blocked a fresh verified grant")
		}
		pending := buffer.pendingForwardingEnrollments[streamId]
		if pending.grant != fresh.grant || pending.keys != fresh.keys {
			t.Fatal("fresh proof did not retain its own pending grant")
		}
		successor := testStreamForwardingBindingSequence(t, pending, adjacentId)
		defer successor.Cancel()
		if !successor.bindStreamForwarding(pending) || successor.forwarding.association.Load() == association {
			t.Fatal("the replacement stream did not establish a new grant owner")
		}
	})
}

// A new verified proof may replace a canceled native owner. Repeating the
// prior transcript cannot create a second owner or reset its replay state.
func TestStreamForwardingBindingReplacesRetiredNativeSequence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
		initial := testStreamForwardingBindingEnrollment(t, path)
		initialCtx, cancelNative := context.WithCancel(initial.ctx)
		defer cancelNative()
		initial.ctx = initialCtx
		successor := testStreamForwardingBindingEnrollment(t, path)
		adjacentId := NewId()
		sequence := testStreamForwardingBindingSequence(t, initial, adjacentId)
		defer sequence.Cancel()
		if !sequence.bindStreamForwarding(initial) {
			t.Fatal("initial verified proof did not bind")
		}
		association := sequence.forwarding.association.Load()
		buffer := &StreamBuffer{
			ctx: t.Context(), streamSequencesByStreamId: map[Id]*StreamSequence{path.StreamId: sequence},
		}
		if buffer.enrollStreamForwarding(successor) || !association.live() {
			t.Fatal("successor displaced the live native owner")
		}
		cancelNative()
		repeat := initial
		repeat.ctx = t.Context()
		if !buffer.enrollStreamForwarding(repeat) || !association.live() ||
			sequence.forwarding.association.Load() != association {
			t.Fatal("same transcript did not retain its existing association")
		}
		if !buffer.enrollStreamForwarding(successor) {
			t.Fatal("canceled native owner blocked a fresh verified proof")
		}
		if association.live() || sequence.ctx.Err() == nil ||
			buffer.enrollStreamForwarding(repeat) {
			t.Fatal("retired physical ownership or repeated keys survived replacement")
		}
		pending := buffer.pendingForwardingEnrollments[path.StreamId]
		next := testStreamForwardingBindingSequence(t, pending, adjacentId)
		defer next.Cancel()
		if !next.bindStreamForwarding(pending) {
			t.Fatal("successor StreamOpen did not bind the new proof")
		}
		nextAssociation := next.forwarding.association.Load()
		if !nextAssociation.live() || nextAssociation == association ||
			nextAssociation.readinessKeys.transcript == association.readinessKeys.transcript ||
			nextAssociation.grant != successor.grant {
			t.Fatal("successor did not retain independent exact grant ownership")
		}
	})
}
