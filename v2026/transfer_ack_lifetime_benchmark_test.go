// Measure the added expiry ownership work against the existing recovery queue
// at the same outstanding-message counts, including steady-state allocation.
package connect

import (
	"testing"
	"time"
	"unsafe"
)

// Each turn acknowledges one old item and offers one replacement. Both arms
// retain the same encoded bytes and update the same two recovery heaps.
func benchmarkAckLifetimeFlight(b *testing.B, count int, indexed bool) {
	sequence := &SendSequence{resendQueue: newResendQueue(nil, 0)}
	items := make([]sendItem, count)
	wire := make([]byte, 1280)
	origin := time.Unix(1700000000, 0)
	for i := range items {
		item := &items[i]
		*item = sendItem{
			transferItem: transferItem{messageId: NewId(), sequenceNumber: uint64(i)},
			sendTime:     origin.Add(time.Duration(i) * time.Microsecond), ackTimeout: time.Minute,
			resendTime:         origin.Add(time.Duration(i)*time.Microsecond + time.Second),
			transferFrameBytes: wire, expectsAck: true,
		}
		if indexed {
			sequence.addResendItem(item)
		} else {
			sequence.resendQueue.Add(item)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := &items[i%count]
		sequence.resendQueue.RemoveByMessageId(item.messageId)
		if indexed {
			sequence.ackLifetimes.remove(item)
		}
		item.sendTime = origin.Add(time.Duration(count+i) * time.Microsecond)
		item.resendTime = item.sendTime.Add(time.Second)
		if indexed {
			sequence.addResendItem(item)
			if _, err := sequence.nextAckLifetime(item.sendTime); err != nil {
				b.Fatal(err)
			}
		} else {
			sequence.resendQueue.Add(item)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(cap(sequence.ackLifetimes.items))*float64(unsafe.Sizeof(sendAckLifetime{}))/float64(count), "expiry-B/live")
	sequence.ackLifetimes.clear()
	sequence.resendQueue.Clear()
}

// One outstanding message exercises the common short-window case.
func BenchmarkAckLifetimeBaseline1(b *testing.B) { benchmarkAckLifetimeFlight(b, 1, false) }

// The same short flight also maintains its independent expiry index.
func BenchmarkAckLifetimeIndexed1(b *testing.B) { benchmarkAckLifetimeFlight(b, 1, true) }

// A modest shared flight checks the next heap depth.
func BenchmarkAckLifetimeBaseline64(b *testing.B) { benchmarkAckLifetimeFlight(b, 64, false) }

// The modest flight includes expiry insertion, removal and earliest lookup.
func BenchmarkAckLifetimeIndexed64(b *testing.B) { benchmarkAckLifetimeFlight(b, 64, true) }

// A deep flight verifies logarithmic update work and stable allocations.
func BenchmarkAckLifetimeBaseline1024(b *testing.B) { benchmarkAckLifetimeFlight(b, 1024, false) }

// The deep flight measures the additional expiry heap work.
func BenchmarkAckLifetimeIndexed1024(b *testing.B) { benchmarkAckLifetimeFlight(b, 1024, true) }

// A large outstanding window must not cause a scan on each offered packet.
func BenchmarkAckLifetimeBaseline16384(b *testing.B) { benchmarkAckLifetimeFlight(b, 16384, false) }

// The large indexed flight checks update cost without an outstanding scan.
func BenchmarkAckLifetimeIndexed16384(b *testing.B) { benchmarkAckLifetimeFlight(b, 16384, true) }
