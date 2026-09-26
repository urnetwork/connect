package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The actual queue is full when provider selection makes its initial zero-
// timeout attempt. The same native call retains and successfully admits the
// input after a slot is released. Neither admission nor lifecycle is mocked.
func TestMultiClientPacketGroupAdmissionRetryLifecycle(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		name         string
		timeout      time.Duration
		udp          bool
		selected     bool
		cancel       bool
		admit        bool
		terminalErr  error
		wantRefusals int
	}{
		{name: "tcp recovered", timeout: -1, admit: true, wantRefusals: 1},
		{name: "udp recovered", timeout: -1, udp: true, admit: true, wantRefusals: 1},
		{name: "final refusal", timeout: 0, wantRefusals: 1},
		{name: "selected final refusal", timeout: 0, selected: true, wantRefusals: 1},
		{name: "exhausted retry", timeout: 20 * time.Millisecond, wantRefusals: 2},
		{name: "cancellation", timeout: -1, cancel: true, wantRefusals: 2},
		{name: "async terminal sentinel", timeout: -1, admit: true, terminalErr: ErrSendPackNotAdmitted, wantRefusals: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				destination := NewId()
				observe, events := sendPackLifecycleTestObserver(destination)
				settings := closeWaitClientSettings()
				settings.SendBufferSettings.SequenceBufferSize = 2
				settings.SendBufferSettings.SendPackLifecycleObserver = observe
				settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination != ControlId {
						<-ctx.Done()
					}
				}
				client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
				defer func() {
					cancel()
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Errorf("close admission retry client: %v", err)
					}
				}()
				queue := &sendPackCallerOwnerFixture{
					client: client,
					id:     sendSequenceId{Destination: destination, EncryptionRole: sequenceTlsRoleClient},
				}
				queue.fill(t)
				parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
				defer closeParent()
				channel := groupTestStalledChannel(parent.settings.ProtocolVersion)
				channel.stalled.Store(false)
				channel.client = client
				channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
				if test.selected {
					update.client.Store(channel)
				}
				parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
					return []*multiClientChannel{channel}
				}
				path := &IpPath{
					Version: 4, Protocol: IpProtocolTcp,
					SourceIp: net.IPv4(198, 51, 100, 10), SourcePort: 32100,
					DestinationIp: net.IPv4(203, 0, 113, 20), DestinationPort: 443,
				}
				packet := ipOosTcpPacketSequence(path, tcpFlagAck, 100, []byte{1})
				if test.udp {
					packet = ipOosUdpPacket(udpTestPath(4), []byte{1})
				}
				packets := [][]byte{MessagePoolCopy(packet)}
				witnesses := groupTestPacketWitnesses(t, packets)
				defer requireGroupTestWitnessesReleased(t, packets, witnesses)
				accepted := []bool{false}
				done := make(chan int, 1)
				go func() {
					done <- parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, test.timeout, accepted)
				}()
				synctest.Wait()
				if test.timeout != 0 {
					select {
					case count := <-done:
						t.Fatalf("native send returned before queue release: %d", count)
					default:
					}
				}
				// Release one real admission slot; the already-started retry owns the
				// same original packet and waits on this exact gate.
				if test.admit {
					filler := <-queue.sequence.packs
					filler.returnFrames()
					filler.releaseRaw()
				} else if test.cancel {
					closeParent()
					cancel()
				}
				wantCount := 0
				if test.admit {
					wantCount = 1
				}
				if count := <-done; count != wantCount || accepted[0] != test.admit {
					t.Fatalf("final native admission: count=%d accepted=%v", count, accepted)
				}
				if test.cancel {
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				// The paused worker has never written. Finish only the now-admitted
				// Pack via its real receipt so both attempt lifecycles can be inspected.
				for len(queue.sequence.packs) != 0 {
					pack := <-queue.sequence.packs
					if pack.lifecycleToken != 0 {
						pack.lifecycleRecord().firstRouteWrite(nil)
						pack.invokeAck(test.terminalErr)
					}
					pack.returnFrames()
					pack.releaseRaw()
				}
				refusals, successes, asyncFailures := 0, 0, 0
				phases := map[uint64]SendPackLifecyclePhase{}
				for len(events) != 0 {
					event := <-events
					if event.Phase != phases[event.Token]+1 {
						t.Errorf("token %d phase order: prior=%d current=%d", event.Token, phases[event.Token], event.Phase)
					}
					phases[event.Token] = event.Phase
					if event.Phase != SendPackLifecyclePhaseTerminal {
						continue
					}
					if event.Err == nil {
						successes++
						continue
					}
					var admissionErr *SendPackAdmissionError
					if !errors.As(event.Err, &admissionErr) {
						asyncFailures++
						if test.terminalErr == nil || event.Err != test.terminalErr {
							t.Errorf("unexpected async terminal: %v", event.Err)
						}
						continue
					}
					refusals++
					if !test.cancel && !errors.Is(event.Err, ErrSendPackNotAdmitted) {
						t.Errorf("refusal lost sentinel: %v", event.Err)
					}
					if refusals == 1 && !strings.Contains(event.Err.Error(), "stage=enqueue boundary=pack-admission timeout=0s") {
						t.Errorf("missing exact admission stage: %v", event.Err)
					}
					if admissionErr.RecoveredByOwner != test.admit {
						t.Errorf("native final admission=%t, correlated refusal=%v", test.admit, event.Err)
					}
					// A counter keyed only on terminal Err sees this as a failure even
					// though this native call proved final admission of the same input.
					t.Logf("native admission retained failed attempt: %+v", event)
				}
				wantSuccess, wantAsync := 0, 0
				if test.admit {
					if test.terminalErr == nil {
						wantSuccess = 1
					} else {
						wantAsync = 1
					}
				}
				if refusals != test.wantRefusals || successes != wantSuccess || asyncFailures != wantAsync {
					t.Fatalf("native retry lifecycle: refusals=%d successes=%d async=%d", refusals, successes, asyncFailures)
				}
				for token, phase := range phases {
					if phase != SendPackLifecyclePhaseTerminal {
						t.Errorf("token %d left at phase %d", token, phase)
					}
				}
			})
		})
	}
}

// Same Client, destination and input tuple, but distinct call/input owners.
// Success in the first call cannot bless the other call's final refusal.
func TestMultiClientPacketGroupAdmissionConcurrentOwnerIsolation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		observe, events := sendPackLifecycleTestObserver(destination)
		settings := closeWaitClientSettings()
		settings.SendBufferSettings.SequenceBufferSize = 2
		settings.SendBufferSettings.SendPackLifecycleObserver = observe
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination != ControlId {
				<-ctx.Done()
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		queue := &sendPackCallerOwnerFixture{client: client, id: sendSequenceId{Destination: destination, EncryptionRole: sequenceTlsRoleClient}}
		queue.fill(t)
		makeParent := func() *RemoteUserNatMultiClient {
			parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
			t.Cleanup(closeParent)
			channel := groupTestStalledChannel(parent.settings.ProtocolVersion)
			channel.stalled.Store(false)
			channel.client = client
			channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
			parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return []*multiClientChannel{channel} }
			return parent
		}
		first, second := makeParent(), makeParent()
		packet := ipOosUdpPacket(udpTestPath(4), []byte{1})
		firstPackets, secondPackets := [][]byte{MessagePoolCopy(packet)}, [][]byte{MessagePoolCopy(packet)}
		firstWitness, secondWitness := groupTestPacketWitnesses(t, firstPackets), groupTestPacketWitnesses(t, secondPackets)
		defer requireGroupTestWitnessesReleased(t, firstPackets, firstWitness)
		defer requireGroupTestWitnessesReleased(t, secondPackets, secondWitness)
		done := make(chan int, 1)
		go func() {
			done <- first.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, firstPackets, -1)
		}()
		synctest.Wait()
		var firstRefusalToken uint64
		for len(events) != 0 {
			event := <-events
			if event.Phase == SendPackLifecyclePhaseTerminal {
				t.Fatalf("terminal published before retained owner outcome: %+v", event)
			}
			if event.Phase == SendPackLifecyclePhaseFirstRouteWrite && event.Err != nil {
				firstRefusalToken = event.Token
			}
		}
		if firstRefusalToken == 0 {
			t.Fatal("first call did not hit real full queue")
		}
		if count := second.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, secondPackets, 0); count != 0 {
			t.Fatalf("second call admitted: %d", count)
		}
		filler := <-queue.sequence.packs
		filler.returnFrames()
		filler.releaseRaw()
		if count := <-done; count != 1 {
			t.Fatalf("first call did not admit after release: %d", count)
		}
		for len(queue.sequence.packs) != 0 {
			pack := <-queue.sequence.packs
			if pack.lifecycleToken != 0 {
				pack.lifecycleRecord().firstRouteWrite(nil)
				pack.invokeAck(nil)
			}
			pack.returnFrames()
			pack.releaseRaw()
		}
		recovered, refused := 0, 0
		for len(events) != 0 {
			event := <-events
			if event.Phase != SendPackLifecyclePhaseTerminal || event.Err == nil {
				continue
			}
			admissionErr, ok := event.Err.(*SendPackAdmissionError)
			if !ok {
				t.Fatalf("unexpected final error: %v", event.Err)
			}
			if admissionErr.RecoveredByOwner {
				recovered++
				if event.Token != firstRefusalToken {
					t.Fatal("another call's refusal inherited successful admission")
				}
			} else {
				refused++
				if event.Token == firstRefusalToken {
					t.Fatal("retained input lost its own successful admission")
				}
			}
		}
		if recovered != 1 || refused != 1 {
			t.Fatalf("owner-isolated terminals recovered=%d refused=%d", recovered, refused)
		}
	})
}

// Concurrent candidate publications remain bounded and fail closed when the
// optional diagnostic cannot retain more history. No packet bytes are stored.
func TestMultiClientAdmissionObservationBoundedCandidates(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		scope := &sendPackAdmissionObservations{}
		events := make(chan SendPackLifecycleObservation, sendPackAdmissionObservationCapacity+1)
		observer := scope.wrap(func(event SendPackLifecycleObservation) { events <- event })
		var group sync.WaitGroup
		for token := 1; token <= sendPackAdmissionObservationCapacity+1; token++ {
			group.Add(1)
			go func(token int) {
				defer group.Done()
				observer(SendPackLifecycleObservation{Phase: SendPackLifecyclePhaseTerminal, Token: uint64(token), Err: &SendPackAdmissionError{Boundary: "pack-admission", Err: ErrSendPackNotAdmitted}})
			}(token)
		}
		group.Wait()
		if len(scope.pending) != sendPackAdmissionObservationCapacity || len(events) != 1 {
			t.Fatalf("unbounded pending=%d published=%d", len(scope.pending), len(events))
		}
		overflow := <-events
		if err := overflow.Err.(*SendPackAdmissionError); err.RecoveredByOwner || !err.OwnerTrackingOverflow {
			t.Fatalf("overflow did not fail closed: %v", err)
		}
		scope.complete(accepted)
		seen := map[uint64]bool{overflow.Token: true}
		for len(events) != 0 {
			event := <-events
			if seen[event.Token] {
				t.Fatal("duplicated candidate terminal")
			}
			seen[event.Token] = true
			if event.Err.(*SendPackAdmissionError).RecoveredByOwner != accepted {
				t.Fatal("candidate owner outcome changed")
			}
		}
		if len(seen) != sendPackAdmissionObservationCapacity+1 {
			t.Fatal("lost candidate terminal")
		}
	}
}

func TestMultiClientAdmissionObserverDisabledAllocations(t *testing.T) {
	client := &multiClientChannel{client: &Client{settings: DefaultClientSettings()}}
	group := &parsedPacketGroup{}
	if allocations := testing.AllocsPerRun(100, func() { group.prepareAdmissionObservations(client) }); allocations != 0 || group.admissionObservations != nil {
		t.Fatalf("nil-observer admission added allocations=%g scope=%p", allocations, group.admissionObservations)
	}
}
