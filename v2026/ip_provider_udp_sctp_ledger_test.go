//go:build !js

package connect

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"
)

const (
	udpSctpLedgerSharedBytes    ByteCount = 768 * 1024
	udpSctpLedgerSCTPOwnerBytes ByteCount = 512 * 1024
)

// This is a detached, real SCTP diagnostic, not a replay of a round-04 run.
// The source retains its 93/468 fixed offers, 1000-byte payload, zero-wait
// provider callback, 32-Pack/4-route bounds and 512-KiB receive reservation.
// These profiles copy the frozen cell-edge rates, delay, jitter, byte/count
// limits, burst credit and loss processes. The fresh PRNG starts after an
// explicit clean warmup; historical ICE/DTLS/control draws are unavailable.
// Wire charging includes the original 28-byte IP/UDP and 37-byte DTLS cost.
// ICE, encryption and the clean provider link are not instantiated here.
type udpSctpLedgerProfile struct {
	name                       string
	downRate, upRate           int64
	rtt, jitter, queueDuration time.Duration
	mtu                        int
	independentLoss            float64
	burst                      bool
}

type udpSctpLedgerLinkStats struct {
	Submitted, Admitted, Delivered, QueueDrops, LossDrops, Canceled int
	OwnedPackets, OwnedBytes, PeakPackets, PeakBytes                int
}

type udpSctpLedgerPacket struct {
	bytes  []byte
	charge int
	at     time.Time
	lost   bool
}

type udpSctpLedgerLink struct {
	ctx                      context.Context
	profile                  udpSctpLedgerProfile
	rate                     int64
	queueBytes, queuePackets int
	out                      chan udpSctpAckExperimentPacket
	ready                    chan struct{}
	done                     chan struct{}
	mutex                    sync.Mutex
	random                   *rand.Rand
	bad                      bool
	rateCursor, fifo         time.Time
	packets                  []udpSctpLedgerPacket
	stats                    udpSctpLedgerLinkStats
}

func newUdpSctpLedgerLink(ctx context.Context, p udpSctpLedgerProfile, download bool, seed int64, out chan udpSctpAckExperimentPacket) *udpSctpLedgerLink {
	rate := p.upRate
	if download {
		rate = p.downRate
	}
	queueBytes := int(float64(rate) / 8 * p.queueDuration.Seconds())
	q := &udpSctpLedgerLink{ctx: ctx, profile: p, rate: rate,
		queueBytes: queueBytes, queuePackets: max(8, (queueBytes+p.mtu-1)/p.mtu),
		out: out, ready: make(chan struct{}, 1), done: make(chan struct{}),
		random: rand.New(rand.NewSource(seed))}
	go q.run()
	return q
}

func (q *udpSctpLedgerLink) submit(wire []byte) (int, error) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	q.stats.Submitted++
	charge := len(wire) + 65
	if charge > q.profile.mtu {
		return 0, fmt.Errorf("diagnostic SCTP wire %d exceeds frozen MTU %d", charge, q.profile.mtu)
	}
	if q.stats.OwnedPackets >= q.queuePackets || q.stats.OwnedBytes+charge > q.queueBytes {
		q.stats.QueueDrops++
		return len(wire), nil
	}
	q.stats.Admitted++
	q.stats.OwnedPackets++
	q.stats.OwnedBytes += charge
	q.stats.PeakPackets = max(q.stats.PeakPackets, q.stats.OwnedPackets)
	q.stats.PeakBytes = max(q.stats.PeakBytes, q.stats.OwnedBytes)
	lost := false
	if q.profile.burst {
		if q.bad {
			if q.random.Float64() < 0.35 {
				q.bad = false
			}
		} else if q.random.Float64() < 0.01 {
			q.bad = true
		}
		probability := 0.002
		if q.bad {
			probability = 0.65
		}
		lost = q.random.Float64() < probability
	} else {
		lost = q.random.Float64() < q.profile.independentLoss
	}
	now := time.Now()
	byteRate := float64(q.rate) / 8
	burstDuration := time.Duration(float64(time.Second) * float64(q.profile.mtu) / byteRate)
	serialization := time.Duration(float64(time.Second) * float64(charge) / byteRate)
	if minimum := now.Add(-burstDuration); q.rateCursor.Before(minimum) {
		q.rateCursor = minimum
	}
	q.rateCursor = q.rateCursor.Add(serialization)
	rateReady := now
	if rateReady.Before(q.rateCursor) {
		rateReady = q.rateCursor
	}
	jitter := time.Duration(0)
	if !lost {
		jitter = time.Duration(q.random.Int63n(int64(2*q.profile.jitter)+1) - int64(q.profile.jitter))
		_ = q.random.Float64() // unchanged zero-probability reorder draw
	}
	at := rateReady.Add(q.profile.rtt/2 + jitter)
	if at.Before(now) {
		at = now
	}
	if at.Before(q.fifo) {
		at = q.fifo
	}
	q.fifo = at
	if !lost {
		_ = q.random.Float64()
	} // unchanged zero-probability duplicate draw
	q.packets = append(q.packets, udpSctpLedgerPacket{wire, charge, at, lost})
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return len(wire), nil
}

func (q *udpSctpLedgerLink) run() {
	defer close(q.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		q.mutex.Lock()
		if q.ctx.Err() != nil {
			q.stats.Canceled += len(q.packets)
			q.packets = nil
			q.stats.OwnedPackets = 0
			q.stats.OwnedBytes = 0
			q.mutex.Unlock()
			return
		}
		var wait time.Duration
		if len(q.packets) > 0 {
			packet := q.packets[0]
			wait = time.Until(packet.at)
			if wait <= 0 {
				q.packets[0] = udpSctpLedgerPacket{}
				q.packets = q.packets[1:]
				if packet.lost {
					q.stats.LossDrops++
				} else {
					select {
					case q.out <- udpSctpAckExperimentPacket{bytes: packet.bytes, at: packet.at}:
						q.stats.Delivered++
					default:
						panic("SCTP diagnostic receiver overflow")
					}
				}
				q.stats.OwnedPackets--
				q.stats.OwnedBytes -= packet.charge
				q.mutex.Unlock()
				continue
			}
		}
		q.mutex.Unlock()
		var tick <-chan time.Time
		if wait > 0 {
			timer.Reset(wait)
			tick = timer.C
		}
		select {
		case <-q.ctx.Done():
		case <-q.ready:
		case <-tick:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (q *udpSctpLedgerLink) snapshot() udpSctpLedgerLinkStats {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	return q.stats
}

type udpSctpLedgerPoint struct {
	At                                time.Duration
	Offered, Admitted, Refused        int
	PoolBytes                         ByteCount
	PoolRoots                         uint64
	SctpStreamBytes                   uint64
	SctpAssociationBytes              int
	Cwnd, Rwnd                        uint32
	Route, PackAdmission, PackChannel int
	PhysicalWrites                    int64
	QueueBudget                       ByteCount
}

// A sampled LOWER BOUND, not an admission budget or a proof of all-owner green.
// The root counter includes provider, Pack, route and compact copies together;
// a shared pool root is counted once. Stream.BufferedAmount includes the extra
// copy packetized before a blocked sendPayloadData enters its pending queue.
// The known compact owner pays for its entries and notification channels.
// Every still-admitted SendPack has one distinct descriptor, counted by its
// actual compiled size. Remaining SCTP/provider metadata, marshaling roots and runtime stacks
// are deliberately free here: exceeding the ceiling remains conclusive.
func (p udpSctpLedgerPoint) lowerBound() ByteCount {
	compactOwner := p2pLegacySendQueueOwnerByteCount + 36*ByteCount(unsafe.Sizeof(p2pLegacySendEntry{}))
	return udpSctpLedgerSCTPOwnerBytes + compactOwner + p.PoolBytes + ByteCount(p.PoolRoots)*MessagePoolMetaByteCount + ByteCount(p.SctpStreamBytes) + ByteCount(p.PackAdmission)*ByteCount(unsafe.Sizeof(SendPack{}))
}

// Distinct original payloads, with every copy, pool-size rounding, IP/Pack
// header and descriptor free. All calls in this fixture are one 1132-byte
// envelope. The stream count includes the active pre-pending packetization,
// so subtracting calls and adding stream-owned messages removes the copy
// overlap without crediting unacknowledged SCTP storage as freed service.
// Refused offers are included: this is the necessary lower bound for a
// full-admission counterfactual with the SAME measured service prefix.
func (p udpSctpLedgerPoint) distinctPayloads() int64 {
	return int64(p.Offered) - p.PhysicalWrites + int64(p.SctpStreamBytes/1132)
}

func (p udpSctpLedgerPoint) idealPayloadBytes() ByteCount {
	return udpSctpLedgerSCTPOwnerBytes + ByteCount(p.distinctPayloads()*1000)
}

func TestProviderUdpSctpCellEdgeLedgerWitness(t *testing.T) {
	testProviderUdpSctpCellEdgeLedger(t, false)
}

// This deliberately red research selector applies the unchanged source gate
// and a NECESSARY memory condition. A pass would still require an all-owner
// ledger; failure is already conclusive. Do not include it in production CI.
func TestProviderUdpSctpCellEdgeNecessarySourceMemoryGate(t *testing.T) {
	if os.Getenv("URNETWORK_UDP_LEDGER_REQUIRE_GREEN") != "1" {
		t.Skip("opt-in deliberately red research gate: URNETWORK_UDP_LEDGER_REQUIRE_GREEN=1")
	}
	testProviderUdpSctpCellEdgeLedger(t, true)
}

func testProviderUdpSctpCellEdgeLedger(t *testing.T, requireGreen bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	profiles := []udpSctpLedgerProfile{
		{"5m-down-1m-up", 5_000_000, 1_000_000, 120 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 1400, 0.005, false},
		{"1m-down-250k-up", 1_000_000, 250_000, 300 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, 1280, 0, true},
	}
	// Preserve all six archived cell seeds, without selecting favorable seeds.
	// Starting their PRNG at the workload boundary does NOT reproduce the
	// archived loss decisions after unavailable setup/control packet draws.
	cells := []struct {
		record, profile int
		seed            int64
	}{
		{20, 1, 2894553644369317648},
		{68, 0, 8076106085321773020},
		{139, 0, 2452518751217058672},
		{210, 0, 5545198744060918357},
		{308, 1, 3243622375786476634},
		{356, 0, 945450483909521906},
	}
	for _, cell := range cells {
		profile := profiles[cell.profile]
		t.Run(fmt.Sprintf("seed-record%d/%s", cell.record, profile.name), func(t *testing.T) {
			budget := NewTransferMemoryBudget(udpSctpLedgerSharedBytes)
			if !budget.TryReserve(udpSctpLedgerSCTPOwnerBytes) {
				t.Fatal("fixed receive reservation refused")
			}
			var firstRefusal, firstOver, peak, staging, firstIdealOver, idealPeak udpSctpLedgerPoint
			var down, up udpSctpLedgerLinkStats
			observe := func(p udpSctpLedgerPoint) {
				if p.SctpStreamBytes%1132 != 0 || p.distinctPayloads() < 0 {
					t.Fatalf("not whole frozen messages: %+v", p)
				}
				if p.Refused > 0 && firstRefusal.Offered == 0 {
					firstRefusal = p
				}
				if p.lowerBound() > udpSctpLedgerSharedBytes && firstOver.Offered == 0 {
					firstOver = p
				}
				if p.lowerBound() > peak.lowerBound() {
					peak = p
				}
				if p.SctpStreamBytes > uint64(p.SctpAssociationBytes) && staging.Offered == 0 {
					staging = p
				}
				if p.idealPayloadBytes() > idealPeak.idealPayloadBytes() {
					idealPeak = p
				}
				if p.idealPayloadBytes() > udpSctpLedgerSharedBytes && firstIdealOver.Offered == 0 {
					firstIdealOver = p
				}
			}
			result := runProviderUdpSctpQueueExperiment(t, profile.rtt, false, false, 4, 0,
				udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget,
					ledgerProfile: &profile, ledgerSeed: cell.seed, settledLedger: observe, frozenEnvelope: true,
					closedLedgerLinks: func(d, u udpSctpLedgerLinkStats) { down, up = d, u }})
			t.Logf("detached real SCTP; fixed source=%d admitted=%d refused=%d; result=%+v", result.admitted+result.refused, result.admitted, result.refused, result)
			t.Logf("first_refusal=%+v lower_bound=%d", firstRefusal, firstRefusal.lowerBound())
			t.Logf("first_over_budget=%+v lower_bound=%d; peak=%+v lower_bound=%d", firstOver, firstOver.lowerBound(), peak, peak.lowerBound())
			t.Logf("first_pre_pending_staging=%+v uncounted_by_association=%d", staging, int64(staging.SctpStreamBytes)-int64(staging.SctpAssociationBytes))
			t.Logf("ideal_distinct_payloads=%d ideal_bytes=%d minimum_extra_freed_messages=%d first_ideal_over=%+v ideal_peak=%+v", idealPeak.distinctPayloads(), idealPeak.idealPayloadBytes(), max(0, (idealPeak.idealPayloadBytes()-udpSctpLedgerSharedBytes+999)/1000), firstIdealOver, idealPeak)
			t.Logf("modeled_link_owners_after_cancel down=%+v up=%+v", down, up)
			for _, link := range []udpSctpLedgerLinkStats{down, up} {
				if link.Submitted != link.Admitted+link.QueueDrops || link.Admitted != link.Delivered+link.LossDrops+link.Canceled || link.OwnedPackets != 0 || link.OwnedBytes != 0 {
					t.Fatalf("modeled link owner or terminal accounting leaked: %+v", link)
				}
			}
			if profile.downRate == 5_000_000 && (down.PeakPackets > 45 || down.PeakBytes > 62500 || up.PeakPackets > 9 || up.PeakBytes > 12500) {
				t.Fatal("changed fixed 45/9 packet or byte limit")
			}
			want := 468
			if profile.downRate == 1_000_000 {
				want = 93
			}
			if result.admitted+result.refused != want {
				t.Fatal("changed original finite offer")
			}
			if result.wireBytes != int64(1132*result.admitted) || result.wireWrites != int64(result.admitted) {
				t.Fatal("changed frozen ordinary NoAck wire envelope")
			}
			if result.finalSctpStreamBytes != 0 || result.finalSctpAssociationBytes != 0 {
				t.Fatal("SCTP pending/staging/inflight payload owner did not drain")
			}
			if budget.UsedByteCount() != udpSctpLedgerSCTPOwnerBytes {
				t.Fatalf("compact owner leak: %d", budget.UsedByteCount())
			}
			budget.Release(udpSctpLedgerSCTPOwnerBytes)
			if staging.Offered == 0 {
				t.Fatal("did not observe packetized SCTP staging outside association queue")
			}
			if firstRefusal.Offered > 0 && (firstRefusal.Route != 4 || firstRefusal.PackAdmission != 32 || firstRefusal.Rwnd <= firstRefusal.Cwnd) {
				t.Fatal("first refusal was not the open-rwnd, full-route/Pack boundary")
			}
			if requireGreen && (result.refused != 0 || peak.lowerBound() > udpSctpLedgerSharedBytes) {
				t.Errorf("unchanged necessary source/memory gate RED: admitted=%d/%d lower_bound=%d limit=%d (unmeasured owners can only add bytes)", result.admitted, want, peak.lowerBound(), udpSctpLedgerSharedBytes)
			}
			// No assertion calls this source gate green merely because the
			// queue-local reservation remained within its advertised limit.
		})
	}
}
