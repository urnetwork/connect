// Retiring timing history cannot reprice an unchanged delivered-byte stream.
package connect

import (
	"testing"
	"time"
)

// The byte checkpoints are an unchanged 12.5 MB/s serializer compressed into
// a recurring 125/125/250/0 kB pattern. All physical metadata after the first
// sample includes 40 ms of queue residence above a constant 300 us path.
// Retiring the low RTT observation cannot manufacture faster service from
// those same bytes before their following ACK accounting applies.
func TestWindowPacingReceiverTimingQueueRetirementDoesNotRepriceBytes(t *testing.T) {
	at := time.Unix(1700000000, 0)
	settings := DefaultSendBufferSettings()
	service := newWindowPacingService(settings)
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	for i := range 101 {
		bytes := []ByteCount{125000, 125000, 250000, 0}[i%4]
		service.observe(bytes, at.Add(time.Duration(i)*10*time.Millisecond))
	}
	now := at.Add(time.Second)
	service.sent = service.total + 650000
	service.observeReceiverRoundTrip(2, 50300*time.Microsecond, 40300*time.Microsecond, 10*time.Millisecond, now)
	before, beforeTotal, _ := service.measured(time.Second, now)
	beforeBacklog := service.backloggedAt(before, now)
	if before != 12500000 || !beforeBacklog {
		t.Fatalf("fixture did not retain sustained queued service: rate=%d backlog=%t", before, beforeBacklog)
	}
	// ACK ingress can precede the worker's coalesced byte application. The
	// sample count is the real configured bound, not a tuned short history.
	for i := range settings.RttWindowSize {
		service.observeReceiverRoundTrip(uint64(i+3), 50300*time.Microsecond, 40300*time.Microsecond, 10*time.Millisecond, now)
	}
	after, total, _ := service.measured(time.Second, now)
	afterBacklog := service.backloggedAt(after, now)
	paced := windowPacingRate(SendWindowEstimate{ServiceByteRate: after, ServiceEstablished: true, ServiceBacklogged: afterBacklog}, 125000000)
	t.Logf("same delivered=%d/%d service=%d->%d backlog=%t->%t paced=%d ring=%d", beforeTotal, total, before, after, beforeBacklog, afterBacklog, paced, settings.RttWindowSize)
	if total != beforeTotal {
		t.Fatal("metadata invented delivered byte accounting")
	}
	if after > before || !afterBacklog || paced > 12500000 {
		t.Errorf("retiring RTT count promoted an unchanged compressed peak: rate=%d backlog=%t paced=%d", after, afterBacklog, paced)
	}
}
