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
		// R20: the claim is a fifth of the owner's carrier budget, never less
		// than the fixed rows plus 448 KiB of connection credit, and it is
		// exactly that credit plus the fixed rows
		ledger.fixed = shareTableH3FixedByteCount
		budget := min(target, max(mib(3), target/4))
		claim := max(ledger.fixed+kib(448), budget/5)
		ledger.connection = min(ledger.connection, claim-ledger.fixed)
		ledger.stream = min(ledger.stream, ledger.connection*3/4)
		ledger.reservation = ledger.connection + ledger.fixed
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
		// every retained target through 32 MiB takes the 2-MiB floor claim:
		// a fifth of its budget is less than the fixed rows plus the least
		// credit, so the old 24-MiB step and its draw boundary are gone
		{mib(8), kib(2048), kib(448), kib(336), true},
		{kib(11776) - 8, kib(2048), kib(448), kib(336), true},
		{kib(11776), kib(2048), kib(448), kib(336), true},
		{mib(16), kib(2048), kib(448), kib(336), true},
		{mib(20), kib(2048), kib(448), kib(336), true},
		{mib(24), kib(2048), kib(448), kib(336), true},
		{mib(24) + 8, kib(2048), kib(448), kib(336), true},
		{mib(28), kib(2048), kib(448), kib(336), true},
		{mib(32), kib(2048), kib(448), kib(336), true},
		{mib(32) + 8, kib(4096) + 1, kib(4096) + 1, kib(3072), false},
		{mib(48), kib(6144), kib(6144), kib(4608), false},
		{mib(128), kib(16384), kib(16384), kib(12288), false},
	} {
		for _, process := range []ByteCount{0, mib(8), mib(32), mib(40), mib(256)} {
			t.Run(fmt.Sprintf("target%d/process%d", row.target, process), func(t *testing.T) {
				SetMemoryBudget(process)
				settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(row.target)
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
				if settings.PlatformTransportBudget.parent != nil || settings.PlatformTransportBudget.root != settings.PlatformTransportBudget {
					t.Fatal("explicit owner carrier unexpectedly shares an admission root")
				}
			})
		}
	}
}
