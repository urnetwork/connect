// Local TLS cancellation exercises capture lifetime ownership before failure cleanup.
package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The fixture owns completion so competing sources can be ordered without a scheduler race.
type tlsCancellationFixture struct {
	session *peerEncryptionSession
	epoch   *tlsHandshakeEpoch
	events  chan *EncryptionEvent
	log     *receiveGapTestLogger
}

// Builds a real manager and session with an in-memory epoch and no background handshake.
func newTlsCancellationFixture(t *testing.T, parent context.Context) *tlsCancellationFixture {
	t.Helper()
	log := &receiveGapTestLogger{Logger: &captureLogger{}}
	client := &Client{log: log, settings: DefaultClientSettings()}
	manager := NewEncryptionSessionManager(parent, client, nil, &EncryptionSettings{Mode: EncryptionModeOff})
	session := newPeerEncryptionSession(manager.ctx, manager, client, NewId(), sequenceTlsRoleServer, DefaultEncryptionSettings(), nil, true)
	// Own completion in the test. No TLS or timeout worker competes with the
	// ordered caller transitions below.
	ctx, cancel := context.WithCancel(session.ctx)
	epoch := &tlsHandshakeEpoch{ctx: ctx, cancel: cancel, handshakeDone: make(chan struct{}), establishmentDone: make(chan struct{})}
	session.epoch = epoch
	session.refs = 1
	events := make(chan *EncryptionEvent, 4)
	manager.AddEncryptionEventCallback(func(event *EncryptionEvent) { events <- event })
	t.Cleanup(func() {
		session.close()
		session.workers.close()
		<-session.workers.Done()
		manager.Close()
	})
	return &tlsCancellationFixture{session: session, epoch: epoch, events: events, log: log}
}

// The existing event and error remain identical apart from the bounded owner field.
func (f *tlsCancellationFixture) confirm(t *testing.T, err error, source string) {
	t.Helper()
	readCount := func() uint64 {
		for _, cell := range GetTlsHandshakeCancellationCounts() {
			if cell.Source == source && cell.Role == "server" && cell.Companion {
				return cell.Count
			}
		}
		return 0
	}
	before := readCount()
	f.session.completeHandshake(f.epoch, err)
	select {
	case event := <-f.events:
		if event.Type != EncryptionEventEstablishFailed || event.Reason != err.Error() || event.CancellationSource != source || event.PeerId != f.session.peerId {
			t.Fatalf("wrong establishment disposition, cancellation source %q", event.CancellationSource)
		}
	default:
		t.Fatal("missing establishment callback")
	}
	if source != "" && readCount() != before+1 {
		t.Fatal("published cohort did not count the event")
	}
	if logger := f.log.Logger.(*captureLogger); len(logger.info) != 0 {
		t.Fatal("cancellation diagnostic added a logger call")
	}
	if logs := f.log.messages(); len(logs) != 1 || logs[0] != fmt.Sprintf("[tls]%s completeHandshake failed: %s\n", f.session.logTag, err) {
		t.Fatal("the existing failure log changed")
	}
}

// The winning owner survives later teardown, including a private parent error.
func TestTlsHandshakeCancellationReportsFirstOwner(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		{"parent canceled before construction", "parent_lifetime"},
		{"parent then session", "parent_lifetime"},
		{"session then parent", "session_retired"},
		{"manager then parent", "manager_retired"},
		{"control enqueue rejected then parent", "control_enqueue_rejected"},
		{"control send worker recovered then parent", "worker_recovery"},
		{"context still live", "unknown"},
	} {
		parent, cancel := context.WithCancelCause(t.Context())
		// An inherited cause may contain private application details.
		privateCause := errors.New("private fixture text")
		if test.name == "parent canceled before construction" {
			cancel(privateCause)
		}
		f := newTlsCancellationFixture(t, parent)
		switch test.name {
		case "parent then session":
			cancel(privateCause)
			f.session.close()
		case "session then parent":
			f.session.close()
			cancel(privateCause)
		case "manager then parent":
			f.session.manager.Close()
			cancel(privateCause)
		case "control enqueue rejected then parent", "control send worker recovered then parent":
			// The sequence admission lifetime ends while the epoch is still
			// wanted. Exercise the real send worker and its recovery handler.
			sendCtx, stop := context.WithCancel(t.Context())
			stop()
			buffer := &SendBuffer{ctx: sendCtx, client: f.session.client, log: f.log}
			if test.want == "worker_recovery" {
				buffer.beforeEncryptedControlPackForTest = func([]byte) { panic("fixture worker stopped") }
			}
			f.session.client.sendBuffer = buffer
			f.session.sendEncryptedControl(f.epoch, &protocol.EncryptedControl{}, nil)
			select {
			case <-f.epoch.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("control worker did not cancel the session")
			}
			cancel(privateCause)
		}
		f.confirm(t, context.Canceled, test.want)
		cancel(privateCause)
		if f.epoch.ctx.Err() != context.Canceled {
			t.Fatalf("%s changed the context error", test.name)
		}
		if cause := context.Cause(f.epoch.ctx); cause != privateCause && !errors.Is(cause, context.Canceled) {
			t.Fatalf("%s lost ordinary cancellation matching", test.name)
		}
	}
}

// Simultaneous requests must agree with the first cause installed on this epoch.
func TestTlsHandshakeCancellationRacingParentAndSession(t *testing.T) {
	parent, cancel := context.WithCancelCause(t.Context())
	f := newTlsCancellationFixture(t, parent)
	inherited := errors.New("private parent cause")
	start := make(chan struct{})
	var both sync.WaitGroup
	both.Go(func() { <-start; cancel(inherited) })
	both.Go(func() { <-start; f.session.close() })
	close(start)
	both.Wait()
	want := "session_retired"
	if context.Cause(f.epoch.ctx) == inherited {
		want = "parent_lifetime"
	}
	f.confirm(t, context.Canceled, want)
}

// A replaced epoch cannot attribute its retirement to the replacement.
func TestTlsHandshakeCancellationSupersededEpochStaysSilent(t *testing.T) {
	f := newTlsCancellationFixture(t, t.Context())
	// Construct production epoch contexts but drive both completions here.
	f.session.workers.close()
	f.session.tlsConfig = &tls.Config{}
	f.session.reset()
	old := f.session.currentEpoch()
	f.session.reset()
	replacement := f.session.currentEpoch()
	if tlsHandshakeCancellationOwner(old.ctx) != tlsEpochRetired || replacement.ctx.Err() != nil {
		t.Fatal("epoch replacement did not retire just the superseded context")
	}
	f.session.completeHandshake(old, context.Canceled)
	if len(f.events) != 0 || len(f.log.messages()) != 0 {
		t.Fatal("superseded epoch published a failure")
	}
}

// The watchdog publishes its timeout before canceling the worker.
func TestTlsHandshakeCancellationTimeoutWinsBeforeWorker(t *testing.T) {
	f := newTlsCancellationFixture(t, t.Context())
	f.session.settings.TlsTimeout = time.Millisecond
	f.session.establishmentTimeoutWatcher(f.epoch)
	f.confirm(t, errors.New("tls handshake timeout after 1ms"), "")
	f.session.completeHandshake(f.epoch, context.Canceled)
	if len(f.events) != 0 || len(f.log.messages()) != 1 {
		t.Fatal("late canceled worker published a second outcome")
	}
}

// The watchdog leaves an already completed cancellation alone.
func TestTlsHandshakeCancellationWorkerWinsBeforeTimeout(t *testing.T) {
	f := newTlsCancellationFixture(t, t.Context())
	f.session.close()
	f.confirm(t, context.Canceled, "session_retired")
	f.session.establishmentTimeoutWatcher(f.epoch)
	if len(f.events) != 0 || len(f.log.messages()) != 1 {
		t.Fatal("late timeout published a second outcome")
	}
}

// Many sessions update a finite role/companion counter without adding any
// logger calls. Unobserved combinations stay zero.
func TestTlsHandshakeCancellationCountsAcrossPeers(t *testing.T) {
	owner := &tlsCancellationOwner{name: "session_retired"}
	var peers sync.WaitGroup
	for range 100 {
		peers.Go(func() { owner.record(sequenceTlsRoleServer, true) })
	}
	peers.Wait()
	if owner.counts[sequenceTlsRoleServer][1].Load() != 100 ||
		owner.counts[sequenceTlsRoleServer][0].Load() != 0 ||
		owner.counts[sequenceTlsRoleClient][1].Load() != 0 {
		t.Fatal("unexpected cancellation count for role and companion")
	}
}

// A new cancellation owner cannot split an existing failure streak.
func TestTlsHandshakeCancellationPreservesCoalescing(t *testing.T) {
	f := newTlsCancellationFixture(t, t.Context())
	f.confirm(t, context.Canceled, "unknown")
	ctx, cancel := context.WithCancel(f.session.ctx)
	defer cancel()
	next := &tlsHandshakeEpoch{ctx: ctx, cancel: cancel, handshakeDone: make(chan struct{}), establishmentDone: make(chan struct{})}
	f.session.epoch = next
	f.session.close()
	f.session.completeHandshake(next, context.Canceled)
	if len(f.events) != 0 || len(f.log.messages()) != 1 {
		t.Fatal("another owner split the existing cancellation streak")
	}
}

// A synthetic logger panic exercises the native epoch worker recovery handler.
type tlsOutboxRecoveryLogger struct{ captureLogger }

// Only the first outbox batch raises the local fixture fault.
func (self *tlsOutboxRecoveryLogger) Infof(format string, args ...any) {
	if format == "[tls]%s outbox batch %d: %d bytes (record type 0x%02x)\n" {
		panic("fixture outbox stopped")
	}
}

// An epoch-owned worker recovery is distinguishable from its still-live
// session, even if the TLS worker observes transport EOF instead of cancellation.
func TestTlsHandshakeCancellationRecoveredEpochWorker(t *testing.T) {
	f := newTlsCancellationFixture(t, t.Context())
	f.session.client.log = &tlsOutboxRecoveryLogger{captureLogger: captureLogger{enabled: true}}
	f.session.tlsConfig = &tls.Config{}
	f.session.settings.TlsTimeout = 0
	f.session.reset()
	epoch := f.session.currentEpoch()
	if _, err := epoch.transport.Write([]byte{1}); err != nil {
		t.Fatal("could not wake the outbox fixture")
	}
	select {
	case <-epoch.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("recovered epoch worker did not cancel its own lifetime")
	}
	if tlsHandshakeCancellationOwner(epoch.ctx) != tlsWorkerRecovery || f.session.ctx.Err() != nil {
		t.Fatal("recovered epoch worker was attributed to its session")
	}
}
