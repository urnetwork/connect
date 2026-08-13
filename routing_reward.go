package connect

import "fmt"

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
func (r *rewardAccumulator) drainLines() []string {
	if len(r.m) == 0 {
		return nil
	}
	lines := make([]string, 0, len(r.m))
	for k, s := range r.m {
		avg := s.goodputSum / float64(s.samples)
		frac := float64(s.stallFreeN) / float64(s.samples)
		lines = append(lines, fmt.Sprintf(
			"%sevent=reward class=%s exit=%s samples=%d goodput=%.0f stallfree=%.2g",
			relPrefix, k.Class, k.ExitId, s.samples, avg, frac))
	}
	r.m = map[rewardKey]*rewardStat{}
	return lines
}
