// Constructor normalization separates safe cap2 behavior from optional callback8.
package connect

import (
	"testing"
	"testing/synctest"
)

// Request8 before the real NAT constructor; never overwrite the normalized
// settings. Both arms must deliver every byte before their batch sizes differ.
func TestTcpAdaptiveConstructorReadQueuePromotionAndDrain(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderMemoryRetirementFixtureWithWriteBatchSize(t, NewTransferMemoryBudget(mib(2)), 8)
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		sequence := fixture.nextSequence(1000)
		effectiveBatch := fixture.nat.settings.TcpBufferSettings.WriteBatchSize
		if sequence.tcpBufferSettings != fixture.nat.settings.TcpBufferSettings ||
			(effectiveBatch != 2 && effectiveBatch != 8) {
			t.Fatalf("constructor batch policy: effective=%d want old2 or candidate8 with exact constructed settings", effectiveBatch)
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
		if bytes != 18 || string(observed) != "ab0123456789ABCDEF" {
			t.Fatalf("constructor socket return payload: bytes=%d want18, maximum batch=%d, ordered=%q", bytes, maximum, observed)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("socket writer did not join after drain")
		}
		<-writerDone
		// Observe correct delivery and the normalized constructor policy before
		// requiring the new eight-ready-packet performance behavior.
		t.Logf("constructor socket return: requested=8 effective=%d bytes=%d maximum=%d ordered=%q writer_joined=true", effectiveBatch, bytes, maximum, observed)
		if maximum != effectiveBatch {
			t.Fatalf("constructor socket return policy mismatch: effective=%d maximum=%d", effectiveBatch, maximum)
		}
		if maximum != 8 {
			t.Fatalf("constructor socket return batching: requested=8 effective=%d bytes=%d maximum=%d want8 ordered=%q", effectiveBatch, bytes, maximum, observed)
		}
	})
}
