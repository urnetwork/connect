package connect

// rewardAccumulator is the Phase 0 divergence gate: it records per-(class,exit)
// outcomes so a field capture can answer "do per-class rankings actually
// diverge" before any learner is built. Reward = class-normalized goodput +
// stall-free fraction. It is measurement only — it changes NO placement.
type rewardKey struct {
	Class  TrafficClass
	ExitId string
}

type rewardStat struct {
	samples    int
	goodputSum float64
	stallFreeN int
}

type rewardAccumulator struct {
	m map[rewardKey]*rewardStat
}

func newRewardAccumulator() *rewardAccumulator {
	return &rewardAccumulator{m: map[rewardKey]*rewardStat{}}
}

func (r *rewardAccumulator) add(class TrafficClass, exitId string, goodputBytes float64, stallFree bool) {
	k := rewardKey{Class: class, ExitId: exitId}
	s := r.m[k]
	if s == nil {
		s = &rewardStat{}
		r.m[k] = s
	}
	s.samples++
	s.goodputSum += goodputBytes
	if stallFree {
		s.stallFreeN++
	}
}

// drainLines emits one grammar line per key and resets. Interval-triggered by
// the caller (the heartbeat tick), never per-packet.
//
// Built on relEvent (ip_remote_multi_client_observability.go) like every
// other structured line in this codebase, rather than a hand-rolled format
// string, so a future grammar change (a new field, a rendering fix) lands
// here for free. exit= is truncated to relExitId's 8-char tail purely for
// display -- the SAME truncation every other [rel] exit= field uses -- so a
// capture can `grep 'exit=a1b2c3d4'` across a demote line, a classify line,
// and a reward line and get one provider's story. The untruncated id is what
// actually keys the map and what foldInto persists; only the log line is
// shortened.
func (r *rewardAccumulator) drainLines() []string {
	if len(r.m) == 0 {
		return nil
	}
	lines := make([]string, 0, len(r.m))
	for k, s := range r.m {
		avg := s.goodputSum / float64(s.samples)
		frac := float64(s.stallFreeN) / float64(s.samples)
		lines = append(lines, relEvent(
			"reward",
			"class", k.Class,
			"exit", rewardExitDisplay(k.ExitId),
			"samples", s.samples,
			"goodput", avg,
			"stallfree", frac,
		))
	}
	r.m = map[rewardKey]*rewardStat{}
	return lines
}

// rewardExitDisplay shortens a raw exit id string to relExitId's convention
// for the log line only -- the accumulator and provider-priors keys stay the
// untruncated string, so two different providers whose ids happen to share
// an 8-character tail can never be confused with each other in the data,
// only look similar in a human-scanned line.
func rewardExitDisplay(exitId string) string {
	if len(exitId) <= relExitIdLength {
		return exitId
	}
	return exitId[len(exitId)-relExitIdLength:]
}

// rewardScore turns one interval's raw goodput/stall-free stats into the
// [0,1] figure ProviderPriors.Observe expects: the same goodput
// normalization exitScore uses (1MB/s -> 0.5, routing_score.go), blended
// evenly with the stall-free fraction. Provider priors are deliberately
// class-agnostic (routing_priors.go's own doc: only the coarse,
// per-provider-identity shape survives a restart), so this collapses
// whatever mix of classes an exit carried this interval into one number,
// unlike the [rel] event=reward line above, which stays per-class.
func rewardScore(s rewardStat) float64 {
	if s.samples <= 0 {
		return 0
	}
	goodput := s.goodputSum / float64(s.samples)
	if goodput < 0 {
		goodput = 0
	}
	goodputGood := goodput / (goodput + 1e6)
	stallFree := float64(s.stallFreeN) / float64(s.samples)

	score := 0.5*goodputGood + 0.5*stallFree
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}
	return score
}

// foldInto blends each key's interval stats into priors' per-provider EWMA,
// keyed by the untruncated ExitId. Deliberately does NOT reset the
// accumulator -- the caller (foldRewardAndPersist) folds first and calls
// drainLines separately, under the same lock, so both operations read the
// exact same interval's data before it is cleared for the next one.
func (r *rewardAccumulator) foldInto(priors *ProviderPriors, nowUnix int64) {
	if priors == nil {
		return
	}
	for k, s := range r.m {
		priors.Observe(k.ExitId, rewardScore(*s), nowUnix)
	}
}
