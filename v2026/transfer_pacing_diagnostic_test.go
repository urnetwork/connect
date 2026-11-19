// Diagnostic reads must not consume pacing permission or admission ownership.
package connect

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// No worker or pooled buffer is needed to exercise the locked owner boundaries.
func newTransferPacingDiagnosticSequence() *SendSequence {
	return &SendSequence{ctx: context.Background(), sequenceId: NewId(), destination: NewId(),
		sendBufferSettings: &SendBufferSettings{ResendQueueMaxByteCount: 8192},
		packs:              make(chan *SendPack, 2), packAdmission: newSendPackAdmission(2)}
}

// A far-later observation must not refill old credit, dequeue a waiter, or
// consume the buffered wake that its owner has not read yet.
func TestTransferPacingDiagnosticPreservesReservationAndAdmission(t *testing.T) {
	origin := time.Unix(100, 0)
	sequence := newTransferPacingDiagnosticSequence()
	service := &windowPacingService{}
	sequence.windowPacer.service = service
	first, second := &sequence.windowPacer.waiter, &windowPacingWaiter{}
	service.reserve(origin, 1024, 4096, 4096, 0, 0, false, first)
	service.reserve(origin, 1024, 4096, 4096, 0, 0, false, second)
	first.ready <- struct{}{}
	meter, next, reservations, sent := service.burstMeter, service.next, service.pacingReservations, service.sent
	key := sendSchedulingKey{}
	for range 2 {
		if acquired, _, _ := sequence.packAdmission.tryAcquire(key, false); !acquired {
			t.Fatal("fixture admission")
		}
	}
	_, _, waiter := sequence.packAdmission.tryAcquire(key, true)
	clock := origin.Add(10 * time.Second)
	now := func() time.Time {
		if !sequence.packAdmission.mutex.TryLock() {
			t.Fatal("admission lock crossed component clock boundary")
		}
		sequence.packAdmission.mutex.Unlock()
		if !service.stateLock.TryLock() {
			t.Fatal("service lock crossed component clock boundary")
		}
		service.stateLock.Unlock()
		clock = clock.Add(time.Nanosecond)
		return clock
	}
	row := sequence.pacingDiagnosticSnapshot(origin, now)
	if row.Admission.Count != 2 || row.Admission.Capacity != 2 || row.Admission.WaiterCount != 1 ||
		row.Service.Reservations != 2 || !row.Service.WaiterHead || !row.Service.WaiterQueued || row.Service.WaiterReadyCount != 1 {
		t.Fatalf("lost locked custody: %+v", row)
	}
	if service.burstMeter != meter || service.next != next || service.pacingReservations != reservations || service.sent != sent ||
		service.waiterHead != first || service.waiterTail != second || first.next != second || second.previous != first || len(first.ready) != 1 ||
		sequence.packAdmission.count != 2 || sequence.packAdmission.waiters[0] != waiter || waiter.granted {
		t.Fatal("observation changed rate, credit, waiter order or admission ownership")
	}
	if row.Estimate.StartElapsedNanoseconds > row.Estimate.EndElapsedNanoseconds ||
		row.Estimate.EndElapsedNanoseconds > row.Admission.StartElapsedNanoseconds ||
		row.Admission.EndElapsedNanoseconds > row.Service.StartElapsedNanoseconds ||
		row.Service.StartElapsedNanoseconds > row.Service.EndElapsedNanoseconds || row.Service.MeterAtElapsedNanoseconds != 0 {
		t.Fatal("component clock brackets were not independent and ordered")
	}
	sequence.packAdmission.release(key)
	if !waiter.granted || len(sequence.packAdmission.waiters) != 0 || sequence.packAdmission.count != 2 {
		t.Fatal("snapshot changed the next real slot grant")
	}
	sequence.packAdmission.abandon(waiter)
	sequence.packAdmission.release(key)
	service.stateLock.Lock()
	service.removeWaiterWithLock(first)
	service.stateLock.Unlock()
	if service.waiterHead != second || second.previous != nil || len(second.ready) != 1 {
		t.Fatal("snapshot changed the next real waiter wake")
	}
}

// Reuse actual measured service to catch an accidental retaining estimate call.
func TestTransferPacingDiagnosticDoesNotLearnService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, _, sequence, service := windowPacingStatsObserverFixture(t, 5000, true)
		origin := time.Now()
		for range 3 {
			row := sequence.pacingDiagnosticSnapshot(origin, time.Now)
			if row.Estimate.ServiceByteRate != 500000 || service.serviceHoldRate != 10000000 {
				t.Fatalf("snapshot learned a controller hold: estimate=%d hold=%d", row.Estimate.ServiceByteRate, service.serviceHoldRate)
			}
		}
	})
}

// Caps bound total retained object pointers and output, including many clients.
func TestTransferPacingDiagnosticBoundsInventoryAndSerializedOutput(t *testing.T) {
	for _, counts := range []struct {
		clients, sequences int
		truncated          bool
	}{
		{clients: 1, sequences: 1}, {clients: 1, sequences: 17, truncated: true},
		{clients: 16, sequences: 1}, {clients: 17, sequences: 1, truncated: true},
	} {
		window := &multiClientWindow{clients: map[Id]*multiClientChannel{}}
		for range counts.clients {
			buffer := &SendBuffer{activeSendSequences: map[*SendSequence]bool{}}
			for range counts.sequences {
				buffer.activeSendSequences[newTransferPacingDiagnosticSequence()] = true
			}
			window.clients[NewId()] = &multiClientChannel{client: &Client{sendBuffer: buffer}}
		}
		multi := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
		clock := time.Unix(100, 0)
		row := multi.pacingDiagnosticSnapshot(clock, func() time.Time { clock = clock.Add(time.Nanosecond); return clock })
		if row.Truncated != counts.truncated || len(row.Sequences) != min(16, counts.clients*counts.sequences) || row.ClientCount != counts.clients {
			t.Fatalf("inventory cap: counts=%+v row=%+v", counts, row)
		}
		for _, sequence := range row.Sequences {
			if row.StartElapsedNanoseconds > sequence.Estimate.StartElapsedNanoseconds || sequence.Service.EndElapsedNanoseconds > row.EndElapsedNanoseconds {
				t.Fatal("row escaped capture clock bracket")
			}
		}
	}
	// Maximal scalar widths and longer-than-production identity/reason strings.
	var worst transferPacingDiagnosticSequence
	var fill func(reflect.Value)
	fill = func(value reflect.Value) {
		switch value.Kind() {
		case reflect.Struct:
			for i := range value.NumField() {
				fill(value.Field(i))
			}
		case reflect.Int, reflect.Int64:
			value.SetInt(math.MaxInt64)
		case reflect.Uint32:
			value.SetUint(math.MaxUint32)
		case reflect.Float64:
			value.SetFloat(math.MaxFloat64)
		case reflect.String:
			value.SetString(strings.Repeat("x", 128))
		case reflect.Bool:
			value.SetBool(true)
		default:
			t.Fatalf("diagnostic row retained a non-scalar field: %s", value.Kind())
		}
	}
	fill(reflect.ValueOf(&worst).Elem())
	row := TransferPacingDiagnosticSnapshot{Sequences: make([]transferPacingDiagnosticSequence, 16)}
	for index := range row.Sequences {
		row.Sequences[index] = worst
	}
	encoded, err := json.Marshal(row)
	if err != nil || len(encoded) > 63*1024 {
		t.Fatalf("bounded rows exceed SDK envelope allowance: bytes=%d error=%v", len(encoded), err)
	}
}

// Cancellation, admission close and registry removal may overlap a cold read;
// no goroutine-owned Pack state is inspected and both owners are always joined.
func TestTransferPacingDiagnosticConcurrentCloseIsSafe(t *testing.T) {
	sequence := newTransferPacingDiagnosticSequence()
	sequence.ctx, sequence.cancel = context.WithCancel(context.Background())
	sequence.windowPacer.service = &windowPacingService{}
	buffer := &SendBuffer{activeSendSequences: map[*SendSequence]bool{sequence: true}}
	window := &multiClientWindow{clients: map[Id]*multiClientChannel{NewId(): {client: &Client{sendBuffer: buffer}}}}
	multi := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var observed TransferPacingDiagnosticSnapshot
	go func() {
		defer close(done)
		calls := 0
		observed = multi.pacingDiagnosticSnapshot(time.Now(), func() time.Time {
			calls++
			if calls == 2 {
				close(entered)
				<-release
			}
			return time.Now()
		})
	}()
	<-entered // sequence pointer selected, no owner lock may still be held
	sequence.Close()
	sequence.windowPacer.close()
	buffer.mutex.Lock()
	delete(buffer.activeSendSequences, sequence)
	buffer.mutex.Unlock()
	window.stateLock.Lock()
	clear(window.clients)
	window.stateLock.Unlock()
	close(release)
	<-done
	if len(observed.Sequences) != 1 || !observed.Sequences[0].Admission.Closed {
		t.Fatal("selected closing owner was not captured")
	}
	row := sequence.pacingDiagnosticSnapshot(time.Now(), time.Now)
	if !row.Canceled || !row.Admission.Closed {
		t.Fatal("closing owner state was not retained")
	}
}
