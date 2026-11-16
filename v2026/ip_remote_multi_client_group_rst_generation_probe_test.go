// Separate source-generation blocker, intentionally outside the narrow
// canceled-source successor's execution overlay. No test result is claimed.
package connect

import (
	"testing"
	"testing/synctest"
)

// Both public calls finish while the actual source is held before Run. The
// second accepted SYN therefore belongs to a live flow at its own admission,
// but per-group RST retirement can cancel it before source replay reaches it.
func TestTcpGroupRecoverySeparateAcceptedResetThenNewSynKeepsCohort(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		old := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		f.update.updateSequence(groupRecoveryParsed(t, old))
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		syn := groupDispositionControlPacket(900, 800, 64, tcpFlagSyn)
		if f.offer(rst) != 1 || f.offer(syn) != 1 {
			t.Fatal("separate reset/new-SYN did not both reach public successful admission")
		}
		f.requireParked()
		if f.update.IsDone() || f.update.sourceRstSequence() != 800 {
			t.Fatal("pre-Run reset already canceled the later accepted SYN")
		}
		f.unpark()
		f.acknowledgeAll(rst, syn)
		if f.update.IsDone() || f.update.synGenerationNumber != 900 || f.update.sourceRstSequence() != 800 {
			t.Fatal("earlier source reset orphaned the separately accepted new-SYN cohort")
		}
		if f.offer(syn) != 0 {
			t.Fatal("surviving retained new-SYN cohort lost its durable duplicate gate")
		}
	})
}
