package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The single-destination client keys its IP traffic per flow, so a cell that
// drives traffic through it measures the queueing production has.
//
// What this layer is. It is not a production path: the only non-test reference
// to it in connect, the SDK or the server is a commented-out line, so it is a
// test fixture. Production client traffic goes through the multi client, which
// already passes the flow-scheduling option at both of its IP send sites using
// the same helper the provider uses. An earlier reading of this file as "the
// client's IP layer" was wrong, and the head-of-line concern it produced does
// not exist in the shipping path.
//
// Why it is still worth keying. A send sequence drains its channel into a
// per-flow scheduler and takes a flow only at its head. Unkeyed, every data
// pack through this fixture shares one flow with the control packs, so a
// control pack waiting for capacity holds all of the fixture's data behind it —
// a queueing behaviour production does not have. Several cells in this program
// drive traffic through here, so the fixture matching production is the
// difference between measuring the transfer layer and measuring the fixture.
//
// The key reuses the provider's derivation rather than a second five-tuple
// hash. Two implementations that must agree is a defect waiting to happen, and
// the key never crosses the wire.
//
// Prediction, recorded before the run: both IP packets reach the lane gate with
// a valid scheduling key; the control packs that share the sequence have no IP
// flow and correctly carry none.
func TestTheSingleDestinationClientKeysItsIpTrafficPerFlow(t *testing.T) {
	assertMessagePoolOwnership(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.SendBufferSettings.LogicalDataLaneCount = 8
	route := make(chan []byte, 256)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close the client: %v", err)
		}
		// No producer can publish another pooled share after the join.
		for {
			select {
			case transferFrameBytes := <-route:
				MessagePoolReturn(transferFrameBytes)
			default:
				return
			}
		}
	})

	providerId := NewId()
	var stateLock sync.Mutex
	validObservationCount := NewMonitorValue(0)
	observations := []logicalLaneGateObservation{}
	client.sendBuffer.logicalLaneGateObserverForTest.Store(
		&logicalLaneGateObserver{
			observe: func(observation logicalLaneGateObservation) {
				if observation.destination != providerId {
					return
				}
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					observations = append(observations, observation)
				}()
				if observation.schedulingValid {
					validObservationCount.Update(func(count int) int { return count + 1 })
				}
			},
		},
	)
	t.Cleanup(func() {
		client.sendBuffer.logicalLaneGateObserverForTest.Store(nil)
	})

	client.ContractManager().AddNoContractPeer(providerId)
	// a route that accepts and discards, so the send path runs to the gate
	client.RouteManager().UpdateTransport(
		NewSendGatewayTransport(), []Route{route})

	userNat := NewRemoteUserNatClient(
		client,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		[]MultiHopId{RequireMultiHopId(providerId)},
		protocol.ProvideMode_Network,
	)
	t.Cleanup(userNat.Close)

	// These control decisions must not satisfy the wait for two keyed packets.
	for range 2 {
		client.sendBuffer.selectLogicalLane(&SendPack{Destination: providerId})
	}
	source := SourceId(NewId())
	for _, port := range []int{45001, 45002} {
		packet := MessagePoolCopy(craftSecurityPacket(
			IpProtocolTcp,
			net.ParseIP("192.0.2.13"),
			port,
			net.ParseIP("198.51.100.34"),
			443,
			true,
			nil,
		))
		if !userNat.SendPacket(source, protocol.ProvideMode_Network, packet, time.Second) {
			MessagePoolReturn(packet)
			t.Fatalf("the client refused a packet on port %d", port)
		}
	}

	timeout := time.After(5 * time.Second)
	for {
		count, update := validObservationCount.Get()
		if 2 <= count {
			break
		}
		select {
		case <-update:
		case <-timeout:
			t.Fatalf("only %d keyed packets reached the provider's lane gate, want two", count)
		}
	}

	stateLock.Lock()
	seen := append([]logicalLaneGateObservation{}, observations...)
	stateLock.Unlock()

	valid := 0
	for _, observation := range seen {
		if observation.schedulingValid {
			valid += 1
		}
	}
	t.Logf("%d gate decisions, %d with a valid scheduling key", len(seen), valid)
	if len(seen) == 0 {
		t.Fatal("no packet reached the lane gate, so this cell reads nothing")
	}
	// The control packs this sequence also carries have no IP flow and
	// correctly have no key; what must be keyed is the IP traffic, which is
	// the two packets sent above.
	if valid < 2 {
		t.Errorf(
			"%d of %d gate decisions carried a flow key, and two IP packets were sent; this layer parsed the packet for policy and routing and then told the sequence nothing, so every flow shares one head-of-line queue with the control packs",
			valid, len(seen),
		)
	}
}
