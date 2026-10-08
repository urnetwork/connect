package connect

import (
	"errors"
	"testing"
)

// Both public snapshots publish the same lifetime eviction-resend counter;
// reading either view must neither consume it nor count unrelated recovery.
func TestClientReceiveStatsPublishesEvictionResends(t *testing.T) {
	client := &Client{}
	check := func(want uint64) {
		t.Helper()
		for range 3 {
			recovery := client.SendRecoveryStats()
			receive := client.ReceiveStats()
			if receive.SendEvictionResendCount != want || recovery.SendEvictionResendCount != want {
				t.Fatalf("eviction resends: receive=%d recovery=%d, want both %d",
					receive.SendEvictionResendCount, recovery.SendEvictionResendCount, want)
			}
		}
		if got := client.sendEvictionResendCount.Load(); got != want {
			t.Fatalf("snapshot reads changed lifetime eviction resends to %d, want %d", got, want)
		}
	}
	check(0)
	client.recordSendRecovery(sendRecoverySelectiveGap, nil)
	check(0)
	client.recordSendRecovery(sendRecoveryEviction, nil)
	check(1)
	previous := client.ReceiveStats()
	client.recordSendRecovery(sendRecoveryEviction, errors.New("failed eviction resend"))
	check(2)
	if previous.SendEvictionResendCount != 1 {
		t.Fatal("later recovery mutated an earlier receive snapshot")
	}
	if got := client.SendRecoveryStats().RecoveryWriteErrorCount; got != 1 {
		t.Fatalf("failed eviction resend error count=%d, want one", got)
	}
}
