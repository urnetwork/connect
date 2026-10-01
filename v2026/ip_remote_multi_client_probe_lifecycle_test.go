package connect

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The marker occupies existing padding. The optional diagnostic cannot grow
// any original Pack, retained ACK record, event-ring entry, or option record.
func TestProviderProbeLifecycleMarkerHasNoLayoutOrAllocationCost(t *testing.T) {
	for _, record := range []struct {
		typeOf reflect.Type
		field  string
	}{
		{reflect.TypeFor[SendPack](), "lifecycleHealthProbe"},
		{reflect.TypeFor[sendPackLifecycleRecord](), "healthProbe"},
		{reflect.TypeFor[SendPackLifecycleObservation](), "HealthProbe"},
		{reflect.TypeFor[resolvedSendOptions](), "healthProbe"},
	} {
		fields := make([]reflect.StructField, 0, record.typeOf.NumField()-1)
		for i := range record.typeOf.NumField() {
			field := record.typeOf.Field(i)
			if field.Name != record.field {
				fields = append(fields, reflect.StructField{Name: fmt.Sprintf("Field%d", i), Type: field.Type})
			}
		}
		previousSize := reflect.StructOf(fields).Size()
		if size := record.typeOf.Size(); size != previousSize {
			t.Errorf("%s marker grew size from %d to %d", record.typeOf, previousSize, size)
		} else {
			t.Logf("%s bytes=%d unchanged", record.typeOf, size)
		}
	}
	client := &Client{ctx: context.Background(), settings: DefaultClientSettings()}
	if count := testing.AllocsPerRun(100, func() {
		opts := [2]any{sendPackHealthProbeOption{}, ForceStream()}
		resolved := client.resolveSendOptions(opts[:])
		if !resolved.healthProbe {
			panic("lost marker")
		}
	}); count != 0 {
		t.Fatalf("probe options allocated %g times", count)
	}
}

// The real health-probe sender uses the same IP message type as application
// traffic. Its original one-second queue deadline must retain that producer
// identity through all three lifecycle phases, including a pre-wire expiry.
// An ordinary packet on the same sequence must never inherit the marker.
func TestProviderProbeLifecycleKeepsExactProducerThroughQueueExpiry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startSequence := make(chan struct{})
		observer, events := sendPackLifecycleTestObserver(destination)
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.SendPackLifecycleObserver = observer
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-startSequence:
				case <-ctx.Done():
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 4)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			drainFlightGateRoute(route)
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{route})
		channel := newPacketTransferTestChannel()
		channel.client = client
		channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
		path := icmpTcpTestPath(4)
		probePacket := probeSynPacket(path, 17)
		if !channel.sendProbe(&parsedPacket{packet: probePacket, ipPath: path}, probeSendTimeout) {
			MessagePoolReturn(probePacket)
			t.Fatal("probe was not admitted")
		}
		synctest.Wait()
		time.Sleep(2 * probeSendTimeout)
		close(startSequence)
		synctest.Wait()
		if len(route) != 0 || client.ReceiveStats().SendPackDeadlineDropCount != 1 {
			t.Fatal("probe did not expire before its first wire write")
		}
		if len(events) != 3 {
			t.Fatalf("probe lifecycle phases=%d, want 3", len(events))
		}
		var token uint64
		for _, phase := range []SendPackLifecyclePhase{
			SendPackLifecyclePhaseStarted, SendPackLifecyclePhaseFirstRouteWrite, SendPackLifecyclePhaseTerminal,
		} {
			event := <-events
			if token == 0 {
				token = event.Token
			}
			if event.Phase != phase || event.Token != token || !event.HealthProbe ||
				!event.AckRequired || event.MessageType != protocol.MessageType_IpIpPacketToProvider {
				t.Fatalf("probe lost exact producer identity: %+v", event)
			}
			if phase != SendPackLifecyclePhaseStarted && !errors.Is(event.Err, errSendPackExpiredUnwritten) {
				t.Fatalf("probe expiry changed: %v", event.Err)
			}
		}
		if _, err := channel.WindowStats(); err != nil {
			t.Fatalf("probe failure changed provider judgment: %v", err)
		}

		packet := MessagePoolGet(1000)
		clear(packet)
		if !channel.SendWithAck(&parsedPacket{packet: packet, ipPath: path}, time.Second, true) {
			MessagePoolReturn(packet)
			t.Fatal("application send refused after probe expiry")
		}
		synctest.Wait()
		if len(route) != 1 {
			t.Fatalf("application writes=%d, want 1", len(route))
		}
		wire := <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.Nack || pack.SequenceNumber != 0 {
			t.Fatal("probe expiry changed reliable application sequencing")
		}
		acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
		synctest.Wait()
		if len(events) != 3 {
			t.Fatalf("application lifecycle phases=%d, want 3", len(events))
		}
		for range 3 {
			event := <-events
			if event.HealthProbe || event.Token == token || event.Err != nil {
				t.Fatalf("application inherited probe identity/failure: %+v", event)
			}
		}
	})
}
