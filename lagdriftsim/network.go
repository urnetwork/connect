package lagdriftsim

// The network between a visitor and the extenders (lag.dev/DRIFTTEST.md V2,
// PLAN.md M4). A ping's round trip is
//
//	b_r + K · d(truth, extender) · (1 + stretch) + queue(t) + jitter + spike(t) + retransmit
//
// where b_r is the run's last mile, the stretch is the path's (drawn once per
// path), the queue and the spikes are the visitor's own link's and so shared by
// every path at the moment a ping is sent, and the jitter is each ping's own.
// The extender sends one ping at a time: the next goes out at the later of
// 250 ms after the last and the last pong, and a ping with no pong within 5 s
// counts as lost, with the next sent then (M4). A packet the link drops costs
// a TCP retransmission, so it arrives late rather than not at all; a ping is
// lost only when the session stalls, or when the visitor's link drops for the
// rest of the run.

import (
	"math"
	"math/rand/v2"
	"slices"
)

const (
	// the extender's ping interval, ms (M4)
	pingIntervalMs = 250
	// a ping with no pong within this is lost, ms (M4)
	pongTimeoutMs = 5000
	// the pings simulated per run, ms after the sessions open: the 12 s cap and
	// a pong in flight at it
	runSpanMs = 12500
	// the shared queue is drawn on this grid, ms
	queueStepMs = 10
	// the shared queue's correlation time, ms
	queueCorrelationMs = 800
	// the extender's own jitter, mean, ms
	extenderJitterMs = 0.2
	// an extender's occasional delay of its own: its chance per ping, and its
	// range, ms
	extenderHiccupShare = 0.003
	extenderHiccupMinMs = 5
	extenderHiccupMaxMs = 30
	// a session that never opens, and when a spare replaces it, ms (E2)
	sessionFailShare = 0.03
	spareAfterMs     = 4000
	// a run whose visitor's link drops for the rest of it
	linkDropShare = 0.01
)

// The kinds of access the study's visitors have.
const (
	accessFiber  = "fiber"
	accessCable  = "cable"
	accessWifi   = "wifi"
	accessMobile = "mobile"
)

// How a visitor reaches the internet: the last mile and how its queue behaves.
type accessLink struct {
	kind string
	// b before a run's own change, ms
	lastMileMs float64
	// each ping's own queueing: exponential with this mean, ms
	jitterMs float64
	// the shared queue: |x(t)| times this, x an Ornstein–Uhlenbeck process, ms
	queueMs float64
	// spike episodes per second, their mean length, ms, and their height's
	// range, log-uniform, ms
	spikesPerS    float64
	spikeLengthMs float64
	spikeMinMs    float64
	spikeMaxMs    float64
	// a ping's chance of a TCP retransmission, and of no pong within 5 s
	retransmitShare float64
	lostShare       float64
}

// A visitor's access: 25% fiber, 25% cable or DSL, 35% Wi-Fi and 15% mobile.
func drawAccessLink(r *rand.Rand) accessLink {
	u := r.Float64()
	switch {
	case u < 0.25:
		return accessLink{
			kind:            accessFiber,
			lastMileMs:      uniform(r, 2, 6),
			jitterMs:        0.2,
			queueMs:         0.3,
			spikesPerS:      0.02,
			spikeLengthMs:   200,
			spikeMinMs:      3,
			spikeMaxMs:      30,
			retransmitShare: 0.001,
			lostShare:       0.0002,
		}
	case u < 0.50:
		return accessLink{
			kind:            accessCable,
			lastMileMs:      uniform(r, 6, 20),
			jitterMs:        0.5,
			queueMs:         1,
			spikesPerS:      0.05,
			spikeLengthMs:   250,
			spikeMinMs:      5,
			spikeMaxMs:      50,
			retransmitShare: 0.003,
			lostShare:       0.0005,
		}
	case u < 0.85:
		return accessLink{
			kind:            accessWifi,
			lastMileMs:      uniform(r, 5, 24),
			jitterMs:        1.5,
			queueMs:         2.5,
			spikesPerS:      0.3,
			spikeLengthMs:   300,
			spikeMinMs:      5,
			spikeMaxMs:      80,
			retransmitShare: 0.01,
			lostShare:       0.001,
		}
	default:
		return accessLink{
			kind:            accessMobile,
			lastMileMs:      uniform(r, 20, 50),
			jitterMs:        3,
			queueMs:         5,
			spikesPerS:      0.3,
			spikeLengthMs:   400,
			spikeMinMs:      10,
			spikeMaxMs:      150,
			retransmitShare: 0.015,
			lostShare:       0.002,
		}
	}
}

func uniform(r *rand.Rand, lo float64, hi float64) float64 {
	return lo + (hi-lo)*r.Float64()
}

func logUniform(r *rand.Rand, lo float64, hi float64) float64 {
	return math.Exp(uniform(r, math.Log(lo), math.Log(hi)))
}

// A spike episode on the visitor's link: every ping sent in it waits longer.
type spike struct {
	startMs  float64
	endMs    float64
	heightMs float64
}

// The visitor's link during one run.
type runLink struct {
	access *accessLink
	// b_r, ms
	lastMileMs float64
	// the shared queue's delay on the queueStepMs grid, ms
	queueMs []float64
	spikes  []spike
	// the link drops for the rest of the run then, ms; +Inf when it doesn't
	dropMs float64
}

// A run's link: b changes by up to ±15% between runs (V7), and the queue,
// spikes and any drop are the run's own.
func drawRunLink(access *accessLink, r *rand.Rand) *runLink {
	link := &runLink{
		access:     access,
		lastMileMs: access.lastMileMs * uniform(r, 0.85, 1.15),
		dropMs:     math.Inf(1),
	}
	steps := runSpanMs/queueStepMs + 1
	link.queueMs = make([]float64, steps)
	a := math.Exp(-queueStepMs / queueCorrelationMs)
	b := math.Sqrt(1 - a*a)
	x := r.NormFloat64()
	for i := range link.queueMs {
		link.queueMs[i] = access.queueMs * math.Abs(x)
		x = a*x + b*r.NormFloat64()
	}
	if 0 < access.spikesPerS {
		t := r.ExpFloat64() * 1000 / access.spikesPerS
		for t < runSpanMs {
			length := r.ExpFloat64() * access.spikeLengthMs
			link.spikes = append(link.spikes, spike{
				startMs:  t,
				endMs:    t + length,
				heightMs: logUniform(r, access.spikeMinMs, access.spikeMaxMs),
			})
			t += length + r.ExpFloat64()*1000/access.spikesPerS
		}
	}
	if r.Float64() < linkDropShare {
		link.dropMs = uniform(r, 1000, 10000)
	}
	return link
}

// The shared queue's delay for a ping sent at t, ms.
func (self *runLink) queueAt(t float64) float64 {
	i := int(t / queueStepMs)
	i = max(0, min(len(self.queueMs)-1, i))
	return self.queueMs[i]
}

// The spike a ping sent at t waits in, ms, if any.
func (self *runLink) spikeAt(t float64) (float64, bool) {
	for _, s := range self.spikes {
		if s.startMs <= t && t < s.endMs {
			return s.heightMs, true
		}
		if t < s.startMs {
			break
		}
	}
	return 0, false
}

// One ping of a session.
type ping struct {
	// sent, ms after the sessions opened
	sentMs float64
	// its round trip, ms; 0 when lost
	ms float64
	// the pong reached the extender, which measured the round trip, ms
	measuredMs float64
	// the round trip's rtt event reached the browser, ms
	seenMs float64
}

// The pings of one session that opened at openMs, to an extender whose
// propagation is propagationMs, until runSpanMs.
func sessionPings(link *runLink, propagationMs float64, openMs float64, r *rand.Rand) []ping {
	pings := []ping{}
	base := link.lastMileMs + propagationMs
	access := link.access
	for sent := openMs; sent < runSpanMs; {
		spikeMs, inSpike := link.spikeAt(sent)
		lostShare := access.lostShare
		retransmitShare := access.retransmitShare
		if inSpike {
			lostShare *= 3
			retransmitShare *= 5
		}
		lost := link.dropMs <= sent || r.Float64() < lostShare
		ms := base + link.queueAt(sent) + r.ExpFloat64()*access.jitterMs + r.ExpFloat64()*extenderJitterMs + spikeMs
		if r.Float64() < extenderHiccupShare {
			ms += uniform(r, extenderHiccupMinMs, extenderHiccupMaxMs)
		}
		if r.Float64() < retransmitShare {
			// the retransmission timer, at least 200 ms
			ms += math.Max(200, 2*base) + uniform(r, 0, 100)
		}
		if pongTimeoutMs <= ms || link.dropMs <= sent+ms {
			lost = true
		}
		if lost {
			pings = append(pings, ping{sentMs: sent})
			sent += pongTimeoutMs
			continue
		}
		measured := sent + ms
		pings = append(pings, ping{
			sentMs:     sent,
			ms:         ms,
			measuredMs: measured,
			seenMs:     measured + base/2,
		})
		sent = math.Max(sent+pingIntervalMs, measured)
	}
	return pings
}

// How long a session takes to open: a WebSocket's TCP, TLS and upgrade round
// trips and the extender's hello, ms.
func sessionOpenMs(link *runLink, propagationMs float64, r *rand.Rand) float64 {
	return 60 + 3*(link.lastMileMs+propagationMs) + uniform(r, 0, 250)
}

// The route stretch's shapes. skewed is the study's: mean 0 and standard
// deviation κ, like the model's, but no path is faster than light in fiber
// along the great circle, so the stretch is a Gamma floored at −0.3 (the
// model's speed is about 2/3 of fiber's) with a long tail of detours. normal
// is the model's own assumption, N(0, κ²), unfloored.
const (
	stretchSkewed = "skewed"
	stretchNormal = "normal"
)

// A path's route stretch, drawn once per path (V3).
func drawStretch(r *rand.Rand, kappa float64, shape string) float64 {
	if !(0 < kappa) {
		return 0
	}
	switch shape {
	case stretchNormal:
		return math.Max(-0.9, kappa*r.NormFloat64())
	default:
		const floor = 0.3
		k := (floor / kappa) * (floor / kappa)
		theta := kappa * kappa / floor
		return gammaDraw(r, k)*theta - floor
	}
}

// The 10th percentile of xs, by nearest rank, for checks.
func percentile10(xs []float64) float64 {
	sorted := slices.Clone(xs)
	slices.Sort(sorted)
	rank := max(1, int(math.Ceil(0.1*float64(len(sorted))-1e-9)))
	return sorted[rank-1]
}
