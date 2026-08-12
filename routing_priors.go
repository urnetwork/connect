package connect

import "sync"

// ProviderPrior is the coarse, persistable memory of one provider IDENTITY
// (never an exit instance): a smoothed score, a conviction count, and a
// last-seen stamp. This is all that survives a restart — never raw learner
// windows, which are stale-on-load at our churn and a poisoning surface.
type ProviderPrior struct {
	ScoreEwma    float64
	Convictions  int
	LastSeenUnix int64
}

type ProviderPriors struct {
	mu sync.Mutex
	m  map[string]ProviderPrior
}

func NewProviderPriors() *ProviderPriors {
	return &ProviderPriors{m: map[string]ProviderPrior{}}
}

const priorsEwmaAlpha = 0.2

func (p *ProviderPriors) Observe(providerId string, score float64, nowUnix int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.m[providerId]
	if !ok {
		pr.ScoreEwma = score
	} else {
		pr.ScoreEwma = priorsEwmaAlpha*score + (1-priorsEwmaAlpha)*pr.ScoreEwma
	}
	pr.LastSeenUnix = nowUnix
	p.m[providerId] = pr
}

func (p *ProviderPriors) Convict(providerId string, nowUnix int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.m[providerId]
	if !ok {
		// Only convict providers we have already observed; presence-keyed like Observe.
		return
	}
	pr.Convictions++
	pr.LastSeenUnix = nowUnix
	p.m[providerId] = pr
}

// Bias returns a recruitment bias in [0,1]; unknown providers are neutral 0.5.
// A conviction history subtracts; the score EWMA is the base. The Bias method is
// deliberately time-independent for testability; staleness and TTL are enforced by
// the persistence layer in a later task.
func (p *ProviderPriors) Bias(providerId string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.m[providerId]
	if !ok {
		return 0.5
	}
	b := pr.ScoreEwma - 0.15*float64(pr.Convictions)
	if b < 0 {
		b = 0
	}
	if b > 1 {
		b = 1
	}
	return b
}

func (p *ProviderPriors) Snapshot() map[string]ProviderPrior {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]ProviderPrior, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

func (p *ProviderPriors) Load(m map[string]ProviderPrior) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = make(map[string]ProviderPrior, len(m))
	for k, v := range m {
		p.m[k] = v
	}
}

// PriorsStore is the persistence seam; the sdk supplies a LocalState-backed
// implementation. A nil store means in-memory only (bare fixtures, mobile before
// wiring). Retention/TTL is enforced by the store on load, not here.
type PriorsStore interface {
	Load() map[string]ProviderPrior
	Save(map[string]ProviderPrior) error
}
