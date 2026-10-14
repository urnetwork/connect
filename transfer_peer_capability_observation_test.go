package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Returns a recorder whose clock steps one second per observation from a
// fixed origin, and the time of the nth (0-based) observation, so every
// recorded time is exact.
func newSteppedPeerCapabilityRecorderForTest(maxPeerCount int) (*PeerCapabilityRecorder, func(n int) time.Time) {
	origin := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	recorder := NewPeerCapabilityRecorder(maxPeerCount)
	var stepLock sync.Mutex
	step := 0
	recorder.nowForTest = func() time.Time {
		stepLock.Lock()
		defer stepLock.Unlock()
		at := origin.Add(time.Duration(step) * time.Second)
		step += 1
		return at
	}
	return recorder, func(n int) time.Time {
		return origin.Add(time.Duration(n) * time.Second)
	}
}

func requirePeerCapabilityObservation(t *testing.T, recorder *PeerCapabilityRecorder, want PeerCapabilityObservation) {
	t.Helper()
	got, ok := recorder.Get(want.PeerId)
	if !ok {
		t.Fatalf("no evidence was recorded for %s", want.PeerId)
	}
	if got != want {
		t.Fatalf("evidence\n got %+v\nwant %+v", got, want)
	}
}

// The prober's positive case: a ClientHello, a handshake that negotiated the
// hybrid group, and the peer's identity proof sealing that epoch.
func TestPeerCapabilityRecorderRecordPostQuantumSession(t *testing.T) {
	recorder, at := newSteppedPeerCapabilityRecorderForTest(0)
	peerId := NewId()
	if _, ok := recorder.Get(peerId); ok {
		t.Fatal("a peer nothing was observed for has a record")
	}

	recorder.observeE2eAttempt(peerId)
	recorder.observeE2eHandshake(peerId, tls.X25519MLKEM768)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eAttempted:        true,
		E2eFirstAttemptTime: at(0),
		E2eNegotiated:       true,
		KeyExchange:         "X25519MLKEM768",
		PostQuantum:         true,
		LastObservationTime: at(1),
	})

	recorder.observeE2eSealed(peerId, tls.X25519MLKEM768)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eAttempted:        true,
		E2eFirstAttemptTime: at(0),
		E2eNegotiated:       true,
		E2eSealed:           true,
		KeyExchange:         "X25519MLKEM768",
		PostQuantum:         true,
		LastObservationTime: at(2),
	})
}

// A peer that seals but negotiates only the classical group is e2e capable
// and not post-quantum.
func TestPeerCapabilityRecorderRecordClassicalSession(t *testing.T) {
	recorder, at := newSteppedPeerCapabilityRecorderForTest(0)
	peerId := NewId()

	recorder.observeE2eAttempt(peerId)
	recorder.observeE2eHandshake(peerId, tls.X25519)
	recorder.observeE2eSealed(peerId, tls.X25519)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eAttempted:        true,
		E2eFirstAttemptTime: at(0),
		E2eNegotiated:       true,
		E2eSealed:           true,
		KeyExchange:         "X25519",
		PostQuantum:         false,
		LastObservationTime: at(2),
	})
}

// A peer that never answers the ClientHello: attempted, with the first
// attempt's time kept across a retry, and no session evidence.
func TestPeerCapabilityRecorderRecordUnansweredAttempt(t *testing.T) {
	recorder, at := newSteppedPeerCapabilityRecorderForTest(0)
	peerId := NewId()

	recorder.observeE2eAttempt(peerId)
	recorder.observeE2eAttempt(peerId)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eAttempted:        true,
		E2eFirstAttemptTime: at(0),
		LastObservationTime: at(1),
	})
}

// Acknowledgements alone (no session attempted, as under EncryptionModeOff)
// say nothing about the end-to-end session.
func TestPeerCapabilityRecorderRecordAcknowledgementsWithoutSession(t *testing.T) {
	recorder, at := newSteppedPeerCapabilityRecorderForTest(0)
	peerId := NewId()

	recorder.observeAck(peerId)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		AckObserved:         true,
		LastObservationTime: at(0),
	})
}

// Before any seal the latest handshake's group is reported. A seal's group
// replaces it and is not replaced by a later unauthenticated handshake; only a
// later seal replaces it.
func TestPeerCapabilityRecorderSealedKeyExchangeOutranksLaterHandshake(t *testing.T) {
	recorder, at := newSteppedPeerCapabilityRecorderForTest(0)
	peerId := NewId()

	recorder.observeE2eHandshake(peerId, tls.X25519MLKEM768)
	recorder.observeE2eHandshake(peerId, tls.X25519)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eNegotiated:       true,
		KeyExchange:         "X25519",
		LastObservationTime: at(1),
	})

	recorder.observeE2eSealed(peerId, tls.X25519MLKEM768)
	recorder.observeE2eHandshake(peerId, tls.X25519)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eNegotiated:       true,
		E2eSealed:           true,
		KeyExchange:         "X25519MLKEM768",
		PostQuantum:         true,
		LastObservationTime: at(3),
	})

	recorder.observeE2eSealed(peerId, tls.X25519)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eNegotiated:       true,
		E2eSealed:           true,
		KeyExchange:         "X25519",
		LastObservationTime: at(4),
	})
}

// Every ML-KEM group, hybrid or pure, is post-quantum by codepoint, whether or
// not the toolchain names it; the classical groups and none are not.
func TestPostQuantumKeyExchangeClassification(t *testing.T) {
	cases := []struct {
		curveId     tls.CurveID
		postQuantum bool
	}{
		{curveId: tls.X25519MLKEM768, postQuantum: true},
		{curveId: tlsKeyExchangeSecP256r1MlKem768, postQuantum: true},
		{curveId: tlsKeyExchangeSecP384r1MlKem1024, postQuantum: true},
		{curveId: tlsKeyExchangeMlKem512, postQuantum: true},
		{curveId: tlsKeyExchangeMlKem768, postQuantum: true},
		{curveId: tlsKeyExchangeMlKem1024, postQuantum: true},
		{curveId: tls.X25519, postQuantum: false},
		{curveId: tls.CurveP256, postQuantum: false},
		{curveId: tls.CurveP384, postQuantum: false},
		{curveId: tls.CurveP521, postQuantum: false},
		{curveId: 0, postQuantum: false},
	}
	for _, c := range cases {
		if got := isPostQuantumKeyExchange(c.curveId); got != c.postQuantum {
			t.Errorf("group %d post-quantum = %t, want %t", uint16(c.curveId), got, c.postQuantum)
		}
	}
	names := []struct {
		curveId tls.CurveID
		name    string
	}{
		{curveId: tls.X25519MLKEM768, name: "X25519MLKEM768"},
		{curveId: tls.X25519, name: "X25519"},
		{curveId: 0, name: ""},
	}
	for _, c := range names {
		if got := tlsKeyExchangeName(c.curveId); got != c.name {
			t.Errorf("group %d name = %q, want %q", uint16(c.curveId), got, c.name)
		}
	}
}

// An acknowledgement without the field is the legacy reading; zero is a real
// advertisement; the record keeps the largest value any sequence advertised.
func TestPeerCapabilityRecorderRecordReceiveWindow(t *testing.T) {
	recorder, at := newSteppedPeerCapabilityRecorderForTest(0)
	peerId := NewId()

	recorder.observeAck(peerId)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		AckObserved:         true,
		LastObservationTime: at(0),
	})

	recorder.observeReceiveWindow(peerId, 0)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		AckObserved:         true,
		WindowAdvertised:    true,
		LastObservationTime: at(1),
	})

	recorder.observeReceiveWindow(peerId, 8*1024*1024)
	// a second sequence's first advertisement can be smaller
	recorder.observeReceiveWindow(peerId, 4*1024*1024)
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:                    peerId,
		AckObserved:               true,
		WindowAdvertised:          true,
		MaxReceiveWindowByteCount: 8 * 1024 * 1024,
		LastObservationTime:       at(3),
	})
}

// The bound refuses a new peer, counts it, and keeps updating the peers it
// holds. The control plane is never a peer. Reads are ordered copies.
func TestPeerCapabilityRecorderBoundAndControlPlane(t *testing.T) {
	recorder := NewPeerCapabilityRecorder(2)
	peerIds := []Id{NewId(), NewId()}
	pastBoundPeerId := NewId()

	for _, peerId := range peerIds {
		recorder.observeAck(peerId)
	}
	recorder.observeAck(pastBoundPeerId)
	recorder.observeReceiveWindow(ControlId, 1)
	recorder.observeE2eAttempt(ControlId)

	if _, ok := recorder.Get(pastBoundPeerId); ok {
		t.Fatal("a peer past the bound was recorded")
	}
	if _, ok := recorder.Get(ControlId); ok {
		t.Fatal("the control plane was recorded as a peer")
	}
	if dropped := recorder.DroppedPeerCount(); dropped != 1 {
		t.Fatalf("dropped peer count = %d, want 1 (the control plane is ignored, not dropped)", dropped)
	}

	recorder.observeReceiveWindow(peerIds[0], 4096)
	if observation, _ := recorder.Get(peerIds[0]); !observation.WindowAdvertised || observation.MaxReceiveWindowByteCount != 4096 {
		t.Fatalf("a held peer stopped updating at the bound: %+v", observation)
	}

	slices.SortFunc(peerIds, func(a Id, b Id) int {
		return a.Cmp(b)
	})
	snapshot := recorder.Snapshot()
	if len(snapshot) != len(peerIds) {
		t.Fatalf("snapshot holds %d peers, want %d", len(snapshot), len(peerIds))
	}
	for i, observation := range snapshot {
		if observation.PeerId != peerIds[i] {
			t.Fatalf("snapshot position %d is %s, want %s (ordered by peer id)", i, observation.PeerId, peerIds[i])
		}
	}
	snapshot[0].AckObserved = false
	if observation, _ := recorder.Get(snapshot[0].PeerId); !observation.AckObserved {
		t.Fatal("a snapshot aliases the recorded evidence")
	}
}

// A nil recorder is inert, and the zero value records with the default bound.
func TestPeerCapabilityRecorderNilAndZeroValue(t *testing.T) {
	var nilRecorder *PeerCapabilityRecorder
	peerId := NewId()
	nilRecorder.observeE2eAttempt(peerId)
	nilRecorder.observeE2eHandshake(peerId, tls.X25519MLKEM768)
	nilRecorder.observeE2eSealed(peerId, tls.X25519MLKEM768)
	nilRecorder.observeAck(peerId)
	nilRecorder.observeReceiveWindow(peerId, 4096)
	if _, ok := nilRecorder.Get(peerId); ok {
		t.Fatal("a nil recorder returned evidence")
	}
	if snapshot := nilRecorder.Snapshot(); snapshot != nil {
		t.Fatalf("a nil recorder returned a snapshot: %+v", snapshot)
	}
	if dropped := nilRecorder.DroppedPeerCount(); dropped != 0 {
		t.Fatalf("a nil recorder dropped %d peers", dropped)
	}

	var zero PeerCapabilityRecorder
	for range defaultPeerCapabilityRecorderMaxPeerCount + 1 {
		zero.observeAck(NewId())
	}
	if n := len(zero.Snapshot()); n != defaultPeerCapabilityRecorderMaxPeerCount {
		t.Fatalf("the zero value holds %d peers, want the default bound %d", n, defaultPeerCapabilityRecorderMaxPeerCount)
	}
	if dropped := zero.DroppedPeerCount(); dropped != 1 {
		t.Fatalf("the zero value dropped %d peers, want 1", dropped)
	}
	if NewPeerCapabilityRecorder(0).maxPeerCount != defaultPeerCapabilityRecorderMaxPeerCount {
		t.Fatal("a non-positive bound did not take the default")
	}
}

// Writers on every recording edge race readers of Get and Snapshot. Every read
// is one consistent record, the largest window never moves backwards for a
// reader, and because the writes commute the final state is exact.
func TestPeerCapabilityRecorderConcurrentReadsAndWrites(t *testing.T) {
	recorder := NewPeerCapabilityRecorder(0)
	peerIds := []Id{NewId(), NewId(), NewId()}
	const writerCount = 4
	const readerCount = 4
	const roundCount = 200

	start := make(chan struct{})
	violations := make(chan string, readerCount)
	var group sync.WaitGroup
	for writer := range writerCount {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for round := range roundCount {
				for _, peerId := range peerIds {
					recorder.observeE2eAttempt(peerId)
					recorder.observeE2eHandshake(peerId, tls.X25519)
					recorder.observeE2eSealed(peerId, tls.X25519MLKEM768)
					recorder.observeAck(peerId)
					recorder.observeReceiveWindow(peerId, ByteCount(writer*roundCount+round))
				}
			}
		}()
	}
	for range readerCount {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			report := func(violation string) {
				select {
				case violations <- violation:
				default:
				}
			}
			lastPeerIdMaxes := map[Id]ByteCount{}
			check := func(observation PeerCapabilityObservation) {
				if observation.E2eSealed && (!observation.E2eNegotiated || observation.KeyExchange != "X25519MLKEM768") {
					report(fmt.Sprintf("a sealed record without its handshake or its sealed group: %+v", observation))
				}
				if observation.PostQuantum != (observation.KeyExchange == "X25519MLKEM768") {
					report(fmt.Sprintf("post-quantum disagrees with the key exchange: %+v", observation))
				}
				if observation.WindowAdvertised && !observation.AckObserved {
					report(fmt.Sprintf("an advertisement without an acknowledgement: %+v", observation))
				}
				if observation.MaxReceiveWindowByteCount < lastPeerIdMaxes[observation.PeerId] {
					report(fmt.Sprintf("the largest window moved backwards to %d: %+v", lastPeerIdMaxes[observation.PeerId], observation))
				}
				lastPeerIdMaxes[observation.PeerId] = observation.MaxReceiveWindowByteCount
			}
			for range roundCount {
				for _, observation := range recorder.Snapshot() {
					check(observation)
				}
				for _, peerId := range peerIds {
					if observation, ok := recorder.Get(peerId); ok {
						check(observation)
					}
				}
			}
		}()
	}
	close(start)
	group.Wait()
	select {
	case violation := <-violations:
		t.Fatal(violation)
	default:
	}

	for _, peerId := range peerIds {
		observation, ok := recorder.Get(peerId)
		if !ok {
			t.Fatalf("no evidence for %s", peerId)
		}
		want := PeerCapabilityObservation{
			PeerId:                    peerId,
			E2eAttempted:              true,
			E2eFirstAttemptTime:       observation.E2eFirstAttemptTime,
			E2eNegotiated:             true,
			E2eSealed:                 true,
			KeyExchange:               "X25519MLKEM768",
			PostQuantum:               true,
			AckObserved:               true,
			WindowAdvertised:          true,
			MaxReceiveWindowByteCount: ByteCount(writerCount*roundCount - 1),
			LastObservationTime:       observation.LastObservationTime,
		}
		if observation != want || observation.E2eFirstAttemptTime.IsZero() || observation.LastObservationTime.IsZero() {
			t.Fatalf("final evidence\n got %+v\nwant %+v", observation, want)
		}
	}
}

// A real receiver's acknowledgement reaches the sender's recorder through the
// production path, for a receiver that advertises its window and one that
// does not (the legacy peer), on both ack wire encodings.
func TestAcknowledgementsRecordReceiveWindowEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	cases := []struct {
		advertise bool
		version   int
	}{
		{advertise: false, version: 1},
		{advertise: false, version: 2},
		{advertise: true, version: 1},
		{advertise: true, version: 2},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			recorder := NewPeerCapabilityRecorder(0)
			fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, c.version, NewNoopLogger(), func(settings *ClientSettings) {
				settings.PeerCapabilityRecorder = recorder
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = c.advertise
			})
			receiverId := fixture.receiver.ClientId()

			pack := fixture.write(64)
			if _, ok := recorder.Get(receiverId); ok {
				t.Fatalf("v%d advertise=%t: evidence before the peer acknowledged anything", c.version, c.advertise)
			}
			ack := fixture.receive(pack)
			if (ack.ack.ReceiveWindowByteCount != nil) != c.advertise {
				t.Fatalf("v%d advertise=%t: the receiver's wire ack carried the window = %t", c.version, c.advertise, ack.ack.ReceiveWindowByteCount != nil)
			}
			fixture.forward(ack, fixture.senderIn)

			observation, ok := recorder.Get(receiverId)
			if !ok {
				t.Fatalf("v%d advertise=%t: the acknowledgement recorded no evidence", c.version, c.advertise)
			}
			want := PeerCapabilityObservation{
				PeerId:              receiverId,
				AckObserved:         true,
				WindowAdvertised:    c.advertise,
				LastObservationTime: observation.LastObservationTime,
			}
			if c.advertise {
				want.MaxReceiveWindowByteCount = ByteCount(*ack.ack.ReceiveWindowByteCount)
				if want.MaxReceiveWindowByteCount <= 0 {
					t.Fatalf("v%d: the fixture advertised no capacity, so the value proves nothing", c.version)
				}
			}
			if observation != want || observation.LastObservationTime.IsZero() {
				t.Fatalf("v%d advertise=%t: evidence\n got %+v\nwant %+v", c.version, c.advertise, observation, want)
			}
		})
	}
}

// Once the peer's record exists, the acknowledgement path allocates nothing for
// it: the edges that record have passed, so a steady acknowledgement stream with
// an unchanged advertised window reaches no recording call at all.
func TestAcknowledgementEvidenceDoesNotAllocatePerAcknowledgement(t *testing.T) {
	recorder := NewPeerCapabilityRecorder(0)
	recordingCount := 0
	recorder.afterObserveForTest = func(Id) {
		recordingCount += 1
	}
	peerId := NewId()
	sequence := &SendSequence{
		client:                 &Client{},
		destination:            peerId,
		resendQueue:            newResendQueue(nil, 0),
		peerCapabilityRecorder: recorder,
	}
	item := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 7},
		expectsAck:   true, sendTime: time.Now(), ackTimeout: 30 * time.Second,
	}
	sequence.addResendItem(item)
	window := newSequenceAckWindow()
	ack := receiveAckMessage{
		messageId:              item.messageId,
		receiveWindowSet:       true,
		receiveWindowByteCount: 2 * 1024 * 1024,
	}

	sequence.coalesceReceivedAck(window, ack)
	if observation, _ := recorder.Get(peerId); !observation.AckObserved || !observation.WindowAdvertised ||
		observation.MaxReceiveWindowByteCount != 2*1024*1024 {
		t.Fatalf("the first acknowledgement recorded %+v", observation)
	}
	// the first acknowledgement and the first advertisement, one call each
	if recordingCount != 2 {
		t.Fatalf("the first acknowledgement made %d recording calls, want 2", recordingCount)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		sequence.detachResendItem(item.messageId)
		sequence.coalesceReceivedAck(window, ack)
		sequence.addResendItem(item)
	})
	if allocs != 0 {
		t.Fatalf("an acknowledgement with evidence recording allocated %g objects", allocs)
	}
	if recordingCount != 2 {
		t.Fatalf("a steady acknowledgement stream made %d more recording calls, want none", recordingCount-2)
	}
	sequence.ackLifetimes.clear()
	sequence.resendQueue.Clear()
}

// Completes one real TLS 1.3 handshake between the sequence client and server
// configs over an in-memory pipe. serverCurves, when set, replaces the
// server's group preference, which models a peer without the hybrid group: the
// client offers X25519MLKEM768 first and the server picks what it supports.
func completedSequenceTlsConnsForTest(t *testing.T, serverCurves []tls.CurveID) (*tls.Conn, *tls.Conn) {
	t.Helper()
	serverConfig, err := DefaultSequenceServerTlsConfig()
	if err != nil {
		t.Fatalf("server tls config: %v", err)
	}
	if serverCurves != nil {
		serverConfig.CurvePreferences = serverCurves
	}
	clientCertificate, err := generateSequenceTlsCertificate()
	if err != nil {
		t.Fatalf("client certificate: %v", err)
	}
	clientConfig := DefaultSequenceClientTlsConfig()
	// the server requires a client certificate (mutual TLS)
	clientConfig.Certificates = []tls.Certificate{clientCertificate}
	clientPipe, serverPipe := net.Pipe()
	t.Cleanup(func() {
		clientPipe.Close()
		serverPipe.Close()
	})
	clientConn := tls.Client(clientPipe, clientConfig)
	serverConn := tls.Server(serverPipe, serverConfig)
	// a liveness bound only: the negotiated group does not depend on time
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serverConn.HandshakeContext(ctx)
	}()
	if err := clientConn.HandshakeContext(ctx); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return clientConn, serverConn
}

// A client that opts in, with encryption sessions in the given mode and its
// background key publication parked, closed and joined at cleanup.
func newPeerCapabilityClientForTest(t *testing.T, mode EncryptionMode, recorder *PeerCapabilityRecorder) *Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = mode
	settings.PeerCapabilityRecorder = recorder
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close capability client: %v", err)
		}
	})
	return client
}

// completeHandshake and the identity-proof seal record the group a real TLS
// handshake negotiated, in both TLS roles: the hybrid against a peer that
// supports it, the classical group against one that does not.
func TestEndToEndHandshakeRecordsItsKeyExchange(t *testing.T) {
	cases := []struct {
		role         sequenceTlsRole
		serverCurves []tls.CurveID
		keyExchange  string
		postQuantum  bool
	}{
		{role: sequenceTlsRoleClient, serverCurves: nil, keyExchange: "X25519MLKEM768", postQuantum: true},
		{role: sequenceTlsRoleServer, serverCurves: nil, keyExchange: "X25519MLKEM768", postQuantum: true},
		{role: sequenceTlsRoleClient, serverCurves: []tls.CurveID{tls.X25519}, keyExchange: "X25519", postQuantum: false},
		{role: sequenceTlsRoleServer, serverCurves: []tls.CurveID{tls.X25519}, keyExchange: "X25519", postQuantum: false},
	}
	for _, c := range cases {
		recorder := NewPeerCapabilityRecorder(0)
		client := newPeerCapabilityClientForTest(t, EncryptionModeOpportunistic, recorder)
		peerId := NewId()
		clientConn, serverConn := completedSequenceTlsConnsForTest(t, c.serverCurves)
		tlsConn := clientConn
		if c.role == sequenceTlsRoleServer {
			tlsConn = serverConn
		}

		// Acquire starts no epoch; install the state runHandshake leaves just
		// before it calls completeHandshake
		session := client.EncryptionSessionManager().Acquire(peerId, c.role, false)
		if session == nil {
			t.Fatalf("%s: no session under Opportunistic", c.role)
		}
		// bound to the session, as buildAndStartEpochWithLock binds an epoch
		epochCtx, epochCancel := context.WithCancel(session.ctx)
		t.Cleanup(epochCancel)
		e := &tlsHandshakeEpoch{
			ctx:               epochCtx,
			cancel:            epochCancel,
			tlsConn:           tlsConn,
			handshakeDone:     make(chan struct{}),
			establishmentDone: make(chan struct{}),
		}
		func() {
			session.stateLock.Lock()
			defer session.stateLock.Unlock()
			session.epoch = e
		}()

		session.completeHandshake(e, nil)
		observation, ok := recorder.Get(peerId)
		if !ok || !observation.E2eNegotiated || observation.E2eSealed ||
			observation.KeyExchange != c.keyExchange || observation.PostQuantum != c.postQuantum {
			t.Fatalf("%s %s: after the handshake, evidence %+v (recorded=%t)", c.role, c.keyExchange, observation, ok)
		}

		// the peer's identity proof over this epoch's exporter seals it
		peerPublicKey, peerPrivateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		exporter := func() []byte {
			session.stateLock.Lock()
			defer session.stateLock.Unlock()
			return e.tlsExporter
		}()
		session.SetPeerClientPublicKey(peerPublicKey)
		session.receivePeerIdentityProof(ed25519.Sign(peerPrivateKey, exporter))
		observation, _ = recorder.Get(peerId)
		if !observation.E2eNegotiated || !observation.E2eSealed ||
			observation.KeyExchange != c.keyExchange || observation.PostQuantum != c.postQuantum {
			t.Fatalf("%s %s: after the seal, evidence %+v", c.role, c.keyExchange, observation)
		}

		// the live per-session view agrees with the evidence
		agrees := false
		for _, state := range client.EncryptionSessionManager().PeerEncryptionStates() {
			if state.PeerId == peerId {
				agrees = state.Sealed && tlsKeyExchangeName(state.KeyExchange) == observation.KeyExchange
			}
		}
		if !agrees {
			t.Fatalf("%s %s: PeerEncryptionStates disagrees with the evidence", c.role, c.keyExchange)
		}
		session.Release()
	}
}

// A client-role send acquisition starts a real epoch whose ClientHello is
// recorded as an attempt. With no peer to answer it, that is all the evidence
// says: the shape a provider without the responder leaves.
func TestClientHelloRecordsEndToEndAttempt(t *testing.T) {
	recorder := NewPeerCapabilityRecorder(0)
	attemptedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	recorder.nowForTest = func() time.Time { return attemptedAt }
	observedPeerIds := make(chan Id, 8)
	recorder.afterObserveForTest = func(peerId Id) {
		select {
		case observedPeerIds <- peerId:
		default:
		}
	}
	client := newPeerCapabilityClientForTest(t, EncryptionModeOpportunistic, recorder)
	peerId := NewId()

	session := client.EncryptionSessionManager().AcquireForSend(peerId, sequenceTlsRoleClient, false, false, false)
	if session == nil {
		t.Fatal("no client-role session under Opportunistic")
	}
	defer session.Release()
	select {
	case observedPeerId := <-observedPeerIds:
		if observedPeerId != peerId {
			t.Fatalf("the first observation named %s, want the peer %s", observedPeerId, peerId)
		}
	// a liveness bound only; the barrier above is the proof
	case <-time.After(30 * time.Second):
		t.Fatal("the client-role epoch handed no ClientHello to the transfer path")
	}
	requirePeerCapabilityObservation(t, recorder, PeerCapabilityObservation{
		PeerId:              peerId,
		E2eAttempted:        true,
		E2eFirstAttemptTime: attemptedAt,
		LastObservationTime: attemptedAt,
	})
}

// Under EncryptionModeOff a recorder starts no session and so records no
// end-to-end evidence, while a send sequence still captures it from its client
// and records acknowledgement evidence.
func TestEncryptionOffRecordsOnlyAcknowledgementEvidence(t *testing.T) {
	recorder := NewPeerCapabilityRecorder(0)
	client := newPeerCapabilityClientForTest(t, EncryptionModeOff, recorder)
	peerId := NewId()

	if session := client.EncryptionSessionManager().AcquireForSend(peerId, sequenceTlsRoleClient, false, false, false); session != nil {
		session.Release()
		t.Fatal("a recorder turned on an end-to-end session under EncryptionModeOff")
	}
	sequence := NewSendSequence(context.Background(), client, client.sendBuffer, peerId,
		MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, client.settings.SendBufferSettings)
	t.Cleanup(sequence.Close)
	if sequence.session != nil {
		t.Fatal("a send sequence holds a session under EncryptionModeOff")
	}
	if sequence.peerCapabilityRecorder != recorder {
		t.Fatal("the send sequence did not capture its client's recorder")
	}
	if _, ok := recorder.Get(peerId); ok {
		t.Fatal("evidence was recorded before any acknowledgement")
	}

	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet:       true,
		receiveWindowByteCount: 64 * 1024,
	})
	observation, ok := recorder.Get(peerId)
	want := PeerCapabilityObservation{
		PeerId:                    peerId,
		AckObserved:               true,
		WindowAdvertised:          true,
		MaxReceiveWindowByteCount: 64 * 1024,
		LastObservationTime:       observation.LastObservationTime,
	}
	if !ok || observation != want {
		t.Fatalf("evidence\n got %+v\nwant %+v", observation, want)
	}
}
