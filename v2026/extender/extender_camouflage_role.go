package extender

// extender_camouflage_role.go — what a provider role runs to turn the tcp
// carrier camouflage on (EXTENDER.md P5, P7, phase 14c; EXTENDER-UPDATE2 §3;
// EXTENDER-SPLICE-ROLLOUT.md). The standalone extender of connectctl and the
// sdk's provider role run the same one, so a phase means one thing on both. A
// role that does not opt in builds none of this, and its server and activation
// are what they were.
//
// It keeps three things together:
//
//   - The key. The X25519 static key is derived from the identity seed (P1), so
//     it has no file of its own: it is exactly as stable and as well kept as the
//     identity, and rotates with it. A seed it cannot derive from refuses the
//     role at start.
//   - The borrowed names. The role verifies every name the bundled lists accept
//     (AcceptNames: staged and retiring included) at start and every
//     revalidation interval, over the path the splice itself dials and on every
//     family this host can dial, and the running server splices to the names
//     that pass. A name reachable on one family of a dual-stack host is dropped,
//     since a client of the other family is then not served what the name
//     promises. A failed name is dropped at once and tried again sooner than the
//     interval, each retry twice as long after the last, so a host that starts
//     before its network is up recovers in minutes. An empty list verifies
//     nothing, dials nothing and is no error: the status says no_verified_target.
//   - The safe flip. The server's splice gate is an
//     ExtenderCamouflageSpliceSettle fed from this role's activations, whatever
//     the initial posture, so the posture can never splice before the records
//     have settled.
//
// The key is published only while at least one name is verified (P5): an extender
// with none serves the legacy path alone, so a bad or empty list degrades to
// today rather than leaving the operator to require a capability of clients for
// an extender that cannot splice. A key the operator has not heard is posted at
// once when a first name verifies (SetReactivate). The reverse waits for the next
// activation, so one bad pass that the retry repairs changes no record and
// restarts no settling. Each activation that posts no key costs the record its
// key, and so the gate its wait (ExtenderCamouflageSpliceSettle).
//
// The status counts a pass's failures in BorrowVerifyFailCount, and the coverage
// of every list (connect.EvaluateExtenderCamouflageCoverage) is what lets
// CamouflageStatus report coverage at all once a role runs.
//
// Safe for concurrent use.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// DefaultExtenderCamouflageRevalidationInterval is how often a role verifies its
// borrowed names again when nothing says otherwise. The value is the owner's to
// decide, and to record in the rollout configuration before Phase B
// (EXTENDER-UPDATE2 §3.1); this default is a placeholder chosen to err toward
// the borrowed sites, whose load a fleet multiplies (§3.3), while a name that
// died is noticed within the record refresh interval a client already lives with.
const DefaultExtenderCamouflageRevalidationInterval = 6 * time.Hour

// ExtenderCamouflageMinRevalidationInterval is the shortest revalidation interval
// a role runs, so a mistyped option or list cannot turn a fleet into a load on
// the sites it borrows.
const ExtenderCamouflageMinRevalidationInterval = 10 * time.Minute

// The settings of an ExtenderCamouflageRole. Zero values take the defaults.
type ExtenderCamouflageRoleSettings struct {
	// The initial splice posture: off is Phase A, on is Phase B once the records
	// have settled (ExtenderSettings.ExtenderCamouflageSplice).
	Splice bool

	// How often the names are verified again. Non-positive takes
	// DefaultExtenderCamouflageRevalidationInterval. A list whose manifest asks
	// for a shorter interval gets it. Never shorter than
	// ExtenderCamouflageMinRevalidationInterval.
	RevalidationInterval time.Duration
	// How long after a pass a failed name is first tried again. Each retry waits
	// twice as long, up to the revalidation interval.
	VerifyRetryInterval time.Duration
	// The budget of one verification dial and handshake of one name.
	VerifyTimeout time.Duration
	// How many verifications run at once.
	VerifyConcurrency int
	// How long a role holds its first activation for the first pass
	// (WaitFirstVerification), so a restart does not post once without the key
	// and again with it. A pass that outlasts this posts the key when it ends.
	FirstVerificationTimeout time.Duration

	// The settings of the safe-flip gate. Nil takes the defaults, on this clock.
	Settle *ExtenderCamouflageSpliceSettleSettings

	// The families this host can dial. Nil is connect.FamilySupported.
	IpVersionSupported func(ipVersion int) bool
	// The clock of the verification times and of a defaulted Settle. Nil is
	// time.Now.
	Now func() time.Time
	// Replaces time.After as the wait between passes. Tests fire passes through
	// it, and read how long each wait was.
	After func(wait time.Duration) <-chan time.Time
	// Replaces the verification of one name on one network. Nil is
	// connect.VerifyExtenderBorrowDomain over the path the splice dials. Tests
	// count and script it. The list is curated for being plausible where it is
	// used, so the default rejects no peer as a shared cdn.
	VerifyBorrowName func(ctx context.Context, network string, borrowName string) bool
	// Installed as ExtenderSettings.CamouflageSpliceDialContext. Tests reach a
	// fake borrowed site through it, for the verification and the splice alike.
	SpliceDialContext connect.DialContextFunction
}

// ExtenderCamouflageStatusLine is a camouflage's standing as a role logs it
// (EXTENDER-UPDATE2 §3.3, EXTENDER-SPLICE-ROLLOUT.md). There is no metrics path off
// an extender yet, so this is what an operator reads. It never calls a degraded
// state coverage, and it names no borrowed site, address or key.
func ExtenderCamouflageStatusLine(status ExtenderCamouflageStatus, stats ExtenderCamouflageStats) string {
	coverage := "covered"
	if !status.Covered {
		coverage = "not covered: " + status.DegradedReason
	}
	return fmt.Sprintf(
		"%s, %s; %d verified names, %d failed verifications; %d authenticated, %d spliced, %d terminated, %d fallback terminated",
		status.Phase,
		coverage,
		status.VerifiedTargetCount,
		stats.BorrowVerifyFailCount,
		stats.AuthenticatedCount,
		stats.SplicedCount,
		stats.TerminatedCount,
		stats.SpliceFallbackTerminatedCount,
	)
}

// DefaultExtenderCamouflageRoleSettings are the settings of a Phase A role.
func DefaultExtenderCamouflageRoleSettings() *ExtenderCamouflageRoleSettings {
	return &ExtenderCamouflageRoleSettings{
		RevalidationInterval:     DefaultExtenderCamouflageRevalidationInterval,
		VerifyRetryInterval:      5 * time.Minute,
		VerifyTimeout:            10 * time.Second,
		VerifyConcurrency:        4,
		FirstVerificationTimeout: 30 * time.Second,
	}
}

// ExtenderCamouflageRole is the camouflage of one provider role (see the file
// comment).
type ExtenderCamouflageRole struct {
	settings          *ExtenderCamouflageRoleSettings
	identityKeySeed   []byte
	identityPublicKey ed25519.PublicKey
	realityPublicKey  []byte
	settle            *ExtenderCamouflageSpliceSettle

	// closed when the first pass has ended
	firstVerification chan struct{}

	stateLock sync.Mutex
	// nil until Start
	cancel context.CancelFunc
	done   chan struct{}
	// each verified name and when it last passed
	verifiedTimes map[string]time.Time
	// an activation posted without the key, and the operator has not been told
	// of it since (PublishedRealityPublicKey)
	postedWithoutKey bool
	reactivate       func()
}

// NewExtenderCamouflageRole is the camouflage of the extender with this identity
// seed. An error means the seed cannot serve one, which the role reports as its
// start failing: a host that opted in does not run without the camouflage it
// asked for. A nil settings takes the defaults.
func NewExtenderCamouflageRole(
	identityKeySeed []byte,
	settings *ExtenderCamouflageRoleSettings,
) (*ExtenderCamouflageRole, error) {
	identityPublicKey, err := connect.ExtenderPublicKeyFromSeed(identityKeySeed)
	if err != nil {
		return nil, fmt.Errorf("the extender camouflage key derives from the identity key, which is not usable: %w", err)
	}
	realityPublicKey, err := connect.ExtenderRealityStaticPublicKey(identityKeySeed)
	if err != nil {
		return nil, fmt.Errorf("the extender camouflage key could not be derived from the identity key: %w", err)
	}

	defaults := DefaultExtenderCamouflageRoleSettings()
	copied := DefaultExtenderCamouflageRoleSettings()
	if settings != nil {
		*copied = *settings
	}
	if copied.RevalidationInterval <= 0 {
		copied.RevalidationInterval = defaults.RevalidationInterval
	}
	if copied.VerifyRetryInterval <= 0 {
		copied.VerifyRetryInterval = defaults.VerifyRetryInterval
	}
	if copied.VerifyTimeout <= 0 {
		copied.VerifyTimeout = defaults.VerifyTimeout
	}
	if copied.VerifyConcurrency <= 0 {
		copied.VerifyConcurrency = defaults.VerifyConcurrency
	}
	if copied.FirstVerificationTimeout <= 0 {
		copied.FirstVerificationTimeout = defaults.FirstVerificationTimeout
	}
	if copied.IpVersionSupported == nil {
		copied.IpVersionSupported = connect.FamilySupported
	}
	if copied.Now == nil {
		copied.Now = time.Now
	}
	if copied.After == nil {
		copied.After = time.After
	}
	settleSettings := copied.Settle
	if settleSettings == nil {
		settleSettings = DefaultExtenderCamouflageSpliceSettleSettings()
		settleSettings.Now = copied.Now
	}

	return &ExtenderCamouflageRole{
		settings:          copied,
		identityKeySeed:   bytes.Clone(identityKeySeed),
		identityPublicKey: identityPublicKey,
		realityPublicKey:  realityPublicKey,
		settle:            NewExtenderCamouflageSpliceSettle(identityPublicKey, realityPublicKey, settleSettings),
		firstVerification: make(chan struct{}),
		verifiedTimes:     map[string]time.Time{},
	}, nil
}

// Sets what the server of this role is built with, on the identity this role
// derives from, so the key the server opens tags under is the key it publishes.
// No name yet: Start feeds them to the running server.
func (self *ExtenderCamouflageRole) ConfigureServer(settings *ExtenderSettings) {
	settings.IdentityKeySeed = self.identityKeySeed
	settings.CamouflageEnabled = true
	settings.ExtenderCamouflageSplice = self.settings.Splice
	settings.CamouflageSpliceGate = self.settle.Open
	settings.CamouflageBorrowNames = nil
	if self.settings.SpliceDialContext != nil {
		settings.CamouflageSpliceDialContext = self.settings.SpliceDialContext
	}
}

// Starts verifying the names for the server this role configured, which it does
// until Close or until ctx is done. Called once.
func (self *ExtenderCamouflageRole) Start(ctx context.Context, server *ExtenderServer) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.cancel = cancel
		self.done = done
	}()
	go connect.HandleError(func() {
		defer close(done)
		self.run(runCtx, server)
	}, cancel)
}

// Waits for the first pass to end, at most FirstVerificationTimeout, so the first
// activation can carry the key. A timeout is not an error: the key is posted once
// a name verifies.
func (self *ExtenderCamouflageRole) WaitFirstVerification(ctx context.Context) {
	select {
	case <-self.firstVerification:
	case <-ctx.Done():
	case <-time.After(self.settings.FirstVerificationTimeout):
	}
}

// PublishedRealityPublicKey is the camouflage key an activation posts now: this
// extender's, while at least one name is verified, and none otherwise (P5). It is
// the value of ExtenderActivatorSettings.PublishedRealityPublicKey.
func (self *ExtenderCamouflageRole) PublishedRealityPublicKey() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.postedWithoutKey = len(self.verifiedTimes) == 0
	if self.postedWithoutKey {
		return nil
	}
	return bytes.Clone(self.realityPublicKey)
}

// Installs what asks for an activation pass when a name verifies after a post
// went out with no key (ExtenderActivator.Reactivate). A name that verified while
// the activator was being built, too late for its first post, asks at once.
func (self *ExtenderCamouflageRole) SetReactivate(reactivate func()) {
	askNow := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reactivate = reactivate
		return self.takeKeyToPostWithLock()
	}()
	if askNow {
		reactivate()
	}
}

// Whether a post went out with no key and a name has verified since. It is taken
// once, so one verified name asks for one pass.
func (self *ExtenderCamouflageRole) takeKeyToPostWithLock() bool {
	if !self.postedWithoutKey || self.reactivate == nil || len(self.verifiedTimes) == 0 {
		return false
	}
	self.postedWithoutKey = false
	return true
}

// Reads the record of one successful activation for the gate
// (ExtenderActivatorSettings.OnActivated), verified under rootKeys.
func (self *ExtenderCamouflageRole) ObserveActivation(
	result *connect.ExtenderActivateResult,
	rootKeys *connect.ExtenderRootKeySet,
) {
	self.settle.ObserveActivation(result, rootKeys)
}

// Ends the passes and joins them, a dial in flight included. Safe on a role that
// never started and when called twice.
func (self *ExtenderCamouflageRole) Close() {
	cancel, done := func() (context.CancelFunc, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.cancel, self.done
	}()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// The passes: every accepted name, then the failed ones again on the retry
// schedule while the interval runs, then every name again.
func (self *ExtenderCamouflageRole) run(ctx context.Context, server *ExtenderServer) {
	firstPass := true
	for {
		revalidationInterval := self.revalidationInterval()
		borrowNames := self.acceptNames()
		failedNames := self.verifyNames(ctx, server, borrowNames, borrowNames)
		if firstPass {
			firstPass = false
			close(self.firstVerification)
		}

		// never later than the interval, and spread so a fleet restarted together
		// does not verify together
		remaining := revalidationInterval - time.Duration(mathrand.Int64N(int64(revalidationInterval/10)+1))
		retryWait := self.settings.VerifyRetryInterval
		for 0 < len(failedNames) && retryWait < remaining {
			select {
			case <-ctx.Done():
				return
			case <-self.settings.After(retryWait):
			}
			remaining -= retryWait
			retryWait = min(2*retryWait, revalidationInterval)
			failedNames = self.verifyNames(ctx, server, borrowNames, failedNames)
		}
		select {
		case <-ctx.Done():
			return
		case <-self.settings.After(remaining):
		}
	}
}

// How often the names are verified again: the setting, or the shortest interval
// a list's manifest asks for when that is shorter, within the floor.
func (self *ExtenderCamouflageRole) revalidationInterval() time.Duration {
	revalidationInterval := self.settings.RevalidationInterval
	for _, borrowList := range connect.ExtenderCamouflageBorrowLists() {
		if 0 < borrowList.RevalidationInterval && borrowList.RevalidationInterval < revalidationInterval {
			revalidationInterval = borrowList.RevalidationInterval
		}
	}
	return max(revalidationInterval, ExtenderCamouflageMinRevalidationInterval)
}

// Every name a bundled list has this extender accept, in list order and distinct.
func (self *ExtenderCamouflageRole) acceptNames() []string {
	borrowNames := []string{}
	for _, borrowList := range connect.ExtenderCamouflageBorrowLists() {
		for _, borrowName := range borrowList.AcceptNames() {
			if !slices.Contains(borrowNames, borrowName) {
				borrowNames = append(borrowNames, borrowName)
			}
		}
	}
	return borrowNames
}

// The dial networks of the families this host can dial, which a name must pass on
// every one of (see the file comment).
func (self *ExtenderCamouflageRole) networks() []string {
	networks := []string{}
	if self.settings.IpVersionSupported(4) {
		networks = append(networks, "tcp4")
	}
	if self.settings.IpVersionSupported(6) {
		networks = append(networks, "tcp6")
	}
	return networks
}

// One pass over candidates, which feeds the server and returns the names that
// failed. borrowNames are all the names the lists accept: anything else is
// forgotten. A pass that ctx ended says nothing about a name, and changes nothing.
func (self *ExtenderCamouflageRole) verifyNames(
	ctx context.Context,
	server *ExtenderServer,
	borrowNames []string,
	candidates []string,
) []string {
	verified := self.verifyConcurrently(ctx, server, candidates)
	if ctx.Err() != nil {
		return nil
	}
	now := self.settings.Now()

	failedNames := []string{}
	verifiedNames := []string{}
	verifiedTimes := map[string]time.Time{}
	reactivate := func() func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for i, candidate := range candidates {
			if verified[i] {
				self.verifiedTimes[candidate] = now
			} else {
				delete(self.verifiedTimes, candidate)
				failedNames = append(failedNames, candidate)
			}
		}
		for borrowName := range self.verifiedTimes {
			if !slices.Contains(borrowNames, borrowName) {
				delete(self.verifiedTimes, borrowName)
			}
		}
		for _, borrowName := range borrowNames {
			if verifiedTime, ok := self.verifiedTimes[borrowName]; ok {
				verifiedNames = append(verifiedNames, borrowName)
				verifiedTimes[borrowName] = verifiedTime
			}
		}
		if self.takeKeyToPostWithLock() {
			return self.reactivate
		}
		return nil
	}()

	if camouflage := server.camouflage; camouflage != nil {
		camouflage.setBorrowNames(verifiedNames)
		coverage := connect.EvaluateExtenderCamouflageCoverage(
			self.identityPublicKey,
			connect.ExtenderCamouflageBorrowLists(),
			verifiedTimes,
			now,
		)
		camouflage.setCoverageIncomplete(!coverage.Complete())
	}
	server.camouflageBorrowVerifyFailCount.Add(int64(len(failedNames)))
	if reactivate != nil {
		reactivate()
	}
	return failedNames
}

// Whether each candidate verified, at most VerifyConcurrency at once. A name that
// never got its turn before ctx ended did not.
func (self *ExtenderCamouflageRole) verifyConcurrently(
	ctx context.Context,
	server *ExtenderServer,
	candidates []string,
) []bool {
	verified := make([]bool, len(candidates))
	networks := self.networks()
	slots := make(chan struct{}, self.settings.VerifyConcurrency)
	var workers sync.WaitGroup
	defer workers.Wait()
	for i, candidate := range candidates {
		select {
		case <-ctx.Done():
			return verified
		case slots <- struct{}{}:
		}
		workers.Add(1)
		go connect.HandleError(func() {
			defer workers.Done()
			defer func() { <-slots }()
			verified[i] = self.verifyOnEveryNetwork(ctx, server, networks, candidate)
		})
	}
	return verified
}

// Whether one name passes on every network. With no network this host can dial
// nothing passes.
func (self *ExtenderCamouflageRole) verifyOnEveryNetwork(
	ctx context.Context,
	server *ExtenderServer,
	networks []string,
	borrowName string,
) bool {
	for _, network := range networks {
		if !self.verifyBorrowName(ctx, server, network, borrowName) {
			return false
		}
	}
	return 0 < len(networks)
}

// Verifies one name on one network, over the dial the splice uses (spliceDial):
// its resolver, its egress and its seam.
func (self *ExtenderCamouflageRole) verifyBorrowName(
	ctx context.Context,
	server *ExtenderServer,
	network string,
	borrowName string,
) bool {
	if self.settings.VerifyBorrowName != nil {
		return self.settings.VerifyBorrowName(ctx, network, borrowName)
	}
	camouflage := server.camouflage
	if camouflage == nil {
		return false
	}
	spliceDial := func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		return camouflage.spliceDial(ctx, network, host)
	}
	return connect.VerifyExtenderBorrowDomain(ctx, spliceDial, network, borrowName, self.settings.VerifyTimeout, nil)
}
