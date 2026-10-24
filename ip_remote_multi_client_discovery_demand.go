package connect

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

// Read opportunities belong to an existing acquisition and its current request
// slot. No demand starts a pass, clears a retry, or increases its mint budget.
// All fields are guarded by the window stateLock.
type discoveryAcquisitionDemand struct {
	owner          *multiClientEvaluationOwner
	ctx            context.Context
	cancel         context.CancelFunc
	deadline       time.Time
	targetSize     int
	hardMax        int
	slot           uint64
	slotCtx        context.Context
	slotCancel     context.CancelFunc
	slotOpen       bool
	pending        bool
	pendingFlow    *multiClientChannelUpdate
	pendingStamp   uint64
	pendingMembers map[Id]*multiClientChannel
	ipVersion      int
	reads          int
	nextRead       time.Time
	mintClaimed    bool
}

// A lease cannot move to a later slot or pass. Its context cancels when the
// ordinary receiver takes an argument, even if a listing or mint is in flight.
type discoveryDemandRead struct {
	owner           *multiClientEvaluationOwner
	slot            uint64
	ctx             context.Context
	deadline        time.Time
	ipVersion       int
	update          *multiClientChannelUpdate
	stamp           uint64
	members         map[Id]*multiClientChannel
	generatorNotify <-chan struct{}
}

func (self *multiClientWindow) discoveryDemandChangedWithLock() <-chan struct{} {
	if self.discoveryDemandChanged == nil {
		self.discoveryDemandChanged = NewMonitor()
	}
	return self.discoveryDemandChanged.NotifyChannel()
}

func (self *multiClientWindow) notifyDiscoveryDemandWithLock() {
	self.discoveryDemandChangedWithLock()
	self.discoveryDemandChanged.NotifyAll()
}

func (self *multiClientWindow) beginDiscoveryAcquisition(owner *multiClientEvaluationOwner, ctx context.Context, deadline time.Time, targetSize, hardMax int) {
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	self.stateLock.Lock()
	previous := self.discoveryDemand.cancel
	self.discoveryDemand = discoveryAcquisitionDemand{owner: owner, ctx: ctx, cancel: cancel, deadline: deadline, targetSize: targetSize, hardMax: hardMax}
	self.notifyDiscoveryDemandWithLock()
	self.stateLock.Unlock()
	if previous != nil {
		previous()
	}
}

func (self *multiClientWindow) endDiscoveryAcquisition(owner *multiClientEvaluationOwner) {
	var cancel context.CancelFunc
	self.stateLock.Lock()
	if self.discoveryDemand.owner == owner {
		cancel = self.discoveryDemand.cancel
		self.discoveryDemand = discoveryAcquisitionDemand{}
		self.notifyDiscoveryDemandWithLock()
	}
	self.stateLock.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (self *multiClientWindow) beginDiscoveryDemandSlot(owner *multiClientEvaluationOwner) {
	var previous context.CancelFunc
	self.stateLock.Lock()
	demand := &self.discoveryDemand
	if demand.owner == owner {
		previous = demand.slotCancel
		demand.slot++
		demand.slotCtx, demand.slotCancel = context.WithCancel(demand.ctx)
		demand.slotOpen = true
		self.notifyDiscoveryDemandWithLock()
	}
	self.stateLock.Unlock()
	if previous != nil {
		previous()
	}
}

// The normal receiver owns the handoff. A demand result for an older request
// is retired before evaluation and does not consume a new acquisition slot.
func (self *multiClientWindow) acceptDiscoveryDemandArgs(owner *multiClientEvaluationOwner, args *multiClientChannelArgs) bool {
	var cancel context.CancelFunc
	self.stateLock.Lock()
	demand := &self.discoveryDemand
	valid := args.discoveryDemandOwner == nil || (demand.owner == owner && args.discoveryDemandOwner == owner &&
		demand.slotOpen && args.discoveryDemandSlot == demand.slot && demand.slotCtx.Err() == nil && time.Now().Before(demand.deadline))
	if valid && demand.owner == owner {
		demand.slotOpen = false
		demand.pending, demand.pendingFlow, demand.pendingMembers = false, nil, nil
		cancel = demand.slotCancel
		self.notifyDiscoveryDemandWithLock()
	}
	self.stateLock.Unlock()
	if cancel != nil {
		cancel()
	}
	return valid
}

// These capabilities promise to return when ctx is done. The API generator
// already has the plain discovery and mint methods; its family method delegates
// to the same context-aware body. Unknown legacy generators get no extra work.
type multiClientGeneratorDiscoveryContext interface {
	NextDestinationsContext(context.Context, int, []MultiHopId, string) (map[MultiHopId]DestinationStats, error)
}
type multiClientGeneratorFamilyDiscoveryContext interface {
	NextDestinationsWithIpFamilyContext(context.Context, int, []MultiHopId, string, IpFamilyFilter) (map[MultiHopId]DestinationStats, error)
}

func (self *multiClientWindow) cooperativeDiscoveryDemand() bool {
	if _, family := self.generator.(MultiClientGeneratorWithIpFamily); family {
		if _, ok := self.generator.(multiClientGeneratorFamilyDiscoveryContext); !ok {
			return false
		}
	} else if _, ok := self.generator.(multiClientGeneratorDiscoveryContext); !ok {
		return false
	}
	switch self.generator.(type) {
	case MultiClientGeneratorWithDestinationContext:
		return true
	case MultiClientGeneratorWithDestination:
		return false
	case multiClientGeneratorArgsContext:
		return true
	default:
		return false
	}
}

// Only a local owner/deadline cancellation uses this result. An actual backend
// refusal, including one racing cancellation, retains normal retry authority.
type discoveryDemandLocalCancel struct{ cause error }

func (self *discoveryDemandLocalCancel) Error() string { return self.cause.Error() }
func (self *discoveryDemandLocalCancel) Unwrap() error { return self.cause }

// The cooperative call joins synchronously, like fixedClientArgs. No abandoned
// call, completion gate or new worker can wedge later ordinary expansion.
func callDiscoveryDemand[T any](window *multiClientWindow, read *discoveryDemandRead, timeout time.Duration, call func(context.Context) (T, error)) (value T, err error) {
	ctx, cancel := context.WithTimeout(read.ctx, timeout)
	defer cancel()
	if cause := ctx.Err(); cause != nil {
		return value, &discoveryDemandLocalCancel{cause: cause}
	}
	networkChanged := window.networkChangeNotify()
	value, err = HandleError2(func() (T, error) { return call(ctx) }, func(err error) (T, error) { var zero T; return zero, err })
	if cause := ctx.Err(); cause != nil && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return value, &discoveryDemandLocalCancel{cause: cause}
	}
	if err != nil && window.ctx.Err() == nil {
		class := window.recordEvaluationFailure(windowFailurePlatform, err)
		window.waitWindowRetry(window.ctx, class, networkChanged)
	}
	return value, err
}

func (self *multiClientWindow) discoveryDemandLiveWithLock(now time.Time) bool {
	demand := &self.discoveryDemand
	return self.ctx.Err() == nil && demand.owner != nil && demand.ctx.Err() == nil &&
		demand.slotOpen && demand.slotCtx.Err() == nil && now.Before(demand.deadline) &&
		!now.Before(self.discoveryRetry.notBefore) &&
		(!self.settings.StrictWindowSizeHardMax || demand.hardMax <= 0 || len(self.clients) < demand.hardMax)
}

func (self *multiClientWindow) discoveryDemandReadLive(read *discoveryDemandRead) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.discoveryDemandLiveWithLock(time.Now()) && self.discoveryDemand.owner == read.owner &&
		self.discoveryDemand.slot == read.slot && read.ctx.Err() == nil && discoveryDemandAttemptLive(read.update, read.stamp) &&
		self.discoveryDemandMembersLiveWithLock(read.members)
}

func (self *multiClientWindow) discoveryDemandMembersLiveWithLock(members map[Id]*multiClientChannel) bool {
	if len(members) != len(self.clients) {
		return false
	}
	for id, client := range members {
		if self.clients[id] != client {
			return false
		}
	}
	return true
}

func discoveryDemandAttemptLive(update *multiClientChannelUpdate, stamp uint64) bool {
	return update != nil && update.discoveryAttemptStamp.Load() == stamp && update.ctx.Err() == nil &&
		!update.receivedInbound.Load() && !update.sourceReset.Load() && !update.sequenceCloseReady.Load()
}

// Each actual read, rather than each notification, spends one window credit.
// Normal producer notifications remain independent and keep their old behavior.
// Only the enumerator calls this, after an empty listing; pacing uses one
// local timer, never a flow timer or a background worker.
func (self *multiClientWindow) waitDiscoveryDemand(generatorNotify <-chan struct{}, allowDemand bool) *discoveryDemandRead {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-generatorNotify:
			return nil
		default:
		}
		self.stateLock.Lock()
		changed := self.discoveryDemandChangedWithLock()
		demand := &self.discoveryDemand
		now := time.Now()
		var readyAt time.Time
		var ownerDone <-chan struct{}
		if allowDemand && demand.owner != nil {
			ownerDone = demand.ctx.Done()
		}
		limit := min(self.settings.DiscoveryDemandListingsPerPass, 3)
		if demand.pending && (!discoveryDemandAttemptLive(demand.pendingFlow, demand.pendingStamp) || !self.discoveryDemandMembersLiveWithLock(demand.pendingMembers)) {
			demand.pending, demand.pendingFlow, demand.pendingMembers = false, nil, nil
		}
		if allowDemand && demand.pending && !demand.mintClaimed &&
			0 < limit && demand.reads < limit && self.discoveryDemandLiveWithLock(now) {
			if now.Before(demand.nextRead) {
				readyAt = demand.nextRead
			} else {
				update, stamp, members := demand.pendingFlow, demand.pendingStamp, demand.pendingMembers
				demand.pending, demand.pendingFlow, demand.pendingMembers = false, nil, nil
				demand.reads++
				demand.nextRead = now.Add(max(self.settings.DiscoveryDemandListingInterval, time.Second))
				read := &discoveryDemandRead{owner: demand.owner, slot: demand.slot, ctx: demand.slotCtx,
					deadline: demand.deadline, ipVersion: demand.ipVersion, update: update, stamp: stamp, members: members, generatorNotify: generatorNotify}
				self.stateLock.Unlock()
				return read
			}
		}
		self.stateLock.Unlock()
		var paced <-chan time.Time
		if !readyAt.IsZero() {
			paced = resetOrCreateTimer(&timer, time.Until(readyAt))
		}
		select {
		case <-self.ctx.Done():
			return nil
		case <-generatorNotify:
			return nil
		case <-ownerDone:
			// The lifecycle clears pending state; avoid repeatedly selecting
			// an already canceled context before that owner cleanup runs.
			allowDemand = false
		case <-changed:
		case <-paced:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// One demand credit means one API read, including for family-aware generators.
// It asks for the waiting flow's family and yields at most one candidate. The
// ordinary pass still owns every setup/evaluation/admission decision.
func (self *multiClientWindow) listDiscoveryDemand(read *discoveryDemandRead, excluded []MultiHopId) ([]enumeratedDestination, error) {
	if !self.discoveryDemandReadLive(read) {
		return nil, context.Canceled
	}
	timeout := time.Until(read.deadline)
	if self.settings.WindowGeneratorTimeout > 0 {
		timeout = min(timeout, self.settings.WindowGeneratorTimeout)
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	destinations, err := callDiscoveryDemand(self, read, timeout, func(ctx context.Context) (map[MultiHopId]DestinationStats, error) {
		if err := read.ctx.Err(); err != nil {
			return nil, &discoveryDemandLocalCancel{cause: err}
		}
		if generator, ok := self.generator.(multiClientGeneratorFamilyDiscoveryContext); ok {
			family := IpFamilyFilterV4Capable
			if read.ipVersion == 6 {
				family = IpFamilyFilterV6Capable
			}
			return generator.NextDestinationsWithIpFamilyContext(ctx, 1, excluded, self.windowType.RankMode(), family)
		}
		return self.generator.(multiClientGeneratorDiscoveryContext).NextDestinationsContext(ctx, 1, excluded, self.windowType.RankMode())
	})
	if err != nil {
		return nil, err
	}
	for destination, stats := range destinations {
		return []enumeratedDestination{{destination: destination, stats: stats}}, nil
	}
	return nil, nil
}

func (self *multiClientWindow) mintDiscoveryDemand(read *discoveryDemandRead, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	if !self.discoveryDemandReadLive(read) {
		return nil, context.Canceled
	}
	timeout := time.Until(read.deadline)
	if self.settings.WindowGeneratorTimeout > 0 {
		timeout = min(timeout, self.settings.WindowGeneratorTimeout)
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	return callDiscoveryDemand(self, read, timeout, func(ctx context.Context) (*MultiClientGeneratorClientArgs, error) {
		if err := read.ctx.Err(); err != nil {
			return nil, &discoveryDemandLocalCancel{cause: err}
		}
		if generator, ok := self.generator.(MultiClientGeneratorWithDestinationContext); ok {
			return generator.NewClientArgsForDestinationContext(ctx, destination)
		}
		return self.generator.(multiClientGeneratorArgsContext).NewClientArgsContext(ctx)
	})
}

// Reserve the current ordinary request once. After any nonempty demand result,
// no further demand mint is permitted in this acquisition, even after failure.
func (self *multiClientWindow) claimDiscoveryDemandMint(read *discoveryDemandRead) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.discoveryDemandLiveWithLock(time.Now()) || self.discoveryDemand.owner != read.owner ||
		self.discoveryDemand.slot != read.slot || read.ctx.Err() != nil || self.discoveryDemand.mintClaimed || !discoveryDemandAttemptLive(read.update, read.stamp) ||
		!self.discoveryDemandMembersLiveWithLock(read.members) {
		return false
	}
	self.discoveryDemand.mintClaimed = true
	self.discoveryDemand.pending, self.discoveryDemand.pendingFlow, self.discoveryDemand.pendingMembers = false, nil, nil
	return true
}

func (self *multiClientWindow) offerDiscoveryDemandArgs(read *discoveryDemandRead, args *multiClientChannelArgs) bool {
	if !self.discoveryDemandReadLive(read) {
		return false
	}
	args.discoveryDemandOwner, args.discoveryDemandSlot = read.owner, read.slot
	select {
	case <-self.ctx.Done():
		return false
	case <-read.ctx.Done():
		return false
	case self.clientChannelArgs <- args:
		return true
	}
}

// A new source sequence starts this observational clock; the canonical source
// and failed-service record remain unchanged. Completion replay never resets it.
type discoveryAttemptObservation struct {
	stamp uint64
}

func (self *multiClientChannelUpdate) observeDiscoveryAttempt(candidates []*multiClientChannel, now time.Time, source *IpPath) (discoveryAttemptObservation, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(candidates) == 0 || self.ctx.Err() != nil || self.receivedInbound.Load() || self.sequenceCloseReady.Load() ||
		self.sourceReset.Load() || !self.dialFailedSynSeen || self.discoveryAttemptStarted.IsZero() ||
		(source != nil && self.dialFailedSyn != source.SequenceNumber) {
		return discoveryAttemptObservation{}, false
	}
	exhausted := true
	for _, client := range candidates {
		if !slices.Contains(self.dialFailedClients, client) {
			exhausted = false
			break
		}
	}
	if !exhausted && now.Sub(self.discoveryAttemptStarted) < inferredDialFailureTimeout {
		return discoveryAttemptObservation{}, false
	}
	return discoveryAttemptObservation{stamp: self.discoveryAttemptStamp.Load()}, true
}

func (self *multiClientWindow) requestDiscoveryDemand(update *multiClientChannelUpdate, ipVersion int, source *IpPath, failed *multiClientChannel) {
	if self.settings == nil || self.settings.DiscoveryDemandListingsPerPass <= 0 || !self.cooperativeDiscoveryDemand() {
		return
	}
	if ipVersion == 6 {
		if _, ok := self.generator.(multiClientGeneratorFamilyDiscoveryContext); !ok {
			return
		}
	}
	members := func() map[Id]*multiClientChannel {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.discoveryDemandLiveWithLock(time.Now()) || self.discoveryDemand.mintClaimed ||
			self.discoveryDemand.reads >= min(self.settings.DiscoveryDemandListingsPerPass, 3) {
			return nil
		}
		return maps.Clone(self.clients)
	}()
	if len(members) == 0 || (failed != nil && !slices.Contains(slices.Collect(maps.Values(members)), failed)) {
		return
	}
	candidates := make([]*multiClientChannel, 0, len(members))
	offeredCount := 0
	for _, client := range members {
		offered, compatible := func() (bool, bool) {
			client.stateLock.Lock()
			defer client.stateLock.Unlock()
			return client.ctx.Err() == nil && client.endErr == nil && !client.warning && !client.quarantined,
				client.supportsIpVersion(ipVersion)
		}()
		if offered {
			offeredCount++
			if compatible {
				candidates = append(candidates, client)
			}
		}
	}
	observation, eligible := update.observeDiscoveryAttempt(candidates, time.Now(), source)
	if !eligible {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.discoveryDemandLiveWithLock(time.Now()) || self.discoveryDemand.mintClaimed ||
		self.discoveryDemand.targetSize <= offeredCount || len(members) != len(self.clients) {
		return
	}
	for id, client := range members {
		if self.clients[id] != client {
			return
		}
	}
	if update.discoveryAttemptStamp.Load() != observation.stamp || update.ctx.Err() != nil ||
		update.receivedInbound.Load() || update.sourceReset.Load() || update.sequenceCloseReady.Load() {
		return
	}
	self.discoveryDemand.pendingFlow, self.discoveryDemand.pendingStamp, self.discoveryDemand.pendingMembers, self.discoveryDemand.ipVersion = update, observation.stamp, members, ipVersion
	if !self.discoveryDemand.pending {
		self.discoveryDemand.pending = true
		self.notifyDiscoveryDemandWithLock()
	}
}

// SYN retransmissions reassert a level; explicit refusal can also request the
// first read immediately. Both edges only request work from the enumerator.
func (self *RemoteUserNatMultiClient) requestDiscoveryForSourceSyn(update *multiClientChannelUpdate, path *IpPath) {
	if path.Protocol == IpProtocolTcp && path.Syn && !path.Ack {
		for _, window := range self.windows {
			window.requestDiscoveryDemand(update, path.Version, path, nil)
		}
	}
}

func (self *RemoteUserNatMultiClient) requestDiscoveryForFailedAttempt(update *multiClientChannelUpdate, failed *multiClientChannel, path *IpPath) {
	if update != nil && path.Protocol == IpProtocolTcp {
		for _, window := range self.windows {
			window.requestDiscoveryDemand(update, path.Version, nil, failed)
		}
	}
}
