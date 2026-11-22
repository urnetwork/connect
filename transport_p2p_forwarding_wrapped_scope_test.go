// The native sealers retain plaintext synchronously while selecting a carrier.
package connect

import (
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Every selected queue still receives ciphertext. A reverse native receipt or
// valid DATA in Required mode can traverse the enrolled stream, while a new
// sequence, contract or opaque wrap can use only its ordinary route.
func TestStreamForwardingApplicationRoutesWrappedWorkByNativeScope(t *testing.T) {
	origin, destination := streamForwardingApplicationPair(t)
	newContractId := NewId()
	for _, test := range []struct {
		name        string
		association *streamForwardingAssociation
		frame       *protocol.TransferFrame
		contractId  *Id
		opaque      bool
		expected    TransportType
	}{
		{"forward DATA", origin, streamForwardingTestPack(origin, []byte("inside"), false), &origin.grant.contractId, false, TransportTypeP2p},
		{"successor implicit grant", origin, streamForwardingTestPack(origin, []byte("new contract"), false), &newContractId, false, TransportTypeH1},
		{"opaque transfer", origin, streamForwardingTestPack(origin, []byte("outside writer"), false), nil, true, TransportTypeH1},
		{"reverse application", destination, streamForwardingTestPack(destination, []byte("outside"), false), nil, false, TransportTypeH1},
		{"native ACK", destination, &protocol.TransferFrame{TransferPath: destination.sendPath.ToProtobuf(),
			Ack: &protocol.Ack{MessageId: NewId().Bytes(), SequenceId: destination.grant.sequenceId.Bytes()}}, nil, false, TransportTypeP2p},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector, forwarded, ordinary := streamForwardingScopeTestWriter(t, test.association)
			inner := marshalStreamForwardingTestFrame(t, test.frame)
			cipher := newFrameCodecTestSequenceCipher(t)
			wire, err := cipher.SealOuterFrame(test.association.sendPath, inner, protocol.SequenceRole_SequenceRoleClient, false)
			if err != nil {
				t.Fatal(err)
			}
			// Hold the ordinary slot for work expected on P2P, independent
			// of the ordinary selector's weighted/shuffled candidate order.
			if test.expected == TransportTypeP2p {
				ordinary <- []byte("occupied")
			}
			if test.opaque {
				err = selector.Write(t.Context(), wire, 0)
			} else if test.frame.Ack != nil {
				var ok bool
				ok, _, err = selector.writeDetailedReplyWithCarrierPreference(t.Context(), wire, 0, TransportTypeP2p, streamForwardingWrite{inner: inner})
				if err == nil && !ok {
					err = errTransferRouteWriteTimeout
				}
			} else {
				_, err = writeMultiRouteWithCarrier(selector, t.Context(), wire, 0, false,
					streamForwardingWrite{inner: inner, contractId: test.contractId})
			}
			if test.expected == TransportTypeP2p {
				<-ordinary
			}
			if err != nil {
				MessagePoolReturn(wire)
				t.Fatal(err)
			}
			requireStreamForwardingScopeRoute(t, test.name, forwarded, ordinary, wire, test.expected)
		})
	}
}

// This selector geometry exercises both policies even if a future platform
// carrier itself uses an enrolled writer. Ineligible pins yield, while a
// congested eligible pin and an available reliable sibling keep their priority.
func TestStreamForwardingApplicationReconsidersOnlyIneligiblePolicies(t *testing.T) {
	origin, _ := streamForwardingApplicationPair(t)
	binding := &streamForwardingBinding{readiness: origin.readiness}
	binding.association.Store(origin)
	forwarded, ordinary, unreliable := make(Route, 1), make(Route, 1), make(Route, 1)
	selector := NewMultiRouteSelector(t.Context(), "forwarding-write-policy", nil, DestinationId(origin.sendPath.DestinationId), false)
	defer selector.Close()
	h1, h3 := newTypedPriorityRouteTestTransport(TransportTypeH1, 1), newTypedPriorityRouteTestTransport(TransportTypeH3, 1)
	selector.updateTransportWithProperties(h1, []Route{forwarded}, TransferCarrierProperties{forwardingSend: binding, forwardingInnerLimit: 65536})
	selector.updateTransportWithProperties(h3, []Route{ordinary}, TransferCarrierProperties{})
	if selector.activeRoutesSnapshot.Load().preferDirectRoute != forwarded {
		t.Fatal("missing the policy fixture's first direct pin")
	}
	data := marshalStreamForwardingTestFrame(t, streamForwardingTestPack(origin, []byte("data"), false))
	other := streamForwardingTestPack(origin, nil, false)
	other.Pack.SequenceId = NewId().Bytes()
	outside := marshalStreamForwardingTestFrame(t, other)
	write := func(wire []byte, reliableOnly bool) (bool, error) {
		ok, _, err := selector.writeDetailedWithRoutePolicy(t.Context(), wire, 0, TransportTypeUnknown, reliableOnly)
		return ok, err
	}
	if ok, err := write(outside, false); !ok || err != nil {
		t.Fatalf("ineligible direct pin hid its ordinary sibling: %v", err)
	}
	requireStreamForwardingScopeRoute(t, "direct policy fallback", forwarded, ordinary, outside, TransportTypeH1)
	forwarded <- []byte("first")
	if ok, err := write(data, false); ok || err != nil || len(ordinary) != 0 {
		t.Fatalf("congested eligible pin did not preserve backpressure: accepted=%t err=%v", ok, err)
	}
	<-forwarded
	if ok, err := write(outside, true); !ok || err != nil {
		t.Fatalf("ineligible direct pin hid its reliable sibling: %v", err)
	}
	requireStreamForwardingScopeRoute(t, "reliable direct sibling", forwarded, ordinary, outside, TransportTypeH1)
	selector.updateTransportWithProperties(newTypedPriorityRouteTestTransport(TransportTypeH3Dns, 1), []Route{unreliable}, TransferCarrierProperties{Unreliable: true})
	ordinary <- []byte("busy")
	if ok, err := write(outside, true); ok || err != nil || len(unreliable) != 0 {
		t.Fatalf("reliable write spilled onto an unreliable carrier: accepted=%t err=%v", ok, err)
	}
	<-ordinary
	selector.updateTransportWithProperties(h3, nil, TransferCarrierProperties{})
	if ok, err := write(outside, true); !ok || err != nil || len(unreliable) != 1 {
		t.Fatalf("unavailable reliable set prevented ordinary degradation: accepted=%t err=%v", ok, err)
	}
	<-unreliable
}
