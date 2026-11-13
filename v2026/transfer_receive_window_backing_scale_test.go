package connect

import (
	"math"
	"math/big"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Reflection keeps the identical private control compilable against the old
// product; only candidate roots require the new immutable policy fields.
func setReceiveWindowBackingScaleForTest(t *testing.T, settings *ReceiveBufferSettings, numerator, denominator uint64) {
	t.Helper()
	value := reflect.ValueOf(settings).Elem()
	for name, number := range map[string]uint64{
		"AdvertisedReceiveWindowBudgetScaleNumerator":   numerator,
		"AdvertisedReceiveWindowBudgetScaleDenominator": denominator,
	} {
		field := value.FieldByName(name)
		if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.Uint8 {
			t.Fatal("receive budget has no bounded immutable encoded/physical conversion", name)
		}
		field.SetUint(number)
	}
}

func TestReceiveWindowBackingScaleTracksCapacityWithoutSpendingCreditTwice(t *testing.T) {
	root := NewTransferMemoryBudget(21810380)
	rx := NewTransferMemoryBudgetWithParent(7633633, root)
	settings := &ReceiveBufferSettings{ReceiveQueueMaxByteCount: 2796202}
	setReceiveWindowBackingScaleForTest(t, settings, 2, 7)
	sequence := &ReceiveSequence{receiveQueue: newReceiveQueue(rx, 0), receiveBufferSettings: settings}
	if got := sequence.receiveWindowAdvertisement(); got != 2181038 {
		t.Fatal("physical backing changed the old encoded permission", got)
	}
	const claim ByteCount = 7500000
	if !rx.TryReserve(claim) {
		t.Fatal("actual paid owner did not fit its physical allowance")
	}
	t.Cleanup(func() {
		rx.Release(claim)
		assertRetainedBudgetBalance(t, rx)
		assertRetainedBudgetBalance(t, root)
	})
	if sequence.receiveWindowAdvertisement() != 2181038 {
		t.Fatal("live physical ownership was subtracted again from encoded credit")
	}
	rx.SetTotalByteCount(7340032)
	unexpectedAdmission := rx.TryReserve(1)
	if unexpectedAdmission {
		rx.Release(1)
	}
	if sequence.receiveWindowAdvertisement() != 2097152 || unexpectedAdmission ||
		rx.UsedByteCount() != claim || root.UsedByteCount() != claim || rx.Parent() != root {
		t.Fatal("role resize changed existing custody or lost its old logical permission")
	}
	rx.SetTotalByteCount(7633633)
	if sequence.receiveWindowAdvertisement() != 2181038 {
		t.Fatal("restoring physical role capacity compounded its conversion")
	}
	settings.AdvertisedReceiveWindowMaxByteCount = 655360
	if sequence.receiveWindowAdvertisement() != 655360 {
		t.Fatal("explicit caller cap did not remain the final encoded ceiling")
	}
}

func TestReceiveWindowBackingScaleOddAndMaximumArithmetic(t *testing.T) {
	settings := &ReceiveBufferSettings{ReceiveQueueMaxByteCount: math.MaxInt64}
	setReceiveWindowBackingScaleForTest(t, settings, 2, 7)
	rx := NewTransferMemoryBudget(0)
	sequence := &ReceiveSequence{receiveQueue: newReceiveQueue(rx, 0), receiveBufferSettings: settings}
	for _, pair := range [][2]ByteCount{{4, 1}, {11, 3}, {53, 15}, {7633637, 2181039}} {
		rx.SetTotalByteCount(pair[0])
		if got := sequence.receiveWindowAdvertisement(); got != uint64(pair[1]) {
			t.Fatalf("odd retained capacity %d advertised %d; want %d", pair[0], got, pair[1])
		}
	}
	for _, ratio := range [][2]uint64{{2, 7}, {254, 255}, {1, 255}} {
		setReceiveWindowBackingScaleForTest(t, settings, ratio[0], ratio[1])
		rx.SetTotalByteCount(math.MaxInt64)
		// An independent arbitrary-precision oracle catches overflow even
		// when an overflowing result accidentally stays below the physical cap.
		want := new(big.Int).Mul(big.NewInt(math.MaxInt64), new(big.Int).SetUint64(ratio[0]))
		want.Div(want, new(big.Int).SetUint64(ratio[1]))
		if got := sequence.receiveWindowAdvertisement(); got != want.Uint64() {
			t.Fatalf("maximum capacity ratio %v advertised %d; want %s", ratio, got, want.String())
		}
	}
}

func TestReceiveWindowBackingScaleKeepsLegacyAndUnpooledBounds(t *testing.T) {
	settings := &ReceiveBufferSettings{ReceiveQueueMaxByteCount: 4096}
	rx := NewTransferMemoryBudget(2048)
	sequence := &ReceiveSequence{receiveQueue: newReceiveQueue(rx, 0), receiveBufferSettings: settings}
	if sequence.receiveWindowAdvertisement() != 2048 {
		t.Fatal("zero-value settings changed legacy pooled permission")
	}
	for _, ratio := range [][2]uint64{{0, 0}, {1, 1}, {0, 7}, {1, 0}, {255, 1}} {
		setReceiveWindowBackingScaleForTest(t, settings, ratio[0], ratio[1])
		if sequence.receiveWindowAdvertisement() != 2048 {
			t.Fatal("identity, incomplete or expansive conversion inflated legacy permission", ratio)
		}
	}
	setReceiveWindowBackingScaleForTest(t, settings, 2, 7)
	sequence.receiveQueue = newReceiveQueue(nil, 0)
	if sequence.receiveWindowAdvertisement() != 4096 {
		t.Fatal("unpooled logical hold was scaled as if it were physical backing")
	}
	settings.AdvertisedReceiveWindowMaxByteCount = 1024
	if sequence.receiveWindowAdvertisement() != 1024 {
		t.Fatal("unpooled caller cap was bypassed")
	}
}

func TestReceiveWindowBackingScaleTravelsOnActualSignedAck(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(21810380)
		rx := NewTransferMemoryBudgetWithParent(7633633, root)
		head := NewTransferMemoryBudgetWithParent(2181038, root)
		pack := NewTransferMemoryBudgetWithParent(2181038, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, head, pack, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("signed advertisement left physical or quota ownership")
				}
			}
		})
		layout := consumerPaidPackByteLayout{label: "backed32", byteLimit: 174762, leafBytes: 2796202, rootBytes: 21810380, countLimit: 85}
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				if receiver {
					settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
					setReceiveWindowBackingScaleForTest(t, settings.ReceiveBufferSettings, 2, 7)
				}
			})
		for index, row := range [][2]ByteCount{{7633633, 2181038}, {7340032, 2097152}, {7633633, 2181038}} {
			rx.SetTotalByteCount(row[0])
			var acknowledged atomic.Bool
			var completed atomic.Int64
			wire := consumerPaidPackByteSend(t, fixture, 1, 8, index, &acknowledged, &completed, time.Now())
			fixture.forward(wire.frame, fixture.receiverIn)
			fixture.acknowledge()
			sender := inflightHoleSender(t, fixture, wire.frame)
			advertisement, known := sender.receivedWindowAdvertisement()
			if !acknowledged.Load() || !known || advertisement != row[1] || root.UsedByteCount() != 0 || len(sender.sendItems) != 0 {
				t.Fatalf("actual signed ACK lost role permission or custody: known=%t credit=%d want=%d", known, advertisement, row[1])
			}
		}
	})
}
