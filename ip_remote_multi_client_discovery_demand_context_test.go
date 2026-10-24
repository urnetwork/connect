package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The context-free contract cannot promise a join. Demand must not issue such
// a call, and must not replace the ordinary bounded abandonment with a gate.
type discoveryDemandTestLegacyGenerator struct {
	testingEmptyMultiClientGenerator
	reads   atomic.Int64
	workers sync.WaitGroup
	release chan struct{}
}

func (self *discoveryDemandTestLegacyGenerator) NextDestinations(int, []MultiHopId, string) (map[MultiHopId]DestinationStats, error) {
	self.workers.Add(1)
	defer self.workers.Done()
	self.reads.Add(1)
	<-self.release
	return map[MultiHopId]DestinationStats{}, nil
}

func TestDiscoveryDemandLegacyHungCallKeepsOrdinaryLiveness(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		legacy := &discoveryDemandTestLegacyGenerator{release: make(chan struct{})}
		f.window.generator = legacy
		t.Cleanup(func() {
			f.cancel()
			close(legacy.release)
			synctest.Wait()
			legacy.workers.Wait()
		})
		f.start()
		if f.window.cooperativeDiscoveryDemand() {
			t.Fatal("legacy context-free generator was misclassified as cooperative")
		}
		for range 32 {
			f.request()
		}
		time.Sleep(16 * time.Second)
		synctest.Wait()
		if legacy.reads.Load() != 1 || f.window.discoveryDemand.pending {
			t.Fatal("unsupported demand issued an uninterruptible extra read")
		}
		f.window.notifyNetworkChanged()
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if legacy.reads.Load() != 2 {
			t.Fatal("a hung legacy callback wedged the next ordinary bounded attempt")
		}
		f.window.notifyNetworkChanged()
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if legacy.reads.Load() != 3 {
			t.Fatal("legacy demand changed ordinary timeout/network recovery across later deadlines")
		}
		t.Log("legacy characterization:3 ordinary calls in45s with all backend callbacks held;0 demanded calls;existing abandoned callbacks released/joined by fixture cleanup")
	})
}

// Exercise the actual API generator's new family-context forwarding through
// HTTP. The handler sends no response, so only caller cancellation can finish.
func TestDiscoveryDemandApiFamilyContextCancelsHttp(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered, handlerJoined := make(chan struct{}), make(chan struct{})
	var once, joinedOnce sync.Once
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-request.Context().Done()
		joinedOnce.Do(func() { close(handlerJoined) })
	}))
	defer server.Close()
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()
	generator := NewApiMultiClientGenerator(ctx, []*ProviderSpec{{BestAvailable: true}}, strategy, nil,
		server.URL, "synthetic-context-token", server.URL, "test", "test", "test", nil, DefaultClientSettings, DefaultApiMultiClientGeneratorSettings())
	defer func() {
		if err := generator.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	if _, ok := any(generator).(multiClientGeneratorFamilyDiscoveryContext); !ok {
		t.Fatal("actual API generator lost the family cancellation capability")
	}
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	window := outcomeTestWindow(ctx, newRecordingLogger())
	result := make(chan error, 1)
	go func() {
		_, err := callDiscoveryDemand(window, &discoveryDemandRead{ctx: callCtx}, 4*time.Second, func(ownedCtx context.Context) (map[MultiHopId]DestinationStats, error) {
			return generator.NextDestinationsWithIpFamilyContext(ownedCtx, 1, nil, "quality", IpFamilyFilterV6Capable)
		})
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual API family request never entered HTTP")
	}
	cancelCall()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled family API request returned success")
		}
	case <-ctx.Done():
		t.Fatal("API family cancellation did not join its actual request")
	}
	select {
	case <-handlerJoined:
	case <-ctx.Done():
		t.Fatal("HTTP family handler outlived cancellation")
	}
	if calls.Load() != 1 || window.errorStreak.Load() != 0 || window.rateLimitStreak.Load() != 0 {
		t.Fatal("actual canceled API request duplicated work or fabricated platform failure")
	}
}
