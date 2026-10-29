// Exercises retained ownership at real cache, public caller and constructor boundaries.
package connect

import (
	"context"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// A mixed stale request must not advance either high-water or clear newer state.
func TestBlockActionMixedStaleLookupKeepsNewerGeneration(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.1")
	for _, versions := range [][2]uint64{{1, 3}, {3, 1}} {
		cache := newBlockActionCache(time.Hour, 16)
		now := time.Now()
		newer := &blockActionDecision{overridesVersion: 2, clusterVersion: 2, expireTime: now.Add(time.Hour)}
		cache.put(addr, newer)
		if cache.get(addr, versions[0], versions[1], false, now) != nil {
			t.Fatal("mixed stale lookup returned a decision from another generation")
		}
		if cache.get(addr, 2, 2, false, now) != newer {
			t.Fatalf("BLOCK_ACTION_MIXED_STALE_LOOKUP_RETIRED_NEWER: request=%v", versions)
		}
	}
}

// Deleting one destination fences its blocked builder while keeping healthy hits.
func TestBlockActionDeletedCacheRejectsInflightMiss(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	state := &blockActionState{version: 1}
	healthyPath := blockActionMemoryPath(members[1])
	healthy := multi.blockActionDecision(state, nil, false, healthyPath)
	lookup := &blockActionMemoryLookup{entered: make(chan struct{}), release: make(chan struct{})}
	multi.config.Store(&multiClientConfig{serverNameLookup: lookup})
	done := make(chan *blockActionDecision, 1)
	go func() { done <- multi.blockActionDecision(state, nil, false, blockActionMemoryPath(members[0])) }()
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
	multi.blockActionCache.delete(members[0])
	finish()
	multi.blockActionCache.stateLock.Lock()
	stale := multi.blockActionCache.decisions[members[0]]
	multi.blockActionCache.stateLock.Unlock()
	if stale != nil {
		t.Fatal("BLOCK_ACTION_DELETED_CACHE_WAS_REPINNED")
	}
	if multi.blockActionDecision(state, nil, false, healthyPath) != healthy {
		t.Fatal("one destination deletion invalidated an unrelated healthy hit")
	}
}

// Source revalidation rejects an old builder without requiring a newer cache hit.
func TestBlockActionUnvisitedPublicationRejectsInflightMiss(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	lookup := &blockActionMemoryLookup{entered: make(chan struct{}), release: make(chan struct{})}
	multi.config.Store(&multiClientConfig{serverNameLookup: lookup})
	done := make(chan *blockActionDecision, 1)
	go func() {
		done <- multi.blockActionDecision(&blockActionState{version: 1}, nil, false, blockActionMemoryPath(members[0]))
	}()
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
	addBlockActionMemoryMember(t, multi, netip.MustParseAddr("198.51.100.1"))
	finish()
	multi.blockActionCache.stateLock.Lock()
	count := len(multi.blockActionCache.decisions)
	multi.blockActionCache.stateLock.Unlock()
	if count != 0 {
		t.Fatal("BLOCK_ACTION_UNVISITED_PUBLICATION_WAS_REPINNED")
	}
}

// A caller's first Enabled read can pause before the real decision entry point.
type blockActionConfigBlocker struct {
	enabled     atomic.Bool
	blockedIp   netip.Addr
	blockedHost string
	entered     chan struct{}
	release     chan struct{}
	blocked     atomic.Bool
}

func (self *blockActionConfigBlocker) Enabled() bool {
	if self.entered != nil && self.blocked.CompareAndSwap(false, true) {
		close(self.entered)
		<-self.release
	}
	return self.enabled.Load()
}
func (self *blockActionConfigBlocker) SetEnabled(enabled bool)      { self.enabled.Store(enabled) }
func (self *blockActionConfigBlocker) BlockIp(addr netip.Addr) bool { return addr == self.blockedIp }
func (self *blockActionConfigBlocker) BlockHost(host string) bool {
	return host == self.blockedHost && host != ""
}

// An old enabled blocker sampled before replacement cannot overwrite the new one.
func TestBlockActionOldBlockerConfigCannotRepinAfterReplacement(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	multi.blockActionState.Store(&blockActionState{version: 1})
	multi.blockActionIgnoreState.Store(&blockActionIgnoreState{})
	old := &blockActionConfigBlocker{blockedIp: members[0], entered: make(chan struct{}), release: make(chan struct{})}
	old.SetEnabled(true)
	multi.config.Store(&multiClientConfig{blocker: old})
	done := make(chan *blockActionDecision, 1)
	go func() {
		decision, _, _ := multi.smtpBlockActionParts(blockActionMemoryPath(members[0]))
		done <- decision
	}()
	released, joined := false, false
	var retired *blockActionDecision
	finish := func() {
		if !released {
			close(old.release)
			released = true
		}
		if !joined {
			retired = <-done
			joined = true
		}
	}
	defer finish()
	<-old.entered
	currentBlocker := &blockActionConfigBlocker{}
	currentBlocker.SetEnabled(true)
	multi.SetBlocker(currentBlocker)
	finish()
	multi.blockActionCache.stateLock.Lock()
	retained := multi.blockActionCache.decisions[members[0]]
	multi.blockActionCache.stateLock.Unlock()
	if retired == nil || !retired.blockerBlock {
		t.Fatal("blocker replacement barrier did not finish the sampled old destination decision")
	}
	if retained != nil {
		t.Fatal("BLOCK_ACTION_OLD_CONFIG_REPINNED_AFTER_BLOCKER_REPLACEMENT")
	}
	current, _, _ := multi.smtpBlockActionParts(blockActionMemoryPath(members[0]))
	if current == nil || current.blockerBlock {
		t.Fatal("current enabled blocker did not recover its own destination decision")
	}
}

// Lookup replacement can finish before a sampled old caller enters the cache.
func TestBlockActionOldLookupConfigCannotAcquireNewCacheEpoch(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	multi.blockActionState.Store(&blockActionState{version: 1})
	multi.blockActionIgnoreState.Store(&blockActionIgnoreState{})
	multi.blockActionIgnoreCache = newBlockActionIgnoreCache(time.Hour, 16)
	blocker := &blockActionConfigBlocker{entered: make(chan struct{}), release: make(chan struct{})}
	blocker.SetEnabled(true)
	multi.config.Store(&multiClientConfig{blocker: blocker, serverNameLookup: &testingServerNameLookup{serverNames: map[string][]string{members[0].String(): {"old.example"}}}})
	done := make(chan *blockActionDecision, 1)
	go func() {
		decision, _, _ := multi.smtpBlockActionParts(blockActionMemoryPath(members[0]))
		done <- decision
	}()
	released, joined := false, false
	var retired *blockActionDecision
	finish := func() {
		if !released {
			close(blocker.release)
			released = true
		}
		if !joined {
			retired = <-done
			joined = true
		}
	}
	defer finish()
	<-blocker.entered
	multi.SetServerNameLookup(&testingServerNameLookup{serverNames: map[string][]string{members[0].String(): {"current.example"}}})
	finish()
	multi.blockActionCache.stateLock.Lock()
	count := len(multi.blockActionCache.decisions)
	multi.blockActionCache.stateLock.Unlock()
	if retired == nil || !slices.Contains(retired.clusterHosts, "current.example") {
		t.Fatal("sampled old caller did not finish after the actual lookup replacement")
	}
	if count != 0 {
		t.Fatal("BLOCK_ACTION_OLD_LOOKUP_CONFIG_ACQUIRED_NEW_CACHE_EPOCH")
	}
	current, _, _ := multi.smtpBlockActionParts(blockActionMemoryPath(members[0]))
	if current == nil || !slices.Contains(current.clusterHosts, "current.example") {
		t.Fatal("current config could not make progress after rejecting the old caller")
	}
}

// A sampled old caller cannot use a current-config hit as its own cache lease.
func TestBlockActionOldConfigCannotReadCurrentCachedDecision(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	multi.blockActionState.Store(&blockActionState{version: 1})
	multi.blockActionIgnoreState.Store(&blockActionIgnoreState{})
	multi.blockActionIgnoreCache = newBlockActionIgnoreCache(time.Hour, 16)
	blocker := &blockActionConfigBlocker{entered: make(chan struct{}), release: make(chan struct{})}
	blocker.SetEnabled(true)
	multi.config.Store(&multiClientConfig{blocker: blocker})
	done := make(chan *blockActionDecision, 1)
	go func() {
		decision, _, _ := multi.smtpBlockActionParts(blockActionMemoryPath(members[0]))
		done <- decision
	}()
	released, joined := false, false
	var retired *blockActionDecision
	finish := func() {
		if !released {
			close(blocker.release)
			released = true
		}
		if !joined {
			retired = <-done
			joined = true
		}
	}
	defer finish()
	<-blocker.entered
	multi.SetServerNameLookup(&testingServerNameLookup{serverNames: map[string][]string{members[0].String(): {"current.example"}}})
	current, _, _ := multi.smtpBlockActionParts(blockActionMemoryPath(members[0]))
	finish()
	if current == nil || retired == nil {
		t.Fatal("current and sampled-old caller did not finish their actual decisions")
	}
	multi.blockActionCache.stateLock.Lock()
	retained := multi.blockActionCache.decisions[members[0]]
	multi.blockActionCache.stateLock.Unlock()
	if retired == current || retained != current {
		t.Fatal("BLOCK_ACTION_OLD_CONFIG_USED_CURRENT_CACHE_OWNERSHIP")
	}
}

// Cluster-wide payload sharing must preserve own-IP and own-host blocker semantics.
func TestBlockActionSharedClusterKeepsDestinationBlockerScope(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	blocker := &blockActionConfigBlocker{blockedIp: members[0], blockedHost: "blocked.example"}
	blocker.SetEnabled(true)
	multi.config.Store(&multiClientConfig{blocker: blocker, serverNameLookup: &testingServerNameLookup{serverNames: map[string][]string{members[1].String(): {"blocked.example"}, members[2].String(): {"healthy.example"}}}})
	state := &blockActionState{version: 1}
	first := multi.blockActionDecision(state, blocker, true, blockActionMemoryPath(members[0]))
	second := multi.blockActionDecision(state, blocker, true, blockActionMemoryPath(members[1]))
	healthy := multi.blockActionDecision(state, blocker, true, blockActionMemoryPath(members[2]))
	if !first.blockerBlock || !second.blockerBlock || healthy.blockerBlock || first == second || first == healthy {
		t.Fatal("shared cluster payload extended a destination blocker to siblings")
	}
}

// The new optional cache close must reach a partial parent's original cancel
// boundary. A sentinel stops before unrelated uninitialized legacy dependencies.
func TestMultiClientCloseWithoutDecisionCacheReachesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := &struct{}{}
	parent := &RemoteUserNatMultiClient{cancel: func() { cancel(); panic(stop) }}
	func() {
		defer func() {
			if recovered := recover(); recovered != stop {
				t.Fatalf("partial parent failed before its original cancel boundary: %v", recovered)
			}
		}()
		parent.Close()
		t.Fatal("partial-parent sentinel was not reached")
	}()
	if ctx.Err() == nil {
		t.Fatal("partial parent cancellation did not execute")
	}
}
