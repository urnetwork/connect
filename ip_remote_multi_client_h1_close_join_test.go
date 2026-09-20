package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The generated client can finish while its external H1 carrier still owns
// socket/receive workers. The generator owns that carrier, so it cannot report
// a successful join merely because Client.CloseAndWait returned.
func TestApiMultiClientGeneratorJoinsActualH1Transport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	generator, removeCount, closeAPI := newRemoveClientTestGenerator(t, 4, ctx, ctx)
	defer closeAPI()
	defer generator.clientStrategy.Close()
	platform := newTestingPlatformServerIpVersion(t, 4)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	settings := testingPlatformTransportSettings()
	budget := NewPlatformTransportBudget(16*1024*1024, 4)
	settings.PlatformTransportBudget = budget
	settings.afterRoutesRemovedForTest = func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}
	transport := NewPlatformTransportWithTargetMode(ctx, generator.clientStrategy,
		client.RouteManager(), platform.url,
		&ClientAuth{ByJwt: "local-fixture", InstanceId: NewId(), AppVersion: "test"},
		TransportModeH1, settings)
	defer func() {
		releaseOnce.Do(func() { close(release) })
		_ = transport.CloseAndWait(context.Background())
		_ = client.CloseAndWait(context.Background())
	}()
	for !transport.IsConnected() {
		notify := transport.ConnectedNotify()
		if transport.IsConnected() {
			break
		}
		select {
		case <-notify:
		case <-ctx.Done():
			t.Fatal("H1 fixture did not connect")
		}
	}
	connectedClaims := budget.MemoryOwnerCensus()
	if connectedClaims.H1Count != 1 || connectedClaims.H1Bytes == 0 {
		t.Fatalf("connected H1 did not acquire its admission claim: %+v", connectedClaims)
	}
	args := &MultiClientGeneratorClientArgs{ClientId: client.ClientId(), ClientAuth: &ClientAuth{InstanceId: NewId()}}
	generator.transportLock.Lock()
	generator.transportIdle = make(chan struct{})
	generator.transports[client] = &apiWindowClientTransport{current: transport}
	generator.transportLock.Unlock()
	go func() {
		<-client.Done()
		generator.RemoveClientWithArgs(client, args)
	}()
	// A short caller deadline is a liveness bound, not an altered production
	// timeout. The held H1 owner must prevent successful completion throughout.
	joinCtx, stopJoin := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopJoin()
	result := make(chan error, 1)
	go func() { result <- generator.CloseAndWait(joinCtx) }()
	waitCloseWaitBarrier(t, ctx, entered, "actual H1 teardown barrier")
	heldClaims := budget.MemoryOwnerCensus()
	if !generator.transportLock.TryLock() {
		t.Fatal("carrier join holds the generator transport lock")
	}
	generator.transportLock.Unlock()
	select {
	case <-transport.Done():
		t.Fatal("H1 finished while its teardown barrier was held")
	default:
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("generator reported completed retirement with live H1 workers: %v", err)
	}
	if count := removeCount.Load(); count != 0 {
		t.Fatalf("identity was revoked before carrier retirement: %d removes", count)
	}
	releaseOnce.Do(func() { close(release) })
	if err := generator.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.Done():
	default:
		t.Fatal("successful generator join still precedes carrier completion")
	}
	if claims := budget.MemoryOwnerCensus(); claims.H1Bytes != 0 || claims.H1Count != 0 {
		t.Fatalf("successful join retained H1 claim: %+v", claims)
	}
	if count := removeCount.Load(); count != 1 {
		t.Fatalf("successful retirement did not finish identity removal once: %d removes", count)
	}
	t.Logf("actual H1 admission bytes: connected=%d paused-teardown=%d joined=0; claims do not prove worker completion",
		connectedClaims.H1Bytes, heldClaims.H1Bytes)
}
