// Proves actual constructor pressure, event and teardown ownership.
package connect

import (
	"context"
	"net/netip"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Uses the actual constructor and registered pressure callback. The sole hook
// observes completed teardown so fixture cleanup never polls registry or timing.
func newBlockActionOwnedMemoryFixture(t *testing.T) (*RemoteUserNatMultiClient, <-chan struct{}, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultMultiClientSettings()
	settings.EventEpoch = time.Hour
	settings.IpAssocSettings.ClusterEpoch = time.Hour
	closed := make(chan struct{})
	var once sync.Once
	settings.afterIpAssocShedForTest = func(stopped bool) {
		if stopped {
			once.Do(func() { close(closed) })
		}
	}
	settings.LocalDialContextSettings, _ = testRefusingLocalDialContextSettings()
	memoryShedders.mutex.Lock()
	firstId := memoryShedders.nextCallbackId
	memoryShedders.mutex.Unlock()
	multi := NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{}, nil, protocol.ProvideMode_Network, settings)
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := multi.CloseAndWait(join); err != nil {
			t.Errorf("memory fixture parent cleanup: %v", err)
		}
		select {
		case <-closed:
		case <-join.Done():
			t.Error("association worker did not finish unregister/shed cleanup")
		}
	})
	callbackId := -1
	method := reflect.ValueOf(multi.ipAssoc.ShedMemory).Pointer()
	memoryShedders.mutex.Lock()
	for index, id := range memoryShedders.callbackIds {
		if firstId <= id && reflect.ValueOf(memoryShedders.callbacks[index]).Pointer() == method {
			if callbackId != -1 {
				memoryShedders.mutex.Unlock()
				t.Fatal("constructor installed duplicate association shedders")
			}
			callbackId = id
		}
	}
	memoryShedders.mutex.Unlock()
	if callbackId == -1 {
		t.Fatal("constructor did not register its actual association shedder")
	}
	return multi, closed, callbackId
}

// Real pressure retires unvisited cache ownership but preserves an active event.
func TestBlockActionOwnedPressureRetiresCacheKeepsEvent(t *testing.T) {
	multi, _, _ := newBlockActionOwnedMemoryFixture(t)
	multi.SetServerNameLookup(&blockActionMemoryLookup{})
	members := []netip.Addr{netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
	for _, addr := range members {
		multi.ipAssoc.AddEgressPacket(blockActionMemoryPath(addr))
	}
	multi.ipAssoc.updateClusters()
	decision := multi.blockActionDecision(multi.blockActionState.Load(), nil, false, blockActionMemoryPath(members[0]))
	before := slices.Clone(decision.clusterIps)
	if len(before) != len(members) {
		t.Fatal("constructor fixture did not publish its actual cluster")
	}
	var actions []*BlockAction
	unsub := multi.blockActionCollector.addCallback(func(emitted []*BlockAction) { actions = emitted })
	defer unsub()
	multi.blockActionCollector.add(decision, false, false, nil, "", 128)
	ShedMemory()
	multi.blockActionCache.stateLock.Lock()
	count := len(multi.blockActionCache.decisions)
	multi.blockActionCache.stateLock.Unlock()
	if count != 0 || len(multi.ipAssoc.GetClusterAddrs(members[0])) != 0 {
		t.Fatal("BLOCK_ACTION_OWNED_PRESSURE_RETAINED_UNVISITED_CACHE")
	}
	multi.blockActionCollector.flush()
	if len(actions) != 1 || actions[0].PacketCount != 1 || actions[0].ByteCount != 128 || !slices.Equal(actions[0].Ips, before) || !slices.Equal(decision.clusterIps, before) {
		t.Fatal("pressure discarded or mutated an existing public event epoch")
	}
}

// Empty-source pressure still revokes an old builder, even without a version bump.
func TestBlockActionOwnedEmptyPressureRejectsInflightMiss(t *testing.T) {
	multi, _, _ := newBlockActionOwnedMemoryFixture(t)
	lookup := &blockActionMemoryLookup{entered: make(chan struct{}), release: make(chan struct{})}
	multi.SetServerNameLookup(lookup)
	path := blockActionMemoryPath(netip.MustParseAddr("192.0.2.1"))
	done := make(chan *blockActionDecision, 1)
	go func() { done <- multi.blockActionDecision(multi.blockActionState.Load(), nil, false, path) }()
	released, joined := false, false
	finish := func() {
		if !released {
			close(lookup.release)
			released = true
		}
		if !joined {
			<-done
			joined = true
		}
	}
	defer finish()
	<-lookup.entered
	version := multi.ipAssoc.ClusterVersion()
	ShedMemory()
	if multi.ipAssoc.ClusterVersion() != version {
		t.Fatal("empty pressure unexpectedly replaced a source publication")
	}
	finish()
	multi.blockActionCache.stateLock.Lock()
	count := len(multi.blockActionCache.decisions)
	multi.blockActionCache.stateLock.Unlock()
	if count != 0 {
		t.Fatal("BLOCK_ACTION_EMPTY_PRESSURE_WAS_REPINNED")
	}
}

// A closed actual parent cannot be refilled and unregisters its source owner.
func TestBlockActionOwnedCloseRejectsInflightMissAndUnregisters(t *testing.T) {
	multi, closed, callbackId := newBlockActionOwnedMemoryFixture(t)
	lookup := &blockActionMemoryLookup{entered: make(chan struct{}), release: make(chan struct{})}
	multi.SetServerNameLookup(lookup)
	path := blockActionMemoryPath(netip.MustParseAddr("192.0.2.1"))
	done := make(chan *blockActionDecision, 1)
	go func() { done <- multi.blockActionDecision(multi.blockActionState.Load(), nil, false, path) }()
	released, joined := false, false
	finish := func() {
		if !released {
			close(lookup.release)
			released = true
		}
		if !joined {
			<-done
			joined = true
		}
	}
	defer finish()
	<-lookup.entered
	multi.Close()
	finish()
	join, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-closed:
	case <-join.Done():
		t.Fatal("actual association teardown did not finish")
	}
	memoryShedders.mutex.Lock()
	_, registered := slices.BinarySearch(memoryShedders.callbackIds, callbackId)
	memoryShedders.mutex.Unlock()
	if registered {
		t.Fatal("association owner remained registered after actual teardown")
	}
	if err := multi.CloseAndWait(join); err != nil {
		t.Fatalf("actual parent did not join: %v", err)
	}
	multi.blockActionCache.clear()
	multi.blockActionDecision(multi.blockActionState.Load(), nil, false, path)
	multi.blockActionCache.stateLock.Lock()
	count, stopped := len(multi.blockActionCache.decisions), multi.blockActionCache.closed
	multi.blockActionCache.stateLock.Unlock()
	if count != 0 || !stopped {
		t.Fatal("BLOCK_ACTION_CLOSED_PARENT_WAS_REPINNED")
	}
}
