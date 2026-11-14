package connect

import (
	"context"
	"testing"
	"testing/synctest"
)

// A memory-policy option must not silently change reliable delivery, and a
// complete later policy must replace it just as it replaces the other options.
func TestNoBudgetOptionPreservesAckAndOptionOrder(t *testing.T) {
	client := &Client{ctx: context.Background(), settings: DefaultClientSettings()}
	ordinary := DefaultTransferOpts()
	for _, test := range []struct {
		name     string
		options  []any
		noBudget bool
		ack      bool
	}{
		{name: "ordinary defaults", ack: true},
		{name: "protocol reliable", options: []any{NoBudget()}, noBudget: true, ack: true},
		{name: "unreliable protocol", options: []any{NoAck(), NoBudget()}, noBudget: true},
		{name: "ack option after exemption", options: []any{NoBudget(), NoAck()}, noBudget: true},
		{name: "complete later policy", options: []any{NoBudget(), ordinary}, ack: true},
		{name: "exemption after complete policy", options: []any{ordinary, NoBudget()}, noBudget: true, ack: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := client.resolveSendOptions(test.options)
			if resolved.transferOptions.NoBudget != test.noBudget || resolved.transferOptions.Ack != test.ack {
				t.Fatalf("resolved memory exemption=%t ack=%t; want exemption=%t ack=%t", resolved.transferOptions.NoBudget, resolved.transferOptions.Ack, test.noBudget, test.ack)
			}
			if client.settings.DefaultTransferOpts != ordinary {
				t.Fatal("per-send exemption changed the client's default policy")
			}
		})
	}
}

// A memory exemption is not a claim that the retained wire original has no
// bytes. Live diagnostics must expose its real cost while its DATA charge is 0.
func TestNoBudgetLiveSnapshotRetainsActualEncodedBytes(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.DefaultTransferOpts.NoBudget = true
			settings.SendBufferSettings.LiveDiagnostics = true
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(0)
		})
		original := fixture.write(64)
		sample := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		encoded := ByteCount(len(original.bytes))
		if encoded == 0 || sample.OriginalItemCount != 1 || sample.UnacknowledgedItemCount != 1 ||
			sample.OriginalEncodedByteCount != encoded || sample.UnacknowledgedEncodedByteCount != encoded ||
			sample.OriginalRetainedChargeByteCount != 0 {
			t.Fatalf("protocol original disappeared from wire diagnostics: %+v; actual encoded bytes=%d", sample, encoded)
		}
	})
}

// DATA with no callback is still a member of a coalesced wire Pack. Dropping
// its empty callback record must not let a neighboring control exempt it.
func TestNoBudgetCoalescingIncludesCallbackFreeData(t *testing.T) {
	burst := make([]sendAckRecord, sendPackH1GroupMaxFrames)
	for index := range burst {
		burst[index] = sendAckRecord{noBudget: true}
	}
	mixedBurst := append(append([]sendAckRecord(nil), burst...), sendAckRecord{})
	for _, test := range []struct {
		name    string
		records []sendAckRecord
		want    bool
	}{
		{name: "no classified originals"},
		{name: "callback free data", records: []sendAckRecord{{}}},
		{name: "protocol only", records: []sendAckRecord{{noBudget: true}, {noBudget: true}}, want: true},
		{name: "data after protocol", records: []sendAckRecord{{noBudget: true}, {}}},
		{name: "data before protocol", records: []sendAckRecord{{}, {noBudget: true}}},
		{name: "protocol group", records: []sendAckRecord{{group: &sendGroupCompletion{ack: sendAckRecord{noBudget: true}}}}, want: true},
		{name: "data group after protocol", records: []sendAckRecord{{noBudget: true}, {group: &sendGroupCompletion{}}}},
		{name: "full protocol burst", records: burst, want: true},
		{name: "callback free data after full burst", records: mixedBurst},
	} {
		t.Run(test.name, func(t *testing.T) {
			var records sendAckSet
			for _, record := range test.records {
				records.add(record)
			}
			defer returnSendAckSetOverflow(records.overflow)
			if got := records.bypassesBudget(); got != test.want {
				t.Fatalf("coalesced budget exemption=%t; want %t", got, test.want)
			}
		})
	}
}
