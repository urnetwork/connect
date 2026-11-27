package connect

import (
	"context"
	"crypto/x509"
	"testing"
	"time"
)

// Captured controls may outlive either the epoch or the owning session.
func TestOptimisticControlRequiresLiveEpoch(t *testing.T) {
	for _, stop := range []string{"live", "failed identity", "epoch canceled", "session canceled"} {
		ctx, cancel := context.WithCancel(t.Context())
		epochCtx, retire := context.WithCancel(ctx)
		e := &tlsHandshakeEpoch{ctx: epochCtx, transport: newSequenceTlsTransport(epochCtx),
			epochId: NewId(), handshakeDone: make(chan struct{}), serverFlightSent: true}
		s := &peerEncryptionSession{ctx: ctx, role: sequenceTlsRoleServer, epoch: e, client: &Client{log: NewNoopLogger()}}
		switch stop {
		case "failed identity":
			e.identityFailed = true
		case "epoch canceled":
			retire()
		case "session canceled":
			cancel()
		}
		s.optimisticallyDeliverHandshakeForEpoch([]byte{23}, e.epochId)
		if (len(e.transport.inboxBuf) != 0) != (stop == "live") {
			t.Errorf("%s buffered %d bytes", stop, len(e.transport.inboxBuf))
		}
		e.transport.Close()
		retire()
		cancel()
	}
}

// A finished TLS transport must relinquish pending bytes and refuse both writers.
func TestClosedHandshakeTransportReleasesBuffers(t *testing.T) {
	tr := newSequenceTlsTransport(t.Context())
	tr.Deliver([]byte{1})
	_, _ = tr.Write([]byte{2})
	tr.Close()
	tr.Deliver([]byte{3})
	if _, err := tr.Write([]byte{4}); err == nil {
		t.Error("write accepted after close")
	}
	if len(tr.inboxBuf)+len(tr.outboxBuf) != 0 {
		t.Error("closed transport retained bytes")
	}
}

// A missing presented leaf must be a verification error at the commitment boundary.
func TestCertificateCommitmentRejectsMissingLeaf(t *testing.T) {
	_, cert := newCertEpochCertificate(t)
	if ok, err := verifyPeerCertificateAgainstContract([]*x509.Certificate{nil}, [][]byte{cert}); ok || err == nil {
		t.Fatal("missing leaf accepted")
	}
}

// Model an already full staging buffer with no TLS reader. The next byte must
// retire only this transport and relinquish the allocation.
func TestHandshakeInboxStopsAtCapacity(t *testing.T) {
	tr := newSequenceTlsTransport(t.Context())
	tr.inboxBuf = make([]byte, 512*1024)
	tr.Deliver([]byte{1})
	if tr.ctx.Err() == nil || len(tr.inboxBuf) != 0 {
		t.Fatal("full handshake inbox accepted more work")
	}
	other := newSequenceTlsTransport(t.Context())
	defer other.Close()
	other.Deliver([]byte{9})
	var out [1]byte
	if n, err := other.Read(out[:]); n != 1 || out[0] != 9 || err != nil {
		t.Fatal("independent transport lost service")
	}
}

// A rate window belongs to one physical lane across selector refresh. Other
// lanes and the next window keep their own recovery allowance.
func TestRouteRecoveryBudgetSurvivesSnapshotRefresh(t *testing.T) {
	manager := NewRouteManager(t.Context(), "recovery quotas")
	first, second := make(Route), make(Route)
	one, two := NewReceiveGatewayTransportWithType(TransportTypeH1), NewReceiveGatewayTransportWithType(TransportTypeH3)
	manager.UpdateTransport(one, []Route{first})
	selector := manager.OpenMultiRouteReader(DestinationId(NewId())).(*MultiRouteSelector)
	defer manager.CloseMultiRouteReader(selector)
	now := time.Unix(42, 0)
	snapshot := selector.activeRoutesSnapshot.Load()
	budget := snapshot.receiveDisposition(first, nil).recoveryBudget
	if budget == nil {
		t.Fatal("receive route has no recovery budget")
	}
	for range routeRecoveryPerSecond {
		if !budget.allow(now) {
			t.Fatal("lost part of the route allowance")
		}
	}
	manager.UpdateTransport(two, []Route{second})
	snapshot = selector.activeRoutesSnapshot.Load()
	if snapshot.receiveDisposition(first, nil).recoveryBudget.allow(now) || !snapshot.receiveDisposition(second, nil).recoveryBudget.allow(now) {
		t.Fatal("refresh reset or shared a route allowance")
	}
	if !budget.allow(now.Add(time.Second)) {
		t.Fatal("route did not recover")
	}
}
