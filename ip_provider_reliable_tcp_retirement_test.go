package connect

import (
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Without a terminal-reset owner, retiring an indexed flow only secures empty
// terminal controls. Application bytes retain the failed final-owner outcome,
// including retirement after lookup, and every prepaid reservation returns to
// its original root. Confirmed reset ownership has separate positive controls.
func TestProviderReliableTcpRetiredPayloadWithoutResetNeverAcked(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, closeAfterLookup := range []bool{false, true} {
			t.Run(fmt.Sprintf("ipv%d/after-lookup-%t", version, closeAfterLookup), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					budget := NewTransferMemoryBudget(mib(2))
					t.Cleanup(func() {
						if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
							t.Error("retired payload leaked prepaid root ownership")
						}
					})
					f := newReliableTcpIngressFixture(t, version, budget)
					f.provider.returnStateLock.Lock()
					f.provider.returnClosed = true
					f.provider.returnStateLock.Unlock()
					f.establishForControl()
					sequence := f.runningReceiveSequence()
					synctest.Wait()
					if closeAfterLookup {
						f.nat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(flow *TcpSequence) { flow.Cancel() }
					} else {
						f.tcp.Cancel()
					}
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, f.path, 0, id, tcpFlagFin|tcpFlagAck, []byte("no final TCP owner"))
					synctest.Wait()
					if present, _ := reliableIngressCumulativeHead(sequence); present {
						t.Error("retired flow falsely cumulatively ACKed application bytes")
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
						t.Error("retired flow falsely selectively ACKed application bytes")
					}
					if sequence.ctx.Err() == nil || len(f.tcp.sendItems) != 1 || f.dials.Load() != 0 {
						t.Error("retired payload no longer rejects its missing final owner")
					}
				})
			})
		}
	}
}

// The retirement recheck may only consume a terminal control while its NAT
// and source admission authority are live. Lose either authority precisely
// between lookup and send; neither an orphan reset nor a Transfer ACK is due.
func TestProviderReliableTcpAdmissionShutdownNeverAcked(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, stop := range []string{"source-retired", "nat-shutdown"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, stop), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					f.establishForControl()
					sequence := f.runningReceiveSequence()
					var resets atomic.Int32
					remove := f.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, _ []byte) { resets.Add(1) })
					defer remove()
					owner := f.nat.newSourceRetirementOwner()
					defer f.nat.releaseSourceRetirementOwner(owner)
					synctest.Wait()
					var lookups int
					f.nat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(flow *TcpSequence) {
						lookups++
						if stop == "source-retired" {
							f.nat.retireSourceForOwner(owner, f.source.SourceId)
						} else {
							f.nat.Close()
						}
					}
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, f.path, 0, id, tcpFlagFin|tcpFlagAck, nil)
					synctest.Wait()
					if lookups != 1 || f.tcp.ctx.Err() == nil {
						t.Fatal("fixture did not retire authority between lookup and admission")
					}
					if present, _ := reliableIngressCumulativeHead(sequence); present {
						t.Error("retired authority falsely cumulatively ACKed terminal control")
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
						t.Error("retired authority falsely selectively ACKed terminal control")
					}
					if resets.Load() != 0 {
						t.Error("retired authority synthesized a terminal reset")
					}
				})
			})
		}
	}
}
