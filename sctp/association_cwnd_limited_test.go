// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

package sctp

import (
	"math"
	"net"
	"testing"
)

func newCwndLimitedGrowthAssociation(t *testing.T) *Association {
	t.Helper()
	left, right := net.Pipe()
	a := createTestAssociation(t, Config{MTU: 1191, NetConn: left})
	t.Cleanup(func() { _ = a.close(); _ = right.Close() })
	return a
}

func TestGrowthNeedsRecordedCwndBlockedFlight(t *testing.T) {
	a := newCwndLimitedGrowthAssociation(t)
	a.setCWND(10000)
	a.ssthresh = 1_000_000
	a.cumulativeTSNAckPoint = 100
	a.onCumulativeTSNAckPointAdvanced(1000)
	if a.CWND() != 10000 {
		t.Fatal("app-limited send grew cwnd")
	}
	a.cwndLimitedFlight, a.cwndLimitedUntilTSN = true, 102
	a.cumulativeTSNAckPoint = 101
	a.onCumulativeTSNAckPointAdvanced(1000)
	if a.CWND() != 11000 || !a.cwndLimitedFlight {
		t.Fatal("first ACK did not retain its limited flight")
	}
	a.cumulativeTSNAckPoint = 102
	a.onCumulativeTSNAckPointAdvanced(1000)
	if a.CWND() != 12000 || a.cwndLimitedFlight {
		t.Fatal("terminal ACK did not consume its limited flight")
	}
	a.cumulativeTSNAckPoint = 103
	a.onCumulativeTSNAckPointAdvanced(1000)
	if a.CWND() != 12000 {
		t.Fatal("expired flight grew cwnd")
	}
	a.cwndLimitedFlight, a.cwndLimitedUntilTSN = true, 110
	a.inFastRecovery = true
	a.cumulativeTSNAckPoint = 104
	a.onCumulativeTSNAckPointAdvanced(1000)
	if a.CWND() != 12000 {
		t.Fatal("fast recovery grew cwnd")
	}
}

func TestFlightMarkRequiresCwndNotReceiverWindow(t *testing.T) {
	for _, rwnd := range []uint32{0, 100000} {
		a := newCwndLimitedGrowthAssociation(t)
		a.setCWND(4380)
		a.setRWND(rwnd)
		a.myNextTSN = 103
		for tsn := uint32(100); tsn < 103; tsn++ {
			a.inflightQueue.pushNoCheck(&chunkPayloadData{tsn: tsn, userData: make([]byte, 1134)})
		}
		a.pendingQueue.push(&chunkPayloadData{userData: make([]byte, 1134), unordered: true, beginningFragment: true, endingFragment: true})
		budget, consumed := int64(math.MaxInt64), false
		chunks, _ := a.popPendingDataChunksToSend(&budget, &consumed)
		if len(chunks) != 0 || a.cwndLimitedFlight != (rwnd > 0) {
			t.Fatalf("wrong mark: rwnd=%d mark=%t chunks=%d", rwnd, a.cwndLimitedFlight, len(chunks))
		}
		if rwnd > 0 && a.cwndLimitedUntilTSN != 102 {
			t.Fatal("mark does not end at actual sent flight")
		}
	}
}

func TestFlightMarkExpiresAcrossTsnWrap(t *testing.T) {
	a := newCwndLimitedGrowthAssociation(t)
	a.setCWND(10000)
	a.ssthresh = 1_000_000
	a.cwndLimitedFlight, a.cwndLimitedUntilTSN = true, 1
	a.cumulativeTSNAckPoint = math.MaxUint32
	a.onCumulativeTSNAckPointAdvanced(1000)
	if !a.cwndLimitedFlight || a.CWND() != 11000 {
		t.Fatal("wrapped flight expired early")
	}
	a.cumulativeTSNAckPoint = 1
	a.onCumulativeTSNAckPointAdvanced(1000)
	if a.cwndLimitedFlight || a.CWND() != 12000 {
		t.Fatal("wrapped flight did not expire")
	}
}

func TestCongestionDecreaseClearsFlightEligibility(t *testing.T) {
	a := newCwndLimitedGrowthAssociation(t)
	a.setCWND(10000)
	a.cwndLimitedFlight, a.cwndLimitedUntilTSN = true, 100
	a.setCWND(5000)
	if a.cwndLimitedFlight {
		t.Fatal("congestion reduction retained old growth eligibility")
	}
}
