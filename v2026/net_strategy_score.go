package connect

// net_strategy_score.go — per-network, delivery-verified, decayed scoring for
// the client strategy racer.
//
// The racer ranked dialers by a global, time-unweighted success ratio credited
// at handshake/response time (`clientDialer.Weight`). Two problems the 2026
// evidence names:
//   - a global time-unweighted ratio mis-ranks when a regime changes within
//     days (iran blocked quic and dns-over-tcp in the ~5-day run-up to the
//     june 2025 outage), so a stale winner keeps its rank long after it stopped
//     working;
//   - "handshake succeeded" is meaningless under russia's 16 KiB freeze and
//     iran's 6-packet grey connections -- a path must DELIVER a real payload,
//     not merely connect (lantern credits a route only after a 64 KiB body
//     actually arrives).
//
// This file adds the deployed-systems consensus (psiphon tactics/replay,
// lantern bandit tracks, outline smart dialer) as a scoring layer that sits
// over the existing race, changing the SCORING and the success definition, not
// the staggered race or the priorities:
//   - delivery verdict: only a delivered payload of `deliveryVerifiedByteCount`
//     with no stall counts as success; a handshake, or a transfer that froze
//     short of it, does not;
//   - per-network keying: scores are keyed by a client-only network id
//     (a platform path generation by default, optionally a hashed stable
//     host identifier), which is NEVER uploaded;
//   - decay + ttl: evidence decays by a half-life and expires past a ttl, so a
//     stale winner loses weight and reverts toward neutral;
//   - retain a failed winner with probability ~0.5, so one transient stall does
//     not flip a proven winner (psiphon replay);
//   - an injected prior: server-pushed per-country/asn priors are a later phase
//     (they need a server channel, not built here); the scoring accepts an
//     injected prior so that phase can plug in.
//
// The score multiplies the dialer's static configured weight. The lifetime
// handshake ratio never enters production ranking. HTTP bodies and H1/WS
// message reads supply payload evidence; short clean responses stay neutral.
//
// Concurrency: `networkStrategyScores` is safe for concurrent use; every method
// takes `stateLock`. No external calls are made under the lock.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	mathrand "math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// deliveryVerifiedByteCount is the payload a dial must deliver, with no stall
// beyond the transport's bound, before it counts as a success. Lantern's rule:
// 64 KiB, enough to outlast the point where russia's tspu freezes a flow (16
// KiB) and where iran cuts a grey connection (a few packets).
const deliveryVerifiedByteCount int64 = 64 * 1024

const (
	// strategyScoreHalfLife is how fast delivery evidence decays. A winner that
	// stops being refreshed loses half its weight each half-life and reverts
	// toward neutral, so a regime change within days is not out-ranked by stale
	// evidence.
	strategyScoreHalfLife = 6 * time.Hour
	// strategyScoreTtl is the hard expiry of a per-network record (psiphon
	// replay default). Past it the record is dropped and the dialer reverts to
	// the prior or neutral, whatever decay had left.
	strategyScoreTtl = 24 * time.Hour
	// strategyScoreRetainFailedProbability keeps a proven winner on a failed
	// delivery this often, so one transient stall does not flip it.
	strategyScoreRetainFailedProbability = 0.5
	// strategyScoreWinnerThreshold is the effective score at or above which a
	// record is a "winner" for the retain-on-failure rule.
	strategyScoreWinnerThreshold = 0.5
	// strategyScorePriorMass is the pseudo-observation weight of the prior (or
	// of neutral when there is no prior). As a record's decayed evidence falls
	// below this, its score is pulled back toward the prior -- which is how a
	// stale winner loses its boost even before the ttl.
	strategyScorePriorMass = 1.0
	// strategyScoreNeutral is the score that maps to a multiplier of 1.0: the
	// value of an unknown dialer and the default prior.
	strategyScoreNeutral = 0.5
)

// strategyVerdict is the outcome of one dial, finalized after the delivery
// canary rather than at the handshake.
type strategyVerdict int

const (
	// verdictHandshake is a dial that connected but has delivered nothing yet.
	// It is NOT a success: it neither credits nor penalizes the score. The
	// delivery verdict replaces it once a real payload reaches (or fails to
	// reach) `deliveryVerifiedByteCount`.
	verdictHandshake strategyVerdict = iota
	// verdictDelivered is a dial that moved `deliveryVerifiedByteCount` with no
	// stall. The only verdict that credits success.
	verdictDelivered
	// verdictStalled is a dial that connected but whose transfer froze short of
	// `deliveryVerifiedByteCount` (russia's 16 KiB freeze) -- a failed
	// delivery, not a success.
	verdictStalled
	// verdictFailed is a dial that did not connect at all. Scored like a stall.
	verdictFailed
)

// classifyDelivery is where a transport finalizes a dial's outcome: a payload
// of at least `deliveryVerifiedByteCount` that did not stall is delivered;
// anything less -- including a handshake that then froze at 16 KiB -- is a
// stall. This is the single place the "a path must deliver" rule lives.
func classifyDelivery(bytesDelivered int64, stalled bool) strategyVerdict {
	if stalled || bytesDelivered < deliveryVerifiedByteCount {
		return verdictStalled
	}
	return verdictDelivered
}

// deriveNetworkId maps the client-only network identifiers (wifi bssid,
// cellular mcc-mnc) to an opaque local key. The raw identifiers are hashed so
// not even the in-process key is the bssid, and the result is used ONLY as a
// local map key -- it is never placed in any uploaded or emitted field (see
// `strategyScoreReport`). An empty input yields the empty id, the "unknown
// network" bucket.
func deriveNetworkId(identifiers ...string) string {
	joined := strings.Join(identifiers, "\x00")
	if joined == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:8])
}

// scoreToMultiplier maps a score in [0,1] to a shuffle-weight multiplier in
// [0.5, 1.5]: neutral (0.5) is 1.0, a proven deliverer is up to 1.5, a proven
// staller down to 0.5. A multiplier keeps the existing weighted shuffle and
// priorities intact and only biases the draw.
func scoreToMultiplier(score float32) float32 {
	return 0.5 + score
}

type networkScoreKey struct {
	networkId string
	dialerKey string
}

// A stable configuration hash plus its activation generation prevents a late
// attempt from repopulating an earlier configuration after it returns.
type strategyScoreConfig struct {
	hash       string
	generation uint64
}

// decayedScore holds the delivered and total evidence mass as of updateTime.
// The ratio alone is decay-invariant, so ranking uses the absolute masses
// blended with the prior (`strategyScorePriorMass`): as the masses decay the
// blend reverts toward the prior, which is what makes a stale winner lose
// weight.
type decayedScore struct {
	deliveredWeight float64
	totalWeight     float64
	// last verdict time: both the decay anchor and the ttl anchor. Reads never
	// advance it, so a frequently-weighed record still ages out on schedule.
	updateTime time.Time
}

// decayFactor is 0.5^(elapsed/halfLife), 1 when now is not after updateTime.
func (self *decayedScore) decayFactor(now time.Time, halfLife time.Duration) float64 {
	if !now.After(self.updateTime) {
		return 1
	}
	return math.Pow(0.5, float64(now.Sub(self.updateTime))/float64(halfLife))
}

// decayTo scales the stored masses to now and moves the anchor; called only
// when a verdict is recorded.
func (self *decayedScore) decayTo(now time.Time, halfLife time.Duration) {
	factor := self.decayFactor(now, halfLife)
	self.deliveredWeight *= factor
	self.totalWeight *= factor
	self.updateTime = now
}

// decayedRatioAt is delivered/total after decaying to now, 0 when there is no
// evidence. Used only for the winner test; the ranking blend uses the masses.
func (self *decayedScore) decayedRatioAt(now time.Time, halfLife time.Duration) float64 {
	factor := self.decayFactor(now, halfLife)
	total := self.totalWeight * factor
	if total <= 0 {
		return 0
	}
	return (self.deliveredWeight * factor) / total
}

type networkStrategyScores struct {
	stateLock sync.Mutex
	scores    map[networkScoreKey]*decayedScore

	// configHash invalidates the whole store when the strategy config changes
	// (a new app version, new server-pushed tactics): a winner learned under
	// the old strategy set must not be replayed under the new one.
	configHash       string
	configGeneration uint64

	halfLife                time.Duration
	ttl                     time.Duration
	retainFailedProbability float64

	// now and retainFailedWinner are seams. now defaults to time.Now;
	// retainFailedWinner defaults to a seeded rng draw. Tests inject a fake
	// clock and a scripted coin for determinism.
	now                func() time.Time
	retainFailedWinner func() bool

	// prior is the injected server prior for a dialer on a network, in [0,1];
	// nil (the default) means no prior, which is neutral. The server-pushed
	// per-country/asn prior is a later phase that supplies this; nothing here
	// opens a server channel.
	prior func(networkId string, dialerKey string) (float32, bool)
}

// newNetworkStrategyScores builds an empty store with the production decay, ttl
// and retain policy and a time-seeded rng.
func newNetworkStrategyScores() *networkStrategyScores {
	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	scores := &networkStrategyScores{
		scores:                  map[networkScoreKey]*decayedScore{},
		halfLife:                strategyScoreHalfLife,
		ttl:                     strategyScoreTtl,
		retainFailedProbability: strategyScoreRetainFailedProbability,
		now:                     time.Now,
	}
	scores.retainFailedWinner = func() bool {
		scores.stateLock.Lock()
		defer scores.stateLock.Unlock()
		return rng.Float64() < scores.retainFailedProbability
	}
	return scores
}

// setConfigHash clears every learned score when the config hash changes,
// invalidating replay across a strategy-set change. The first call establishes
// the baseline and clears nothing.
func (self *networkStrategyScores) setConfigHash(configHash string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.configHash == configHash {
		return
	}
	if self.configHash != "" {
		self.scores = map[networkScoreKey]*decayedScore{}
	}
	self.configHash = configHash
	self.configGeneration++
}

// Snapshots immutable attempt ownership before consulting the strategy's live
// dialers. A concurrent configuration change can only make this snapshot stale.
func (self *networkStrategyScores) configuration() strategyScoreConfig {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strategyScoreConfig{hash: self.configHash, generation: self.configGeneration}
}

// recordVerdict folds one finalized dial outcome into the per-network record.
// A handshake-only verdict is a no-op: nothing is credited until the delivery
// canary resolves. A delivered verdict credits; a stalled/failed verdict
// penalizes, except that a proven winner is retained with
// `retainFailedProbability` so one transient stall does not flip it.
func (self *networkStrategyScores) recordVerdict(networkId string, dialerKey string, verdict strategyVerdict) {
	self.recordVerdictForConfig(nil, networkId, dialerKey, verdict)
}

// Late deliveries cannot repopulate scores after their strategy set changed.
func (self *networkStrategyScores) recordVerdictForConfig(config *strategyScoreConfig, networkId string, dialerKey string, verdict strategyVerdict) {
	if verdict == verdictHandshake {
		return
	}
	// the retain coin is drawn outside stateLock (its default takes the lock)
	retainCoin := false
	if verdict == verdictStalled || verdict == verdictFailed {
		retainCoin = self.retainFailedWinner()
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if config != nil && (config.hash != self.configHash || config.generation != self.configGeneration) {
		return
	}

	now := self.now()
	key := networkScoreKey{networkId: networkId, dialerKey: dialerKey}
	score := self.scores[key]
	if score == nil {
		score = &decayedScore{updateTime: now}
		self.scores[key] = score
	}
	score.decayTo(now, self.halfLife)

	switch verdict {
	case verdictDelivered:
		score.deliveredWeight += 1
		score.totalWeight += 1
	case verdictStalled, verdictFailed:
		isWinner := strategyScoreWinnerThreshold <= score.decayedRatioAt(now, self.halfLife)
		if isWinner && retainCoin {
			// keep the winner: do not count this failure against it
			return
		}
		score.totalWeight += 1
	}
}

// weight is the shuffle-weight multiplier for a dialer on a network: the
// delivered/total evidence (decayed) blended with the prior, mapped through
// `scoreToMultiplier`. An expired (past ttl) or absent record reverts to the
// prior, or to neutral 1.0 when there is none -- so an un-learned strategy
// races exactly as before.
func (self *networkStrategyScores) weight(networkId string, dialerKey string) float32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := self.now()
	priorValue := float64(strategyScoreNeutral)
	if self.prior != nil {
		if p, ok := self.prior(networkId, dialerKey); ok {
			priorValue = float64(p)
		}
	}
	key := networkScoreKey{networkId: networkId, dialerKey: dialerKey}
	score := self.scores[key]
	if score == nil || self.ttl <= now.Sub(score.updateTime) {
		if score != nil {
			delete(self.scores, key)
		}
		return scoreToMultiplier(float32(priorValue))
	}
	factor := score.decayFactor(now, self.halfLife)
	delivered := score.deliveredWeight * factor
	total := score.totalWeight * factor
	effective := (delivered + strategyScorePriorMass*priorValue) / (total + strategyScorePriorMass)
	return scoreToMultiplier(float32(effective))
}

// strategyScoreReport is the uploadable telemetry shape: delivery outcomes
// bucketed by strategy family, aggregated across ALL networks. It carries NO
// network id and NO per-network keys -- the network id is client-only. A test
// asserts the id never appears in this struct or its json.
type strategyScoreReport struct {
	Families map[string]strategyFamilyOutcomes `json:"families"`
}

type strategyFamilyOutcomes struct {
	Delivered int `json:"delivered"`
	Stalled   int `json:"stalled"`
}

// report aggregates the learned evidence into uploadable telemetry, dropping
// the network id entirely: it buckets by strategy family (the dialer key's
// family prefix) and sums across networks. Two clients on different networks
// with the same outcomes produce the same report.
func (self *networkStrategyScores) report() strategyScoreReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	families := map[string]strategyFamilyOutcomes{}
	for key, score := range self.scores {
		family := strategyFamilyOf(key.dialerKey)
		outcomes := families[family]
		// round the decayed masses to whole observations for the bucket
		outcomes.Delivered += int(math.Round(score.deliveredWeight))
		outcomes.Stalled += int(math.Round(score.totalWeight - score.deliveredWeight))
		families[family] = outcomes
	}
	return strategyScoreReport{Families: families}
}

// strategyFamilyOf is the family of a dialer key: the part before the first
// space, so "extender tcptls" and an address-qualified key share the family
// "extender" ... actually the family is the whole description without any
// address, which the dialer key places before a "|". Keys without a "|" are
// their own family.
func strategyFamilyOf(dialerKey string) string {
	if i := strings.IndexByte(dialerKey, '|'); 0 <= i {
		return dialerKey[:i]
	}
	return dialerKey
}

// sortedFamilies is a test/diagnostic helper: the report's families in a
// stable order.
func (self strategyScoreReport) sortedFamilies() []string {
	families := make([]string, 0, len(self.Families))
	for family := range self.Families {
		families = append(families, family)
	}
	sort.Strings(families)
	return families
}

// marshalJson is the exact bytes an uploader would send; the no-id test scans
// it for the network id.
func (self strategyScoreReport) marshalJson() ([]byte, error) {
	return json.Marshal(self)
}

// setPrior installs the injected per-network prior (the server-prior phase's
// seam). nil restores neutral.
func (self *networkStrategyScores) setPrior(prior func(networkId string, dialerKey string) (float32, bool)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.prior = prior
}

// The opaque vless configuration identity includes every dialing field while
// omitting its display name and unused crawl hint. Normalized defaults and user
// ids keep equivalent configurations stable. Only the hash is retained.
func vlessStrategyKey(config *VlessConfig) string {
	identity := config.Copy()
	identity.Name, identity.SpiderX = "", ""
	identity.Network, identity.Security = config.network(), config.security()
	identity.ServerName, identity.Host = config.serverName(), config.httpHost()
	identity.Path = vlessHttpRequestUrl(config).String()
	if id, err := vlessId(config.Id); err == nil {
		identity.Id = hex.EncodeToString(id[:])
	}
	if len(identity.Alpns) == 0 {
		identity.Alpns = nil
	}
	// VlessConfig contains only json-compatible scalar and slice fields.
	encoded, _ := json.Marshal(identity)
	sum := sha256.Sum256(encoded)
	return "vless|" + hex.EncodeToString(sum[:])
}

// The scoring identity separates endpoint configurations without changing the
// description before "|", which is the only part family telemetry publishes.
func (self *clientDialer) dialerKey() string {
	if self.strategyKey != "" {
		return self.strategyKey
	}
	if self.extenderConfig != nil {
		return self.description + "|" + self.extenderConfig.Ip.Unmap().String()
	}
	return self.description
}

// strategyDialerKey rebuilds the scoring key from the info a completed dial
// reports, so a delivery outcome lands on the same record the ranking reads.
func (self *DialerInfo) strategyDialerKey() string {
	if self.strategyKey != "" {
		return self.strategyKey
	}
	if self.ExtenderIp.IsValid() {
		return self.Description + "|" + self.ExtenderIp.Unmap().String()
	}
	return self.Description
}

// Finalizes a stamped attempt at most once, against its original network and
// strategy configuration. HTTP and H1/WS reads report automatically. Consumers
// of the raw WebSocket API can use its returned DialerInfo to report delivery;
// a manually constructed description cannot be attributed and is ignored.
func (self *ClientStrategy) RecordDeliveryOutcome(info *DialerInfo, bytesDelivered int64, stalled bool) {
	if self == nil || self.scores == nil || info == nil || info.delivery == nil || info.delivery.scores != self.scores {
		return
	}
	info.delivery.finish(classifyDelivery(bytesDelivered, stalled))
}

// Owns one attempt's immutable network/config identity and one delivery verdict.
// Reads and explicit outcome reports may run concurrently.
type strategyDeliveryAttempt struct {
	scores         *networkStrategyScores
	networkId      string
	dialerKey      string
	config         strategyScoreConfig
	stateLock      sync.Mutex
	bytesDelivered int64
	finished       bool
}

// Captures attribution before dialing, including for attempts that finish after
// a network transition. An unstamped DialerInfo is never evidence.
func (self *ClientStrategy) dialerInfo(dialer *clientDialer) *DialerInfo {
	info := dialer.Info()
	if info == nil || self.scores == nil {
		return info
	}
	config := self.scores.configuration()
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if !self.dialers[dialer] {
			// A selection retired before stamping owns no current attempt.
			return
		}
		info.delivery = &strategyDeliveryAttempt{
			scores:    self.scores,
			networkId: self.currentNetworkId,
			dialerKey: info.strategyDialerKey(),
			config:    config,
		}
	}()
	return info
}

// Serial preference uses the same decayed network evidence as weighted racing.
func (self *ClientStrategy) dialerDelivered(dialer *clientDialer) bool {
	if self.scores == nil {
		return dialer.IsLastSuccess()
	}
	info := self.dialerInfo(dialer)
	if info == nil || info.delivery == nil {
		return false
	}
	return 1 < self.scores.weight(info.delivery.networkId, info.delivery.dialerKey)
}

// Finalizes at most once, with the score store called outside the attempt lock.
func (self *strategyDeliveryAttempt) finish(verdict strategyVerdict) {
	finished := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.finished {
			return false
		}
		self.finished = true
		return true
	}()
	if finished {
		self.scores.recordVerdictForConfig(&self.config, self.networkId, self.dialerKey, verdict)
	}
}

// Only received payload credits delivery. Clean short responses are neutral;
// canceled race losers are neutral; read failures and deadlines are stalls.
func (self *DialerInfo) observeRead(ctx context.Context, count int, err error, finalEof bool) {
	if self == nil || self.delivery == nil {
		return
	}
	attempt := self.delivery
	bytesDelivered := func() int64 {
		attempt.stateLock.Lock()
		defer attempt.stateLock.Unlock()
		if attempt.finished {
			return -1
		}
		attempt.bytesDelivered += int64(count)
		return attempt.bytesDelivered
	}()
	if bytesDelivered < 0 {
		return
	}
	cleanEnd := errors.Is(err, io.EOF) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		attempt.finish(verdictHandshake)
	case err != nil && !cleanEnd:
		attempt.finish(verdictStalled)
	case deliveryVerifiedByteCount <= bytesDelivered:
		attempt.finish(verdictDelivered)
	case cleanEnd:
		if finalEof {
			attempt.finish(verdictHandshake)
		}
	}
}

// Optionally supplies stable host identifiers to revisit a network's evidence.
// Raw identifiers are hashed and never uploaded. Without this optional hint,
// the existing NetworkChanged platform signal supplies a fresh local generation.
// Existing attempts retain their original identity after either kind of change.
func (self *ClientStrategy) SetNetworkId(identifiers ...string) {
	networkId := deriveNetworkId(identifiers...)
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.currentNetworkId = networkId
}

// SetStrategyPrior installs the injected per-network prior the scoring blends
// in where it has no local evidence. The server-pushed per-country/asn prior is
// a later phase (it needs a server channel, not built here); this is the seam
// it plugs into.
func (self *ClientStrategy) SetStrategyPrior(prior func(networkId string, dialerKey string) (float32, bool)) {
	if self.scores == nil {
		return
	}
	self.scores.setPrior(prior)
}

// strategyConfigHashWithLock hashes the live dialer-set identity and the tls
// fingerprint kill switch, so any change to which dialers exist (vless added,
// extenders configured, the fingerprint flipped) yields a new hash and
// invalidates learned winners. The caller holds mutex.
func (self *ClientStrategy) strategyConfigHashWithLock() string {
	keys := make([]string, 0, len(self.dialers))
	for dialer := range self.dialers {
		keys = append(keys, dialer.dialerKey())
	}
	sort.Strings(keys)
	fingerprint := ""
	if self.settings != nil {
		fingerprint = self.settings.TlsClientHelloFingerprint
	}
	sum := sha256.Sum256([]byte(fingerprint + "\x00" + strings.Join(keys, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// invalidateScoresForConfigChangeWithLock recomputes the config hash and clears
// learned scores if it changed. The caller holds mutex. A no-op when scoring is
// not set up (a bare test-constructed strategy).
func (self *ClientStrategy) invalidateScoresForConfigChangeWithLock() {
	if self.scores == nil {
		return
	}
	self.scores.setConfigHash(self.strategyConfigHashWithLock())
}
