// Actual socket-reader behavior uses only pre-existing production and fixture APIs.
package connect

import (
	"testing"
	"testing/synctest"
)

// A real pipe reader can accumulate more than one two-packet producer chunk
// while its preceding callback waits at Transfer admission. No fill timer is
// added: the consumer only drains packets that are already queued.
func TestTcpAdaptiveReadQueueWorkerPromotionAndDrain(t *testing.T) {
	runTcpAdaptiveReadQueueWorker(t, nil)
}

// The optional test observer runs after byte/order/batch and writer-join checks.
// The behavioral leaf never names a candidate-only production field or helper.
func runTcpAdaptiveReadQueueWorker(t *testing.T, afterDrain func(*testing.T, *TcpSequence)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		// Publish the requested batch before NewLocalUserNat starts its shards.
		fixture := newTcpProviderMemoryRetirementFixtureWithWriteBatchSize(t, NewTransferMemoryBudget(mib(2)), 8)
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		sequence := fixture.nextSequence(1000)
		if sequence.tcpBufferSettings != fixture.nat.settings.TcpBufferSettings || sequence.tcpBufferSettings.WriteBatchSize != 8 {
			t.Fatal("worker did not retain the pre-construction batch8 settings")
		}
		fixture.takeControls()
		fixture.fillFromSocket()
		done := make(chan error, 1)
		writerDone := make(chan struct{})
		t.Cleanup(func() { fixture.upstreams[0].Close(); <-writerDone })
		go func() {
			defer close(writerDone)
			for _, value := range []byte("0123456789ABCDEF") {
				if _, err := fixture.upstreams[0].Write([]byte{value}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		synctest.Wait()
		bytes, maximum := 0, 0
		var observed []byte
		for range 20 {
			synctest.Wait()
			for len(fixture.transfer.packs) != 0 {
				pack := <-fixture.transfer.packs
				func() {
					defer pack.releaseRaw()
					defer pack.returnFrames()
					count := len(pack.frameList())
					maximum = max(maximum, count)
					for _, frame := range pack.frameList() {
						if !frame.Raw {
							t.Fatal("raw callback fixture unexpectedly encoded legacy packet")
						}
						var path IpPath
						payload, err := parseIpPathWithPayloadBorrowed(frame.MessageBytes, &path)
						if err != nil {
							t.Fatal(err)
						}
						observed = append(observed, payload...)
						bytes += path.TcpPayloadByteCount
					}
				}()
			}
			if bytes == 18 {
				break
			}
		}
		if bytes != 18 || maximum != 8 || string(observed) != "ab0123456789ABCDEF" {
			t.Fatalf("socket return drain: bytes=%d want18, maximum batch=%d want8, ordered=%q", bytes, maximum, observed)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("socket writer did not join after drain")
		}
		if afterDrain != nil {
			afterDrain(t, sequence)
		}
	})
}
