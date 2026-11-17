package connect

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Post-quantum encryption turned on in a running multi client (owner report
// 2026-11-17: "When turning on post quantum encryption on the latest main,
// there is still a warm up period where some providers fail to connect. After
// a while the providers start working."). The toggle retired the installed
// window clients, but each window kept what it had formed under the previous
// profile: the destinations of a discovery listing the platform chose without
// the modern requirement, and candidates built, and being evaluated, with
// post-quantum off. A listing made before the toggle was built after it as
// post-quantum candidates, and every provider in it that cannot seal a
// post-quantum session failed its evaluation, each after the full initial ping
// budget; a candidate built before the toggle could be admitted after it and
// carry the window's traffic with post-quantum off. A toggle off, and direct
// mode turned off, kept the same state the same way.

// The profile the tests turn post-quantum on with.
func postQuantumToggleTestProfile() *PerformanceProfile {
	return &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
}

// One client a window asked the generator to build.
type postQuantumToggleTestBuild struct {
	clientId    Id
	destination MultiHopId
	mode        EncryptionMode
}

// The encryption mode a window asked a client to run.
func postQuantumToggleTestMode(clientSettings *ClientSettings) EncryptionMode {
	if clientSettings == nil || clientSettings.EncryptionSettings == nil {
		return EncryptionModeOff
	}
	return clientSettings.EncryptionSettings.Mode
}

// A discovery generator that records, for each destination it lists, whether
// the multi client required post-quantum encryption as the listing began (the
// requirement a platform request carries), and for each client a window asks
// it to build, the destination and the encryption mode. Every build fails, as
// a provider that does not answer, so the windows keep listing. While
// `holdListing` is set, a listing begun with the requirement equal to
// `holdPostQuantum` waits for the release; while `holdMint` is set, so does a
// mint for a destination listed so.
type postQuantumToggleTestGenerator struct {
	testingEmptyMultiClientGenerator

	holdListing     bool
	holdMint        bool
	holdPostQuantum bool

	release     chan struct{}
	releaseOnce sync.Once
	heldChanged chan struct{}
	built       chan struct{}

	stateLock          sync.Mutex
	postQuantum        bool
	listedPostQuantum  map[MultiHopId]bool
	mintedDestinations map[Id]MultiHopId
	builds             []postQuantumToggleTestBuild
	heldCount          int
	// called on the building pass's goroutine, with no lock held, for each
	// build and the requirement its destination was listed under
	onBuild func(build postQuantumToggleTestBuild, listedPostQuantum bool)
}

func newPostQuantumToggleTestGenerator() *postQuantumToggleTestGenerator {
	return &postQuantumToggleTestGenerator{
		release:            make(chan struct{}),
		heldChanged:        make(chan struct{}, 1),
		built:              make(chan struct{}, 1),
		listedPostQuantum:  map[MultiHopId]bool{},
		mintedDestinations: map[Id]MultiHopId{},
	}
}

// Implements MultiClientGeneratorWithPostQuantumEncryption.
func (self *postQuantumToggleTestGenerator) SetPostQuantumEncryption(postQuantumEncryption bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.postQuantum = postQuantumEncryption
}

// Two destinations never listed before.
func (self *postQuantumToggleTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations := map[MultiHopId]DestinationStats{}
	hold := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for range 2 {
			destination := RequireMultiHopId(NewId())
			self.listedPostQuantum[destination] = self.postQuantum
			destinations[destination] = DestinationStats{}
		}
		return self.holdListing && self.postQuantum == self.holdPostQuantum
	}()
	if hold {
		self.wait()
	}
	return destinations, nil
}

// Implements MultiClientGeneratorWithDestination.
func (self *postQuantumToggleTestGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	args := &MultiClientGeneratorClientArgs{ClientId: NewId()}
	hold := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.mintedDestinations[args.ClientId] = destination
		return self.holdMint && self.listedPostQuantum[destination] == self.holdPostQuantum
	}()
	if hold {
		self.wait()
	}
	return args, nil
}

func (self *postQuantumToggleTestGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	build, listedPostQuantum, onBuild := func() (postQuantumToggleTestBuild, bool, func(postQuantumToggleTestBuild, bool)) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		build := postQuantumToggleTestBuild{
			clientId:    args.ClientId,
			destination: self.mintedDestinations[args.ClientId],
			mode:        postQuantumToggleTestMode(clientSettings),
		}
		self.builds = append(self.builds, build)
		return build, self.listedPostQuantum[build.destination], self.onBuild
	}()
	if onBuild != nil {
		onBuild(build, listedPostQuantum)
	}
	select {
	case self.built <- struct{}{}:
	default:
	}
	return nil, errors.New("the provider does not answer")
}

// Sets the build observer before the builds it observes.
func (self *postQuantumToggleTestGenerator) setOnBuild(onBuild func(build postQuantumToggleTestBuild, listedPostQuantum bool)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.onBuild = onBuild
}

// Counts the hold, then waits for the release.
func (self *postQuantumToggleTestGenerator) wait() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.heldCount += 1
	}()
	select {
	case self.heldChanged <- struct{}{}:
	default:
	}
	<-self.release
}

// Waits until every window that discovers holds a listing or a mint. A
// window's enumerator is one goroutine, so it holds at most one. The limited
// window asks only a capability-aware generator, so this one is asked by the
// normal windows alone.
func (self *postQuantumToggleTestGenerator) waitHeld(t *testing.T, ctx context.Context, multi *RemoteUserNatMultiClient) {
	t.Helper()
	windowCount := len(multi.windows)
	if multi.windows[WindowTypeLimited] != nil {
		windowCount -= 1
	}
	for {
		heldCount := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.heldCount
		}()
		if windowCount <= heldCount {
			return
		}
		select {
		case <-self.heldChanged:
		case <-ctx.Done():
			t.Fatalf("%d of %d windows held", heldCount, windowCount)
		}
	}
}

// Releases every hold, now and later. Safe to call more than once.
func (self *postQuantumToggleTestGenerator) releaseHeld() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

// Waits until a window builds a client for a destination listed with the
// requirement equal to postQuantum, and returns every build so far with the
// requirement each destination was listed under.
func (self *postQuantumToggleTestGenerator) waitListingBuilt(t *testing.T, ctx context.Context, postQuantum bool) ([]postQuantumToggleTestBuild, map[MultiHopId]bool) {
	t.Helper()
	for {
		builds, listedPostQuantum, done := func() ([]postQuantumToggleTestBuild, map[MultiHopId]bool, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			done := slices.ContainsFunc(self.builds, func(build postQuantumToggleTestBuild) bool {
				return self.listedPostQuantum[build.destination] == postQuantum
			})
			listedPostQuantum := map[MultiHopId]bool{}
			for _, build := range self.builds {
				listedPostQuantum[build.destination] = self.listedPostQuantum[build.destination]
			}
			return slices.Clone(self.builds), listedPostQuantum, done
		}()
		if done {
			return builds, listedPostQuantum
		}
		select {
		case <-self.built:
		case <-ctx.Done():
			t.Fatalf("no window built a destination listed with post-quantum %t; builds %d", postQuantum, len(builds))
		}
	}
}

// A multi client over generator with the profile, closed with the test. The
// generator deadline is off, so a listing or a mint the test holds is never
// abandoned: the holds are the test's barriers.
func newPostQuantumToggleTestMultiClient(
	t *testing.T,
	ctx context.Context,
	generator MultiClientGenerator,
	performanceProfile *PerformanceProfile,
	configure func(settings *MultiClientSettings),
) *RemoteUserNatMultiClient {
	t.Helper()
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()
	settings.WindowGeneratorTimeout = 0
	settings.DefaultPerformanceProfile = performanceProfile
	if configure != nil {
		configure(settings)
	}
	multi := NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Public, settings)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := multi.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return multi
}

// Waits until the window's expansion pass is acquiring candidates
// (beginDiscoveryAcquisition), subscribed in the same section as the read.
func waitPostQuantumToggleTestAcquiring(t *testing.T, ctx context.Context, window *multiClientWindow) {
	t.Helper()
	for {
		acquiring, changed := func() (bool, <-chan struct{}) {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			changed := window.discoveryDemandChangedWithLock()
			return window.discoveryDemand.owner != nil, changed
		}()
		if acquiring {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("the %s window's pass is not acquiring", windowStatsWindowOf(window.windowType))
		}
	}
}

// The two post-quantum toggles: from the first profile to the second.
type postQuantumToggleTestCase struct {
	name string
	from *PerformanceProfile
	to   *PerformanceProfile
}

func postQuantumToggleTestCases() []postQuantumToggleTestCase {
	return []postQuantumToggleTestCase{
		{name: "post-quantum on", from: nil, to: postQuantumToggleTestProfile()},
		{name: "post-quantum off", from: postQuantumToggleTestProfile(), to: nil},
	}
}

// Reports the first build after the toggle that is not a client of the new
// profile's encryption mode for a destination listed under the new
// requirement.
func assertPostQuantumToggleBuilds(t *testing.T, toggle postQuantumToggleTestCase, builds []postQuantumToggleTestBuild, listedPostQuantum map[MultiHopId]bool) {
	t.Helper()
	postQuantum := toggle.to != nil && toggle.to.PostQuantumEncryption
	mode := EncryptionModeOff
	if postQuantum {
		mode = EncryptionModeRequired
	}
	for _, build := range builds {
		if listedPostQuantum[build.destination] != postQuantum {
			t.Errorf("%s: a window built %s (encryption mode %d) from a listing made before the toggle", toggle.name, build.destination, build.mode)
			return
		}
		if build.mode != mode {
			t.Errorf("%s: a window built %s with encryption mode %d after the toggle, want %d", toggle.name, build.destination, build.mode, mode)
			return
		}
	}
}

// A discovery listing out when post-quantum encryption is turned on was asked
// for without the modern requirement, so the platform may answer providers
// that cannot seal a post-quantum session. None of its destinations is built:
// the window lists again under the requirement and builds what that listing
// returns. Before the fix the listing's destinations were minted and built as
// post-quantum candidates, and each provider among them that cannot seal
// failed its evaluation, the providers the owner saw fail after the toggle.
// Turning post-quantum off drops a listing made with the requirement the same
// way; before the fix its destinations were built with post-quantum off.
//
// The pass each window was running when the profile changed ends with the
// change. It admitted nothing, and that is no evidence about the providers,
// so it does not defer the window's next expansion: the first candidate of
// the new listing is built with no expansion backoff in any window. Every
// window's pass is acquiring when the profile changes, so the window that
// builds first had its pass ended by the change.
func TestPostQuantumToggleNeverBuildsAListingMadeBeforeIt(t *testing.T) {
	for _, toggle := range postQuantumToggleTestCases() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		generator := newPostQuantumToggleTestGenerator()
		generator.holdListing = true
		generator.holdPostQuantum = toggle.from != nil && toggle.from.PostQuantumEncryption
		multi := newPostQuantumToggleTestMultiClient(t, ctx, generator, toggle.from, func(settings *MultiClientSettings) {
			// a pass waits out the test's barriers in its acquisition
			settings.WindowExpandTimeout = time.Minute
		})
		t.Cleanup(generator.releaseHeld)

		// each discovery window's expansion backoff, read as the first
		// candidate of a listing under the new requirement is built
		var backoffLock sync.Mutex
		var backoffFailureCounts map[WindowType]int
		generator.setOnBuild(func(build postQuantumToggleTestBuild, listedPostQuantum bool) {
			if listedPostQuantum == generator.holdPostQuantum {
				return
			}
			backoffLock.Lock()
			defer backoffLock.Unlock()
			if backoffFailureCounts != nil {
				return
			}
			backoffFailureCounts = map[WindowType]int{}
			for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
				window := multi.windows[windowType]
				window.stateLock.Lock()
				backoffFailureCounts[windowType] = window.discoveryRetry.failureCount
				window.stateLock.Unlock()
			}
		})

		// every window's listing is out, asked for under the previous
		// requirement, so whichever window builds first builds after it, and
		// every window's pass is acquiring the candidates the listing holds
		generator.waitHeld(t, ctx, multi)
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			waitPostQuantumToggleTestAcquiring(t, ctx, multi.windows[windowType])
		}
		AssertEqual(t, multi.SetPerformanceProfile(toggle.to), nil)
		generator.releaseHeld()

		builds, listedPostQuantum := generator.waitListingBuilt(t, ctx, !generator.holdPostQuantum)
		assertPostQuantumToggleBuilds(t, toggle, builds, listedPostQuantum)
		backoffLock.Lock()
		for windowType, failureCount := range backoffFailureCounts {
			if 0 < failureCount {
				t.Errorf("%s: the %s window deferred its expansion for %d passes the change ended", toggle.name, windowStatsWindowOf(windowType), failureCount)
			}
		}
		backoffLock.Unlock()
	}
}

// A candidate minted from a listing made under the previous requirement is
// handed to a pass after the toggle. The pass returns it unbuilt, and the
// window builds from a listing made under the new requirement. Before the fix
// the pass built it under the new profile: after post-quantum was turned on,
// as a post-quantum candidate for a provider the platform may not have
// confirmed modern.
func TestPostQuantumToggleNeverBuildsACandidateMintedBeforeIt(t *testing.T) {
	for _, toggle := range postQuantumToggleTestCases() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		generator := newPostQuantumToggleTestGenerator()
		generator.holdMint = true
		generator.holdPostQuantum = toggle.from != nil && toggle.from.PostQuantumEncryption
		multi := newPostQuantumToggleTestMultiClient(t, ctx, generator, toggle.from, nil)
		t.Cleanup(generator.releaseHeld)

		// every window's listing came back under the previous requirement,
		// and the mint of its first destination is out
		generator.waitHeld(t, ctx, multi)
		AssertEqual(t, multi.SetPerformanceProfile(toggle.to), nil)
		generator.releaseHeld()

		builds, listedPostQuantum := generator.waitListingBuilt(t, ctx, !generator.holdPostQuantum)
		assertPostQuantumToggleBuilds(t, toggle, builds, listedPostQuantum)
	}
}

// A fixed generator of one provider, for the data-only provider probe, which
// admits a constructed candidate without a ping. It builds real clients and
// records each one's encryption mode. Its first build waits for the release.
type postQuantumToggleFixedTestGenerator struct {
	testingEmptyMultiClientGenerator

	destination MultiHopId

	release     chan struct{}
	releaseOnce sync.Once
	held        chan Id

	stateLock sync.Mutex
	builds    []postQuantumToggleTestBuild
	// called on the building pass's goroutine, with no lock held, with the
	// count of builds so far, this one included
	onBuild func(buildCount int)
}

func newPostQuantumToggleFixedTestGenerator() *postQuantumToggleFixedTestGenerator {
	return &postQuantumToggleFixedTestGenerator{
		destination: RequireMultiHopId(NewId()),
		release:     make(chan struct{}),
		held:        make(chan Id, 1),
	}
}

func (self *postQuantumToggleFixedTestGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

func (self *postQuantumToggleFixedTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return map[MultiHopId]DestinationStats{self.destination: {}}, nil
}

// Implements MultiClientGeneratorWithDestination.
func (self *postQuantumToggleFixedTestGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
}

func (self *postQuantumToggleFixedTestGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	buildCount, onBuild := func() (int, func(int)) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.builds = append(self.builds, postQuantumToggleTestBuild{
			clientId:    args.ClientId,
			destination: self.destination,
			mode:        postQuantumToggleTestMode(clientSettings),
		})
		return len(self.builds), self.onBuild
	}()
	if onBuild != nil {
		onBuild(buildCount)
	}
	if buildCount == 1 {
		self.held <- args.ClientId
		<-self.release
	}
	return NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings), nil
}

// Sets the build observer before the builds it observes.
func (self *postQuantumToggleFixedTestGenerator) setOnBuild(onBuild func(buildCount int)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.onBuild = onBuild
}

// Releases every hold, now and later. Safe to call more than once.
func (self *postQuantumToggleFixedTestGenerator) releaseHeld() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

// A fixed provider's candidate is being built when post-quantum encryption is
// turned on. It is never admitted: the change ends it, and the window builds
// the provider again under the new profile, with no re-dial deferral, since
// the change is no evidence against the provider. Before the fix the
// candidate was admitted after the toggle and carried the window's traffic
// with post-quantum off, which the user had just turned on. A toggle off, and
// direct mode turned off, which ends every direct stream because a direct
// stream shows the provider the user's own address, are the other changes the
// window's clients cannot carry; before the fix each admitted the candidate
// built before it as well.
func TestRetiringProfileChangeNeverAdmitsACandidateBuiltBeforeIt(t *testing.T) {
	toggles := append(postQuantumToggleTestCases(), postQuantumToggleTestCase{
		name: "direct off",
		from: &PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: true},
		to:   nil,
	})
	for _, toggle := range toggles {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		generator := newPostQuantumToggleFixedTestGenerator()
		multi := newPostQuantumToggleTestMultiClient(t, ctx, generator, toggle.from, func(settings *MultiClientSettings) {
			settings.ProviderProbe = false
			settings.DataOnlyProviderProbe = true
		})
		t.Cleanup(generator.releaseHeld)

		// the provider's re-dial failures as the window builds it again after
		// the change, before an admission clears them
		var retryLock sync.Mutex
		retryFailureCount := -1
		generator.setOnBuild(func(buildCount int) {
			if buildCount != 2 {
				return
			}
			window := multi.windows[WindowTypeQuality]
			window.stateLock.Lock()
			failureCount := 0
			if retry := window.fixedRetries[generator.destination]; retry != nil {
				failureCount = retry.failureCount
			}
			window.stateLock.Unlock()
			retryLock.Lock()
			defer retryLock.Unlock()
			retryFailureCount = failureCount
		})

		// every client the window admits, as the user's provider list shows it
		added := make(chan Id, 16)
		unsub := multi.Monitor().AddMonitorEventCallback(func(_ *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, _ bool) {
			for clientId, providerEvent := range providerEvents {
				if providerEvent != nil && providerEvent.State == ProviderStateAdded {
					select {
					case added <- clientId:
					default:
					}
				}
			}
		})
		defer unsub()

		// the window's candidate is being built under the previous profile
		var heldClientId Id
		select {
		case heldClientId = <-generator.held:
		case <-ctx.Done():
			t.Fatalf("%s: no candidate was built", toggle.name)
		}
		AssertEqual(t, multi.SetPerformanceProfile(toggle.to), nil)
		generator.releaseHeld()

		var addedClientId Id
		select {
		case addedClientId = <-added:
		case <-ctx.Done():
			t.Fatalf("%s: the window admitted no client after the change", toggle.name)
		}
		if addedClientId == heldClientId {
			t.Errorf("%s: a candidate built under the previous profile was admitted after the change", toggle.name)
			continue
		}

		window := multi.windows[WindowTypeQuality]
		window.stateLock.Lock()
		client := window.clients[addedClientId]
		window.stateLock.Unlock()
		if client == nil || !performanceProfilesEqual(client.performanceProfile, toggle.to) {
			t.Errorf("%s: the window admitted %s, not a candidate built under the new profile", toggle.name, addedClientId)
			continue
		}
		// the change cut the attempt; the provider was not deferred for it
		retryLock.Lock()
		failureCount := retryFailureCount
		retryLock.Unlock()
		if failureCount != 0 {
			t.Errorf("%s: the provider was built again with %d re-dial failures the change caused", toggle.name, failureCount)
		}
	}
}
