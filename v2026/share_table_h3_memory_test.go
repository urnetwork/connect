package connect

import (
	"fmt"
	"testing"
)

// Independent ledger oracle: intentionally pin the byte total instead of
// calling applyPlatformH3MemoryPolicy or its fixed-size helper. A production
// envelope change must update both the retained-owner evidence and this table.
const shareTableH3FixedByteCount ByteCount = 1600 * 1024

type shareTableH3Ledger struct {
	reservation, stream, connection, fixed ByteCount
}

func shareTableH3LedgerForTarget(target ByteCount) shareTableH3Ledger {
	draw := target / 8
	ledger := shareTableH3Ledger{
		reservation: max(mib(3), draw),
		stream:      max(kib(384), draw*3/4),
		connection:  max(kib(512), draw),
	}
	if 0 < target && target <= mib(32) {
		ledger.fixed = shareTableH3FixedByteCount
		if target <= mib(24) {
			ledger.connection = min(ledger.connection, ledger.reservation-ledger.fixed)
			ledger.stream = min(ledger.stream, ledger.connection*3/4)
		} else {
			ledger.reservation = max(ledger.reservation, ledger.connection+ledger.fixed)
		}
	}
	return ledger
}

func TestTheShareTableH3RetainedPolicyBoundaries(t *testing.T) {
	restore := MemoryBudget()
	t.Cleanup(func() { SetMemoryBudget(restore) })
	if got := platformH3FixedMemoryByteCount(); got != shareTableH3FixedByteCount {
		t.Fatalf("H3 fixed ownership changed: %d, ledger %d", got, shareTableH3FixedByteCount)
	}
	for _, row := range []struct {
		target, reservation, connection, stream ByteCount
		retained                                bool
	}{
		{mib(8), kib(3072), kib(1024), kib(768), true},
		{kib(11776) - 8, kib(3072), kib(1472) - 1, kib(1104) - 1, true},
		{kib(11776), kib(3072), kib(1472), kib(1104), true},
		{mib(16), kib(3072), kib(1472), kib(1104), true},
		{mib(20), kib(3072), kib(1472), kib(1104), true},
		{mib(24), kib(3072), kib(1472), kib(1104), true},
		{mib(24) + 8, kib(4672) + 1, kib(3072) + 1, kib(2304), true},
		{mib(28), kib(5184), kib(3584), kib(2688), true},
		{mib(32), kib(5696), kib(4096), kib(3072), true},
		{mib(32) + 8, kib(4096) + 1, kib(4096) + 1, kib(3072), false},
		{mib(48), kib(6144), kib(6144), kib(4608), false},
		{mib(128), kib(16384), kib(16384), kib(12288), false},
	} {
		for _, process := range []ByteCount{0, mib(8), mib(32), mib(40), mib(256)} {
			t.Run(fmt.Sprintf("target%d/process%d", row.target, process), func(t *testing.T) {
				SetMemoryBudget(process)
				settings := DefaultPlatformTransportSettingsWithMemoryTarget(row.target)
				config := newPlatformQuicConfig(settings, 1)
				if settings.h3RetainedByteAccounting != row.retained || settings.H3BudgetByteCount != row.reservation ||
					ByteCount(config.MaxConnectionReceiveWindow) != row.connection || ByteCount(config.MaxStreamReceiveWindow) != row.stream {
					t.Fatalf("owner policy crossed surfaces: retained=%t claim=%d conn=%d stream=%d, want %+v",
						settings.h3RetainedByteAccounting, settings.H3BudgetByteCount,
						config.MaxConnectionReceiveWindow, config.MaxStreamReceiveWindow, row)
				}
				initialStream, initialConnection := kib(256), kib(512)
				if row.retained {
					initialStream, initialConnection = kib(128), kib(256)
				}
				if config.InitialStreamReceiveWindow != uint64(initialStream) || config.InitialConnectionReceiveWindow != uint64(initialConnection) {
					t.Fatalf("initial credit inherited process %d instead of target %d: %d/%d, want %d/%d",
						process, row.target, config.InitialStreamReceiveWindow, config.InitialConnectionReceiveWindow,
						initialStream, initialConnection)
				}
				want := shareTableH3LedgerForTarget(row.target)
				if want.reservation != row.reservation || want.connection != row.connection || want.stream != row.stream {
					t.Fatalf("share-table oracle differs from pinned boundary: %+v, row %+v", want, row)
				}
				if settings.PlatformTransportBudget.parent != nil && settings.PlatformTransportBudget.parent != DefaultPlatformTransportBudget() ||
					(process > 0) != (settings.PlatformTransportBudget.parent != nil) {
					t.Fatal("explicit owner carrier lost its process-root parent")
				}
			})
		}
	}
}
