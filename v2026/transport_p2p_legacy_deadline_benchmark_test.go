package connect

import (
	"context"
	"testing"
)

// Each operation sends one 64-packet burst through the real physical send
// worker. The first write waits for the burst, forcing bounded compact storage
// instead of benchmarking only the lightly loaded raw-root path. The reusable
// gates and nil progress observer add no per-burst diagnostic allocations.
func BenchmarkP2pLegacyPhysicalSendQueue(b *testing.B) {
	const burstCount = 64
	const packetSize = 1200
	ctx, cancel := context.WithCancel(context.Background())
	budget := NewTransferMemoryBudget(kib(512))
	start, done := make(chan struct{}), make(chan struct{}, 1)
	written := 0
	conn := &p2pProbePressureConn{ctx: ctx}
	conn.onWire = func([]byte) {
		if written%burstCount == 0 {
			select {
			case <-start:
			case <-ctx.Done():
				return
			}
		}
		written++
		if written%burstCount == 0 {
			done <- struct{}{}
		}
	}
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
	transport, route := NewP2pSendTransport(ctx, cancel, &p2pLegacyQueueBudgetTestConn{Conn: conn, budget: budget}, NewId(), settings)
	defer func() {
		if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
			b.Error(err)
		}
		if stats := budget.Stats(); stats.ReservedByteCount != stats.ReleasedByteCount {
			b.Errorf("benchmark retained queue ownership: %+v", stats)
		}
	}()
	b.SetBytes(burstCount * packetSize)
	b.ReportAllocs()
	for b.Loop() {
		for range burstCount {
			wire := MessagePoolGet(packetSize)
			wire[0] = 0x42
			route <- wire
		}
		start <- struct{}{}
		<-done
	}
}
