package connect

import "testing"

func TestProviderPriorsEwmaAndBias(t *testing.T) {
	p := NewProviderPriors()
	if p.Bias("unknown") != 0.5 {
		t.Fatal("unknown provider must be neutral 0.5")
	}
	for i := 0; i < 20; i++ {
		p.Observe("good", 0.9, 1000)
	}
	for i := 0; i < 20; i++ {
		p.Observe("bad", 0.1, 1000)
	}
	if !(p.Bias("good") > p.Bias("bad")) {
		t.Fatalf("good provider must bias higher: good=%f bad=%f", p.Bias("good"), p.Bias("bad"))
	}
}

func TestProviderPriorsConvictionLowersBias(t *testing.T) {
	p := NewProviderPriors()
	for i := 0; i < 20; i++ {
		p.Observe("x", 0.9, 1000)
	}
	before := p.Bias("x")
	p.Convict("x", 1000)
	p.Convict("x", 1000)
	if !(p.Bias("x") < before) {
		t.Fatal("convictions must lower recruitment bias")
	}
}

func TestProviderPriorsRoundTrip(t *testing.T) {
	p := NewProviderPriors()
	p.Observe("a", 0.7, 1234)
	snap := p.Snapshot()
	q := NewProviderPriors()
	q.Load(snap)
	if q.Snapshot()["a"].ScoreEwma != snap["a"].ScoreEwma {
		t.Fatal("round-trip must preserve EWMA")
	}
}

func TestProviderPriorsSnapshotIsCopy(t *testing.T) {
	p := NewProviderPriors()
	p.Observe("a", 0.7, 1234)
	snap := p.Snapshot()
	// Mutate the returned snapshot
	snap["a"] = ProviderPrior{ScoreEwma: 0.1, Convictions: 99, LastSeenUnix: 9999}
	snap["b"] = ProviderPrior{ScoreEwma: 0.5, Convictions: 1, LastSeenUnix: 1000}
	// Internal state should be unchanged
	if p.Bias("a") != 0.7 {
		t.Fatalf("Snapshot() did not return a copy; internal state was mutated")
	}
	if p.Bias("b") != 0.5 {
		t.Fatalf("Snapshot() did not return a copy; unrelated key was added to internal state")
	}
}

func TestProviderPriorsLoadCopiesIn(t *testing.T) {
	data := map[string]ProviderPrior{
		"x": {ScoreEwma: 0.8, Convictions: 1, LastSeenUnix: 5000},
	}
	p := NewProviderPriors()
	p.Load(data)
	// Mutate the original map
	data["x"] = ProviderPrior{ScoreEwma: 0.2, Convictions: 99, LastSeenUnix: 9999}
	data["y"] = ProviderPrior{ScoreEwma: 0.5, Convictions: 0, LastSeenUnix: 1000}
	// Check the snapshot to verify internal state is unchanged
	snap := p.Snapshot()
	if snap["x"].ScoreEwma != 0.8 || snap["x"].Convictions != 1 {
		t.Fatalf("Load() did not copy in; internal state was changed by mutating caller's map")
	}
	if _, ok := snap["y"]; ok {
		t.Fatalf("Load() did not copy in; unrelated key from mutated map appeared in internal state")
	}
}
