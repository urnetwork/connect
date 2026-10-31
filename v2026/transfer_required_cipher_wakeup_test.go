// Required sends follow session readiness edges, while real caller and retry
// deadlines remain authoritative. The synthetic clock never advances during
// readiness assertions, so a polling implementation cannot accidentally pass.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Builds the real Pack entry gate without a transport, crypto worker, pooled
// payload, or background client. The sentinel cipher is never used to encrypt.
func newRequiredCipherWakeFixture() (*SendSequence, *peerEncryptionSession, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sessionCtx, cancelSession := context.WithCancel(ctx)
	settings := DefaultEncryptionSettings()
	settings.Mode = EncryptionModeRequired
	settings.TlsTimeout = 0
	settings.RequiredCipherPollInterval = time.Hour
	session := &peerEncryptionSession{
		ctx: sessionCtx, cancel: cancelSession,
		client: &Client{log: NewNoopLogger()}, settings: settings,
		role: sequenceTlsRoleServer, refs: 1,
		epoch: &tlsHandshakeEpoch{
			ctx: sessionCtx, cancel: func() {},
			handshakeDone: make(chan struct{}), establishmentDone: make(chan struct{}),
			derivedTlsCipher: &sequenceCipher{},
		},
	}
	sequence := &SendSequence{
		ctx: ctx, cancel: cancel,
		session: session, encryptionRole: sequenceTlsRoleServer,
		sendBufferSettings: &SendBufferSettings{},
		packs:              make(chan *SendPack, 8), idleCondition: NewIdleCondition(),
	}
	return sequence, session, cancel
}

// Carries the actual admission result; a nil error alone is not admission.
type requiredCipherWakeResult struct {
	accepted bool
	err      error
}

// Starts one caller owned by the fixture's context. Every test cancels and
// joins it, including the expected pre-fix failure path.
func startRequiredCipherWakePack(sequence *SendSequence, ctx context.Context, timeout time.Duration) <-chan requiredCipherWakeResult {
	done := make(chan requiredCipherWakeResult, 1)
	go func() {
		accepted, err := sequence.Pack(&SendPack{Ctx: ctx, Frame: &protocol.Frame{}}, timeout)
		done <- requiredCipherWakeResult{accepted: accepted, err: err}
	}()
	return done
}

// Publishes through the same locked promotion used after identity verification.
func publishRequiredCipherWakeSession(session *peerEncryptionSession) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.epoch.peerIdentityVerified = true
	session.markEstablishedWithLock(session.epoch)
}

// Cipher publication must admit a parked Pack before any timer advances.
func TestRequiredCipherWakeEstablishedEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
		started := time.Now()
		synctest.Wait()
		publishRequiredCipherWakeSession(session)
		synctest.Wait()
		select {
		case result := <-done:
			if !result.accepted || result.err != nil {
				t.Fatalf("cipher-ready admission = %+v", result)
			}
		default:
			t.Fatal("cipher publication left Pack waiting for the polling timer")
		}
		if !time.Now().Equal(started) {
			t.Fatal("cipher readiness consumed a timer")
		}
	})
}

// A handshake alone cannot release the signed-history gate; its independent
// verified transition must wake all sends once identity evidence is accepted.
func TestRequiredCipherWakeSignedHistoryVerification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		session.keyHistoryState = clientKeyHistoryPending
		publishRequiredCipherWakeSession(session)
		done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("pending signed history admitted application data")
		default:
		}
		started := time.Now()
		session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		synctest.Wait()
		select {
		case result := <-done:
			if !result.accepted || result.err != nil {
				t.Fatalf("verified-history admission = %+v", result)
			}
		default:
			t.Fatal("signed-history verification did not wake the required send")
		}
		if !time.Now().Equal(started) {
			t.Fatal("verified history consumed a timer")
		}
	})
}

// Notifications are broadcast, not one idle-reaper token shared by waiters.
func TestRequiredCipherWakeBroadcastsToConcurrentPacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		var results []<-chan requiredCipherWakeResult
		for range 8 {
			results = append(results, startRequiredCipherWakePack(sequence, sequence.ctx, -1))
		}
		synctest.Wait()
		publishRequiredCipherWakeSession(session)
		synctest.Wait()
		for index, done := range results {
			select {
			case result := <-done:
				if !result.accepted || result.err != nil {
					t.Errorf("waiter %d = %+v", index, result)
				}
			default:
				t.Errorf("waiter %d missed the cipher readiness broadcast", index)
			}
		}
	})
}

// A real caller budget, shorter than the legacy poll interval, ends exactly
// at its deadline. Advancing the virtual clock is the tested deadline event.
func TestRequiredCipherWakeHonorsExactCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, _, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		budget := 7 * time.Millisecond
		done := startRequiredCipherWakePack(sequence, sequence.ctx, budget)
		synctest.Wait()
		<-time.After(budget)
		synctest.Wait()
		select {
		case result := <-done:
			if result.accepted || !errors.Is(result.err, ErrEncryptionRequiredNotEstablished) {
				t.Fatalf("expired admission = %+v", result)
			}
		default:
			t.Fatal("required send overran its caller deadline waiting for a poll")
		}
		if len(sequence.packs) != 0 {
			t.Fatal("expired application Pack reached the queue")
		}
	})
}

// Closing the owning session is terminal even before sequence teardown catches
// up. It must not strand an infinite wait on a now-impossible cipher.
func TestRequiredCipherWakeSessionCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
		synctest.Wait()
		session.cancel()
		synctest.Wait()
		select {
		case result := <-done:
			if result.accepted || result.err == nil {
				t.Fatalf("closed-session admission = %+v", result)
			}
		default:
			t.Fatal("session close did not release its required sender")
		}
	})
}

// Caller cancellation remains independent of shared session readiness.
func TestRequiredCipherWakeCallerCancellationControl(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, _, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		ctx, cancelCaller := context.WithCancel(sequence.ctx)
		done := startRequiredCipherWakePack(sequence, ctx, -1)
		synctest.Wait()
		cancelCaller()
		synctest.Wait()
		select {
		case result := <-done:
			if result.accepted || result.err == nil {
				t.Fatalf("canceled caller admission = %+v", result)
			}
		default:
			t.Fatal("caller cancellation did not release Pack")
		}
	})
}

// Nonblocking refusal and unwrapped handshake controls retain their contracts.
func TestRequiredCipherWakeNonblockingAndControlBypass(t *testing.T) {
	sequence, _, cancel := newRequiredCipherWakeFixture()
	defer cancel()
	accepted, err := sequence.Pack(&SendPack{Ctx: sequence.ctx, Frame: &protocol.Frame{}}, 0)
	if accepted || !errors.Is(err, ErrEncryptionRequiredNotEstablished) {
		t.Fatalf("nonblocking application = %t, %v", accepted, err)
	}
	accepted, err = sequence.Pack(&SendPack{Ctx: sequence.ctx, Frame: &protocol.Frame{}, ForceUnwrapped: true}, 0)
	if !accepted || err != nil {
		t.Fatalf("handshake bypass = %t, %v", accepted, err)
	}
}

// A rejected history result never opens a previously established cipher.
func TestRequiredCipherWakeRejectedHistoryStaysClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		session.keyHistoryState = clientKeyHistoryPending
		publishRequiredCipherWakeSession(session)
		done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
		synctest.Wait()
		session.openKeyHistoryGate(clientKeyHistoryRejected, "synthetic identity mismatch")
		session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		synctest.Wait()
		if session.Cipher() != nil {
			t.Fatal("terminal identity rejection reopened the cipher")
		}
		select {
		case result := <-done:
			if result.accepted {
				t.Fatal("rejected history admitted a Pack")
			}
		default:
		}
	})
}

// The establishment-report timer is diagnostic, not permission to send or a
// reason to abandon an infinite wait that can still establish later.
func TestRequiredCipherWakeRetainsEstablishmentReportDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		session.settings.TlsTimeout = time.Second
		done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
		synctest.Wait()
		<-time.After(time.Second)
		synctest.Wait()
		session.stateLock.Lock()
		notified := session.requiredSendBlockedNotified
		session.stateLock.Unlock()
		if !notified {
			t.Error("the real establishment-report deadline was lost")
		}
		select {
		case <-done:
			t.Fatal("diagnostic deadline completed an infinite send")
		default:
		}
	})
}

// Readiness must not restart an in-flight epoch or ignore a deliberate initial
// failure cooldown. This control does not launch a TLS worker.
func TestRequiredCipherWakePreservesInitialRetryCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		sequence.encryptionRole = sequenceTlsRoleClient
		session.role = sequenceTlsRoleClient
		session.settings.TlsInitialRetryInterval = time.Second
		session.settings.TlsInitialRetryMaxInterval = time.Second
		session.settings.TlsInitialRetryStagger = 0
		epoch := session.epoch
		done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
		synctest.Wait()
		session.completeHandshake(epoch, errors.New("synthetic handshake refusal"))
		synctest.Wait()
		if session.currentEpoch() != epoch {
			t.Fatal("failure notification bypassed the retry cooldown")
		}
		select {
		case <-done:
			t.Fatal("failed handshake completed an infinite send")
		default:
		}
	})
}
