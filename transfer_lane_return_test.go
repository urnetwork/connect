package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Provider returns preserve the peer's session class while choosing their own
// data lane from the flow key and advertised capability. Observe only this
// peer, retain the base of its written lane-zero generation, and force unrelated
// gate decisions before advertising so background traffic cannot choose it.
func TestProviderReturnsRideTheClientsLane(t *testing.T) {
	assertMessagePoolOwnership(t)

	// a loopback origin, so the NAT has something real to return from
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 8)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
			// server-first, so the provider has a return to make
			conn.Write([]byte("origin data"))
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-acceptDone
		for {
			select {
			case conn := <-accepted:
				conn.Close()
			default:
				return
			}
		}
	})
	originPort := listener.Addr().(*net.TCPAddr).Port

	// the two gates, read directly rather than inferred: the advertised
	// version recorded for the destination's base class, and whether the
	// returns carried a valid scheduling key
	type laneGateReading struct {
		lanes           map[uint32]int
		observations    []logicalLaneGateObservation
		recordedVersion uint32
		versionRecorded bool
	}
	observeLane := func(clientLane uint32, advertised bool) laneGateReading {
		peerId := NewId()
		initialWrites := make(chan sendSequenceId, 16)
		// the provider's own count, which is the setting a rollout turns on,
		// set before the client starts because the send loop reads its
		// settings from its own goroutine
		provider, localUserNat, client := newProviderSourceLifecycleTestFixtureWithClientSettings(
			t,
			NewNoContractClientOob(),
			func(settings *ClientSettings) {
				settings.SendBufferSettings.LogicalDataLaneCount = 8
				settings.SendBufferSettings.LaneFloorByteCount = ByteCount(256 * 1024)
				settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, _ uint64) {
					if id.Destination == peerId {
						select {
						case initialWrites <- id:
						default:
						}
					}
				}
			},
			nil,
		)

		var stateLock sync.Mutex
		observationMonitor := NewMonitor()
		observations := []logicalLaneGateObservation{}
		client.sendBuffer.logicalLaneGateObserverForTest.Store(
			&logicalLaneGateObserver{
				observe: func(observation logicalLaneGateObservation) {
					if observation.destination != peerId {
						return
					}
					stateLock.Lock()
					defer stateLock.Unlock()
					observations = append(observations, observation)
					observationMonitor.NotifyAll()
				},
			},
		)
		t.Cleanup(func() {
			client.sendBuffer.logicalLaneGateObserverForTest.Store(nil)
		})

		route := make(chan []byte, 256)
		// Same-peer control traffic can precede the first keyed return too.
		client.sendBuffer.selectLogicalLane(&SendPack{Destination: peerId})
		client.ContractManager().AddNoContractPeer(peerId)
		client.RouteManager().UpdateTransport(
			NewSendClientTransport(DestinationId(peerId)),
			[]Route{route},
		)
		drained := make(chan struct{})
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			for {
				select {
				case transferFrameBytes := <-route:
					MessagePoolReturn(transferFrameBytes)
				case <-drained:
					return
				}
			}
		}()
		t.Cleanup(func() {
			// The route's consumer outlives every producer; joining the owners
			// replaces the old sleep before the pool reconciliation.
			// The shared fixture's later cleanup repeats these idempotent joins.
			provider.Close()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if err := localUserNat.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close return-path nat: %v", err)
			}
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close return-path client: %v", err)
			}
			close(drained)
			<-drainDone
			for {
				select {
				case transferFrameBytes := <-route:
					MessagePoolReturn(transferFrameBytes)
				default:
					return
				}
			}
		})

		// the client's pack, carrying the lane it is itself on
		clientKey := TransferKey{
			LogicalLane:    clientLane,
			EncryptionRole: protocol.SequenceRole_SequenceRoleServer,
		}
		syn := MessagePoolCopy(craftSecurityPacket(
			IpProtocolTcp,
			net.ParseIP("192.0.2.13"),
			54321,
			net.ParseIP("127.0.0.1"),
			originPort,
			true,
			nil,
		))
		ipPath, err := ParseIpPath(syn)
		if err != nil {
			MessagePoolReturn(syn)
			t.Fatalf("parse the client SYN: %v", err)
		}
		withBorrowedMessage(syn, func(syn []byte) {
			provider.receiveTransferWithRecovery(
				SourceId(peerId),
				clientKey,
				protocol.ProvideMode_Public,
				receiveRecoveryModeTcpSocket,
				ipPath,
				syn,
			)
		})
		// Subscribe and inspect together. A keyed return for the written base,
		// and after advertisement the hashed gate, must satisfy the wait.
		waitForObservation := func(base sendSequenceId, bindingGate string) logicalLaneGateObservation {
			timeout := time.After(10 * time.Second)
			for {
				update, observation, found := func() (<-chan struct{}, logicalLaneGateObservation, bool) {
					stateLock.Lock()
					defer stateLock.Unlock()
					update := observationMonitor.NotifyChannel()
					for _, observation := range observations {
						if observation.schedulingValid && observation.base == base &&
							(bindingGate == "" || observation.bindingGate == bindingGate) {
							return update, observation, true
						}
					}
					return update, logicalLaneGateObservation{}, false
				}()
				if found {
					return observation
				}
				select {
				case <-update:
				case <-timeout:
					t.Fatalf("the provider's returns never reached gate %q for peer %s", bindingGate, peerId)
				}
			}
		}
		waitForWrite := func(dataLane bool) sendSequenceId {
			timeout := time.After(10 * time.Second)
			for {
				select {
				case id := <-initialWrites:
					if !dataLane || id.LogicalLane != 0 {
						return id
					}
				case <-timeout:
					t.Fatalf("the provider did not write a return to peer %s (data lane=%t)", peerId, dataLane)
				}
			}
		}
		base := waitForWrite(false).logicalLaneBase()
		waitForObservation(base, "")

		if advertised {
			// Force unrelated control and data decisions after the return. The
			// shared observer used to let the last one choose the advertised base.
			client.sendBuffer.selectLogicalLane(&SendPack{Destination: NewId()})
			client.sendBuffer.selectLogicalLane(&SendPack{
				Destination: NewId(), schedulingKey: ipSendSchedulingKey(ipPath),
			})
			// The destination's lane-zero class advertised support. In the
			// field this comes from a matching acknowledgement. Set it on the
			// written generation here to isolate the return gate; the capability
			// tests cover the negotiation that supplies it.
			stateLock.Lock()
			observations = nil
			observationMonitor.NotifyAll()
			stateLock.Unlock()
			client.sendBuffer.mutex.Lock()
			if client.sendBuffer.sendSequences[base] == nil {
				client.sendBuffer.mutex.Unlock()
				t.Fatalf("the written lane-zero generation is no longer live: %+v", base)
			}
			client.sendBuffer.logicalLaneVersions[base] = transferLogicalLaneVersion
			client.sendBuffer.publishLogicalLaneVersionsWithLock()
			client.sendBuffer.mutex.Unlock()

			secondSyn := MessagePoolCopy(craftSecurityPacket(
				IpProtocolTcp,
				net.ParseIP("192.0.2.13"),
				54322,
				net.ParseIP("127.0.0.1"),
				originPort,
				true,
				nil,
			))
			secondIpPath, err := ParseIpPath(secondSyn)
			if err != nil {
				MessagePoolReturn(secondSyn)
				t.Fatalf("parse the second client SYN: %v", err)
			}
			withBorrowedMessage(secondSyn, func(secondSyn []byte) {
				provider.receiveTransferWithRecovery(
					SourceId(peerId),
					clientKey,
					protocol.ProvideMode_Public,
					receiveRecoveryModeTcpSocket,
					secondIpPath,
					secondSyn,
				)
			})
			hashedObservation := waitForObservation(base, "hashed")
			writtenId := waitForWrite(true)
			if hashedObservation.base != base || writtenId.logicalLaneBase() != base {
				t.Fatalf("the advertised return changed sequence class: observed %+v, written %+v, want %+v",
					hashedObservation, writtenId, base)
			}
		}

		lanes := map[uint32]int{}
		func() {
			client.sendBuffer.mutex.Lock()
			defer client.sendBuffer.mutex.Unlock()
			for id := range client.sendBuffer.sendSequences {
				if id.Destination == peerId {
					lanes[id.LogicalLane] += 1
				}
			}
		}()
		recordedVersion, versionRecorded := func() (uint32, bool) {
			client.sendBuffer.mutex.Lock()
			defer client.sendBuffer.mutex.Unlock()
			version, recorded := client.sendBuffer.logicalLaneVersions[base]
			return version, recorded
		}()

		stateLock.Lock()
		defer stateLock.Unlock()
		return laneGateReading{
			lanes:           lanes,
			observations:    append([]logicalLaneGateObservation{}, observations...),
			recordedVersion: recordedVersion,
			versionRecorded: versionRecorded,
		}
	}

	zeroReading := observeLane(0, false)
	dataReading := observeLane(3, false)
	advertisedReading := observeLane(0, true)
	zeroLanes, zeroObservations := zeroReading.lanes, zeroReading.observations
	dataLanes, dataObservations := dataReading.lanes, dataReading.observations

	summarise := func(observations []logicalLaneGateObservation) map[string]int {
		gates := map[string]int{}
		for _, observation := range observations {
			gates[observation.bindingGate] += 1
		}
		return gates
	}
	t.Logf(
		"a client on lane 0: provider return sequences by lane %v, gates %v, advertised version recorded=%t value=%d",
		zeroLanes,
		summarise(zeroObservations),
		zeroReading.versionRecorded,
		zeroReading.recordedVersion,
	)
	t.Logf(
		"a client on lane 3: provider return sequences by lane %v, gates %v, advertised version recorded=%t value=%d",
		dataLanes,
		summarise(dataObservations),
		dataReading.versionRecorded,
		dataReading.recordedVersion,
	)
	for _, observation := range zeroObservations {
		t.Logf(
			"  gate: explicit=%t explicitLane=%d schedulingValid=%t version=%d binding=%q lane=%d",
			observation.explicit,
			observation.explicitLane,
			observation.schedulingValid,
			observation.version,
			observation.bindingGate,
			observation.lane,
		)
		break
	}

	// The provider's own gate must decide. Its hash may legitimately choose
	// the client's incoming lane, so inspect the decision instead of rejecting
	// a matching lane number. An explicit reply key would bypass this gate.
	explicitReturnCount := 0
	for _, observation := range append(zeroObservations, dataObservations...) {
		if observation.bindingGate == "explicit reply key" {
			explicitReturnCount += 1
		}
	}
	if 0 < explicitReturnCount {
		t.Errorf(
			"%d provider returns had their lane decided by the client's reply key rather than by the provider's own gate; a client on lane 0 drew returns on %v and a client on lane 3 drew returns on %v, so the provider's count is inert on the download path and a lane rollout needs a reply-key change beside the floor and the lock fix",
			explicitReturnCount,
			zeroLanes,
			dataLanes,
		)
	}

	// and the scheduling key is not what binds: the provider's return path
	// sets it from the flow, so it is valid on every return
	for _, observation := range zeroObservations {
		if observation.bindingGate == "no scheduling key" && observation.schedulingValid {
			t.Errorf("a return was refused a lane for want of a scheduling key it had")
		}
	}

	// The purpose of dropping the lane from the reply key, asserted rather
	// than inferred from the pin's absence: with the destination's support
	// advertised, the provider's own gate reaches its hash. Which lane it
	// picks is the hash's business, so the gate is what this asserts.
	hashedCount := 0
	for _, observation := range advertisedReading.observations {
		if observation.bindingGate == "hashed" {
			hashedCount += 1
		}
	}
	t.Logf(
		"an advertised destination: provider return sequences by lane %v, gates %v",
		advertisedReading.lanes,
		summarise(advertisedReading.observations),
	)
	if hashedCount <= 0 {
		t.Errorf(
			"no return reached the provider's hash with the destination's support advertised; gates were %v, so dropping the lane from the reply key has not made the provider's own count reachable on the download direction",
			summarise(advertisedReading.observations),
		)
	}
}

// THROUGHPUTFIX §30.3. Enabling a nonzero lane count used to add an
// acquisition of the send buffer's mutex to every Pack, a lock every sequence
// of the client shares, because the gate read the advertised version out of a
// map the buffer guards. A count of zero returned before it, which is why the
// cost appeared only when a count was set: a harness arm with the count at
// eight and no lane ever engaging ran 13 to 17 per cent below the same fixture
// at zero over six repetitions with overlapping distributions, and the
// mechanism rather than the statistics is what makes that credible.
//
// The observable is the acquisition and not the throughput, because a
// throughput row at that magnitude would be a timing test and would flake. The
// buffer mutex is held here while the gate runs: a gate that takes it cannot
// finish, and a lock-free one is unaffected.
func TestLaneCountGateDoesNotTakeTheBufferLockPerPack(t *testing.T) {
	assertMessagePoolOwnership(t)

	// set before the client starts: the send loop reads its settings from its
	// own goroutine, so a write after NewClient is a data race
	_, _, client := newProviderSourceLifecycleTestFixtureWithClientSettings(
		t,
		NewNoContractClientOob(),
		func(settings *ClientSettings) {
			settings.SendBufferSettings.LogicalDataLaneCount = 8
			settings.SendBufferSettings.LaneFloorByteCount = ByteCount(256 * 1024)
		},
		nil,
	)

	// a Pack shaped like a provider's return: a valid scheduling key and no
	// explicit lane, so the gate runs its whole path
	sendPack := &SendPack{
		Destination:   NewId(),
		schedulingKey: ipSendSchedulingKey(udpTestPath(4)),
	}
	if !sendPack.schedulingKey.valid {
		t.Fatal("the scheduling key is not valid, so the gate would return before the version is read")
	}

	gated := make(chan uint32, 1)
	var unlockOnce sync.Once
	client.sendBuffer.mutex.Lock()
	unlock := func() { unlockOnce.Do(client.sendBuffer.mutex.Unlock) }
	defer unlock()
	go func() {
		gated <- client.sendBuffer.selectLogicalLane(sendPack)
	}()
	select {
	case <-gated:
	case <-time.After(2 * time.Second):
		t.Error("the lane gate did not complete while the send buffer mutex was held; enabling a count puts a client-wide lock acquisition on every Pack, which is the 13 to 17 per cent the harness measured with no lane ever engaging")
	}
	unlock()
}
