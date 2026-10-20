package extender

// Amendment 15's acceptance (lag.dev/PLAN.md M6, T1): the window's edges and
// short bursts, through the real counting, driven with synthetic arrivals as
// the simulation that set the rules has them.

import (
	"cmp"
	"context"
	"math"
	mathrand "math/rand/v2"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// The reads of a synthetic upload as amendment 15's acceptance has them: four
// streams start sending together at 0, each at its share of mbps, up to 30%
// off an equal share, in records of 16 KiB, the first of each completing one
// gap in, and each arriving up to 3 ms late. With stallChance, a record starts
// a stall of 0.2 to 0.6 s, as TCP recovering a lost packet holds back what
// came after it: the stream's records are held until it ends, then released
// back to back, 10 us apart. The sender keeps sending well past the window. In
// order of arrival, from the first at 0, which opens the window.
func testLagEdgeReads(mbps float64, seed uint64, stallChance float64) []testLagRead {
	random := mathrand.New(mathrand.NewPCG(seed, 15))
	weights := make([]float64, 4)
	weightSum := 0.0
	for k := range weights {
		weights[k] = 1 + 0.3*(2*random.Float64()-1)
		weightSum += weights[k]
	}
	reads := []testLagRead{}
	at := func(seconds float64) time.Duration {
		return time.Duration(seconds * float64(time.Second))
	}
	for k, weight := range weights {
		gapSeconds := 16384 / (mbps * 1e6 / 8 * weight / weightSum)
		heldCount := 0
		stallUntil := -1.0
		for seconds := gapSeconds; seconds < 1.5+5+3; seconds += gapSeconds {
			if 0 < stallChance && stallUntil < seconds && random.Float64() < stallChance {
				stallUntil = seconds + 0.2 + 0.4*random.Float64()
			}
			if seconds < stallUntil {
				heldCount += 1
				continue
			}
			for i := 0; i < heldCount; i += 1 {
				reads = append(reads, testLagRead{arrivedTime: at(seconds + float64(i)*1e-5), streamIndex: k, byteCount: 16384})
			}
			heldCount = 0
			reads = append(reads, testLagRead{arrivedTime: at(seconds + 0.003*random.Float64()), streamIndex: k, byteCount: 16384})
		}
	}
	slices.SortStableFunc(reads, func(a testLagRead, b testLagRead) int {
		return cmp.Compare(a.arrivedTime, b.arrivedTime)
	})
	firstTime := reads[0].arrivedTime
	for i := range reads {
		reads[i].arrivedTime -= firstTime
	}
	return reads
}

// The Mbps of each interval of a 5 s upload window, counted by the phase from
// the reads as they arrive, as the report signs them: a phase whose streams
// all opened before its first byte, the reads given in order of arrival, and
// the window run out if no read past it has ended it.
func testLagEdgeSeries(reads []testLagRead) []float64 {
	startTime := time.Now()
	api := &extenderLagApi{budget: newLagApiBudget(0)}
	phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
		5*time.Second, lagApiInterval, 4, 1<<40, time.Minute, func() {})
	for k := 0; k < 4; k += 1 {
		phase.openStream(k)
	}
	phase.window(startTime)
	for _, read := range reads {
		phase.receive(read.streamIndex, startTime.Add(read.arrivedTime), read.byteCount)
	}
	phase.window(startTime.Add(5 * time.Second))
	values := phase.windowValues()
	phase.finish(lagApiPhaseEndClosed)
	intervalCount := int((5*time.Second + lagApiInterval - 1) / lagApiInterval)
	mbps := make([]float64, intervalCount)
	for i := range mbps {
		if i < len(values) {
			mbps[i] = float64(values[i]) * 8 / float64(lagApiInterval.Microseconds())
		}
	}
	return mbps
}

// The median of values, as the simulation takes it: the mean of the middle
// two of an even count.
func testLagMedian(values []float64) float64 {
	values = slices.Sorted(slices.Values(values))
	middle := len(values) / 2
	if len(values)%2 == 0 {
		return (values[middle-1] + values[middle]) / 2
	}
	return values[middle]
}

// Amendment 15's acceptance: four upload streams start together, at rates up
// to 30% apart, in 16 KiB records, and the sender keeps sending past the end;
// in one set of runs they flow evenly, in the other each record has a 3%
// chance of a 0.2 to 0.6 s stall released back to back. Over 100 uploads a
// rate from 0.5 to 20 Mbps, counted as the reads arrive and taken as the
// report signs them, the median p50 is within 2% of the true rate, the median
// p95 within 6%, and the median first and last intervals within 10% -- 5%,
// 12% and 15% at 0.5 Mbps (M6 R1 to R3, T1). Under amendment 14's counting
// the first interval read over four times the rate at 0.5 Mbps and the last
// under a tenth of it, and stalls put the p95 38% to 54% high at every rate.
func TestLagApiUploadEdgesAccuracy(t *testing.T) {
	const uploadCount = 100
	// in a bubble, the phases' timers keep to its clock, which stands still
	// while the reads are given: the reads alone decide when a window ends
	// and is final
	synctest.Test(t, func(t *testing.T) {
		for _, stallChance := range []float64{0, 0.03} {
			t.Logf("stall chance %.2f: median over %d uploads, %% of the true rate: Mbps, p50, p95, first, last", stallChance, uploadCount)
			for _, mbps := range []float64{0.5, 1, 2, 3, 5, 10, 20} {
				p50s := []float64{}
				p95s := []float64{}
				firsts := []float64{}
				lasts := []float64{}
				for seed := uint64(0); seed < uploadCount; seed += 1 {
					series := testLagEdgeSeries(testLagEdgeReads(mbps, seed, stallChance))
					p50s = append(p50s, 100*testLagNearestRank(series, 50)/mbps)
					p95s = append(p95s, 100*testLagNearestRank(series, 95)/mbps)
					firsts = append(firsts, 100*series[0]/mbps)
					lasts = append(lasts, 100*series[len(series)-1]/mbps)
				}
				p50 := testLagMedian(p50s)
				p95 := testLagMedian(p95s)
				first := testLagMedian(firsts)
				last := testLagMedian(lasts)
				t.Logf("%5.1f %6.1f %6.1f %6.1f %6.1f", mbps, p50, p95, first, last)
				p50Mark, p95Mark, edgeMark := 2.0, 6.0, 10.0
				if mbps < 1 {
					p50Mark, p95Mark, edgeMark = 5, 12, 15
				}
				for _, mark := range []struct {
					name    string
					percent float64
					within  float64
				}{
					{"p50", p50, p50Mark},
					{"p95", p95, p95Mark},
					{"first interval", first, edgeMark},
					{"last interval", last, edgeMark},
				} {
					if mark.within < math.Abs(mark.percent-100) {
						t.Errorf("stall chance %.2f, %.1f Mbps: the median %s is %.1f%% of the rate, outside %.0f%%",
							stallChance, mbps, mark.name, mark.percent, mark.within)
					}
				}
			}
		}
	})
}
