package connect

// The early ClientHello pace (`EncryptionSettings.EarlyClientHelloMinInterval`).
// A Required responder with no established epoch takes a named ClientHello from
// the receive loop, before any contract check (QD-1176), so any source could
// name new attempts there back to back and keep the responder starting TLS
// server handshakes. The pace takes a burst of them back to back and then one
// an interval, per session. Every clock here is the settings' test clock, so
// no bound waits out real time; the timeouts are a broken run's.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The early ClientHello pace tests' clock, which only the test moves.
type earlyHelloClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func newEarlyHelloClock() *earlyHelloClock {
	return &earlyHelloClock{now: time.Now()}
}

func (self *earlyHelloClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *earlyHelloClock) Advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// Settings for a Required client whose early ClientHello pace reads clock and
// takes burst back to back, then one each interval. A handshake never times out
// within a test.
func earlyHelloClientSettings(clock *earlyHelloClock, burst int, interval time.Duration) *ClientSettings {
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	settings.EncryptionSettings.TlsTimeout = time.Minute
	settings.EncryptionSettings.EarlyClientHelloBurst = burst
	settings.EncryptionSettings.EarlyClientHelloMinInterval = interval
	settings.EncryptionSettings.earlyClientHelloNowForTest = clock.Now
	return settings
}

// A responder session of a Required client built on settings, apart from its
// manager's registry, as newTestEncryptionSessionWithMode builds one. No epoch
// is started.
func newEarlyHelloSession(t *testing.T, settings *ClientSettings) (*peerEncryptionSession, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	keyManager, err := NewClientKeyManager(ctx, client)
	if err != nil {
		cancel()
		t.Fatalf("NewClientKeyManager: %s", err)
	}
	manager := NewEncryptionSessionManager(ctx, client, keyManager, settings.EncryptionSettings)
	sess := newPeerEncryptionSession(ctx, manager, client, NewId(), sequenceTlsRoleServer, settings.EncryptionSettings, manager.ServerTlsConfig(), false)
	return sess, func() {
		sess.close()
		client.Cancel()
		cancel()
	}
}

// Delivers a ClientHello naming a new attempt from the receive loop, as
// `Client.run` does, and answers the attempt and whether the session took it:
// it opened the attempt's epoch and answered its ClientHello.
func earlyHelloSend(sess *peerEncryptionSession, variant byte) (Id, bool) {
	attemptId := NewId()
	sess.optimisticallyDeliverClientHelloForEpoch(testAttemptClientHello(variant), attemptId)
	_, currentId, answered := testCurrentAttempt(sess)
	return attemptId, currentId == attemptId && answered
}

// Sends count early ClientHellos of new attempts back to back and answers how
// many the session took, failing when one is taken after one is refused: past
// the bound, no later one of the burst is taken.
func earlyHelloSendBackToBack(t *testing.T, sess *peerEncryptionSession, count int) int {
	t.Helper()
	takenCount := 0
	refused := false
	for i := 0; i < count; i++ {
		if _, taken := earlyHelloSend(sess, byte(i)); taken {
			if refused {
				t.Fatalf("early ClientHello %d was taken after one before it was refused", i)
			}
			takenCount += 1
		} else {
			refused = true
		}
	}
	return takenCount
}

// A source sending new attempts back to back gets at most the burst, then one
// an interval, never two in one; a pace left alone for a burst of intervals
// takes a whole burst again.
func TestEarlyClientHellosTakeABurstThenOneAnInterval(t *testing.T) {
	burst := 3
	interval := time.Second
	clock := newEarlyHelloClock()
	sess, cleanup := newEarlyHelloSession(t, earlyHelloClientSettings(clock, burst, interval))
	defer cleanup()

	if n := earlyHelloSendBackToBack(t, sess, burst+2); n != burst {
		t.Fatalf("back to back, the session took %d early ClientHellos, want the burst of %d", n, burst)
	}
	// none until a whole interval has passed, then one, and only one
	clock.Advance(interval - time.Nanosecond)
	if _, taken := earlyHelloSend(sess, 10); taken {
		t.Fatal("an early ClientHello was taken before the interval passed")
	}
	for i := 0; i < 3; i++ {
		clock.Advance(time.Nanosecond)
		if n := earlyHelloSendBackToBack(t, sess, 3); n != 1 {
			t.Fatalf("interval %d: the session took %d early ClientHellos, want 1", i+1, n)
		}
		clock.Advance(interval - time.Nanosecond)
	}
	// left alone until the burst is whole, it takes a whole burst again
	clock.Advance(time.Duration(burst) * interval)
	if n := earlyHelloSendBackToBack(t, sess, burst+2); n != burst {
		t.Fatalf("after a burst of intervals, the session took %d early ClientHellos, want %d", n, burst)
	}
}

// A retransmitted ClientHello of an attempt the session answered, and the one
// second ClientHello of a HelloRetryRequest, open nothing and spend nothing of
// the pace, so a recovering initiator's own retransmits never refuse its next
// attempt; and a session with an established epoch keeps the ordered path,
// its pace untouched.
func TestEarlyClientHelloRetransmitsSpendNothing(t *testing.T) {
	clock := newEarlyHelloClock()
	sess, cleanup := newEarlyHelloSession(t, earlyHelloClientSettings(clock, 1, time.Second))
	defer cleanup()

	attemptId := NewId()
	first, retry := testAttemptClientHello(1), testAttemptClientHello(2)
	e := injectTestAttemptEpoch(sess, attemptId, first, false, nil)
	for i := 0; i < 3; i++ {
		sess.optimisticallyDeliverClientHelloForEpoch(first, attemptId)
	}
	sess.optimisticallyDeliverClientHelloForEpoch(retry, attemptId)
	sess.optimisticallyDeliverClientHelloForEpoch(retry, attemptId)
	if n := testInboxByteCount(e); n != len(retry) {
		t.Fatalf("the answered attempt was fed %d bytes, want its retry ClientHello's %d", n, len(retry))
	}
	sess.stateLock.Lock()
	wholeTime := sess.earlyClientHelloWholeTime
	sess.stateLock.Unlock()
	if !wholeTime.IsZero() {
		t.Fatal("a retransmit or a retry ClientHello spent the early ClientHello pace")
	}
	// the burst of one is still whole
	if _, taken := earlyHelloSend(sess, 3); !taken {
		t.Fatal("the next attempt was refused after only retransmits and a retry")
	}

	established, establishedCleanup := newEarlyHelloSession(t, earlyHelloClientSettings(clock, 1, time.Second))
	defer establishedCleanup()
	epoch := injectTestAttemptEpoch(established, NewId(), testAttemptClientHello(4), true, nil)
	established.stateLock.Lock()
	established.establishedEpoch = epoch
	established.stateLock.Unlock()
	for i := 0; i < 3; i++ {
		established.optimisticallyDeliverClientHelloForEpoch(testAttemptClientHello(5), NewId())
	}
	established.stateLock.Lock()
	establishedWholeTime := established.earlyClientHelloWholeTime
	established.stateLock.Unlock()
	if current, _, _ := testCurrentAttempt(established); current != epoch || !establishedWholeTime.IsZero() {
		t.Fatal("an early ClientHello reached a session with an established epoch, or spent its pace")
	}
}

// An early ClientHello over the pace is dropped as a retransmit is: nothing of
// its attempt is recorded, and the ordered receive sequence's copy, past the
// contract checks, opens the attempt, which the pace does not cover.
func TestEarlyClientHelloOverThePaceOpensItsAttemptInOrder(t *testing.T) {
	clock := newEarlyHelloClock()
	sess, cleanup := newEarlyHelloSession(t, earlyHelloClientSettings(clock, 1, time.Hour))
	defer cleanup()

	takenId, taken := earlyHelloSend(sess, 1)
	if !taken {
		t.Fatal("the first early ClientHello was refused")
	}
	overId := NewId()
	overClientHello := testAttemptClientHello(2)
	sess.optimisticallyDeliverClientHelloForEpoch(overClientHello, overId)
	if _, currentId, _ := testCurrentAttempt(sess); currentId != takenId {
		t.Fatalf("an early ClientHello over the pace opened %s, want the session still on %s", currentId, takenId)
	}
	sess.deliverHandshake(overClientHello, overId)
	if _, currentId, answered := testCurrentAttempt(sess); currentId != overId || !answered {
		t.Fatalf("the ordered copy of the refused ClientHello opened %s (answered=%t), want %s", currentId, answered, overId)
	}
}

// A session reaped while its pace is spent hands the pace to the next session
// of its key, so a source cannot win a whole burst by getting its session
// reaped. With IdleTimeout zero, a session no sequence holds is reaped as its
// handshake fails, which a source can make happen at will.
func TestEarlyClientHelloPaceOutlivesTheSessionsReap(t *testing.T) {
	interval := time.Second
	clock := newEarlyHelloClock()
	settings := earlyHelloClientSettings(clock, 1, interval)
	settings.EncryptionSettings.IdleTimeout = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	manager := client.EncryptionSessionManager()
	sourceId := NewId()

	sess := manager.getOrCreate(sourceId, sequenceTlsRoleServer, false)
	if sess == nil {
		t.Fatal("no responder session")
	}
	if _, taken := earlyHelloSend(sess, 1); !taken {
		t.Fatal("the first early ClientHello was refused")
	}
	e, _, _ := testCurrentAttempt(sess)
	sess.completeHandshake(e, errors.New("the source failed its handshake"))
	if manager.Lookup(sourceId, sequenceTlsRoleServer, false) != nil {
		t.Fatal("the session no sequence holds was not reaped as its handshake failed")
	}

	next := manager.getOrCreate(sourceId, sequenceTlsRoleServer, false)
	if next == nil || next == sess {
		t.Fatal("no new responder session after the reap")
	}
	if _, taken := earlyHelloSend(next, 2); taken {
		t.Fatal("the reaped session's successor took an early ClientHello its spent pace refuses")
	}
	clock.Advance(interval)
	if _, taken := earlyHelloSend(next, 3); !taken {
		t.Fatal("the successor refused an early ClientHello an interval later")
	}
	manager.stateLock.Lock()
	keptCount := len(manager.earlyClientHelloWholeTimes)
	manager.stateLock.Unlock()
	if keptCount != 0 {
		t.Fatalf("%d paces are kept after the successor took its key's pace", keptCount)
	}
}

// A pace is kept only while it is not whole, and a kept pace leaves at a sweep
// once it is whole, so the kept paces hold only keys reaped within a burst of
// intervals.
func TestKeptEarlyClientHelloPacesLeaveOnceWhole(t *testing.T) {
	clock := newEarlyHelloClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), earlyHelloClientSettings(clock, 1, time.Second))
	defer client.Cancel()
	manager := client.EncryptionSessionManager()
	key := func() sessionKey {
		return sessionKey{peerId: NewId(), role: sequenceTlsRoleServer}
	}

	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	now := clock.Now()
	manager.keepEarlyClientHelloPaceWithLock(key(), now, now)
	manager.keepEarlyClientHelloPaceWithLock(key(), now.Add(-time.Second), now)
	if n := len(manager.earlyClientHelloWholeTimes); n != 0 {
		t.Fatalf("%d whole paces were kept", n)
	}
	soonKeys := []sessionKey{key(), key(), key()}
	for _, k := range soonKeys {
		manager.keepEarlyClientHelloPaceWithLock(k, now.Add(time.Second), now)
	}
	laterKey := key()
	manager.keepEarlyClientHelloPaceWithLock(laterKey, now.Add(time.Hour), now)
	if n := len(manager.earlyClientHelloWholeTimes); n != len(soonKeys)+1 {
		t.Fatalf("%d paces are kept, want %d", n, len(soonKeys)+1)
	}
	// keep until a sweep runs; every pace whole by then leaves
	later := now.Add(time.Minute)
	for i := 0; i < 2*len(soonKeys)+2; i++ {
		manager.keepEarlyClientHelloPaceWithLock(key(), later.Add(time.Second), later)
	}
	for _, k := range soonKeys {
		if _, ok := manager.earlyClientHelloWholeTimes[k]; ok {
			t.Fatal("a pace whole at the sweep was kept")
		}
	}
	if _, ok := manager.earlyClientHelloWholeTimes[laterKey]; !ok {
		t.Fatal("a pace not yet whole at the sweep left")
	}
}

// The defaults pace early ClientHellos, and settings that leave the pace at zero
// take the defaults' burst back to back and their interval after it.
func TestDefaultEncryptionSettingsPaceEarlyClientHellos(t *testing.T) {
	defaults := DefaultEncryptionSettings()
	if defaults.EarlyClientHelloBurst <= 0 || defaults.EarlyClientHelloMinInterval <= 0 {
		t.Fatalf("the defaults leave early ClientHellos unpaced: burst %d, interval %s", defaults.EarlyClientHelloBurst, defaults.EarlyClientHelloMinInterval)
	}
	clock := newEarlyHelloClock()
	sess, cleanup := newEarlyHelloSession(t, earlyHelloClientSettings(clock, 0, 0))
	defer cleanup()
	if n := earlyHelloSendBackToBack(t, sess, defaults.EarlyClientHelloBurst+2); n != defaults.EarlyClientHelloBurst {
		t.Fatalf("with the pace at zero, %d early ClientHellos were taken, want the default burst of %d", n, defaults.EarlyClientHelloBurst)
	}
	clock.Advance(defaults.EarlyClientHelloMinInterval)
	if n := earlyHelloSendBackToBack(t, sess, 2); n != 1 {
		t.Fatalf("a default interval later, %d early ClientHellos were taken, want 1", n)
	}
}

// One judgment of the early ClientHello pace, as the settings' observer sees it.
type earlyHelloJudgment struct {
	peerId  Id
	epochId Id
	taken   bool
}

// Through the real receive loop: a source no contract admits sends named
// ClientHellos of new attempts back to back to a Required client, each in a
// Pack the ordered receive sequence holds behind its missing head, as a
// restarted responder's holds the recovery ClientHello. The early path judges
// every one, takes the burst and then one an interval, and the source's
// session holds the last attempt taken: no attempt over the pace opened.
func TestEarlyClientHellosOfASourceTakeABurstThenOneAnInterval(t *testing.T) {
	burst := 3
	interval := time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := newEarlyHelloClock()
	judgments := make(chan earlyHelloJudgment, 64)
	settings := earlyHelloClientSettings(clock, burst, interval)
	settings.EncryptionSettings.earlyClientHelloPacedForTest = func(peerId Id, epochId Id, taken bool) {
		select {
		case judgments <- earlyHelloJudgment{peerId: peerId, epochId: epochId, taken: taken}:
		case <-ctx.Done():
		}
	}
	receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		if err := receiver.CloseAndWait(closeCtx); err != nil {
			t.Errorf("receiver cleanup: %v", err)
		}
	}()
	toReceiver := make(chan []byte, 64)
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{toReceiver})

	sourceId := NewId()
	sequenceId := NewId()
	sequenceNumber := uint64(0)
	// sends one Pack carrying a named ClientHello of a new attempt, and answers
	// the attempt; the sequence's head, number 0, never comes
	send := func() Id {
		t.Helper()
		attemptId := NewId()
		sequenceNumber += 1
		controlBytes, err := ProtoMarshal(&protocol.EncryptedControl{
			ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
			SessionRole: protocol.SequenceRole_SequenceRoleClient,
			Payload:     testAttemptClientHello(byte(sequenceNumber)),
			EpochId:     attemptId.Bytes(),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer MessagePoolReturn(controlBytes)
		wire, err := ProtoMarshal(&protocol.TransferFrame{
			TransferPath: TransferPath{SourceId: sourceId, DestinationId: receiver.ClientId()}.ToProtobuf(),
			Pack: &protocol.Pack{
				MessageId:      NewId().Bytes(),
				SequenceId:     sequenceId.Bytes(),
				SequenceNumber: sequenceNumber,
				Frames: []*protocol.Frame{{
					MessageType:  protocol.MessageType_TransferEncryptedControl,
					MessageBytes: controlBytes,
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case toReceiver <- wire:
		case <-ctx.Done():
			MessagePoolReturn(wire)
			t.Fatal("the route did not admit the ClientHello")
		}
		return attemptId
	}
	judged := func(attemptId Id) bool {
		t.Helper()
		select {
		case judgment := <-judgments:
			if judgment.peerId != sourceId || judgment.epochId != attemptId {
				t.Fatalf("the pace judged attempt %s of %s, want %s of %s", judgment.epochId, judgment.peerId, attemptId, sourceId)
			}
			return judgment.taken
		case <-ctx.Done():
			t.Fatalf("the early path never judged attempt %s", attemptId)
			return false
		}
	}

	attemptIds := make([]Id, 0, burst+3)
	for i := 0; i < burst+3; i++ {
		attemptIds = append(attemptIds, send())
	}
	var lastTakenId Id
	for i, attemptId := range attemptIds {
		taken := judged(attemptId)
		if taken != (i < burst) {
			t.Fatalf("back to back, early ClientHello %d was taken=%t, want taken=%t with a burst of %d", i, taken, i < burst, burst)
		}
		if taken {
			lastTakenId = attemptId
		}
	}
	for i := 0; i < 2; i++ {
		clock.Advance(interval)
		takenId, overId := send(), send()
		if !judged(takenId) {
			t.Fatalf("interval %d: the first early ClientHello was refused", i+1)
		}
		if judged(overId) {
			t.Fatalf("interval %d: a second early ClientHello was taken", i+1)
		}
		lastTakenId = takenId
	}

	session := receiver.EncryptionSessionManager().Lookup(sourceId, sequenceTlsRoleServer, false)
	if session == nil {
		t.Fatal("the early path opened no session for the source")
	}
	if _, currentId, answered := testCurrentAttempt(session); currentId != lastTakenId || !answered {
		t.Fatalf("the source's session holds attempt %s (answered=%t), want the last one taken, %s", currentId, answered, lastTakenId)
	}
}
