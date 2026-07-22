package connect

import (
	"context"
	"sync"
)

// Window identity persistence (PROXYDRAIN1.md §3.5).
//
// The api generator mints an ephemeral platform client id (with a fresh
// instance id) for every window entry and removes it on teardown. A process
// restart therefore changes every window client id — and the egress
// provider's NAT flows are keyed by source client id, so every established
// inner flow orphans even though the NAT state itself survives (it evicts
// lazily: udp 60s idle, tcp 300s).
//
// A `MultiClientIdentityStore` closes that gap: the generator records each
// live (window client identity, destination) pair as it forms, and a
// restarted process restores the pairs — reusing the SAME client id,
// jwt, and instance id against the SAME destination, so the provider's
// flows resume instead of orphaning. The store is provided by the embedding
// host (e.g. the proxy service persists per hosted device in redis with a
// short ttl); with no store set, behavior is exactly as before.

// WindowClientIdentity pairs a window client identity with the destination
// it serves.
type WindowClientIdentity struct {
	ClientId   Id
	ByJwt      string
	InstanceId Id
	// Destination is the provider destination this identity dials.
	Destination MultiHopId
}

// MultiClientIdentityStore persists the live window identities across a
// process restart. Implementations must tolerate concurrent calls.
//
// Store semantics: called with the FULL live set on every change (small,
// bounded by the window size), replacing the previous snapshot. Load is
// read once, at the first window expansion after construction.
type MultiClientIdentityStore interface {
	StoreWindowClientIdentities(identities []*WindowClientIdentity)
	LoadWindowClientIdentities() []*WindowClientIdentity
}

// MultiClientGeneratorWithDestination is an optional generator extension:
// mint client args bound to a destination, so a persisted identity for that
// destination can be reused. The window expand path prefers this over
// `NewClientArgs` when implemented.
type MultiClientGeneratorWithDestination interface {
	NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error)
}

// windowIdentityState is the generator-side bookkeeping for identity
// persistence: restored identities pending reuse, and the live pairs backing
// the store snapshot. Safe for concurrent use.
//
// Store writes are ASYNC: a mutation assigns a monotonic generation and
// captures the snapshot under `mutex`, then hands it to a single writer
// goroutine that calls the (possibly slow — the proxy adapter's redis store
// retries up to ~60s) store OUTSIDE the mutex. `mutex` is therefore never
// held across a blocking call, so a store stall cannot wedge window
// expand/teardown (`Record`/`Remove`/`TakeRestored`). The writer always
// drains the NEWEST pending snapshot and skips any generation at or below
// the last written one, so a newer snapshot can never be overwritten by an
// older one (the ordering guarantee `TestWindowIdentityStoresCannotComplete-
// OutOfOrder` pins). Bounded: one goroutine, one pending snapshot slot, a
// 1-slot coalescing notify; the writer exits with ctx after a best-effort
// final drain.
type windowIdentityState struct {
	ctx   context.Context
	store MultiClientIdentityStore

	mutex    sync.Mutex
	loadOnce bool
	// restored identities pending reuse, by destination. A destination can
	// carry SEVERAL identities (the window may run more than one client to
	// the same destination, e.g. a racy initial double-expand), so each
	// take pops one. Consumed at most once: a failed reuse falls back to
	// minting, never to a second restore.
	restored map[MultiHopId][]*WindowClientIdentity
	// the live pairs, by client id, mirrored to the store on every change
	live map[Id]*WindowClientIdentity

	// the monotonic snapshot generation, advanced under `mutex` per mutation
	generation uint64
	// the newest snapshot awaiting the writer (nil when none pending),
	// with its generation. an unwritten older pending snapshot is simply
	// replaced — only the newest state needs to persist.
	pendingGeneration uint64
	pendingSnapshot   []*WindowClientIdentity

	// writeNotify wakes the writer goroutine; capacity 1 coalesces bursts
	writeNotify chan struct{}
}

func newWindowIdentityState(ctx context.Context, store MultiClientIdentityStore) *windowIdentityState {
	state := &windowIdentityState{
		ctx:         ctx,
		store:       store,
		restored:    map[MultiHopId][]*WindowClientIdentity{},
		live:        map[Id]*WindowClientIdentity{},
		writeNotify: make(chan struct{}, 1),
	}
	if store != nil {
		go HandleError(state.runStoreWriter)
	}
	return state
}

// hasStore reports whether an identity store is configured (the proxy case:
// identities must survive teardown for the replacement container).
func (self *windowIdentityState) hasStore() bool {
	return self.store != nil
}

// loadWithLock reads the persisted identities once.
func (self *windowIdentityState) loadWithLock() {
	if self.loadOnce {
		return
	}
	self.loadOnce = true
	if self.store == nil {
		return
	}
	for _, identity := range self.store.LoadWindowClientIdentities() {
		if identity == nil {
			continue
		}
		self.restored[identity.Destination] = append(self.restored[identity.Destination], identity)
	}
}

// RestoredDestinations returns the destinations with an identity pending
// reuse, so the window expand can dial them first.
func (self *windowIdentityState) RestoredDestinations() []MultiHopId {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.loadWithLock()

	destinations := make([]MultiHopId, 0, len(self.restored))
	for destination := range self.restored {
		destinations = append(destinations, destination)
	}
	return destinations
}

// TakeRestored consumes one restored identity for a destination, if any.
func (self *windowIdentityState) TakeRestored(destination MultiHopId) *WindowClientIdentity {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.loadWithLock()

	identities, ok := self.restored[destination]
	if !ok || len(identities) == 0 {
		return nil
	}
	identity := identities[0]
	if len(identities) == 1 {
		delete(self.restored, destination)
	} else {
		self.restored[destination] = identities[1:]
	}
	return identity
}

// Record adds a live (identity, destination) pair and mirrors the snapshot
// to the store.
func (self *windowIdentityState) Record(identity *WindowClientIdentity) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.live[identity.ClientId] = identity
	self.storeSnapshotWithLock()
}

// Remove drops the live pair for a client id and mirrors the snapshot to
// the store.
func (self *windowIdentityState) Remove(clientId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if _, ok := self.live[clientId]; !ok {
		return
	}
	delete(self.live, clientId)
	self.storeSnapshotWithLock()
}

func (self *windowIdentityState) snapshotWithLock() []*WindowClientIdentity {
	identities := make([]*WindowClientIdentity, 0, len(self.live))
	for _, identity := range self.live {
		identities = append(identities, identity)
	}
	return identities
}

// storeSnapshotWithLock assigns the next generation, captures the snapshot,
// and wakes the async writer. It never blocks: the store call itself happens
// on the writer goroutine, outside mutex. The caller holds mutex.
func (self *windowIdentityState) storeSnapshotWithLock() {
	if self.store == nil {
		return
	}
	self.generation += 1
	self.pendingGeneration = self.generation
	self.pendingSnapshot = self.snapshotWithLock()
	select {
	case self.writeNotify <- struct{}{}:
	default:
		// a wake is already queued; the writer drains the newest pending
	}
}

// runStoreWriter is the single async store writer: it drains the newest
// pending snapshot and stores it outside `mutex`. It exits with ctx, after a
// best-effort final drain so a snapshot scheduled just before shutdown still
// reaches the store.
func (self *windowIdentityState) runStoreWriter() {
	lastWrittenGeneration := uint64(0)
	for {
		select {
		case <-self.ctx.Done():
			self.drainPendingStores(&lastWrittenGeneration)
			return
		case <-self.writeNotify:
			self.drainPendingStores(&lastWrittenGeneration)
		}
	}
}

// drainPendingStores repeatedly takes the newest pending (generation,
// snapshot) under `mutex` and stores it outside the lock, until nothing is
// pending. A snapshot whose generation is <= the last successfully written
// generation is superseded and dropped, preserving the ordering guarantee: a
// newer Record/Remove can never be overwritten by an older one.
func (self *windowIdentityState) drainPendingStores(lastWrittenGeneration *uint64) {
	for {
		var generation uint64
		var snapshot []*WindowClientIdentity
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			if self.pendingSnapshot == nil {
				return
			}
			generation = self.pendingGeneration
			snapshot = self.pendingSnapshot
			self.pendingSnapshot = nil
		}()
		if snapshot == nil {
			return
		}
		if generation <= *lastWrittenGeneration {
			// superseded: a newer snapshot has already been written
			continue
		}
		// the store call runs outside the mutex — it may block/retry for a
		// long time, and mutations meanwhile coalesce into pendingSnapshot
		self.store.StoreWindowClientIdentities(snapshot)
		*lastWrittenGeneration = generation
	}
}
