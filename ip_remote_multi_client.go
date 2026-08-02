package connect

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"

	// "reflect"
	"errors"
	"fmt"
	"math"
	"math/bits"
	mathrand "math/rand"
	"net"
	"net/netip"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
	"maps"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// multi client is a sender approach to mitigate bad destinations
// it maintains a window of compatible clients chosen using specs
// (e.g. from a desription of the intent of use)
// - the clients are rate limited by the number of outstanding acks (nacks)
// - the size of allowed outstanding nacks increases with each ack,
// scaling up successful destinations to use the full transfer buffer
// - the clients are chosen with probability weighted by their
// net frame count statistics (acks - nacks)

// the following functions handle moving clients in and out of the window:
// - `resize`
//   The goal of the resize is to meet a target window size based on the number
//   of different source ip:port per destination ip:port.
//   Two statistics are used: effective bytes per second ([ack used])
//     and expected bytes per second ([capacity]-[ack used]-[unacked used]).
//   Unhealthy clients are removed from the window based on low effective stats,
//   unless the window is fixed size.
//   Fundamentally this approach can't tell the difference between an
//   unhealthy client and an idle client, so the norm is to continually change clients
//   in the lull after a burst of usage.
// - `detectBlackhole`
//   When a client acks traffic but does not return traffic,
//   it gets labeled a black hole. Black hole clients may be malicious
//   or have network filtering. Black hole clients are removed.
// - `ping`
//   When a client is idle it must continually ack ping requests.
//   Clients that fail to ack are removed.

// TODO surface window stats to show to users

type clientReceivePacketFunction func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath IpPath, packet []byte)

type DestinationStats struct {
	EstimatedBytesPerSecond ByteCount
	Tier                    int
}

type WindowType int

func (self WindowType) RankMode() string {
	switch self {
	case WindowTypeQuality:
		return "quality"
	case WindowTypeSpeed:
		return "speed"
	default:
		return ""
	}
}

const (
	// WindowTypeAuto is the zero value: no fixed window type. A nil
	// performance profile and a profile with `WindowTypeAuto` mean the same
	// thing — traffic balances across the window types and each window uses
	// its own default size settings (the profile `WindowSize` is ignored).
	WindowTypeAuto    WindowType = 0
	WindowTypeQuality WindowType = 1
	WindowTypeSpeed   WindowType = 2
)

// for each `NewClientArgs`,
//
//	`RemoveClientWithArgs` will be called if a client was created for the args,
//	else `RemoveClientArgs`
type MultiClientGenerator interface {
	// path -> estimated byte count per second
	// the enumeration should typically
	// 1. not repeat final destination ids from any path
	// 2. not repeat intermediary elements from any path
	NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error)
	// client id, client auth
	NewClientArgs() (*MultiClientGeneratorClientArgs, error)
	RemoveClientArgs(args *MultiClientGeneratorClientArgs)
	RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs)
	NewClientSettings() *ClientSettings
	NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error)
	FixedDestinationSize() (int, bool)
}

// MultiClientGeneratorContext is an optional, maintenance-safe generator
// capability. The API generator implements it; legacy/custom generators keep
// the original interface. Calls must return when callCtx is done. NewClientContext
// deliberately separates the persistent client's ctx from its setup deadline:
// using a deadline as the client parent would kill a successfully-created client
// when that deadline later elapsed.
type MultiClientGeneratorContext interface {
	NextDestinationsContext(ctx context.Context, count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error)
	NewClientArgsContext(ctx context.Context) (*MultiClientGeneratorClientArgs, error)
	NewClientContext(
		ctx context.Context,
		callCtx context.Context,
		args *MultiClientGeneratorClientArgs,
		clientSettings *ClientSettings,
	) (*Client, error)
}

// MultiClientGeneratorTransportMigrator is an optional generator capability
// for make-before-break drain migration. Window clients own platform
// transports through their generator, so the generator is the only layer
// with enough information to construct a replacement using the same auth and
// route manager. Implementations must return promptly and deduplicate
// overlapping requests; the existing window/client lifecycle remains the
// fallback for generators that do not implement it.
type MultiClientGeneratorTransportMigrator interface {
	MigrateClientTransport(client *Client, args *MultiClientGeneratorClientArgs, migrateTime time.Time)
}

// the icmp send gate is not part of a normal handshake; it flips to a
// default-on release once the provider fleet broadly parses icmp (see ICMP.md)
var errIcmpDisabled = errors.New("icmp send is disabled")

func DefaultMultiClientSettings() *MultiClientSettings {
	return &MultiClientSettings{
		SequenceBufferSize:  defaultTransferBufferSize,
		SequenceIdleTimeout: 120 * time.Second,
		EnableIcmp:          false,

		WindowSizes: map[WindowType]WindowSizeSettings{
			// TODO increase `WindowSizeMinP2pOnly` when p2p is deployed
			WindowTypeQuality: WindowSizeSettings{
				WindowSizeMin:     2,
				WindowSizeMax:     6,
				WindowSizeHardMax: 12,
				// reconnects per source
				WindowSizeReconnectScale: 1.2,
				KeepHealthiestCount:      0,
			},
			WindowTypeSpeed: WindowSizeSettings{
				WindowSizeMin:     1,
				WindowSizeMax:     2,
				WindowSizeHardMax: 4,
				FixedWindowSize:   1,
				// WindowSizeUseMax:     1,
				// reconnects per source
				WindowSizeReconnectScale: 1.0,
				KeepHealthiestCount:      0,
			},
		},
		SendRetryTimeout: 2000 * time.Millisecond,
		// while the window has NO clients at all (formation, the first seconds after
		// connect), poll on this much shorter cadence: the first packets (the first
		// page's dns + syn) then leave moments after the first client lands instead
		// of up to SendRetryTimeout later. Once any client exists the normal
		// SendRetryTimeout applies (failed sends against live clients should not
		// hammer). See PACKETRESEARCH1 §12.
		FormationSendRetryTimeout:  200 * time.Millisecond,
		PingWriteTimeout:           5 * time.Second,
		CPingWriteTimeout:          15 * time.Second,
		CPingMaxByteCountPerSecond: kib(32),
		// the initial ping includes creating the transports and contract
		// ease up the timeout until perf issues are fully resolved
		PingTimeout:  30 * time.Second,
		CPingTimeout: 30 * time.Second,
		// the rest between continuous pings. decoupled from `CPingTimeout` (the
		// ack wait) so a dead idle client is detected within
		// ~CPingRestTimeout+CPingTimeout instead of ~2x CPingTimeout
		CPingRestTimeout: 10 * time.Second,
		// probe an actively-sending, ack-stale flow after 5s (well under
		// AckTimeout). A dead peer can be detected in roughly this window plus
		// the probe wait; a live peer's successful probe suppresses another
		// probe for a full stale window. A missed probe can still remove a live
		// peer, so the timeout needs lossy/high-rtt device validation — and on
		// a host that KNOWS it is slow (low power mode, thermal, constrained
		// network), DegradedMode scales these windows up instead of guessing.
		CPingBusyStaleTimeout: 5 * time.Second,
		// hosts toggle this on low power mode / thermal throttling / a
		// constrained network (see SetPerformanceDegraded); the probe timings
		// scale by the factor below so a slow-but-alive device is not
		// misdiagnosed as a dead peer
		DegradedMode:          &atomic.Bool{},
		DegradedLivenessScale: 3.0,
		// A timer that fires much later than armed indicates scheduler/process
		// suspension rather than a peer timeout. Give the same outstanding
		// probe a fresh budget and briefly suppress stale-stat blackhole
		// decisions after resume, preventing a screen-off/wake thundering herd.
		SchedulerPauseTolerance:       2 * time.Second,
		SchedulerPauseRecoveryTimeout: 5 * time.Second,
		// a lower ack timeout helps cycle through bad providers faster
		AckTimeout:              30 * time.Second,
		BlackholeTimeout:        5 * time.Second,
		BlackholeConnectTimeout: 30 * time.Second,
		// when sibling clients are passing traffic (comparative health), a
		// client whose first connect stays silent is cut at this shorter
		// timeout instead, freeing its window slot for a replacement
		BlackholeConnectComparativeTimeout:        10 * time.Second,
		WindowResizeTimeout:                       15 * time.Second,
		StatsWindowGraceperiod:                    30 * time.Second,
		StatsWindowMaxEstimatedByteCountPerSecond: mib(16),
		// StatsWindowMaxEffectiveByteCountPerSecondScale: 0.8,
		StatsWindowEntropy:  0.0,
		WindowExpandTimeout: 15 * time.Second,
		// WindowExpandBlockTimeout: 5 * time.Second,
		WindowExpandBlockCount: 4,
		// wait this time before enumerating potential clients again
		// WindowEnumerateEmptyTimeout: 5 * time.Second,
		WindowEnumerateErrorTimeout: 1 * time.Second,
		// Bound each production generator discovery/auth call independently of
		// the long-lived multi-client context. The default API request budget is
		// 15s; the extra margin keeps the maintenance bound authoritative while
		// still allowing the strategy's own retry/cleanup to finish.
		WindowGeneratorTimeout: 20 * time.Second,
		// Client setup includes transport formation and provide-secret
		// registration. It is separate from WindowExpandTimeout because setup
		// historically could already take the control timeout (30s), but it must
		// never own resize without a deadline.
		WindowClientCreateTimeout: 30 * time.Second,
		// WindowMaxScale:              4.0,
		// WindowExpandMaxOvershotScale: 2.0,
		// WindowRevisitTimeout:      2 * time.Minute,
		WindowExpandArgsTimeout:   2 * time.Minute,
		StatsWindowDuration:       30 * time.Second,
		StatsWindowBucketDuration: 1 * time.Second,
		StatsSampleWeightsCount:   8,
		// percentile
		StatsSourceCountSelection: 0.9,
		// ClientAffinityTimeout:        0 * time.Second,

		MultiRaceSetOnNoResponseTimeout:      5 * time.Second,
		MultiRaceSetOnResponseTimeout:        2 * time.Second,
		MultiRaceClientSentPacketMaxCount:    16,
		MultiRaceClientPacketMaxCount:        8,
		MultiRacePacketMaxCount:              32,
		MultiRaceClientEarlyCompleteFraction: 0.25,
		// TODO on platforms with more memory, increase this
		MultiRaceClientCount: 0,

		StatsWindowMaxUnhealthyDuration:  15 * time.Second,
		StatsWindowWarnUnhealthyDuration: 5 * time.Second,
		// how long a rank-kept client (FixedWindowSize/KeepHealthiestCount) may
		// remain continuously unhealthy before it is removed anyway. Keeping the
		// healthiest is meant to ride out transient badness, not to pin a dead
		// client (and its ui dot) in the window forever.
		StatsWindowKeepUnhealthyDuration: 60 * time.Second,
		// StatsWindowKeepHealthiestCount:   2,
		// the effective byte count is per stats window `StatsWindowDuration`
		// StatsWindowMinHealthyEffectiveSendByteCount:    kib(1),
		// StatsWindowMinHealthyEffectiveReceiveByteCount: kib(32),

		MaxClientLifetime: 60 * time.Minute,

		ProtocolVersion:     DefaultProtocolVersion,
		DestinationAffinity: true,

		DefaultReconnectScale: 1.0,
		DefaultUlimit:         0,

		TcpCollapsePrevention: true,
		UdpCollapsePrevention: false,

		SecurityPolicyGenerator: DefaultSecurityPolicyWithStats,

		// the epoch for flushing block action and packet stats events to listeners
		EventEpoch:                  1 * time.Second,
		BlockActionDecisionTtl:      30 * time.Second,
		BlockActionDecisionMaxCount: 4096,
		BlockActionAggMaxCount:      1024,
		// TCP resets synthesized while removing a dead client are best effort.
		// Deliver them off the sole resize goroutine through a small bounded
		// queue: a suspended packet-flow/TUN consumer must not stop replacement
		// peer discovery, and repeated removals must not grow memory without bound.
		RemovalReceiveQueueSize: 256,
		IpAssocSettings:         DefaultIpAssocSettings(),

		RemoteUserNatMultiClientMonitorSettings: *DefaultRemoteUserNatMultiClientMonitorSettings(),
	}
}

type MultiClientSettings struct {
	// Log, when set, is used by the multi client, its windows, channels, and
	// internal local user nat. nil resolves to `DefaultLogger()`.
	Log Logger

	SequenceBufferSize  int
	SequenceIdleTimeout time.Duration
	WindowSizes         map[WindowType]WindowSizeSettings
	// icmp echo egress. off by default until the provider fleet broadly
	// parses icmp: a not-yet-upgraded provider silently blackholes icmp
	// flows, and flow stickiness pins a ping run to its client (see ICMP.md)
	EnableIcmp bool
	// ClientNackInitialLimit int
	// ClientNackMaxLimit int
	// ClientNackScale float64
	// ClientWriteTimeout time.Duration
	// SendTimeout time.Duration
	// WriteTimeout time.Duration
	SendRetryTimeout time.Duration
	// FormationSendRetryTimeout is the send retry cadence while the window has
	// no clients at all (formation): a short poll so the first packets leave
	// moments after the first client lands. 0 disables (always SendRetryTimeout).
	FormationSendRetryTimeout  time.Duration
	PingWriteTimeout           time.Duration
	CPingWriteTimeout          time.Duration
	CPingMaxByteCountPerSecond ByteCount
	PingTimeout                time.Duration
	CPingTimeout               time.Duration
	CPingRestTimeout           time.Duration
	// CPingBusyStaleTimeout enables an active liveness probe on a BUSY flow
	// (one the idle-only continuous ping would skip): when the flow is
	// actively sending but has received no ack within this window, a ping is
	// sent to confirm the peer is alive. If the ping is acked the flow
	// continues; if it also times out the client is errored. This adds a
	// fast detection path for a mid-transfer dead peer without lowering
	// AckTimeout, but a lost/delayed probe can still cause a spurious removal.
	// 0 disables (the historical idle-only behavior). See PACKETRESEARCH1 §10.
	CPingBusyStaleTimeout time.Duration
	// DegradedMode, when its value is true, reports that the HOST is in a
	// degraded-performance state (low power mode, thermal throttling, a weak
	// or constrained network): the liveness probe timings are scaled by
	// DegradedLivenessScale so a device that answers control pings slowly is
	// not mistaken for a dead peer — a false removal (flow RSTs + reconnect
	// churn) costs far more than the extra detection latency. A live shared
	// value: the host toggles it via SetPerformanceDegraded as OS signals
	// change. nil = never degraded.
	DegradedMode *atomic.Bool
	// DegradedLivenessScale multiplies the busy-stale window, its probe
	// budgets, and the idle continuous-ping rest while DegradedMode is set
	// (values <= 1 mean no scaling).
	DegradedLivenessScale float64
	// SchedulerPauseTolerance is the excess timer delay treated as a host
	// pause/scheduler stall rather than peer failure. <= 0 disables detection.
	SchedulerPauseTolerance time.Duration
	// SchedulerPauseRecoveryTimeout suppresses stale-stat blackhole decisions
	// briefly after a detected pause so transport/network-change recovery can
	// produce fresh evidence. <= 0 means one poll only.
	SchedulerPauseRecoveryTimeout time.Duration
	AckTimeout                    time.Duration
	BlackholeTimeout              time.Duration
	BlackholeConnectTimeout       time.Duration
	// BlackholeConnectComparativeTimeout replaces BlackholeConnectTimeout for
	// a silent first connect while sibling window clients show recent receive
	// activity (the comparative health signal). 0 disables the shortening.
	BlackholeConnectComparativeTimeout        time.Duration
	WindowResizeTimeout                       time.Duration
	StatsWindowGraceperiod                    time.Duration
	StatsWindowMaxEstimatedByteCountPerSecond ByteCount
	// StatsWindowMaxEffectiveByteCountPerSecondScale float32
	StatsWindowEntropy  float32
	WindowExpandTimeout time.Duration
	// WindowExpandBlockTimeout     time.Duration
	WindowExpandBlockCount int
	// WindowEnumerateEmptyTimeout time.Duration
	WindowEnumerateErrorTimeout time.Duration
	// WindowGeneratorTimeout bounds context-aware destination discovery and
	// client-auth calls. Values <= 0 disable the extra maintenance deadline.
	WindowGeneratorTimeout time.Duration
	// WindowClientCreateTimeout bounds context-aware client setup without
	// imposing that deadline on the successfully-created client's lifetime.
	// Values <= 0 disable the extra maintenance deadline.
	WindowClientCreateTimeout time.Duration
	// WindowMaxScale              float64
	// WindowExpandMaxOvershotScale float64
	// WindowRevisitTimeout      time.Duration
	WindowExpandArgsTimeout   time.Duration
	StatsWindowDuration       time.Duration
	StatsWindowBucketDuration time.Duration
	StatsSampleWeightsCount   int
	// percentile
	StatsSourceCountSelection float64
	// lower affinity is more private
	// however, there may be some applications that assume the same ip across multiple connections
	// in those cases, we would need some small affinity
	// ClientAffinityTimeout time.Duration

	// time since first send to end the race, if no response
	MultiRaceSetOnNoResponseTimeout time.Duration
	// time after the first response to end the race
	MultiRaceSetOnResponseTimeout        time.Duration
	MultiRaceClientSentPacketMaxCount    int
	MultiRaceClientPacketMaxCount        int
	MultiRacePacketMaxCount              int
	MultiRaceClientEarlyCompleteFraction float32
	MultiRaceClientCount                 int

	StatsWindowMaxUnhealthyDuration  time.Duration
	StatsWindowWarnUnhealthyDuration time.Duration
	StatsWindowKeepUnhealthyDuration time.Duration
	// StatsWindowKeepHealthiestCount                 int
	// StatsWindowMinHealthyEffectiveSendByteCount    ByteCount
	// StatsWindowMinHealthyEffectiveReceiveByteCount ByteCount

	// active clients longer than this lifetime will not be forced closed
	// new connections will be routed to new clients
	MaxClientLifetime time.Duration

	ProtocolVersion int

	// note destination affinity will affect retry with different source ports
	// it relies on the performance of the initial race being good enough,
	// and all-or-nothing bad clients where if one destination does not route via a client,
	// all destinations should not route, so that the client can be detected as unhealthy
	DestinationAffinity bool

	DefaultPerformanceProfile *PerformanceProfile

	// OverrideAllowDirect, when set, hard-overrides direct mode
	// (`AllowDirect`) no matter what performance profile is set, in either
	// direction, superseding the same-network force. Cloud hosted clients
	// set false because a direct connection would leak that the client is
	// hosted and where it is hosted: the host addresses appear in the
	// direct connection setup. true forces direct mode on regardless of
	// the profile. When unset, a trusted same-network peer connection
	// (provide mode Network) forces direct mode on; otherwise the
	// profile's own `AllowDirect` applies.
	OverrideAllowDirect *bool

	// used when reconnect scale is not set in a custom performance profile
	DefaultReconnectScale float64
	// used when ulimit is not set in a custom performance profile
	DefaultUlimit int

	TcpCollapsePrevention bool
	UdpCollapsePrevention bool

	SecurityPolicyGenerator func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy

	// the epoch for flushing block action and packet stats events to listeners
	EventEpoch time.Duration
	// how long a cached block action decision stays valid while the overrides
	// and cluster versions are unchanged (server names for a destination can drift)
	BlockActionDecisionTtl      time.Duration
	BlockActionDecisionMaxCount int
	// max distinct block actions aggregated per epoch
	BlockActionAggMaxCount int
	// RemovalReceiveQueueSize bounds best-effort packets synthesized while a
	// client is removed (currently per-flow TCP resets). Delivery is isolated
	// from window maintenance because the downstream receiver may block while a
	// mobile app is suspended or a server TUN is backpressured. Values <= 0 use
	// the default.
	RemovalReceiveQueueSize int
	// nil disables activity association (`IpAssoc`)
	IpAssocSettings *IpAssocSettings

	RemoteUserNatMultiClientMonitorSettings
}

type WindowSizeSettings struct {
	WindowSizeMin int
	// the minimumum number of items in the windows that must be connected via p2p only
	WindowSizeMinP2pOnly int
	// inclusive
	WindowSizeMax     int
	WindowSizeHardMax int
	// leave 0 to automatically size between `WindowSizeMin` and `WindowSizeMax`
	FixedWindowSize int
	// WindowSizeUseMax     int
	// clients per source (leave 0 for default)
	WindowSizeReconnectScale float64
	KeepHealthiestCount      int
	// (leave 0 for default)
	Ulimit int
}

func (self *WindowSizeSettings) Validate() error {
	if self.WindowSizeMax < self.WindowSizeMin {
		return fmt.Errorf(
			"Window size [%d, %d] invalid. Max must be >= min",
			self.WindowSizeMin,
			self.WindowSizeMax,
		)
	}

	if 0 < self.FixedWindowSize {
		if self.FixedWindowSize < self.WindowSizeMin || self.WindowSizeMax < self.FixedWindowSize {
			return fmt.Errorf(
				"Window size [%d, %d] must include the fixed size =%d",
				self.WindowSizeMin,
				self.WindowSizeMax,
				self.FixedWindowSize,
			)
		}
	}

	return nil
}

// not setting a performance profile, or setting one with `WindowTypeAuto`,
// uses the default "auto" mode which balances traffic across multiple window
// types with an internal set of profiles. In auto mode `WindowSize` is
// ignored; the orthogonal settings (`AllowDirect`,
// `PostQuantumEncryption`) still apply.
type PerformanceProfile struct {
	WindowType  WindowType
	WindowSize  WindowSizeSettings
	AllowDirect bool
	// enable the per-peer e2e encryption sessions (post-quantum key
	// exchange) on the window clients. Opportunistic: a provider that does
	// not support the sessions falls back to plaintext at this layer.
	PostQuantumEncryption bool
}

// performanceProfilesEqual compares the behavior a profile installs, rather
// than its representation. nil and auto are the same mode when their
// orthogonal flags are false, and auto ignores WindowSize. This distinction
// matters because presentation code commonly reconstructs an equivalent
// value when it resumes; treating the fresh pointer as a settings change
// unnecessarily retires every healthy window client.
func performanceProfilesEqual(a *PerformanceProfile, b *PerformanceProfile) bool {
	allowDirect := func(profile *PerformanceProfile) bool {
		return profile != nil && profile.AllowDirect
	}
	postQuantumEncryption := func(profile *PerformanceProfile) bool {
		return profile != nil && profile.PostQuantumEncryption
	}
	if allowDirect(a) != allowDirect(b) ||
		postQuantumEncryption(a) != postQuantumEncryption(b) {
		return false
	}

	windowType := func(profile *PerformanceProfile) WindowType {
		if profile == nil {
			return WindowTypeAuto
		}
		return profile.WindowType
	}
	aWindowType := windowType(a)
	bWindowType := windowType(b)
	if aWindowType != bWindowType {
		return false
	}
	if aWindowType == WindowTypeAuto {
		return true
	}
	return a.WindowSize == b.WindowSize
}

// FixedWindow returns the fixed window type and size when the profile fixes
// one. ok is false when the profile is nil or auto — the equivalent cases
// where each window uses its own default size settings and the profile
// `WindowSize` is ignored.
func (self *PerformanceProfile) FixedWindow() (windowType WindowType, windowSize WindowSizeSettings, ok bool) {
	if self == nil || self.WindowType == WindowTypeAuto {
		return WindowTypeAuto, WindowSizeSettings{}, false
	}
	return self.WindowType, self.WindowSize, true
}

func (self *PerformanceProfile) Validate() error {
	err := self.WindowSize.Validate()
	if err != nil {
		return err
	}

	return nil
}

func DefaultWindowSizeSettings() WindowSizeSettings {
	return WindowSizeSettings{
		WindowSizeMin:            1,
		WindowSizeMax:            1,
		WindowSizeHardMax:        4,
		WindowSizeReconnectScale: 1.0,
		KeepHealthiestCount:      1,
	}
}

type receivePacket struct {
	Source      TransferPath
	ProvideMode protocol.ProvideMode
	IpPath      *IpPath
	Packet      []byte
	Pooled      bool
}

// removalResetCandidates is a min-heap by last activity. Client teardown can
// own thousands of flows but the reset delivery queue is intentionally fixed,
// so retain the queue-sized set most likely to still matter instead of a
// random map-order subset. The heap itself is bounded by that same queue.
type removalResetCandidates []*multiClientChannelUpdate

func (self removalResetCandidates) Len() int {
	return len(self)
}

func (self removalResetCandidates) Less(i int, j int) bool {
	return self[i].activityTime.Before(self[j].activityTime)
}

func (self removalResetCandidates) Swap(i int, j int) {
	self[i], self[j] = self[j], self[i]
}

func (self *removalResetCandidates) Push(value any) {
	*self = append(*self, value.(*multiClientChannelUpdate))
}

func (self *removalResetCandidates) Pop() any {
	values := *self
	last := len(values) - 1
	value := values[last]
	values[last] = nil
	*self = values[:last]
	return value
}

type RemoteUserNatMultiClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	generator MultiClientGenerator

	// Atomic so Close can sever the callback's ownership graph without
	// racing ingress. A callback already running at Close remains intentional
	// backpressure; later deliveries observe nil and stop retaining/calling
	// the retired owner (typically an UpgradeMux).
	receivePacketCallback atomic.Pointer[receivePacketCallbackHolder]
	// Best-effort removal-generated packets are delivered by one isolated
	// worker. A permanently blocked downstream therefore cannot wedge resize;
	// the fixed queue caps retained packets and memory.
	removalReceiveQueue     chan receivePacket
	removalReceiveDropCount atomic.Uint64
	// Malformed/unsupported packets and policy failures are untrusted,
	// potentially high-rate input. Keep separate lifetime counters and emit
	// only power-of-two summaries so the drop path has fixed memory and
	// bounded logging work while retaining first-error and total visibility.
	sendParseDropCount        atomic.Uint64
	sendPolicyDropCount       atomic.Uint64
	sendIcmpDisabledDropCount atomic.Uint64

	settings *MultiClientSettings
	log      Logger

	windows map[WindowType]*multiClientWindow
	monitor MultiClientMonitor

	securityPolicyStats *SecurityPolicyStatsCollector
	securityPolicy      SecurityPolicy

	// the provide mode of the source packets
	// for locally generated packets this is `ProvideMode_Network`
	provideMode protocol.ProvideMode

	stateLock        sync.Mutex
	ip4PathUpdates   map[Ip4Path]*multiClientChannelUpdate
	ip6PathUpdates   map[Ip6Path]*multiClientChannelUpdate
	affinityIp4Paths map[Ip4Path]map[Ip4Path]time.Time
	affinityIp6Paths map[Ip6Path]map[Ip6Path]time.Time
	clientUpdates    map[*multiClientChannel]map[*multiClientChannelUpdate]bool

	// config is an immutable snapshot of the rarely-changed routing config
	// (performance profile + local security bypass). it is rebuilt under
	// stateLock by the setters and read lock-free by selectWindowTypes, the
	// affinity selection, and the SendPacket drop path.
	config atomic.Pointer[multiClientConfig]

	localUserNat      *LocalUserNat
	localUserNatUnsub func()

	// nil when `IpAssocSettings` is not set
	ipAssoc *IpAssoc
	// immutable snapshot of the compiled overrides, swapped by `SetBlockActionOverrides`
	blockActionState     atomic.Pointer[blockActionState]
	blockActionCache     *blockActionCache
	blockActionCollector *blockActionCollector
	// immutable snapshot of the compiled ignore host values,
	// swapped by `SetBlockActionIgnoreHosts`
	blockActionIgnoreState atomic.Pointer[blockActionIgnoreState]
	blockActionIgnoreCache *blockActionIgnoreCache
	// unsubscribe from the current server-name-lookup's learned notifications
	// (guarded by stateLock); see SetServerNameLookup
	serverNamesLearnedUnsub func()
	packetStatsCounters     *packetStatsCounters
	packetStatsCallbacks    *CallbackList[PacketStatsFunction]
}

type receivePacketCallbackHolder struct {
	callback ReceivePacketFunction
}

// ServerNameLookup resolves a destination IP to the server name(s) previously observed
// for it — e.g. a DNS upgrade mux that recorded which hostnames resolved to the IP. The
// multi-client uses it for ServerName-based path affinity, so flows to the same site
// share a client channel even when the SNI is not visible on the wire (point 4).
type ServerNameLookup interface {
	ServerNames(ip string) []string
}

// ServerNamesLearnedFunction is called with the ips for which a new server name
// was just learned.
type ServerNamesLearnedFunction func(addrs []netip.Addr)

// ServerNamesLearnedNotifier is an optional capability of a ServerNameLookup: it
// notifies when a new server name is learned for an ip (e.g. an out-of-band DNS
// resolution after a flow already started). The multi-client uses it to
// invalidate that ip's cached block-action decision so subsequent block actions
// report the server name instead of the ip — we prefer the server name wherever
// possible. A lookup that doesn't implement it simply reconciles on the ttl.
type ServerNamesLearnedNotifier interface {
	AddServerNamesLearnedCallback(callback ServerNamesLearnedFunction) func()
}

type multiClientConfig struct {
	performanceProfile  *PerformanceProfile
	localSecurityBypass bool
	serverNameLookup    ServerNameLookup
	// the ad/tracker blocker consulted in the egress decision (nil = none)
	blocker Blocker
}

func NewRemoteUserNatMultiClientWithDefaults(
	ctx context.Context,
	generator MultiClientGenerator,
	receivePacketCallback ReceivePacketFunction,
	provideMode protocol.ProvideMode,
) *RemoteUserNatMultiClient {
	return NewRemoteUserNatMultiClient(
		ctx,
		generator,
		receivePacketCallback,
		provideMode,
		DefaultMultiClientSettings(),
	)
}

func NewRemoteUserNatMultiClient(
	ctx context.Context,
	generator MultiClientGenerator,
	receivePacketCallback ReceivePacketFunction,
	provideMode protocol.ProvideMode,
	settings *MultiClientSettings,
) *RemoteUserNatMultiClient {
	cancelCtx, cancel := context.WithCancel(ctx)

	log := loggerOrDefault(settings.Log)

	securityPolicyStats := DefaultSecurityPolicyStatsCollector()

	localUserNatSettings := DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNatSettings.Log = log
	localUserNat := NewLocalUserNat(cancelCtx, "multi local", localUserNatSettings)

	removalReceiveQueueSize := settings.RemovalReceiveQueueSize
	if removalReceiveQueueSize <= 0 {
		removalReceiveQueueSize = DefaultMultiClientSettings().RemovalReceiveQueueSize
	}

	multiClient := &RemoteUserNatMultiClient{
		ctx:                    cancelCtx,
		cancel:                 cancel,
		log:                    log,
		generator:              generator,
		removalReceiveQueue:    make(chan receivePacket, removalReceiveQueueSize),
		settings:               settings,
		windows:                map[WindowType]*multiClientWindow{},
		securityPolicyStats:    securityPolicyStats,
		securityPolicy:         settings.SecurityPolicyGenerator(cancelCtx, securityPolicyStats),
		provideMode:            provideMode,
		ip4PathUpdates:         map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:         map[Ip6Path]*multiClientChannelUpdate{},
		affinityIp4Paths:       map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths:       map[Ip6Path]map[Ip6Path]time.Time{},
		clientUpdates:          map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		localUserNat:           localUserNat,
		blockActionCache:       newBlockActionCache(settings.BlockActionDecisionTtl, settings.BlockActionDecisionMaxCount),
		blockActionCollector:   newBlockActionCollector(settings.BlockActionAggMaxCount, log),
		blockActionIgnoreCache: newBlockActionIgnoreCache(settings.BlockActionDecisionTtl, settings.BlockActionDecisionMaxCount),
		packetStatsCounters:    &packetStatsCounters{},
		packetStatsCallbacks:   NewCallbackList[PacketStatsFunction](),
	}
	multiClient.receivePacketCallback.Store(&receivePacketCallbackHolder{callback: receivePacketCallback})
	if settings.IpAssocSettings != nil {
		multiClient.ipAssoc = NewIpAssoc(cancelCtx, settings.IpAssocSettings)
	}
	initialPerformanceProfile := multiClient.overrideAllowDirect(settings.DefaultPerformanceProfile)
	if initialPerformanceProfile != nil {
		if err := initialPerformanceProfile.Validate(); err != nil {
			panic(err)
		}
	}
	multiClient.config.Store(&multiClientConfig{
		performanceProfile:  initialPerformanceProfile,
		localSecurityBypass: false,
		serverNameLookup:    nil,
		blocker:             nil,
	})
	multiClient.blockActionState.Store(&blockActionState{
		version: 0,
		matcher: nil,
	})
	multiClient.blockActionIgnoreState.Store(&blockActionIgnoreState{
		version: 0,
		matcher: nil,
	})

	multiClient.windows[WindowTypeQuality] = newMultiClientWindow(
		cancelCtx,
		cancel,
		generator,
		multiClient.clientReceivePacket,
		multiClient.removeClient,
		WindowTypeQuality,
		provideMode == protocol.ProvideMode_Network,
		settings,
		initialPerformanceProfile,
	)
	if _, fixed := generator.FixedDestinationSize(); !fixed {
		multiClient.windows[WindowTypeSpeed] = newMultiClientWindow(
			cancelCtx,
			cancel,
			generator,
			multiClient.clientReceivePacket,
			multiClient.removeClient,
			WindowTypeSpeed,
			provideMode == protocol.ProvideMode_Network,
			settings,
			initialPerformanceProfile,
		)
	}
	// else only keep the quality window for fixed destination

	multiClient.localUserNatUnsub = localUserNat.AddReceivePacketCallback(multiClient.localReceivePacket)

	monitors := []MultiClientMonitor{}
	for _, window := range multiClient.windows {
		monitors = append(monitors, window.monitor)
	}
	multiClient.monitor = NewMergedMultiClientMonitor(monitors)

	go HandleError(multiClient.runEventEpoch, cancel)
	go HandleError(multiClient.runRemovalReceive)

	return multiClient
}

// runRemovalReceive isolates best-effort packets produced by client teardown
// from the resize loop. Normal ingress keeps its direct low-latency path; only
// synthetic teardown traffic pays this queue hop.
func (self *RemoteUserNatMultiClient) runRemovalReceive() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case packet := <-self.removalReceiveQueue:
			if self.ctx.Err() != nil {
				return
			}
			HandleError(func() {
				self.deliverReceivePacket(
					packet.Source,
					packet.ProvideMode,
					packet.IpPath,
					packet.Packet,
				)
			})
		}
	}
}

func (self *RemoteUserNatMultiClient) deliverReceivePacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	if holder := self.receivePacketCallback.Load(); holder != nil {
		holder.callback(source, provideMode, ipPath, packet)
	}
}

func (self *RemoteUserNatMultiClient) enqueueRemovalReceive(packet *receivePacket) {
	if packet == nil {
		return
	}
	select {
	case <-self.ctx.Done():
	case self.removalReceiveQueue <- *packet:
	default:
		// A reset is advisory; bounded loss is preferable to either blocking
		// maintenance or retaining every dead flow while the receiver is stuck.
		// Log at powers of two so a persistent stall is visible without spam.
		self.addRemovalReceiveDrops(1)
	}
}

func (self *RemoteUserNatMultiClient) addRemovalReceiveDrops(count uint64) {
	if count == 0 {
		return
	}
	previous := self.removalReceiveDropCount.Add(count) - count
	current := previous + count
	// Log when this batch crosses a power-of-two threshold. This remains
	// sparse for a large flow-table teardown while preserving visibility.
	nextPower := uint64(1)
	if 0 < previous {
		nextPower = 1 << uint64(bits.Len64(previous))
	}
	if nextPower <= current {
		self.log.Infof("[multi]removal receive queue full; dropped %d best-effort packets\n", current)
	}
}

func (self *RemoteUserNatMultiClient) logSparseSendDrop(
	category string,
	counter *atomic.Uint64,
	err error,
) {
	count := counter.Add(1)
	if count == 0 || count&(count-1) != 0 {
		return
	}
	self.log.Infof(
		"[multi]send bad packet (%s; count=%d) = %s\n",
		category,
		count,
		err,
	)
}

// flushes block action and packet stats events to listeners on the event epoch
func (self *RemoteUserNatMultiClient) runEventEpoch() {
	defer self.cancel()

	lastPacketStats := PacketStats{}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.EventEpoch):
		}

		self.blockActionCollector.flush()

		if callbacks := self.packetStatsCallbacks.Get(); 0 < len(callbacks) {
			packetStats := self.packetStatsCounters.snapshot()
			if *packetStats != lastPacketStats {
				lastPacketStats = *packetStats
				for _, callback := range callbacks {
					HandleError(func() {
						callback(packetStats)
					})
				}
			}
		}
	}
}

// the local user nat receive callback. return traffic for locally routed flows
func (self *RemoteUserNatMultiClient) localReceivePacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	self.packetStatsCounters.localIngressPacketCount.Add(1)
	self.packetStatsCounters.localIngressByteCount.Add(int64(len(packet)))
	if self.ipAssoc != nil && !self.blockActionIgnored(ipPath) {
		// the local user nat delivers the flow's egress-oriented path
		// (the remote endpoint is the destination)
		self.ipAssoc.AddEgressPacket(ipPath)
	}
	self.deliverReceivePacket(source, provideMode, ipPath, packet)
}

func (self *RemoteUserNatMultiClient) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicyStats.Stats(reset)
}

func (self *RemoteUserNatMultiClient) Monitor() MultiClientMonitor {
	return self.monitor
}

func (self *RemoteUserNatMultiClient) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	subs := []func(){}
	for _, window := range self.windows {
		sub := window.AddContractStatusCallback(contractStatusCallback)
		subs = append(subs, sub)
	}
	return func() {
		for _, sub := range subs {
			sub()
		}
	}
}

// AddContractStatsCallback registers a listener for the epoch contract stats
// events of all the window clients (see `ContractManager.AddContractStatsCallback`)
func (self *RemoteUserNatMultiClient) AddContractStatsCallback(contractStatsCallback ContractStatsFunction) func() {
	subs := []func(){}
	for _, window := range self.windows {
		sub := window.AddContractStatsCallback(contractStatsCallback)
		subs = append(subs, sub)
	}
	return func() {
		for _, sub := range subs {
			sub()
		}
	}
}

// AddPeerIdentityChangeCallback registers a listener fired whenever any
// window client's established + identity-verified peer set may have changed
// (see `EncryptionSessionManager.AddPeerIdentityChangeCallback`). Consumers
// re-read `PeerIdentities`.
func (self *RemoteUserNatMultiClient) AddPeerIdentityChangeCallback(callback func()) func() {
	subs := []func(){}
	for _, window := range self.windows {
		sub := window.AddPeerIdentityChangeCallback(callback)
		subs = append(subs, sub)
	}
	return func() {
		for _, sub := range subs {
			sub()
		}
	}
}

// PeerIdentities returns the peers with an established, identity-verified
// e2e session across all window clients, deduplicated by peer id.
func (self *RemoteUserNatMultiClient) PeerIdentities() []*PeerIdentity {
	byPeer := map[Id]*PeerIdentity{}
	for _, window := range self.windows {
		for _, clientChannel := range window.unorderedClients() {
			for _, peerIdentity := range clientChannel.client.EncryptionSessionManager().PeerIdentities() {
				if _, ok := byPeer[peerIdentity.PeerId]; !ok {
					byPeer[peerIdentity.PeerId] = peerIdentity
				}
			}
		}
	}
	out := make([]*PeerIdentity, 0, len(byPeer))
	for _, peerIdentity := range byPeer {
		out = append(out, peerIdentity)
	}
	return out
}

// overrideAllowDirect applies the allow-direct override chain to the
// profile. A trusted same-network peer connection (provide mode Network)
// always enables direct mode, superseding the profile, so the connection can
// upgrade to a direct p2p stream. An explicit `settings.OverrideAllowDirect`
// then supersedes everything — false is the cloud-hosted hard limit (a
// direct connection would leak that the client is hosted and where), true
// forces direct mode on regardless of the profile.
func (self *RemoteUserNatMultiClient) overrideAllowDirect(performanceProfile *PerformanceProfile) *PerformanceProfile {
	if self.provideMode == protocol.ProvideMode_Network {
		performanceProfile = forceAllowDirect(performanceProfile, true)
	}
	if self.settings.OverrideAllowDirect != nil {
		performanceProfile = forceAllowDirect(performanceProfile, *self.settings.OverrideAllowDirect)
	}
	return performanceProfile
}

// forceAllowDirect returns a profile with `AllowDirect` forced to the value,
// fabricating a profile when forcing on with none set. The input profile is
// never mutated in place.
func forceAllowDirect(performanceProfile *PerformanceProfile, allowDirect bool) *PerformanceProfile {
	if performanceProfile == nil {
		if !allowDirect {
			// direct mode is already off with no profile
			return nil
		}
		return &PerformanceProfile{
			WindowType:  WindowTypeAuto,
			AllowDirect: true,
		}
	}
	// Always copy before publication. Profiles are read lock-free by window
	// workers, so retaining the caller's pointer would let a later caller-side
	// mutation race with those readers even when AllowDirect already matches.
	overridden := *performanceProfile
	overridden.AllowDirect = allowDirect
	return &overridden
}

func (self *RemoteUserNatMultiClient) SetPerformanceProfile(performanceProfile *PerformanceProfile) {
	performanceProfile = self.overrideAllowDirect(performanceProfile)
	if performanceProfile != nil {
		err := performanceProfile.Validate()
		if err != nil {
			panic(err)
		}
	}

	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// rebuild the immutable config snapshot under the lock so concurrent
		// setters do not lose each other's field
		prev := self.config.Load()
		if performanceProfilesEqual(prev.performanceProfile, performanceProfile) {
			return false
		}
		self.config.Store(&multiClientConfig{
			performanceProfile:  performanceProfile,
			localSecurityBypass: prev.localSecurityBypass,
			serverNameLookup:    prev.serverNameLookup,
			blocker:             prev.blocker,
		})
		return true
	}()
	if !changed {
		return
	}
	for _, window := range self.windows {
		window.SetPerformanceProfile(performanceProfile)
		// reset the window
		window.shuffle()
	}
}

func (self *RemoteUserNatMultiClient) SetLocalSecurityBypass(localSecurityBypass bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	prev := self.config.Load()
	self.config.Store(&multiClientConfig{
		performanceProfile:  prev.performanceProfile,
		localSecurityBypass: localSecurityBypass,
		serverNameLookup:    prev.serverNameLookup,
		blocker:             prev.blocker,
	})
}

// SetPerformanceDegraded reports the host's degraded-performance state (low
// power mode, thermal throttling, a weak or constrained network) to the
// window clients: while set, the busy-flow liveness probe timings and the
// idle continuous-ping rest scale by DegradedLivenessScale — a slow-but-alive
// device is not misdiagnosed as a dead peer, and an idle tunnel wakes the
// radio less often. Cheap and safe to call whenever the OS signals change.
func (self *RemoteUserNatMultiClient) SetPerformanceDegraded(degraded bool) {
	if degradedMode := self.settings.DegradedMode; degradedMode != nil {
		degradedMode.Store(degraded)
	}
}

// SetServerNameLookup installs (or clears, with nil) the ServerNameLookup used for
// ServerName-based path affinity. Safe to call at runtime.
func (self *RemoteUserNatMultiClient) SetServerNameLookup(serverNameLookup ServerNameLookup) {
	var prevUnsub func()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		prev := self.config.Load()
		self.config.Store(&multiClientConfig{
			performanceProfile:  prev.performanceProfile,
			localSecurityBypass: prev.localSecurityBypass,
			serverNameLookup:    serverNameLookup,
			blocker:             prev.blocker,
		})
		// re-subscribe to server-name-learned notifications: a name learned for an
		// ip invalidates that ip's cached block-action decisions so they re-resolve
		// with the server name. A lookup that doesn't notify just reconciles on the
		// ttl.
		prevUnsub = self.serverNamesLearnedUnsub
		self.serverNamesLearnedUnsub = nil
		if notifier, ok := serverNameLookup.(ServerNamesLearnedNotifier); ok {
			self.serverNamesLearnedUnsub = notifier.AddServerNamesLearnedCallback(self.invalidateServerNames)
		}
	}()
	if prevUnsub != nil {
		prevUnsub()
	}
	if self.ipAssoc != nil {
		self.ipAssoc.SetServerNameLookup(serverNameLookup)
	}
	// full reset on install: the new lookup may report different names. (Ongoing
	// single-name learns are handled incrementally by invalidateServerNames.)
	self.blockActionCache.clear()
	self.blockActionIgnoreCache.clear()
}

// invalidateServerNames drops the cached block-action decisions for the given
// ips, so the next flow to each rebuilds its decision and re-resolves the server
// name(s) — reporting the server name instead of the ip going forward. A cached
// decision unions server names over the destination's entire cluster, so the
// learned ip's cluster siblings are invalidated too: their cached decisions
// would otherwise keep reporting without the newly learned name until the ttl.
// Wired to the server-name lookup's learned notifications in SetServerNameLookup.
func (self *RemoteUserNatMultiClient) invalidateServerNames(addrs []netip.Addr) {
	for _, addr := range addrs {
		// the caches key on the unmapped addr (ipAssocAddr)
		key := addr.Unmap()
		self.blockActionCache.delete(key)
		self.blockActionIgnoreCache.delete(key)
		if self.ipAssoc != nil {
			for _, member := range self.ipAssoc.GetClusterAddrs(key) {
				memberKey := member.Unmap()
				if memberKey != key {
					self.blockActionCache.delete(memberKey)
					self.blockActionIgnoreCache.delete(memberKey)
				}
			}
		}
	}
}

// SetBlocker installs (or clears, with nil) the ad/tracker Blocker consulted
// in the egress decision: a destination is blocked when its ip falls in the
// blocker's ranges or any of its own observed server names is a blocked
// hostname. the check is destination scoped — deliberately not extended over
// the IpAssoc cluster, which would over-block co-clustered infrastructure.
// user overrides take precedence: an un-blocked blocker match egresses
// remotely as normal. enabling/disabling happens on the blocker itself and
// takes effect immediately (cached decisions revalidate against the enabled
// state). safe to call at runtime.
func (self *RemoteUserNatMultiClient) SetBlocker(blocker Blocker) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		prev := self.config.Load()
		self.config.Store(&multiClientConfig{
			performanceProfile:  prev.performanceProfile,
			localSecurityBypass: prev.localSecurityBypass,
			serverNameLookup:    prev.serverNameLookup,
			blocker:             blocker,
		})
	}()
	self.blockActionCache.clear()
}

func (self *RemoteUserNatMultiClient) LocalSecurityBypass() bool {
	return self.config.Load().localSecurityBypass
}

// ordered by choice descending
func (self *RemoteUserNatMultiClient) selectWindowTypes(sendPacket *parsedPacket) []WindowType {
	// - web traffic is routed to quality providers
	// - all other traffic is routed to speed providers

	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return []WindowType{WindowTypeQuality}
	}

	if windowType, _, ok := self.config.Load().performanceProfile.FixedWindow(); ok {
		return []WindowType{windowType}
	} else {
		if sendPacket.ipPath.DestinationPort == 443 {
			return []WindowType{WindowTypeQuality, WindowTypeSpeed}
		}
		return []WindowType{WindowTypeSpeed, WindowTypeQuality}
	}
}

// called with stateLock
func (self *RemoteUserNatMultiClient) affinityIpPathsWithLock(ipPath *IpPath) (affinityPaths []*IpPath) {
	config := self.config.Load()

	singleIp := false
	if _, windowSize, ok := config.performanceProfile.FixedWindow(); ok {
		singleIp = (windowSize.FixedWindowSize == 1)
	}

	if singleIp {
		singlePath := &IpPath{
			Version: ipPath.Version,
		}
		affinityPaths = append(affinityPaths, singlePath)
	} else {
		var serverNames []string
		// resolve the destination IP to the server name(s) observed for it (e.g. by a
		// DNS upgrade mux), giving ServerName path affinity without parsing the SNI off
		// the wire. affinity is by the base domain — a.foo.com, b.c.foo.com and foo.com
		// all collapse to foo.com — so a site's flows pin to one client channel.
		if config.serverNameLookup != nil && ipPath.DestinationIp != nil {
			serverNames = config.serverNameLookup.ServerNames(ipPath.DestinationIp.String())
		}

		if 0 < len(serverNames) {
			seen := map[string]bool{}
			for _, serverName := range serverNames {
				affinityName := serverName
				if rootDomain, err := publicsuffix.EffectiveTLDPlusOne(serverName); err == nil {
					affinityName = rootDomain
				}
				if seen[affinityName] {
					continue
				}
				seen[affinityName] = true
				affinityPaths = append(affinityPaths, &IpPath{
					ServerName: affinityName,
				})
			}
		} else if ipPath.Protocol == IpProtocolIcmp {
			// cycle per destination ip: pings to one host share a client with
			// each other, and with the host's other flows when the
			// destination resolves via the server name branch above
			destinationPath := &IpPath{
				Version:       ipPath.Version,
				DestinationIp: ipPath.DestinationIp,
			}
			affinityPaths = append(affinityPaths, destinationPath)
		} else if ipPath.DestinationPort == 80 || ipPath.DestinationPort == 53 || ipPath.DestinationPort == 443 {
			// for these ports, cycle the path per destination ip/port, regardless of protocol
			destinationPath := &IpPath{
				Version:         ipPath.Version,
				DestinationIp:   ipPath.DestinationIp,
				DestinationPort: ipPath.DestinationPort,
			}
			affinityPaths = append(affinityPaths, destinationPath)
		} else if ipPath.DestinationPort < 1024 {
			// for these ports, cycle the path per destination port, regardless of protocol or ip
			destinationPortPath := &IpPath{
				Version:         ipPath.Version,
				DestinationPort: ipPath.DestinationPort,
			}
			affinityPaths = append(affinityPaths, destinationPortPath)
		} else {
			// for user space ports, use a single path, regardless of protocol, ip, or port
			singlePath := &IpPath{
				Version: ipPath.Version,
			}
			affinityPaths = append(affinityPaths, singlePath)
		}
	}

	return
}

func (self *RemoteUserNatMultiClient) reconcileSendClientPath(
	update *multiClientChannelUpdate,
	previousClient *multiClientChannel,
) {
	// fast path: if the flow's client did not change during the callback, no
	// clientUpdates bookkeeping is needed, so skip the parent lock entirely
	// (client is atomic). this is the steady-state egress path.
	if previousClient == update.client.Load() {
		return
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// re-read under the lock (the lock-free check above can race a
		// concurrent client change)
		client := update.client.Load()

		if previousClient != client {
			if previousClient != nil {
				if updates, ok := self.clientUpdates[previousClient]; ok {
					delete(updates, update)
					if len(updates) == 0 {
						delete(self.clientUpdates, previousClient)
					}
				}
			}
			owned := false
			switch update.ipPath.Version {
			case 4:
				owned = self.ip4PathUpdates[update.ipPath.ToIp4Path()] == update
			case 6:
				owned = self.ip6PathUpdates[update.ipPath.ToIp6Path()] == update
			}
			if self.ctx.Err() != nil || update.ctx.Err() != nil || !owned {
				// A send that began before Close/idle retirement may finish
				// selecting its client afterward. Do not let that stale
				// completion republish the retired generation through
				// clientUpdates (or retain the closed channel from update).
				update.client.Store(nil)
			} else if client != nil && !client.IsDone() {
				updates, ok := self.clientUpdates[client]
				if !ok {
					updates = map[*multiClientChannelUpdate]bool{}
					self.clientUpdates[client] = updates
				}
				updates[update] = true
			}
		}
	}()
}

// waitForIdleUpdate blocks until the flow update has been idle for
// SequenceIdleTimeout, or the update ctx is done. it runs only inside the
// per-flow teardown goroutine; hoisted out of sendUpdate (rather than an inline
// closure) so the per-packet steady-state path does not allocate it.
func (self *RemoteUserNatMultiClient) waitForIdleUpdate(update *multiClientChannelUpdate) {
	for {
		select {
		case <-update.ctx.Done():
			return
		default:
		}

		var idleTimeout time.Duration
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			idleTimeout = update.activityTime.Add(self.settings.SequenceIdleTimeout).Sub(time.Now())
		}()
		if idleTimeout <= 0 {
			return
		} else {
			select {
			case <-update.ctx.Done():
				return
			case <-time.After(idleTimeout):
			}
		}
	}
}

// rstFlow sends a reset to both ends of a flow being torn down. like
// waitForIdleUpdate it runs only in the teardown goroutine and is a method, not
// an inline closure, to avoid a per-packet allocation in sendUpdate.
func (self *RemoteUserNatMultiClient) rstFlow(ipPath *IpPath, client *multiClientChannel) {
	if client != nil {
		// rst to destination
		if packet, ok := ipOosRst(ipPath); ok {
			client.Send(&parsedPacket{
				packet: packet,
				ipPath: ipPath,
			}, 0)
		}
	}
	// rst to source
	if packet, ok := ipOosRst(ipPath.Reverse()); ok {
		self.deliverReceivePacket(TransferPath{}, protocol.ProvideMode_Network, ipPath, packet)
	}
}

// returns the flow's update, the client it was previously associated with (for
// the caller's `clientUpdates` bookkeeping), and the current client to send to.
// the current client is read here under the parent lock that is already held,
// so the egress hot path does not reacquire the parent lock to read it.
func (self *RemoteUserNatMultiClient) sendUpdate(ipPath *IpPath) (
	*multiClientChannelUpdate,
	*multiClientChannel,
	*multiClientChannel,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// Close cancels before taking stateLock and clearing the flow maps. Check
	// cancellation while holding that same lock so an ingress/send racing
	// teardown cannot recreate a flow after Close has already cleared it.
	// Returning nil is also cheaper than parsing/routing work for packets a
	// retired provider can no longer deliver.
	if self.ctx.Err() != nil {
		return nil, nil, nil
	}

	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		var previousClient *multiClientChannel
		update, ok := self.ip4PathUpdates[ip4Path]
		if !ok || update.IsDone() {
			update = newMultiClientChannelUpdate(self.ctx, ipPath)
			go HandleError(func() {
				defer update.cancel()

				var client *multiClientChannel
				for {
					self.waitForIdleUpdate(update)

					success := func() bool {
						self.stateLock.Lock()
						defer self.stateLock.Unlock()

						updateDone := update.IsDone()
						if !updateDone {
							if t := update.activityTime.Add(self.settings.SequenceIdleTimeout).Sub(time.Now()); 0 < t {
								// updated since wait for idle
								return false
							}
						}

						client = update.client.Load()
						update.client.Store(nil)

						// A newer flow generation may already occupy this tuple
						// (for example after cancellation/restart). Retire only
						// the map membership still owned by this update; stale
						// teardown must never delete the replacement or its
						// affinity membership.
						if self.ip4PathUpdates[ip4Path] == update {
							delete(self.ip4PathUpdates, ip4Path)

							for affinityIp4Path := range update.affinityIp4Paths {
								if paths, ok := self.affinityIp4Paths[affinityIp4Path]; ok {
									delete(paths, ip4Path)
									if len(paths) == 0 {
										delete(self.affinityIp4Paths, affinityIp4Path)
									}
								}
							}
						}

						if client != nil {
							if updates, ok := self.clientUpdates[client]; ok {
								delete(updates, update)
								if len(updates) == 0 {
									delete(self.clientUpdates, client)
								}
							}
						}
						return true
					}()

					if success {
						break
					}
				}

				select {
				case <-self.ctx.Done():
				case <-update.ctx.Done():
				default:
					// The caller's path can borrow address bytes from a packet
					// buffer whose ownership was transferred after sendUpdate.
					// Teardown must use the flow's retained copy.
					self.rstFlow(update.ipPath, client)
				}
			}, update.cancel)
			self.ip4PathUpdates[ip4Path] = update

			var affinityIpPaths []*IpPath
			if self.settings.DestinationAffinity {
				affinityIpPaths = self.affinityIpPathsWithLock(ipPath)
			}

			for _, affinityIpPath := range affinityIpPaths {
				affinityIp4Path := affinityIpPath.ToIp4Path()
				update.affinityIp4Paths[affinityIp4Path] = true
				paths, ok := self.affinityIp4Paths[affinityIp4Path]
				if !ok {
					paths = map[Ip4Path]time.Time{}
					self.affinityIp4Paths[affinityIp4Path] = paths
				}
				paths[ip4Path] = time.Now()

				if update.client.Load() == nil {
					var mostRecentCreateTime time.Time
					for copyIp4Path, createTime := range paths {
						if copyUpdate, ok := self.ip4PathUpdates[copyIp4Path]; ok {
							if c := copyUpdate.client.Load(); c != nil && !c.IsDone() && !c.isWarning() && createTime.After(mostRecentCreateTime) {
								mostRecentCreateTime = createTime
								update.client.Store(c)
							}
						}
					}
				}
			}
		} else {
			previousClient = update.client.Load()
		}

		update.activityTime = time.Now()
		return update, previousClient, update.client.Load()
	case 6:
		ip6Path := ipPath.ToIp6Path()
		var previousClient *multiClientChannel
		update, ok := self.ip6PathUpdates[ip6Path]
		if !ok || update.IsDone() {
			update = newMultiClientChannelUpdate(self.ctx, ipPath)
			go HandleError(func() {
				defer update.cancel()

				var client *multiClientChannel
				for {
					self.waitForIdleUpdate(update)

					success := func() bool {
						self.stateLock.Lock()
						defer self.stateLock.Unlock()

						updateDone := update.IsDone()
						if !updateDone {
							if t := update.activityTime.Add(self.settings.SequenceIdleTimeout).Sub(time.Now()); 0 < t {
								// updated since wait for idle
								return false
							}
						}

						client = update.client.Load()
						update.client.Store(nil)

						// Mirror the IPv4 generation guard: an old cleanup may
						// release its own client bookkeeping, but it cannot
						// erase a newer flow with the same tuple.
						if self.ip6PathUpdates[ip6Path] == update {
							delete(self.ip6PathUpdates, ip6Path)

							for affinityIp6Path := range update.affinityIp6Paths {
								if paths, ok := self.affinityIp6Paths[affinityIp6Path]; ok {
									delete(paths, ip6Path)
									if len(paths) == 0 {
										delete(self.affinityIp6Paths, affinityIp6Path)
									}
								}
							}
						}

						if client != nil {
							if updates, ok := self.clientUpdates[client]; ok {
								delete(updates, update)
								if len(updates) == 0 {
									delete(self.clientUpdates, client)
								}
							}
						}
						return true
					}()

					if success {
						break
					}
				}

				select {
				case <-self.ctx.Done():
				case <-update.ctx.Done():
				default:
					// See the IPv4 path above: teardown outlives the borrowed
					// caller path and must use the retained flow key.
					self.rstFlow(update.ipPath, client)
				}
			}, update.cancel)
			self.ip6PathUpdates[ip6Path] = update

			var affinityIpPaths []*IpPath
			if self.settings.DestinationAffinity {
				affinityIpPaths = self.affinityIpPathsWithLock(ipPath)
			}

			for _, affinityIpPath := range affinityIpPaths {
				affinityIp6Path := affinityIpPath.ToIp6Path()
				update.affinityIp6Paths[affinityIp6Path] = true
				paths, ok := self.affinityIp6Paths[affinityIp6Path]
				if !ok {
					paths = map[Ip6Path]time.Time{}
					self.affinityIp6Paths[affinityIp6Path] = paths
				}
				paths[ip6Path] = time.Now()

				if update.client.Load() == nil {
					var mostRecentCreateTime time.Time
					for copyIp6Path, createTime := range paths {
						if copyUpdate, ok := self.ip6PathUpdates[copyIp6Path]; ok {
							if c := copyUpdate.client.Load(); c != nil && !c.IsDone() && !c.isWarning() && createTime.After(mostRecentCreateTime) {
								mostRecentCreateTime = createTime
								update.client.Store(c)
							}
						}
					}
				}
			}
		} else {
			previousClient = update.client.Load()
		}

		update.activityTime = time.Now()
		return update, previousClient, update.client.Load()
	default:
		panic(fmt.Errorf("Bad protocol version %d", ipPath.Version))
	}
}

func (self *RemoteUserNatMultiClient) receiveClientPath(ipPath *IpPath, callback func(*multiClientChannelUpdate)) bool {
	update := self.receiveUpdate(ipPath)
	if update == nil {
		return false
	}
	callback(update)
	return true
}

func (self *RemoteUserNatMultiClient) receiveUpdate(ipPath *IpPath) *multiClientChannelUpdate {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		update := self.ip4PathUpdates[ip4Path]
		if update != nil {
			update.activityTime = time.Now()
			return update
		}
	case 6:
		ip6Path := ipPath.ToIp6Path()
		update := self.ip6PathUpdates[ip6Path]
		if update != nil {
			update.activityTime = time.Now()
			return update
		}
	default:
		panic(fmt.Errorf("Bad protocol version %d", ipPath.Version))
	}

	return nil
}

/*
func (self *RemoteUserNatMultiClient) updateClient(update *multiClientChannelUpdate, previousClient *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	client := update.client

	if previousClient != client {
		if previousClient != nil {
			if updates, ok := self.clientUpdates[previousClient]; ok {
				delete(updates, update)
				if len(updates) == 0 {
					delete(self.clientUpdates, previousClient)
				}
			}
		}
		if client != nil && !client.IsDone() {
			updates, ok := self.clientUpdates[client]
			if !ok {
				updates = map[*multiClientChannelUpdate]bool{}
				self.clientUpdates[client] = updates
			}
			updates[update] = true
		}
	}
}
*/

// remove a client from all updates
func (self *RemoteUserNatMultiClient) removeClient(client *multiClientChannel) {
	// Retain only descriptors for packets that can fit right now. Clearing
	// every flow association is mandatory; reset delivery is advisory.
	// Selecting the most recently active flows makes the bounded work useful
	// for live browser connections instead of depending on random map order.
	// Packet construction happens after stateLock is released, avoiding both
	// lock-held allocation and one allocation per reset that will be dropped.
	resetBudget := cap(self.removalReceiveQueue) - len(self.removalReceiveQueue)
	resetCandidates := make(removalResetCandidates, 0, max(0, resetBudget))
	resetCandidateCount := 0

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// note client must be marked as done, otherwise it may be re-added by updates in flight
		if !client.IsDone() {
			self.log.Errorf("[multi]removed client that is not marked as done. This might lead to memory leak.")
		}

		if updates, ok := self.clientUpdates[client]; ok {
			delete(self.clientUpdates, client)
			for update, _ := range updates {
				if update.client.Load() == client {
					update.client.Store(nil)

					if update.ipPath.Protocol == IpProtocolTcp {
						resetCandidateCount += 1
						if 0 < resetBudget {
							if len(resetCandidates) < resetBudget {
								heap.Push(&resetCandidates, update)
							} else if resetCandidates[0].activityTime.Before(update.activityTime) {
								resetCandidates[0] = update
								heap.Fix(&resetCandidates, 0)
							}
						}
					}
				} else {
					self.log.Errorf("[multi]update associated with incorrect client")
				}
			}
		}
	}()
	self.addRemovalReceiveDrops(uint64(resetCandidateCount - len(resetCandidates)))

	// Deliver newest first. Sorting is bounded by RemovalReceiveQueueSize and
	// runs outside the shared flow lock.
	slices.SortFunc(resetCandidates, func(a *multiClientChannelUpdate, b *multiClientChannelUpdate) int {
		return b.activityTime.Compare(a.activityTime)
	})

	select {
	case <-self.ctx.Done():
	default:
		for _, update := range resetCandidates {
			if packet, ok := ipOosRst(update.ipPath.Reverse()); ok {
				self.enqueueRemovalReceive(&receivePacket{
					Source:      TransferPath{},
					ProvideMode: protocol.ProvideMode_Network,
					IpPath:      update.ipPath,
					Packet:      packet,
				})
			}
		}
	}
}

// orderClientsSuspectLast stably moves suspect clients (busy-stale, liveness
// probe outstanding — see multiClientChannel.IsSuspect) behind the healthy
// ones, so a flow racing the head of the order never lands on a client that
// is likely about to be errored — while a window that is entirely suspect
// still routes (suspects beat nothing).
func orderClientsSuspectLast(clients []*multiClientChannel) []*multiClientChannel {
	suspectCount := 0
	for _, client := range clients {
		if client.IsSuspect() {
			suspectCount += 1
		}
	}
	if suspectCount == 0 || suspectCount == len(clients) {
		return clients
	}
	ordered := make([]*multiClientChannel, 0, len(clients))
	for _, client := range clients {
		if !client.IsSuspect() {
			ordered = append(ordered, client)
		}
	}
	for _, client := range clients {
		if client.IsSuspect() {
			ordered = append(ordered, client)
		}
	}
	return ordered
}

// `SendPacketFunction`
func (self *RemoteUserNatMultiClient) SendPacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	timeout time.Duration,
) bool {
	if self.ctx.Err() != nil {
		return false
	}

	relationship := egressRelationship(provideMode, self.provideMode)

	// The packet remains owned by this call until the asynchronous send accepts
	// it, so policy, association, and routing may borrow its address slices.
	// sendUpdate copies the path only when it creates a long-lived flow record;
	// copying both addresses on every packet made the steady-state path allocate
	// even though existing flow records never retain this packet's IpPath.
	var ipPathValue IpPath
	ipPath := &ipPathValue
	payload, err := parseIpPathWithPayloadBorrowed(packet, ipPath)
	if err != nil {
		self.logSparseSendDrop("parse", &self.sendParseDropCount, err)
		return false
	}
	if ipPath.Protocol == IpProtocolIcmp && !self.settings.EnableIcmp {
		// the client send gate (see ICMP.md): default off until the provider
		// fleet broadly parses icmp, since a not-yet-upgraded provider
		// silently blackholes icmp flows
		self.logSparseSendDrop("icmp disabled", &self.sendIcmpDisabledDropCount, errIcmpDisabled)
		return false
	}
	r, err := inspectAndRefreshEgressBorrowed(self.securityPolicy, relationship, ipPathValue, payload)
	if err != nil {
		self.logSparseSendDrop("policy", &self.sendPolicyDropCount, err)
		return false
	}

	// infrastructure destinations (the resolver endpoints) are excluded
	// from the association and override logic
	ignored := self.blockActionIgnored(ipPath)

	if !ignored && self.ipAssoc != nil {
		self.ipAssoc.AddEgressPacket(ipPath)
	}

	if r != SecurityPolicyResultAllow && r != SecurityPolicyResultDrop {
		// incident (martian/malformed). always blocked, not overridable
		if self.log.V(1).Enabled() {
			self.log.Infof("[multi]drop packet ipv%d p%v -> %s:%d\n", ipPath.Version, ipPath.Protocol, ipPath.DestinationIp, ipPath.DestinationPort)
		}
	}

	// the overrides take precedence over the default decisions (the security
	// result and the ad/tracker blocker)
	blockActionState := self.blockActionState.Load()
	config := self.config.Load()
	blockerActive := config.blocker != nil && config.blocker.Enabled()
	var decision *blockActionDecision
	if !ignored && (blockActionState.matcher != nil || self.blockActionCollector.hasCallbacks() || blockerActive) {
		decision = self.blockActionDecision(blockActionState, config.blocker, blockerActive, ipPath)
	}
	var match *blockActionMatch
	blockerBlock := false
	if decision != nil {
		match = decision.match
		blockerBlock = decision.blockerBlock
	}
	block, local := blockActionApply(r, config.localSecurityBypass, blockerBlock, match)

	byteCount := ByteCount(len(packet))
	if decision != nil && self.blockActionCollector.hasCallbacks() {
		self.blockActionCollector.add(decision, block, local, match, byteCount)
	}

	if block {
		self.packetStatsCounters.blockEgressPacketCount.Add(1)
		self.packetStatsCounters.blockEgressByteCount.Add(int64(byteCount))
		return false
	}
	if local {
		success := self.localUserNat.SendPacket(source, provideMode, packet, 0)
		if success {
			self.packetStatsCounters.localEgressPacketCount.Add(1)
			self.packetStatsCounters.localEgressByteCount.Add(int64(byteCount))
		}
		return success
	}
	success := self.sendPacket(source, provideMode, packet, ipPath, len(payload), timeout)
	if success {
		self.packetStatsCounters.remoteEgressPacketCount.Add(1)
		self.packetStatsCounters.remoteEgressByteCount.Add(int64(byteCount))
	}
	return success
}

// the cached override match, blocker match, and server names for a
// destination. the external lookups (server names, cluster) run outside the
// cache lock
func (self *RemoteUserNatMultiClient) blockActionDecision(state *blockActionState, blocker Blocker, blockerActive bool, ipPath *IpPath) *blockActionDecision {
	addr, ok := ipAssocAddr(ipPath.DestinationIp)
	if !ok {
		return nil
	}

	var clusterVersion uint64
	if self.ipAssoc != nil {
		clusterVersion = self.ipAssoc.ClusterVersion()
	}
	now := time.Now()

	if decision := self.blockActionCache.get(addr, state.version, clusterVersion, blockerActive, now); decision != nil {
		return decision
	}

	// decisions are made on the cluster level.
	// a destination with no cluster is a cluster of itself
	clusterIps := []netip.Addr{addr}
	if self.ipAssoc != nil {
		if members := self.ipAssoc.GetClusterAddrs(addr); 0 < len(members) {
			clusterIps = members
		}
	}
	slices.SortFunc(clusterIps, func(a netip.Addr, b netip.Addr) int {
		return a.Compare(b)
	})

	decision := &blockActionDecision{
		overridesVersion: state.version,
		clusterVersion:   clusterVersion,
		blockerEnabled:   blockerActive,
		expireTime:       now.Add(self.blockActionCache.ttl),
		clusterKey:       clusterIps[0],
		clusterIps:       clusterIps,
	}
	var match *blockActionMatch
	if state.matcher != nil {
		match = &blockActionMatch{}
	}
	destSeen := false
	seenHosts := map[string]bool{}
	for _, member := range clusterIps {
		memberServerNames := self.serverNames(member)
		for _, serverName := range memberServerNames {
			if !seenHosts[serverName] {
				seenHosts[serverName] = true
				decision.clusterHosts = append(decision.clusterHosts, serverName)
			}
		}
		if match != nil {
			// the match extends over the destination's entire cluster
			state.matcher.matchAddr(match, member, memberServerNames)
		}
		if blockerActive && member == addr {
			// the blocker matches the destination itself: its ip and its own
			// observed server names, never the cluster's (over-blocking
			// co-clustered infrastructure would break sites)
			destSeen = true
			decision.blockerBlock = blockerCheck(blocker, addr, memberServerNames)
		}
	}
	if blockerActive && !destSeen {
		decision.blockerBlock = blockerCheck(blocker, addr, self.serverNames(addr))
	}
	if match != nil && match.any() {
		decision.match = match
	}
	self.blockActionCache.put(addr, decision)
	return decision
}

// blockerCheck is the destination-scoped blocker match: the destination ip
// against the blocked ranges, and each of the destination's own observed
// server names against the blocked host set.
func blockerCheck(blocker Blocker, addr netip.Addr, serverNames []string) bool {
	if blocker.BlockIp(addr) {
		return true
	}
	for _, serverName := range serverNames {
		if blocker.BlockHost(serverName) {
			return true
		}
	}
	return false
}

func (self *RemoteUserNatMultiClient) serverNames(addr netip.Addr) []string {
	config := self.config.Load()
	if config.serverNameLookup == nil {
		return nil
	}
	return config.serverNameLookup.ServerNames(addr.String())
}

// SetBlockActionOverrides replaces the override rules.
// overrides take precedence over the default security and local routing decisions.
// safe to call at runtime
func (self *RemoteUserNatMultiClient) SetBlockActionOverrides(overrides []*BlockActionOverride) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		prev := self.blockActionState.Load()
		self.blockActionState.Store(&blockActionState{
			version: prev.version + 1,
			matcher: newBlockActionMatcher(overrides),
		})
	}()
	self.blockActionCache.clear()
}

// SetBlockActionIgnoreHosts replaces the host values (hostnames and ips, in
// the same forms as `BlockActionOverride.Hosts`) excluded from the override
// and association logic. used for infrastructure destinations like the dns
// resolver endpoints, which must never be captured by user override rules or
// cluster with user traffic. the default security and routing decisions and
// the packet stats still apply to ignored destinations, but no block actions
// are surfaced for them. safe to call at runtime.
// the hard coded remote doh resolver ips are always excluded in addition to
// these host values (see `defaultRemoteDohIgnoreAddrs`).
func (self *RemoteUserNatMultiClient) SetBlockActionIgnoreHosts(hostValues []string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		prev := self.blockActionIgnoreState.Load()
		var matcher *blockActionMatcher
		if 0 < len(hostValues) {
			matcher = newBlockActionMatcher([]*BlockActionOverride{{
				Hosts:         hostValues,
				BlockOverride: &BlockOverride{},
			}})
		}
		self.blockActionIgnoreState.Store(&blockActionIgnoreState{
			version: prev.version + 1,
			matcher: matcher,
		})
	}()
	self.blockActionIgnoreCache.clear()
}

// blockActionIgnored returns whether the destination is excluded from the
// override and association logic. the result is cached per destination.
// the hard coded remote doh resolver ips are always excluded (see
// `defaultRemoteDohIgnoreAddrs`), independent of `SetBlockActionIgnoreHosts`.
func (self *RemoteUserNatMultiClient) blockActionIgnored(ipPath *IpPath) bool {
	addr, ok := ipAssocAddr(ipPath.DestinationIp)
	if !ok {
		return false
	}
	if defaultRemoteDohIgnoreAddrs()[addr] {
		return true
	}
	state := self.blockActionIgnoreState.Load()
	if state.matcher == nil {
		return false
	}
	now := time.Now()
	if entry := self.blockActionIgnoreCache.get(addr, state.version, now); entry != nil {
		return entry.ignored
	}
	match := &blockActionMatch{}
	state.matcher.matchAddr(match, addr, self.serverNames(addr))
	entry := &blockActionIgnoreEntry{
		ignored:    match.any(),
		version:    state.version,
		expireTime: now.Add(self.blockActionIgnoreCache.ttl),
	}
	self.blockActionIgnoreCache.put(addr, entry)
	return entry.ignored
}

// AddBlockActionCallback registers a listener for the epoch block action events.
// all egress routing decisions are surfaced, deduplicated per destination per epoch
func (self *RemoteUserNatMultiClient) AddBlockActionCallback(blockActionCallback BlockActionFunction) func() {
	return self.blockActionCollector.addCallback(blockActionCallback)
}

// PacketStats returns the cumulative packet counts by route
func (self *RemoteUserNatMultiClient) PacketStats() *PacketStats {
	return self.packetStatsCounters.snapshot()
}

// AddPacketStatsCallback registers a listener fired on the event epoch when the
// packet stats change
func (self *RemoteUserNatMultiClient) AddPacketStatsCallback(packetStatsCallback PacketStatsFunction) func() {
	callbackId := self.packetStatsCallbacks.Add(packetStatsCallback)
	return func() {
		self.packetStatsCallbacks.Remove(callbackId)
	}
}

func (self *RemoteUserNatMultiClient) canSendPacket(ipPath *IpPath, payloadByteCount int, update *multiClientChannelUpdate) (allow bool) {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		if self.settings.TcpCollapsePrevention {
			// limit sender tcp collapse
			// as soon as a packet is sent to a client, either the client will eith reliabily transfer the packet,
			// or the client will be dropped
			// retransmits don't need to be sent as soon as the packet is committed to a client
			if ipPath.Syn || ipPath.Rst {
				allow = true
			} else if update.canUpdateSequence(ipPath, payloadByteCount) {
				// sequence state is guarded by the per-flow `stateLock`, not
				// the parent `stateLock`
				allow = true
			}
		} else {
			allow = true
		}
	default:
		allow = true
	}
	return
}

func (self *RemoteUserNatMultiClient) sendPacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	ipPath *IpPath,
	payloadByteCount int,
	timeout time.Duration,
) (success bool) {
	update, previousClient, currentClient := self.sendUpdate(ipPath)
	if update == nil {
		return false
	}
	defer self.reconcileSendClientPath(update, previousClient)

	if !self.canSendPacket(ipPath, payloadByteCount, update) {
		return false
	}

	if ipPath.Syn || ipPath.Rst {
		update.resetSequence(ipPath)
	}

	if currentClient != nil {
		var err error
		success, err = currentClient.sendPacketDetailed(packet, ipPath, timeout)
		if success {
			update.updateSequence(ipPath, payloadByteCount)
		} else if err != nil {
			self.log.Infof("[multi]reset error = %s\n", err)
			update.client.Store(nil)
			if rstPacket, ok := ipOosRst(update.ipPath.Reverse()); ok {
				select {
				case <-self.ctx.Done():
				default:
					self.deliverReceivePacket(
						TransferPath{},
						protocol.ProvideMode_Network,
						update.ipPath,
						rstPacket,
					)
				}
			}
		}
		// A nil error with false success is intentional route backpressure. Keep
		// the committed client until it reports a terminal error.
		return success
	}

	// Only first-flow/reroute work needs descriptors captured by peer-race
	// goroutines. Construct them here so the committed-flow fast path keeps its
	// parsed path and descriptor on the caller's stack.
	raceIpPath := *ipPath
	sendPacket := &parsedPacket{
		packet: packet,
		ipPath: &raceIpPath,
	}
	return self.sendPacketUncommitted(source, provideMode, sendPacket, update, timeout)
}

func (self *RemoteUserNatMultiClient) sendPacketUncommitted(
	source TransferPath,
	provideMode protocol.ProvideMode,
	sendPacket *parsedPacket,
	update *multiClientChannelUpdate,
	timeout time.Duration,
) (success bool) {
	enterTime := time.Now()

	// find a new client
	// the race is between as many clients as can send in parallel

	// if _, fixed := self.generator.FixedDestinationSize(); fixed {
	// 	window := self.windows[WindowTypeQuality]
	// 	orderedClients := window.OrderedClients()

	// 	for _, client := range orderedClients {
	// 		if client.Send(sendPacket, timeout) {
	// 			success = true

	// 			func() {
	// 				self.stateLock.Lock()
	// 				defer self.stateLock.Unlock()

	// 				update.client = client
	// 			}()
	// 		}
	// 	}

	// 	return
	// }

	raceClients := func(orderedClients []*multiClientChannel, sendTimeout time.Duration) {
		switch len(orderedClients) {
		case 0:
			return
		case 1:
			// send to one client, no race
			client := orderedClients[0]
			if client.Send(sendPacket, sendTimeout) {
				success = true

				// Serialize the commit with update.Close. Close cancels before
				// taking stateLock, so a send that completes after retirement
				// cannot restore a client pointer into the closed generation.
				update.stateLock.Lock()
				if update.ctx.Err() == nil {
					update.client.Store(client)
				}
				update.stateLock.Unlock()
			}
			return

		default:

			defer func() {
				if success {
					MessagePoolReturn(sendPacket.packet)
				}
			}()

			var successCount atomic.Int32

			send := func(client *multiClientChannel) {
				select {
				case <-update.ctx.Done():
					return
				default:
				}

				if update.client.Load() != nil {
					// another client already chosen, done
					return
				}

				sent := sendMultiClientRaceAttempt(
					client,
					sendPacket.packet,
					update.ipPath,
					sendTimeout,
				)
				if sent {
					successCount.Add(1)

					var initRace *multiClientChannelUpdateRace
					var initRaceEarlyComplete <-chan struct{}
					var abandonedClients []*multiClientChannel
					func() {
						// race state is guarded by the per-flow stateLock (a
						// leaf); client is atomic
						update.stateLock.Lock()
						defer update.stateLock.Unlock()

						if update.client.Load() != nil {
							// another client already chosen, done
							return
						}

						race := update.race
						if race == nil {
							update.initRaceWithLock()
							race = update.race

							initRace = race
							initRaceEarlyComplete = race.completeMonitor.NotifyChannel()
						}
						state := race.clientStates[client]
						if state == nil {
							state = &multiClientChannelRaceClientState{
								sendTime: time.Now(),
							}
							race.clientStates[client] = state
						}
						state.sentPacketCount += 1
						race.sentPacketCount += 1
						bufferExceeded := state != nil && self.settings.MultiRaceSetOnNoResponseTimeout <= time.Now().Sub(state.sendTime) || self.settings.MultiRaceClientSentPacketMaxCount < state.sentPacketCount
						if race.packetCount == 0 && bufferExceeded {
							// no client response in timeout, lock in this client
							// this happens for example when the client only sends and does not receive (e.g. udp send)

							for abandonedClient, _ := range race.clientStates {
								if abandonedClient != client {
									abandonedClients = append(abandonedClients, abandonedClient)
								}
							}

							update.clearRaceWithLock()
							update.client.Store(client)
						}
					}()

					if initRace != nil {
						self.scheduleCompleteRace(update.ipPath, initRace, initRaceEarlyComplete)
					}

					if 0 < len(abandonedClients) {
						if rstPacket, ok := ipOosRst(update.ipPath); ok {
							for _, abandonedClient := range abandonedClients {
								abandonedClient.Send(&parsedPacket{
									packet: rstPacket,
									ipPath: update.ipPath,
								}, 0)
							}
						}
					}
				}
			}

			var raceOrderedClients []*multiClientChannel
			if 0 < self.settings.MultiRaceClientCount && self.settings.MultiRaceClientCount < len(orderedClients) {
				raceOrderedClients = orderedClients[:self.settings.MultiRaceClientCount]
			} else {
				raceOrderedClients = orderedClients
			}

			// if 0 < timeout {
			var wg sync.WaitGroup

			for _, client := range raceOrderedClients {
				wg.Add(1)
				go HandleError(func() {
					defer wg.Done()

					send(client)
				})
			}

			wg.Wait()
			// } else {
			// 	for _, client := range raceOrderedClients {
			// 		send(client)
			// 	}
			// }

			if 0 < successCount.Load() {
				success = true
			}
			return
		}
	}

	coalesceOrderedClients := func() []*multiClientChannel {
		for _, windowType := range self.selectWindowTypes(sendPacket) {
			if window, ok := self.windows[windowType]; ok {
				orderedClients := window.OrderedClients()
				if 0 < len(orderedClients) {
					// route this (new or re-routing) flow away from suspect
					// clients while any healthy one exists
					return orderClientsSuspectLast(orderedClients)
				}
			}
		}
		return []*multiClientChannel{}
	}

	raceClients(coalesceOrderedClients(), 0)
	if success {
		return
	}

	for {
		var retryTimeout time.Duration
		if 0 <= timeout {
			remainingTimeout := enterTime.Add(timeout).Sub(time.Now())

			if remainingTimeout <= 0 {
				// drop
				return
			}

			retryTimeout = min(remainingTimeout, self.settings.SendRetryTimeout)
		} else {
			retryTimeout = self.settings.SendRetryTimeout
		}

		startTime := time.Now()
		orderedClients := coalesceOrderedClients()
		raceClients(orderedClients, retryTimeout)
		if success {
			return
		}
		endTime := time.Now()
		retryTimeout -= endTime.Sub(startTime)

		if len(orderedClients) == 0 && 0 < self.settings.FormationSendRetryTimeout {
			// window formation: there is nothing to send to yet. Poll on the short
			// formation cadence so this packet leaves moments after the first client
			// lands instead of up to SendRetryTimeout later (the overall send timeout
			// still bounds the loop). With clients present a failed send keeps the
			// normal cadence — live clients should not be hammered.
			retryTimeout = min(retryTimeout, self.settings.FormationSendRetryTimeout)
		}
		if 0 < retryTimeout {
			select {
			case <-update.ctx.Done():
				return
			case <-time.After(retryTimeout):
			}
		}
	}
}

// sendMultiClientRaceAttempt lends one read-only packet reference to a race
// candidate. A successful send consumes that reference; a rejected send
// consumes nothing, so this function returns the share exactly once.
//
// Keep this ownership boundary in one function. Returning a rejected share in
// both the immediate failure branch and the caller's else branch used to
// release the original owner too. When a sibling candidate succeeded, its
// asynchronous SendSequence later returned the same buffer and exposed the
// premature release as "[mp]return message not taken".
func sendMultiClientRaceAttempt(
	client *multiClientChannel,
	packet []byte,
	ipPath *IpPath,
	timeout time.Duration,
) bool {
	sharedPacket := &parsedPacket{
		packet: MessagePoolShareReadOnly(packet),
		ipPath: ipPath,
	}
	if client.SendWithAck(sharedPacket, timeout, true) {
		return true
	}
	MessagePoolReturn(sharedPacket.packet)
	return false
}

// clientReceivePacketFunction
func (self *RemoteUserNatMultiClient) clientReceivePacket(
	sourceClient *multiClientChannel,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath IpPath,
	packet []byte,
) {
	r, err := inspectAndRefreshIngressBorrowed(self.securityPolicy, provideMode, ipPath, nil)
	if err != nil {
		return
	}
	if r != SecurityPolicyResultAllow {
		return
	}

	self.packetStatsCounters.remoteIngressPacketCount.Add(1)
	self.packetStatsCounters.remoteIngressByteCount.Add(int64(len(packet)))
	if self.ipAssoc != nil {
		// before reverse, the remote endpoint is the source
		self.ipAssoc.AddIngressPacket(&ipPath)
	}

	ipPath = ipPath.ReverseValue()

	update := self.receiveUpdate(&ipPath)
	if update == nil {
		// This is not a response to a known outgoing flow. Preserve the public
		// callback's owned-path behavior on this rare path because no retained
		// flow key exists to borrow.
		self.deliverReceivePacket(source, provideMode, retainIpPath(&ipPath), packet)
		return
	}

	// Common download path: the flow is already committed to this source.
	// Deliver its immutable, owned flow key directly. This avoids a callback
	// closure, a receivePacket object, a one-element slice, and a path copy.
	if update.client.Load() == sourceClient {
		self.deliverReceivePacket(source, provideMode, update.ipPath, packet)
		return
	}

	var abandonedClients []*multiClientChannel
	var receivePackets []*receivePacket
	var returnPackets []*receivePacket

	// Race / not-yet-committed paths are guarded by the per-flow stateLock.
	update.stateLock.Lock()
	client := update.client.Load()
	if client == sourceClient {
		// Committed between the lock-free check and acquiring the lock.
		receivePackets = append(receivePackets, &receivePacket{
			Source:      source,
			ProvideMode: provideMode,
			IpPath:      update.ipPath,
			Packet:      packet,
		})
	} else if client != nil {
		// Another client already won; drop.
	} else if race := update.race; race == nil {
		self.log.Infof("[multi]receive no race and no client")
	} else if state, ok := race.clientStates[sourceClient]; !ok {
		self.log.Infof("[multi]receive client not part of race")
	} else if len(state.packets) < self.settings.MultiRaceClientPacketMaxCount && race.packetCount < self.settings.MultiRacePacketMaxCount {
		packetCopy, pooled := MessagePoolCopyDetailed(packet)
		receivePacket := &receivePacket{
			Source:      source,
			ProvideMode: provideMode,
			IpPath:      update.ipPath,
			Packet:      packetCopy,
			Pooled:      pooled,
		}
		state.packets = append(state.packets, receivePacket)
		if 1 == len(state.packets) {
			state.receiveTime = time.Now()
		}
		race.packetCount += 1
		if len(state.packets) == 1 {
			race.clientsWithPacketCount += 1
			if int(float32(len(race.clientStates))*self.settings.MultiRaceClientEarlyCompleteFraction) <= race.clientsWithPacketCount {
				race.completeMonitor.NotifyAll()
			}
		}
	} else {
		// Race buffer limits exceeded, end the race immediately.
		self.log.Infof("[multi]receive race buffer limit reached")

		for abandonedClient, abandonedState := range race.clientStates {
			if abandonedClient != sourceClient {
				abandonedClients = append(abandonedClients, abandonedClient)
				for _, p := range abandonedState.packets {
					if p.Pooled {
						p.Pooled = false
						returnPackets = append(returnPackets, p)
					}
				}
			}
		}

		update.clearRaceWithLock()
		update.client.Store(sourceClient)
		receivePacket := &receivePacket{
			Source:      source,
			ProvideMode: provideMode,
			IpPath:      update.ipPath,
			Packet:      packet,
		}
		receivePackets = append(state.packets, receivePacket)
		for _, p := range receivePackets {
			if p.Pooled {
				p.Pooled = false
				returnPackets = append(returnPackets, p)
			}
		}
	}
	update.stateLock.Unlock()

	if 0 < len(abandonedClients) {
		if rstPacket, ok := ipOosRst(update.ipPath); ok {
			for _, abandonedClient := range abandonedClients {
				abandonedClient.Send(&parsedPacket{
					packet: rstPacket,
					ipPath: update.ipPath,
				}, 0)
			}
		}
	}
	for _, p := range receivePackets {
		self.deliverReceivePacket(p.Source, p.ProvideMode, p.IpPath, p.Packet)
	}
	for _, p := range returnPackets {
		MessagePoolReturn(p.Packet)
	}
}

// spawns a goroutine that completes the race after a timeout. it acquires the
// per-flow stateLock (not the parent lock) when it evaluates the race.
func (self *RemoteUserNatMultiClient) scheduleCompleteRace(
	ipPath *IpPath,
	race *multiClientChannelUpdateRace,
	earlyComplete <-chan struct{},
) {
	go HandleError(func() {
		// wait for the race to finish, then choose

		select {
		case <-race.ctx.Done():
			return
		case <-earlyComplete:
		case <-time.After(self.settings.MultiRaceSetOnResponseTimeout):
		}

		var abandonedClients []*multiClientChannel
		var receivePackets []*receivePacket
		var returnPackets []*receivePacket
		self.receiveClientPath(ipPath, func(update *multiClientChannelUpdate) {
			// race state is guarded by the per-flow stateLock (a leaf); client
			// is atomic
			update.stateLock.Lock()
			defer update.stateLock.Unlock()

			if update.race == race {
				defer update.clearRaceWithLock()

				if update.client.Load() == nil {

					// weighted shuffle clients by rtt
					orderedClients := []*multiClientChannel{}
					weights := map[*multiClientChannel]float32{}
					for client, state := range race.clientStates {
						if 0 < len(state.packets) {
							orderedClients = append(orderedClients, client)
							rtt := state.receiveTime.Sub(state.sendTime)
							weights[client] = float32(rtt / time.Millisecond)
						}
					}
					WeightedShuffleWithEntropy(orderedClients, weights, self.settings.StatsWindowEntropy)

					if 0 < len(orderedClients) {
						// the last is the lowest rtt
						client := orderedClients[len(orderedClients)-1]

						update.client.Store(client)
						receivePackets = race.clientStates[client].packets
						for _, p := range receivePackets {
							if p.Pooled {
								p.Pooled = false
								returnPackets = append(returnPackets, p)
							}
						}
					}
				}
				// else the client is already set
				committedClient := update.client.Load()
				for abandonedClient, abandonedState := range race.clientStates {
					if abandonedClient != committedClient {
						abandonedClients = append(abandonedClients, abandonedClient)
						for _, p := range abandonedState.packets {
							if p.Pooled {
								p.Pooled = false
								returnPackets = append(returnPackets, p)
							}
						}
					}
				}
			}
			// else the client is on a new race
		})
		if 0 < len(abandonedClients) {
			if rstPacket, ok := ipOosRst(ipPath); ok {
				for _, abandonedClient := range abandonedClients {
					abandonedClient.Send(&parsedPacket{
						packet: rstPacket,
						ipPath: ipPath,
					}, 0)
				}
			}
		}
		for _, p := range receivePackets {
			self.deliverReceivePacket(p.Source, p.ProvideMode, p.IpPath, p.Packet)
		}
		for _, p := range returnPackets {
			MessagePoolReturn(p.Packet)
		}
	})
}

func (self *RemoteUserNatMultiClient) Shuffle() {
	for _, window := range self.windows {
		window.shuffle()
	}
}

func (self *RemoteUserNatMultiClient) Close() {
	self.cancel()
	// A closed multi-client can remain reachable from provider transfer
	// sequence state until that peer's idle timeout. Do not let that bounded
	// protocol retention keep the retired UpgradeMux, its two resolver caches,
	// and every h2/TLS connection graph alive for the same interval.
	//
	// Storing nil does not interrupt a callback already executing (send,
	// receive, and forward callbacks remain intentional backpressure while
	// live); it only prevents future post-close delivery.
	self.receivePacketCallback.Store(nil)
	self.SetServerNameLookup(nil)

	for _, window := range self.windows {
		window.Close()
	}

	var removedUpdates []*multiClientChannelUpdate
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		for _, update := range self.ip4PathUpdates {
			removedUpdates = append(removedUpdates, update)
		}
		for _, update := range self.ip6PathUpdates {
			removedUpdates = append(removedUpdates, update)
		}
		clear(self.ip4PathUpdates)
		clear(self.ip6PathUpdates)
		clear(self.affinityIp4Paths)
		clear(self.affinityIp6Paths)
		// In-flight sends reconcile against this table. Clear it in the same
		// teardown critical section as the flow maps; their generation guard
		// prevents any late completion from repopulating it.
		clear(self.clientUpdates)
	}()

	// close updates outside the parent lock: update.Close() takes the per-flow
	// stateLock (clearRaceWithLock), and keeping it off the parent lock means
	// the per-flow stateLock never nests under the parent lock anywhere.
	for _, update := range removedUpdates {
		update.Close()
	}

	self.localUserNat.Close()
	self.localUserNatUnsub()
}

type multiClientChannelUpdate struct {
	ctx    context.Context
	cancel context.CancelFunc

	// client is the channel this flow is committed to. it is read on the
	// per-packet hot path (egress send target, ingress steady-state match) and
	// across flows by affinity selection, so it is an atomic for lock-free
	// reads. writes happen under the parent stateLock (which serializes them
	// against the path maps) or a context where the writer has exclusive access.
	client atomic.Pointer[multiClientChannel]

	// stateLock guards the per-flow mutable state below: the active race and the
	// tcp collapse-prevention sequence counters. it is a leaf — its holders
	// never take the parent `RemoteUserNatMultiClient.stateLock` — so
	// independent flows never serialize on the parent lock for race or sequence
	// work, and there is no ordering constraint with the parent lock.
	stateLock sync.Mutex
	// race is guarded by stateLock.
	race *multiClientChannelUpdateRace

	// activityTime is guarded by the parent stateLock (written during the path
	// map lookup in send/receiveUpdate).
	activityTime time.Time
	ipPath       *IpPath

	sequencePacketCount int    // guarded by stateLock
	ackSequenceNumber   uint32 // guarded by stateLock
	// sequenceNumber wraps at 2^32. ordering is determined via `int32(a - b)`
	// signed-delta arithmetic (per RFC 1323 PAWS / RFC 7323), wraparound-tolerant
	// across the 32-bit boundary.
	sequenceNumber uint32 // guarded by stateLock

	// affinityIp{4,6}Paths are guarded by the parent stateLock (creation only).
	affinityIp4Paths map[Ip4Path]bool
	affinityIp6Paths map[Ip6Path]bool
}

func newMultiClientChannelUpdate(ctx context.Context, ipPath *IpPath) *multiClientChannelUpdate {
	cancelCtx, cancel := context.WithCancel(ctx)
	// The caller's path may borrow address slices from a packet buffer that is
	// transferred immediately after this call. Flow state outlives that buffer,
	// so take the ownership copy exactly once when the flow record is created.
	return &multiClientChannelUpdate{
		ctx:              cancelCtx,
		cancel:           cancel,
		ipPath:           retainIpPath(ipPath),
		affinityIp4Paths: map[Ip4Path]bool{},
		affinityIp6Paths: map[Ip6Path]bool{},
	}
}

// retainIpPath makes the immutable, flow-oriented path shared by asynchronous
// flow state and receive callbacks. It deliberately retains only the tuple:
// per-packet TCP sequence/flag fields are tracked separately and Reverse never
// exposed them on the return callback path.
func retainIpPath(ipPath *IpPath) *IpPath {
	retained := &IpPath{
		Version:         ipPath.Version,
		Protocol:        ipPath.Protocol,
		SourcePort:      ipPath.SourcePort,
		DestinationPort: ipPath.DestinationPort,
		ServerName:      ipPath.ServerName,
	}
	if addressByteCount := len(ipPath.SourceIp) + len(ipPath.DestinationIp); 0 < addressByteCount {
		addresses := make(net.IP, addressByteCount)
		sourceByteCount := copy(addresses, ipPath.SourceIp)
		copy(addresses[sourceByteCount:], ipPath.DestinationIp)
		retained.SourceIp = addresses[:sourceByteCount:sourceByteCount]
		retained.DestinationIp = addresses[sourceByteCount:]
	}
	return retained
}

func (self *multiClientChannelUpdate) resetSequence(ipPath *IpPath) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.ackSequenceNumber = ipPath.AckSequenceNumber
	self.sequenceNumber = ipPath.SequenceNumber
	self.sequencePacketCount = 0
}

func (self *multiClientChannelUpdate) updateSequence(ipPath *IpPath, payloadByteCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	update := false

	nextAckSequenceNumber := ipPath.AckSequenceNumber
	if self.ackSequenceNumber != nextAckSequenceNumber {
		self.ackSequenceNumber = nextAckSequenceNumber
		update = true
	}

	// modular uint32 add wraps correctly across the 4 GB boundary
	nextSequenceNumber := ipPath.SequenceNumber + uint32(payloadByteCount)
	// signed-delta comparison is wraparound-tolerant: > 0 means nextSequenceNumber
	// is later in TCP sequence space than self.sequenceNumber
	if 0 < int32(nextSequenceNumber-self.sequenceNumber) {
		self.sequenceNumber = nextSequenceNumber
		update = true
	}

	if update {
		self.sequencePacketCount += 1
	}
}

func (self *multiClientChannelUpdate) canUpdateSequence(ipPath *IpPath, payloadByteCount int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.sequencePacketCount == 0 {
		return true
	}

	if self.ackSequenceNumber != ipPath.AckSequenceNumber {
		return true
	}

	nextSequenceNumber := ipPath.SequenceNumber + uint32(payloadByteCount)
	// strict signed-delta > 0 mirrors updateSequence; treating equality as
	// "can update" would let identical retransmits pass the gate even though
	// updateSequence won't advance state, defeating TcpCollapsePrevention.
	if 0 < int32(nextSequenceNumber-self.sequenceNumber) {
		return true
	}

	return false
}

// must be called with `stateLock`
func (self *multiClientChannelUpdate) initRaceWithLock() {
	if self.race == nil {
		self.race = newMultiClientChannelUpdateRace(self.ctx)
	}
}

// must be called with `stateLock`
func (self *multiClientChannelUpdate) clearRaceWithLock() {
	if self.race != nil {
		self.race.Close()
		self.race = nil
	}
}

func (self *multiClientChannelUpdate) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *multiClientChannelUpdate) Close() {
	self.cancel()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clearRaceWithLock()
	// A retired flow must not retain its selected provider while another
	// owner briefly retains the update (for example a send defer unwinding).
	self.client.Store(nil)
}

type multiClientChannelUpdateRace struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	clientStates           map[*multiClientChannel]*multiClientChannelRaceClientState
	sentPacketCount        int
	packetCount            int
	clientsWithPacketCount int
	completeMonitor        *Monitor
}

func newMultiClientChannelUpdateRace(ctx context.Context) *multiClientChannelUpdateRace {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &multiClientChannelUpdateRace{
		ctx:          cancelCtx,
		cancel:       cancel,
		clientStates: map[*multiClientChannel]*multiClientChannelRaceClientState{},
		// sentPacketCount:        0,
		// packetCount:            0,
		// clientsWithPacketCount: 0,
		completeMonitor: NewMonitor(),
	}
}

func (self *multiClientChannelUpdateRace) Close() {
	self.cancel()
	// return any still-pooled packets that no other path has claimed.
	// the Pooled flag is the single-owner marker: paths that consume a
	// receivePacket (race-buffer-exceeded in clientReceivePacket, the
	// scheduleCompleteRace winner/abandoned drains) set Pooled=false
	// before appending to their own return list. anything still
	// Pooled=true here was abandoned without a cleanup pass.
	for _, state := range self.clientStates {
		for _, p := range state.packets {
			if p.Pooled {
				p.Pooled = false
				MessagePoolReturn(p.Packet)
			}
		}
		state.packets = nil
	}
	clear(self.clientStates)
}

type multiClientChannelRaceClientState struct {
	sendTime        time.Time
	receiveTime     time.Time
	packets         []*receivePacket
	sentPacketCount int
}

type parsedPacket struct {
	packet []byte
	ipPath *IpPath
}

type multiClientWindow struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	generator                   MultiClientGenerator
	clientReceivePacketCallback clientReceivePacketFunction
	clientRemoveCallback        func(client *multiClientChannel)
	windowType                  WindowType
	// networkPeerDestination is true only when the embedding app explicitly
	// selected a trusted same-network peer and the entire multi-client uses the
	// Network relationship.
	networkPeerDestination bool

	settings *MultiClientSettings

	clientChannelArgs chan *multiClientChannelArgs

	monitor *RemoteUserNatMultiClientMonitor

	contractStatusCallbacks *CallbackList[*contractStatusCallbackWorker]
	contractStatsCallbacks  *CallbackList[*contractStatsCallbackWorker]
	// relayed from every window client's encryption session manager
	peerIdentityChangeCallbacks *CallbackList[*coalescingCallbackWorker]

	stateLock sync.Mutex
	clients   map[Id]*multiClientChannel
	// Profiles are immutable once published. Expansion reads them outside
	// stateLock while runtime profile changes update them, so use an atomic
	// pointer both for race-free publication and to keep the hot read cheap.
	performanceProfile atomic.Pointer[PerformanceProfile]
	// formationLogged guards the one-shot window-formation log line: the time
	// from window creation to the first usable (ping-verified) client, the
	// head of the first-load critical path. Guarded by stateLock.
	formationLogged bool

	createTime time.Time

	generatorMonitor *Monitor
	resizeMonitor    *Monitor
}

func newMultiClientWindow(
	ctx context.Context,
	cancel context.CancelFunc,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
	clientRemoveCallback func(client *multiClientChannel),
	windowType WindowType,
	networkPeerDestination bool,
	settings *MultiClientSettings,
	performanceProfile *PerformanceProfile,
) *multiClientWindow {
	window := &multiClientWindow{
		ctx:                         ctx,
		cancel:                      cancel,
		log:                         loggerOrDefault(settings.Log),
		generator:                   generator,
		clientReceivePacketCallback: clientReceivePacketCallback,
		clientRemoveCallback:        clientRemoveCallback,
		windowType:                  windowType,
		networkPeerDestination:      networkPeerDestination,
		settings:                    settings,
		clientChannelArgs:           make(chan *multiClientChannelArgs),
		monitor:                     NewRemoteUserNatMultiClientMonitor(&settings.RemoteUserNatMultiClientMonitorSettings),
		contractStatusCallbacks:     NewCallbackList[*contractStatusCallbackWorker](),
		contractStatsCallbacks:      NewCallbackList[*contractStatsCallbackWorker](),
		peerIdentityChangeCallbacks: NewCallbackList[*coalescingCallbackWorker](),
		clients:                     map[Id]*multiClientChannel{},
		createTime:                  time.Now(),
		generatorMonitor:            NewMonitor(),
		resizeMonitor:               NewMonitor(),
	}
	window.performanceProfile.Store(performanceProfile)

	go HandleError(window.randomEnumerateClientArgs, cancel)
	go HandleError(window.resize, cancel)

	return window
}

func (self *multiClientWindow) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	worker := newContractStatusCallbackWorker(self.ctx, contractStatusCallback, self.settings.SequenceBufferSize)
	callbackId := self.contractStatusCallbacks.Add(worker)
	return func() {
		self.contractStatusCallbacks.Remove(callbackId)
		worker.Close()
	}
}

func (self *multiClientWindow) contractStatus(contractStatus *ContractStatus) {
	for _, contractStatusCallback := range self.contractStatusCallbacks.Get() {
		contractStatusCallback.Dispatch(contractStatus)
	}
}

func (self *multiClientWindow) AddContractStatsCallback(contractStatsCallback ContractStatsFunction) func() {
	worker := newContractStatsCallbackWorker(self.ctx, contractStatsCallback, self.settings.SequenceBufferSize)
	callbackId := self.contractStatsCallbacks.Add(worker)
	return func() {
		self.contractStatsCallbacks.Remove(callbackId)
		worker.Close()
	}
}

// registered on every window client's contract manager.
// the manager's epoch worker calls this off the packet paths
func (self *multiClientWindow) contractStats(contractStatsEvents []*ContractStatsEvent) {
	for _, contractStatsCallback := range self.contractStatsCallbacks.Get() {
		contractStatsCallback.Dispatch(contractStatsEvents)
	}
}

func (self *multiClientWindow) AddPeerIdentityChangeCallback(callback func()) func() {
	worker := newCoalescingCallbackWorker(self.ctx, callback)
	callbackId := self.peerIdentityChangeCallbacks.Add(worker)
	return func() {
		self.peerIdentityChangeCallbacks.Remove(callbackId)
		worker.Close()
	}
}

// registered on every window client's encryption session manager
// (and fired once more when a window client is removed)
func (self *multiClientWindow) peerIdentityChanged() {
	for _, callback := range self.peerIdentityChangeCallbacks.Get() {
		callback.Dispatch()
	}
}

// the performance profile will take effect at the next `resize` iteration
func (self *multiClientWindow) SetPerformanceProfile(performanceProfile *PerformanceProfile) {
	self.performanceProfile.Store(performanceProfile)
}

func (self *multiClientWindow) generatorCallContext() (context.Context, context.CancelFunc) {
	if 0 < self.settings.WindowGeneratorTimeout {
		return context.WithTimeout(self.ctx, self.settings.WindowGeneratorTimeout)
	}
	return context.WithCancel(self.ctx)
}

func (self *multiClientWindow) nextDestinations(
	count int,
	excludeDestinations []MultiHopId,
	rankMode string,
) (map[MultiHopId]DestinationStats, error) {
	if generator, ok := self.generator.(MultiClientGeneratorContext); ok {
		callCtx, cancel := self.generatorCallContext()
		defer cancel()
		return generator.NextDestinationsContext(callCtx, count, excludeDestinations, rankMode)
	}
	return self.generator.NextDestinations(count, excludeDestinations, rankMode)
}

func (self *multiClientWindow) newClientArgs(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	callCtx, cancel := self.generatorCallContext()
	defer cancel()

	if destinationGenerator, ok := self.generator.(MultiClientGeneratorWithDestinationContext); ok {
		return destinationGenerator.NewClientArgsForDestinationContext(callCtx, destination)
	}
	if generator, ok := self.generator.(MultiClientGeneratorContext); ok {
		return generator.NewClientArgsContext(callCtx)
	}
	if destinationGenerator, ok := self.generator.(MultiClientGeneratorWithDestination); ok {
		return destinationGenerator.NewClientArgsForDestination(destination)
	}
	return self.generator.NewClientArgs()
}

func (self *multiClientWindow) randomEnumerateClientArgs() {
	defer func() {
		close(self.clientChannelArgs)

		// drain the channel
		func() {
			for {
				select {
				case args, ok := <-self.clientChannelArgs:
					if !ok {
						return
					}
					self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
				}
			}
		}()
	}()

	for {
		generatorNotify := self.generatorMonitor.NotifyChannel()

		// exclude healthy destinations that are already in the window
		windowDestinations := func() map[MultiHopId]bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			windowDestinations := map[MultiHopId]bool{}
			for _, client := range self.clients {
				if !client.isWarning() {
					windowDestinations[client.Destination()] = true
				}
			}
			return windowDestinations
		}

		destinations, err := self.nextDestinations(
			self.settings.WindowExpandBlockCount,
			slices.Collect(maps.Keys(windowDestinations())),
			self.windowType.RankMode(),
		)
		if err != nil {
			self.log.Infof("[multi]window enumerate error timeout = %s\n", err)
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(self.settings.WindowEnumerateErrorTimeout):
			}
			continue
		}

		func() {
			// destinations must be used by `expirationTime`
			expirationTime := time.Now().Add(self.settings.WindowExpandArgsTimeout)
			for destination, stats := range destinations {

				for {
					timeout := expirationTime.Sub(time.Now())
					if timeout <= 0 {
						return
					}

					// a destination-aware generator can reuse a persisted
					// identity for this destination (PROXYDRAIN1.md §3.5)
					clientArgs, err := self.newClientArgs(destination)
					if err != nil {
						self.log.Infof("[multi]create client args error = %s\n", err)
						select {
						case <-self.ctx.Done():
							return
						case <-time.After(self.settings.WindowEnumerateErrorTimeout):
						}
						continue
					}

					args := &multiClientChannelArgs{
						Destination:                    destination,
						DestinationStats:               stats,
						MultiClientGeneratorClientArgs: *clientArgs,
					}
					select {
					case <-self.ctx.Done():
						self.generator.RemoveClientArgs(clientArgs)
						return
					case self.clientChannelArgs <- args:
					case <-time.After(timeout):
						// destination expired
						self.log.Infof("[multi]create client args expired\n")
						self.generator.RemoveClientArgs(clientArgs)
						return
					}
					break
				}
			}
		}()

		select {
		case <-self.ctx.Done():
			return
		case <-generatorNotify:
		}
	}
}

func (self *multiClientWindow) resize() {
	for {
		update := self.resizeMonitor.NotifyChannel()

		var windowSize WindowSizeSettings
		var fixedWindowType *WindowType
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if profileWindowType, profileWindowSize, ok := self.performanceProfile.Load().FixedWindow(); ok {
				fixedWindowType = &profileWindowType
				windowSize = profileWindowSize
			} else {
				windowSize = self.settings.WindowSizes[self.windowType]
			}
		}()

		startTime := time.Now()

		warnedClients := []*multiClientChannel{}
		clients := []*multiClientChannel{}
		maxSourceCount := 0
		weights := map[*multiClientChannel]float32{}
		durations := map[*multiClientChannel]time.Duration{}

		removeClient := func(client *multiClientChannel) {
			client.Close()
			removed := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				// guard the slot: the client may have been replaced under the same
				// client id by a concurrent expand (see the replace on ping success).
				// in that case do not delete the new client's entry and do not emit
				// Removed for the shared id — the monitor dot now belongs to the
				// new client.
				if self.clients[client.ClientId()] == client {
					delete(self.clients, client.ClientId())
					removed = true
				}
			}()
			if removed {
				self.removeClients(client)
			}
		}
		keepClient := func(client *multiClientChannel, stats *clientWindowStats) {
			clients = append(clients, client)
			maxSourceCount = max(maxSourceCount, stats.sourceCount)
			weights[client] = float32(stats.EffectiveByteCountPerSecond())
			durations[client] = stats.clientDuration
		}
		warnClient := func(client *multiClientChannel, stats *clientWindowStats) {
			warnedClients = append(warnedClients, client)
			maxSourceCount = max(maxSourceCount, stats.sourceCount)
			weights[client] = float32(stats.EffectiveByteCountPerSecond())
			durations[client] = stats.clientDuration
		}

		clientStats := map[*multiClientChannel]*clientWindowStats{}
		for _, client := range self.unorderedClients() {
			if stats, err := client.WindowStats(); err == nil {
				clientStats[client] = stats
			} else {
				self.log.Infof("[multi]remove error client [%s] = %s\n", client.ClientId(), err)
				removeClient(client)
			}
		}

		netHealthRanks := map[*multiClientChannel]int{}
		func() {
			orderedClients := slices.Collect(maps.Keys(clientStats))
			slices.SortFunc(orderedClients, func(a *multiClientChannel, b *multiClientChannel) int {
				// descending healthy duration, net healthy duration
				statsA := clientStats[a]
				statsB := clientStats[b]

				if c := statsB.healthyDuration - statsA.healthyDuration; c < 0 {
					return -1
				} else if 0 < c {
					return 1
				}

				if c := statsB.netHealthyDuration - statsA.netHealthyDuration; c < 0 {
					return -1
				} else if 0 < c {
					return 1
				}

				return 0
			})
			for i, client := range orderedClients {
				netHealthRanks[client] = i
			}
		}()

		for client, stats := range clientStats {
			// note for fixed destination size, the destination might still be aliased with multiple clients
			// TODO it's still not clear why one client might stop working occasionally

			healthy := stats.unhealthyDuration < self.settings.StatsWindowMaxUnhealthyDuration

			printStats := func(status string) {
				effectiveByteCountPerSecond := stats.EffectiveByteCountPerSecond()
				expectedByteCountPerSecond := stats.ExpectedByteCountPerSecond()

				if self.log.V(1).Enabled() {
					self.log.Infof(
						"[multi]%s [%s]: h=%d+%dms/u=%d+%dms effective=%db/s expected=%db/s send=%db sendNack=%db receive=%db\n",
						status,
						client.ClientId(),
						stats.netHealthyDuration/time.Millisecond,
						stats.healthyDuration/time.Millisecond,
						stats.netUnhealthyDuration/time.Millisecond,
						stats.unhealthyDuration/time.Millisecond,
						effectiveByteCountPerSecond,
						expectedByteCountPerSecond,
						stats.sendAckByteCount,
						stats.sendNackByteCount,
						stats.receiveAckByteCount,
					)
				}
			}
			// the top `StatsWindowKeepHealthiestCount` won't be marked as warning or removed
			netHealthRank := netHealthRanks[client]
			remove := max(windowSize.FixedWindowSize, windowSize.KeepHealthiestCount) <= netHealthRank
			// a rank-kept client is still removed once it has been continuously
			// unhealthy past the keep cap: keeping the healthiest rides out
			// transient badness, but a client that never recovers must be
			// replaced so the window re-expands with fresh candidates (and its
			// grid dot is reclaimed) instead of pinning a dead client forever
			if 0 < self.settings.StatsWindowKeepUnhealthyDuration &&
				self.settings.StatsWindowKeepUnhealthyDuration <= stats.unhealthyDuration {
				remove = true
			}
			if healthy {
				// a client after its `removeTime` will be in a permananent warning state as long as it continues to route traffic
				// this prevents new connections from using the client
				if stats.unhealthyDuration < self.settings.StatsWindowWarnUnhealthyDuration {
					if !stats.removeTime.IsZero() && stats.removeTime.Before(startTime) {
						printStats("client drain")
						client.setWarning(remove)
						warnClient(client, stats)
					} else {
						printStats("client ok")
						windowSizeUlimit := self.settings.DefaultUlimit
						if 0 < windowSize.Ulimit {
							windowSizeUlimit = windowSize.Ulimit
						}
						ulimit := 0 < windowSizeUlimit && windowSizeUlimit <= stats.netSourceCount
						if ulimit {
							client.setWarning(true)
							warnClient(client, stats)
						} else {
							client.setWarning(false)
							keepClient(client, stats)
						}
					}
				} else {
					printStats("client health warning")
					client.setWarning(remove)
					warnClient(client, stats)
				}
			} else {
				printStats(fmt.Sprintf("unhealthy client (#%d remove=%t)", netHealthRank, remove))

				if remove {
					client.setWarning(true)
					removeClient(client)
				} else {
					client.setWarning(false)
					warnClient(client, stats)
				}
			}
		}

		collapseLowestWeighted := func(targetWindowSize int) {

			n := (len(warnedClients) + len(clients)) - targetWindowSize

			collapse := func(cs []*multiClientChannel) {
				if 0 < n && 0 < len(cs) {
					m := min(len(cs), n)
					if 0 < m {
						slices.SortFunc(cs, func(a *multiClientChannel, b *multiClientChannel) int {
							// descending weight
							aWeight := weights[a]
							bWeight := weights[b]
							if aWeight < bWeight {
								return 1
							} else if bWeight < aWeight {
								return -1
							}
							aDuration := durations[a]
							bDuration := durations[b]
							if aDuration < bDuration {
								return 1
							} else if bDuration < aDuration {
								return -1
							}
							return 0
						})

						for _, client := range cs[len(cs)-m : len(cs)] {
							if self.settings.StatsWindowGraceperiod <= durations[client] && weights[client] <= 0 {
								removeClient(client)
							}
						}
						n -= m
					}
				}
			}
			collapse(warnedClients)
			collapse(clients)
		}

		p2pOnlyWindowSize := 0
		for _, client := range clients {
			if client.IsP2pOnly() {
				p2pOnlyWindowSize += 1
			}
		}

		fixedDestinationSize, fixedDestination := self.generator.FixedDestinationSize()

		var windowSizeMin int
		var targetWindowSize int
		if fixedDestination {
			targetWindowSize = fixedDestinationSize
			windowSizeMin = targetWindowSize
		} else if 0 < windowSize.FixedWindowSize {
			if fixedWindowType == nil || self.windowType == *fixedWindowType {
				targetWindowSize = windowSize.FixedWindowSize
			} else {
				// not the active window, disable resize
				targetWindowSize = 0
			}
			windowSizeMin = targetWindowSize
		} else {
			// scale the number of reconnects
			reconnectScale := self.settings.DefaultReconnectScale
			if 0 < windowSize.WindowSizeReconnectScale {
				reconnectScale = windowSize.WindowSizeReconnectScale
			}
			targetWindowSize = int(math.Ceil(float64(maxSourceCount) * reconnectScale))

			if n := windowSize.WindowSizeMinP2pOnly - p2pOnlyWindowSize; 0 < n {
				targetWindowSize += n
			}

			targetWindowSize = min(
				windowSize.WindowSizeMax,
				max(
					windowSize.WindowSizeMin,
					targetWindowSize,
				),
			)
			windowSizeMin = windowSize.WindowSizeMin
		}

		minSatisfied := func(clientCount int) bool {
			return windowMinSatisfied(
				windowSizeMin,
				clientCount,
				len(warnedClients),
				fixedDestination,
			)
		}

		addedCount := 0
		if len(clients) < targetWindowSize {
			// expand
			n := targetWindowSize - len(clients)
			self.monitor.AddWindowExpandEvent(
				minSatisfied(len(clients)),
				targetWindowSize+len(warnedClients),
			)
			addedCount = self.expand(
				windowSize,
				len(clients),
				p2pOnlyWindowSize,
				targetWindowSize,
				windowSizeMin,
				targetWindowSize-len(clients),
			)
			if self.log.V(1).Enabled() {
				self.log.Infof("[multi]window expand +%d %d->%d (+%d)\n", n, len(clients), targetWindowSize, addedCount)
			}
		}
		if 0 < windowSize.WindowSizeHardMax && windowSize.WindowSizeHardMax < len(clients)+len(warnedClients)+addedCount {
			self.monitor.AddWindowExpandEvent(
				minSatisfied(len(clients)+addedCount),
				windowSize.WindowSizeHardMax,
			)
			collapseLowestWeighted(max(0, windowSize.WindowSizeHardMax-addedCount))
			if self.log.V(1).Enabled() {
				self.log.Infof("[multi]window collapse -%d ->%d\n", (len(clients)+len(warnedClients)+addedCount)-windowSize.WindowSizeHardMax, windowSize.WindowSizeHardMax)
			}
		} else {
			self.monitor.AddWindowExpandEvent(
				minSatisfied(len(clients)+addedCount),
				len(clients)+len(warnedClients)+addedCount,
			)
		}

		timeout := self.settings.WindowResizeTimeout - time.Now().Sub(startTime)
		if timeout <= 0 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			case <-update:
			case <-time.After(timeout):
			}
		}
	}
}

// runMultiClientPingAdmission serializes only the shared evaluation state,
// never the send admission itself. Send may intentionally wait for transfer
// backpressure up to PingWriteTimeout; holding the window evaluation mutex
// across that wait prevents acknowledgements from already-sent candidates
// from committing and turns one full client queue into a whole-window pause.
func runMultiClientPingAdmission(
	pingDone context.Context,
	stateLock *sync.Mutex,
	beginWithLock func(),
	send func(func(error)) (bool, error),
	result func(error),
	admissionFailed func(),
) (bool, error) {
	admitted := func() bool {
		stateLock.Lock()
		defer stateLock.Unlock()
		select {
		case <-pingDone.Done():
			return false
		default:
		}
		beginWithLock()
		return true
	}()
	if !admitted {
		return false, nil
	}

	success, err := send(result)
	if err != nil || !success {
		admissionFailed()
	}
	return success, err
}

func (self *multiClientWindow) expand(
	windowSize WindowSizeSettings,
	currentWindowSize int,
	currentP2pOnlyWindowSize int,
	targetWindowSize int,
	windowSizeMin int,
	n int,
) (returnPingSuccess int) {
	stateLock := sync.Mutex{}
	pendingPingDones := []context.Context{}
	added := 0
	addedP2pOnly := 0
	pingSuccess := 0

	defer func() {
		stateLock.Lock()
		defer stateLock.Unlock()

		returnPingSuccess = pingSuccess
	}()

	endTime := time.Now().Add(self.settings.WindowExpandTimeout)

	for i := 0; i < n; i += 1 {
		timeout := endTime.Sub(time.Now())
		if timeout < 0 {
			self.log.V(1).Infof("[multi]expand window timeout\n")
			return
		}

		self.generatorMonitor.NotifyAll()
		select {
		case <-self.ctx.Done():
			return
		// case <- update:
		//     // continue
		case args, ok := <-self.clientChannelArgs:
			if !ok {
				return
			}
			// func() {
			// 	self.stateLock.Lock()
			// 	defer self.stateLock.Unlock()
			// 	_, ok = self.destinationClients[args.Destination]
			// }()

			// if ok {
			// 	// already have a client in the window for this destination
			// 	self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
			// } else {
			// randomly set to p2p only to meet the minimum requirement
			if !args.MultiClientGeneratorClientArgs.P2pOnly {
				var a int
				var b int
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()

					a = max(windowSize.WindowSizeMin-(currentWindowSize+added), 0)
					b = max(windowSize.WindowSizeMinP2pOnly-(currentP2pOnlyWindowSize+addedP2pOnly), 0)
				}()
				var p2pOnlyP float32
				if a+b == 0 {
					p2pOnlyP = 0
				} else {
					p2pOnlyP = float32(b) / float32(a+b)
				}
				args.MultiClientGeneratorClientArgs.P2pOnly = mathrand.Float32() < p2pOnlyP
			}

			client, err := newMultiClientChannel(
				self.ctx,
				args,
				self.generator,
				self.clientReceivePacketCallback,
				self.contractStatus,
				self.contractStats,
				self.peerIdentityChanged,
				self.performanceProfile.Load(),
				self.networkPeerDestination,
				self.settings,
			)
			if err != nil {
				self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
				self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
			} else {
				// comparative health: a silent first connect next to passing
				// siblings cuts its blackhole wait (see detectBlackhole)
				client.SetWindowHealth(self.otherClientRecentReceive)

				// send an initial ping on the client and let the ack timeout close it
				pingDone, pingCancel := context.WithCancel(self.ctx)
				pendingPingDones = append(pendingPingDones, pingDone)
				pingFinished := false

				claimPing := func() bool {
					stateLock.Lock()
					defer stateLock.Unlock()
					if pingFinished {
						return false
					}
					select {
					case <-pingDone.Done():
						return false
					default:
					}
					pingFinished = true
					return true
				}

				fail := func() {
					if !claimPing() {
						return
					}
					pingCancel()
					client.Cancel()
					self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
					self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
				}

				go HandleError(func() {
					clientP2pOnly := client.IsP2pOnly()
					success, err := runMultiClientPingAdmission(
						pingDone,
						&stateLock,
						func() {
							added += 1
							if clientP2pOnly {
								addedP2pOnly += 1
							}
						},
						func(ack func(error)) (bool, error) {
							self.monitor.AddProviderEvent(args.ClientId, ProviderStateInEvaluation)
							return client.SendDetailedMessage(
								&protocol.IpPing{},
								self.settings.PingWriteTimeout,
								ack,
							)
						},
						func(err error) {
							if err == nil {
								if !claimPing() {
									return
								}
								self.log.V(1).Infof("[multi]expand new client\n")

								var replacedClient *multiClientChannel
								formationMillis := int64(-1)
								func() {
									self.stateLock.Lock()
									defer self.stateLock.Unlock()
									clientId := client.ClientId()
									replacedClient = self.clients[clientId]
									self.clients[clientId] = client
									if !self.formationLogged {
										self.formationLogged = true
										formationMillis = time.Since(self.createTime).Milliseconds()
									}
								}()
								if 0 <= formationMillis {
									// window formation: creation → first usable (ping-verified)
									// client, the head of the first-load critical path
									self.log.Infof("[multi]window %v formed in %dms\n", self.windowType, formationMillis)
								}
								if replacedClient != nil {
									// the replaced client is stored under the same client id
									// as the new client, so they share one monitor dot. Cancel
									// it without emitting Removed — a Removed here would
									// terminal-arm the dot of the NEW live client (Added
									// below), and the ui would reap it while the client is
									// still routing.
									replacedClient.Cancel()
								}
								self.monitor.AddProviderEvent(args.ClientId, ProviderStateAdded)
								// reap promptly when the client dies (the continuous ping or
								// blackhole detection cancels the channel): wake the resize
								// loop instead of waiting for its next tick
								go HandleError(func() {
									select {
									case <-self.ctx.Done():
									case <-client.Done():
										self.resizeMonitor.NotifyAll()
									}
								})
								func() {
									stateLock.Lock()
									defer stateLock.Unlock()
									pingSuccess += 1
								}()
								pingCancel()
							} else {
								if self.log.V(1).Enabled() {
									self.log.Infof("[multi]create ping error = %s\n", err)
								}
								fail()
							}
						},
						fail,
					)
					if err != nil {
						self.log.Infof("[multi]create client ping error = %s\n", err)
					} else if success {
						// async wait for the ping
						pingWaitStart := time.Now()
						go HandleError(func() {
							timer := time.NewTimer(self.settings.PingTimeout)
							defer timer.Stop()
							for {
								select {
								case <-pingDone.Done():
									return
								case <-timer.C:
									if schedulerPauseDetected(
										pingWaitStart,
										self.settings.PingTimeout,
										self.settings.SchedulerPauseTolerance,
									) {
										// The host was paused, not the peer. Give the
										// already-outstanding evaluation ping one fresh
										// budget after resume instead of evicting every
										// candidate at once.
										pingWaitStart = time.Now()
										timer.Reset(self.settings.PingTimeout)
										continue
									}
									self.log.V(2).Infof("[multi]expand window timeout waiting for ping\n")
									fail()
									return
								}
							}
						}, client.Cancel)
					}
				})
			}
		case <-time.After(timeout):
			self.log.V(2).Infof("[multi]expand window timeout waiting for args\n")
		}
	}

	// wait for pending pings
	for _, pingDone := range pendingPingDones {
		timeout := endTime.Sub(time.Now())
		if timeout <= 0 {
			break
		}

		select {
		case <-self.ctx.Done():
			return
		case <-pingDone.Done():
		case <-time.After(timeout):
		}
	}

	return
}

func (self *multiClientWindow) shuffle() {
	for _, client := range self.unorderedClients() {
		client.Cancel()
	}
	self.resizeMonitor.NotifyAll()
}

func (self *multiClientWindow) unorderedClients() []*multiClientChannel {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Collect(maps.Values(self.clients))
}

// otherClientRecentReceive reports whether any window client OTHER than
// exclude received return traffic within the blackhole poll window — the
// comparative signal that a silent client is its own problem, not an outage.
func (self *multiClientWindow) otherClientRecentReceive(exclude *multiClientChannel) bool {
	for _, client := range self.unorderedClients() {
		if client != exclude && client.recentReceiveActivity(self.settings.BlackholeTimeout) {
			return true
		}
	}
	return false
}

// windowMinSatisfied reports whether the window meets its minimum destination
// count, which is what the UI renders as connected rather than connecting.
//
// A warning marks a client the window should stop steering NEW flows to,
// because a better destination is expected to exist. A fixed-destination
// window — a user-selected network peer — has exactly one candidate and no
// substitute, so a warned sole destination that is still routing does not make
// the window unsatisfied: it is the only destination the minimum could ever be
// met by, and a replacement would be the same endpoint. Counting only unwarned
// clients there reported a live selected peer as "Connecting to providers" for
// its whole session.
func windowMinSatisfied(
	windowSizeMin int,
	clientCount int,
	warnedCount int,
	fixedDestination bool,
) bool {
	if fixedDestination {
		return windowSizeMin <= clientCount+warnedCount
	}
	return windowSizeMin <= clientCount
}

func (self *multiClientWindow) OrderedClients() []*multiClientChannel {
	var windowSize WindowSizeSettings
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if _, profileWindowSize, ok := self.performanceProfile.Load().FixedWindow(); ok {
			windowSize = profileWindowSize
		} else {
			windowSize = self.settings.WindowSizes[self.windowType]
		}
	}()

	clients := []*multiClientChannel{}
	lruTimes := map[*multiClientChannel]time.Time{}
	weights := map[*multiClientChannel]float32{}

	addClient := func(client *multiClientChannel, stats *clientWindowStats) {
		clients = append(clients, client)
		if !stats.lastEventTime.IsZero() {
			lruTimes[client] = stats.lastEventTime
		}
		weights[client] = float32(1 + stats.ExpectedByteCountPerSecond())
	}

	type warnedCandidate struct {
		client *multiClientChannel
		stats  *clientWindowStats
	}
	var warnedCandidates []warnedCandidate

	for _, client := range self.unorderedClients() {
		stats, err := client.WindowStats()
		if err != nil {
			continue
		}
		if client.isWarning() {
			// Retained only as a last resort for a fixed destination below.
			// A torn-down client is already excluded: WindowStats errors once
			// its own or its parent client's context is done.
			warnedCandidates = append(warnedCandidates, warnedCandidate{
				client: client,
				stats:  stats,
			})
			continue
		}
		addClient(client, stats)
	}

	if 0 == len(clients) && 0 < len(warnedCandidates) {
		// A warning steers NEW flows away from a client because a better
		// destination is expected to exist. An expanding window really does
		// have one — a replacement dials a different provider — so waiting for
		// it is correct there.
		//
		// A fixed-destination window has no such alternative: its replacement
		// is another client to the same endpoint, so excluding every warned
		// client leaves new flows with nowhere to go and stalls them on the
		// send retry cadence until one forms. For a user-selected network peer
		// that is strictly worse than using the warned peer that is still
		// routing. Only reached when no unwarned client exists, so a healthy
		// client always wins; `FixedDestinationSize` allocates, so it is
		// evaluated only on this path.
		if _, fixedDestination := self.generator.FixedDestinationSize(); fixedDestination {
			for _, candidate := range warnedCandidates {
				addClient(candidate.client, candidate.stats)
			}
		}
	}

	if 0 == len(clients) {
		return clients
	}

	if self.log.V(1).Enabled() {
		self.statsSampleWeights(weights)
	}

	if 0 < windowSize.FixedWindowSize && windowSize.FixedWindowSize < len(clients) {
		slices.SortFunc(clients, func(a *multiClientChannel, b *multiClientChannel) int {
			lruTimeA := lruTimes[a]
			lruTimeB := lruTimes[b]
			// descending
			if lruTimeA.Before(lruTimeB) {
				return 1
			} else if lruTimeB.Before(lruTimeA) {
				return -1
			}
			weightA := weights[a]
			weightB := weights[b]
			// descending
			if weightA < weightB {
				return 1
			} else if weightB < weightA {
				return -1
			}
			return 0
		})
		clients = clients[:windowSize.FixedWindowSize]
	}

	WeightedShuffleWithEntropy(clients, weights, self.settings.StatsWindowEntropy)

	// use only clients in the min tier
	// this prevents the window from crossing rank until necessary
	minTierClients := []*multiClientChannel{}
	minTier := clients[0].Tier()
	for _, client := range clients[1:] {
		minTier = min(minTier, client.Tier())
	}
	for _, client := range clients {
		if client.Tier() == minTier {
			minTierClients = append(minTierClients, client)
		} else {
			if self.log.V(1).Enabled() {
				self.log.Infof("[multi]exclude tier from window %d>%d\n", client.Tier(), minTier)
			}
		}
	}

	// use only the top n items from the window
	// if 0 < windowSize.WindowSizeUseMax {
	// 	minTierClients = minTierClients[:min(len(minTierClients), windowSize.WindowSizeUseMax)]
	// }

	return minTierClients
}

func (self *multiClientWindow) statsSampleWeights(weights map[*multiClientChannel]float32) {
	// randonly sample log statistics for weights
	if mathrand.Intn(self.settings.StatsSampleWeightsCount) == 0 {
		// sample the weights
		weightValues := slices.Collect(maps.Values(weights))
		slices.SortFunc(weightValues, func(a float32, b float32) int {
			// descending
			if a < b {
				return 1
			} else if b < a {
				return -1
			} else {
				return 0
			}
		})
		net := float32(0)
		for _, weight := range weightValues {
			net += weight
		}
		if 0 < net {
			var sb strings.Builder
			netThresh := float32(0.99)
			netp := float32(0)
			netCount := 0
			for i, weight := range weightValues {
				p := 100 * weight / net
				netp += p
				netCount += 1
				if 0 < i {
					sb.WriteString(" ")
				}
				sb.WriteString(fmt.Sprintf("[%d]%.2f", i, p))
				if netThresh*100 <= netp {
					break
				}
			}

			self.log.Infof("[multi]sample weights: %s (+%d more in window <%.0f%%)\n", sb.String(), len(weights)-netCount, 100*(1-netThresh))
		} else {
			self.log.Infof("[multi]sample weights: zero (%d in window)\n", len(weights))
		}
	}
}

func (self *multiClientWindow) Close() {
	var removedClients []*multiClientChannel
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, client := range self.clients {
			// client.Close()
			removedClients = append(removedClients, client)
		}
		clear(self.clients)
	}()
	for _, client := range removedClients {
		client.Close()
	}
	// self.removeClients(removedClients)
}

func (self *multiClientWindow) removeClients(removedClients ...*multiClientChannel) {
	for _, client := range removedClients {
		self.monitor.AddProviderEvent(client.ClientId(), ProviderStateRemoved)
	}
	for _, client := range removedClients {
		self.clientRemoveCallback(client)
	}
}

type multiClientChannelArgs struct {
	MultiClientGeneratorClientArgs

	Destination MultiHopId
	DestinationStats
}

type multiClientEventType int

const (
	multiClientEventTypeAck    multiClientEventType = 1
	multiClientEventTypeNack   multiClientEventType = 2
	multiClientEventTypeError  multiClientEventType = 3
	multiClientEventTypeSource multiClientEventType = 4
)

type multiClientEventBucket struct {
	createTime time.Time
	eventTime  time.Time

	sendAckCount        int
	sendAckByteCount    ByteCount
	sendNackCount       int
	sendNackByteCount   ByteCount
	sendSynCount        int
	receiveAckCount     int
	receiveAckByteCount ByteCount
	receiveSynCount     int
	sendAckTime         time.Time
	sendNackTime        time.Time
	sendSynTime         time.Time
	errs                []error
	ip4Paths            map[Ip4Path]bool
	ip6Paths            map[Ip6Path]bool
}

func newMultiClientEventBucket() *multiClientEventBucket {
	now := time.Now()
	return &multiClientEventBucket{
		createTime: now,
		eventTime:  now,
	}
}

type clientWindowStats struct {
	log Logger

	sourceCount                 int
	netSourceCount              int
	sendAckCount                int
	sendAckByteCount            ByteCount
	sendNackCount               int
	sendNackByteCount           ByteCount
	sendSynCount                int
	receiveAckCount             int
	receiveAckByteCount         ByteCount
	receiveSynCount             int
	ackByteCount                ByteCount
	windowDuration              time.Duration
	firstSendAckTime            time.Time
	firstSendNackTime           time.Time
	firstSendSynTime            time.Time
	estimatedByteCountPerSecond ByteCount
	// last-activity timestamps on the LIVE packetStats (not the windowed
	// snapshot), for the busy-flow liveness probe (see CPingBusyStaleTimeout):
	// lastSendTime is any send, lastSendAckTime is the last remote transfer
	// acknowledgement, lastReceiveAckTime is the last return IP packet, and
	// lastBusyProbeAckTime is the last positive control-probe result.
	// firstOutstandingSendTime is reset whenever all reliable sends are
	// acknowledged, so acked one-way traffic cannot become stale evidence.
	lastSendTime             time.Time
	lastSendAckTime          time.Time
	lastReceiveAckTime       time.Time
	lastBusyProbeAckTime     time.Time
	firstOutstandingSendTime time.Time
	// FIXME firstStatDuration
	clientDuration       time.Duration
	healthyDuration      time.Duration
	unhealthyDuration    time.Duration
	netHealthyDuration   time.Duration
	netUnhealthyDuration time.Duration
	healthy              bool
	removeTime           time.Time
	lastEventTime        time.Time

	// internal
	bucketCount int
}

func (self *clientWindowStats) EffectiveByteCountPerSecond() ByteCount {
	millis := int64(self.windowDuration / time.Millisecond)
	if millis <= 0 {
		return ByteCount(0)
	}
	netByteCount := int64(self.sendAckByteCount + self.receiveAckByteCount)
	return ByteCount((1000*netByteCount + millis/2) / millis)
}

func (self *clientWindowStats) EffectiveByteCount() (send ByteCount, receive ByteCount) {
	millis := int64(self.windowDuration / time.Millisecond)
	if millis <= 0 {
		return
	}
	send = self.sendAckByteCount
	receive = self.receiveAckByteCount
	return
}

func (self *clientWindowStats) ExpectedByteCountPerSecond() ByteCount {
	millis := int64(self.windowDuration / time.Millisecond)
	if millis <= 0 {
		return self.estimatedByteCountPerSecond
	}
	netByteCount := int64(self.sendAckByteCount + self.sendNackByteCount + self.receiveAckByteCount)
	if self.log.V(2).Enabled() {
		self.log.Infof("[multi]expected use estimated = %dbps (net = %db/%dms)\n", self.estimatedByteCountPerSecond, netByteCount, millis)
	}
	return max(
		self.estimatedByteCountPerSecond-ByteCount((1000*netByteCount+millis/2)/millis),
		0,
	)
}

type multiClientChannel struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	args *multiClientChannelArgs

	api *BringYourApi

	clientReceivePacketCallback clientReceivePacketFunction
	performanceProfile          *PerformanceProfile
	createTime                  time.Time

	settings *MultiClientSettings
	// optional owner of this client's platform transport. Captured once at
	// construction so ordinary receive frames do not pay a type assertion.
	transportMigrator MultiClientGeneratorTransportMigrator

	// sourceFilter map[TransferPath]bool

	client *Client

	stateLock    sync.Mutex
	eventBuckets []*multiClientEventBucket
	// destination -> source -> count
	ip4DestinationSourceCount          map[Ip4Path]map[Ip4Path]int
	ip6DestinationSourceCount          map[Ip6Path]map[Ip6Path]int
	packetStats                        *clientWindowStats
	endErr                             error
	maxEffectiveByteCountPerSecond     ByteCount
	maxEffectiveByteCountPerSecondTime time.Time
	firstEventTime                     time.Time

	healthy              bool
	lastHealthyTime      time.Time
	lastUnhealthyTime    time.Time
	netHealthyDuration   time.Duration
	netUnhealthyDuration time.Duration

	// affinityCount int
	// affinityTime  time.Time

	clientReceiveUnsub func()

	// lifetime count of return packets dropped at the parse gate, with
	// power-of-two log summaries (see clientReceive)
	receiveParseDropCount atomic.Uint64

	warning bool

	// suspect mirrors the busy-stale state (unacked sends with stale transfer
	// and return acknowledgements; a probe is outstanding or unanswered): new
	// flows are routed away from a suspect while any healthy client exists
	// (see orderClientsSuspectLast). Cleared the moment liveness returns.
	suspect atomic.Bool
	// windowHealth reports whether any OTHER window client shows recent
	// receive activity — the comparative signal that shortens the connect
	// blackhole timeout (a silent client next to passing siblings is its own
	// problem, not an outage). Set by the owning window after construction;
	// nil means no comparative signal (full timeout).
	windowHealth atomic.Pointer[func(exclude *multiClientChannel) bool]
}

func newMultiClientChannel(
	ctx context.Context,
	args *multiClientChannelArgs,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
	contractStatusCallback ContractStatusFunction,
	contractStatsCallback ContractStatsFunction,
	peerIdentityChangeCallback func(),
	performanceProfile *PerformanceProfile,
	networkPeerDestination bool,
	settings *MultiClientSettings,
) (*multiClientChannel, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientSettings := generator.NewClientSettings()
	clientSettings.SendBufferSettings.AckTimeout = settings.AckTimeout
	// This client is dedicated to the selected destination. Stamp every send
	// with the authenticated relationship before NewClient can emit its
	// initial ping; the platform's NetworkPeers batch may arrive later.
	clientSettings.DefaultTransferOpts.NetworkPeer = networkPeerDestination
	if performanceProfile != nil && performanceProfile.PostQuantumEncryption {
		// pqe: opportunistic per-peer e2e sessions (post-quantum key
		// exchange). A provider without session support falls back to
		// plaintext at this layer.
		if clientSettings.EncryptionSettings == nil {
			clientSettings.EncryptionSettings = DefaultEncryptionSettings()
		}
		clientSettings.EncryptionSettings.Encrypt = true
	}

	var client *Client
	var err error
	if contextGenerator, ok := generator.(MultiClientGeneratorContext); ok {
		callCtx := context.Context(cancelCtx)
		cancelCall := func() {}
		if 0 < settings.WindowClientCreateTimeout {
			callCtx, cancelCall = context.WithTimeout(cancelCtx, settings.WindowClientCreateTimeout)
		}
		client, err = contextGenerator.NewClientContext(
			cancelCtx,
			callCtx,
			&args.MultiClientGeneratorClientArgs,
			clientSettings,
		)
		cancelCall()
	} else {
		client, err = generator.NewClient(
			cancelCtx,
			&args.MultiClientGeneratorClientArgs,
			clientSettings,
		)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	// The selected destination is authenticated by the app's explicit Network
	// relationship before this client exists. Mark it before the initial ping
	// can open a P2P stream. Waiting for the first incoming Network signal is
	// too late on the active side: peer-connection admission happens while
	// constructing the outbound offer, so a full public pool can refuse the
	// selected peer before any trusted signal has a chance to return.
	if networkPeerDestination && 0 < args.Destination.Len() {
		client.webRtcManager.PrioritizePeer(args.Destination.Tail())
	}
	contractStatusSub := client.ContractManager().AddContractStatusCallback(contractStatusCallback)
	contractStatsSub := client.ContractManager().AddContractStatsCallback(contractStatsCallback)
	peerIdentitySub := client.EncryptionSessionManager().AddPeerIdentityChangeCallback(peerIdentityChangeCallback)
	go HandleError(func() {
		select {
		case <-cancelCtx.Done():
		case <-client.Done():
		}
		// The channel/client contexts are already canceled at this point.
		// Deregister the platform identity before synchronous local teardown:
		// Pion close can be slow, and making RemoveClientWithArgs wait behind it
		// leaves the remote provider's StreamOpen/P2P state alive while also
		// parking the multi-client resize loop that initiated retirement.
		contractStatusSub()
		peerIdentitySub()
		generator.RemoveClientWithArgs(client, &args.MultiClientGeneratorClientArgs)
		client.Cancel()

		// fire the contract-close events for this client's still-open contracts
		// while the stats listener below is still attached, BEFORE cancelling the
		// stats listener. (The channel/client may already be cancelled—that is
		// what wakes this cleanup—but CloseAllContractStats is the deterministic
		// synchronous backstop.) Otherwise a removed peer's contracts linger open
		// forever in the contract-details UI.
		client.CloseContractStats()
		contractStatsSub()
		// the removed client's established peers leave the aggregate set
		peerIdentityChangeCallback()
	}, cancel)

	// sourceFilter := map[TransferPath]bool{
	//     Path{ClientId:args.DestinationId}: true,
	// }

	clientChannel := &multiClientChannel{
		ctx:                         cancelCtx,
		cancel:                      cancel,
		log:                         loggerOrDefault(settings.Log),
		args:                        args,
		clientReceivePacketCallback: clientReceivePacketCallback,
		performanceProfile:          performanceProfile,
		createTime:                  time.Now(),
		settings:                    settings,
		transportMigrator: func() MultiClientGeneratorTransportMigrator {
			migrator, _ := generator.(MultiClientGeneratorTransportMigrator)
			return migrator
		}(),
		// sourceFilter: sourceFilter,
		client:                    client,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: loggerOrDefault(settings.Log)},
		// affinityCount:             0,
		// affinityTime:              time.Time{},
	}
	go HandleError(clientChannel.detectBlackhole, cancel)
	go HandleError(clientChannel.ping, cancel)

	clientReceiveUnsub := client.AddReceiveCallback(clientChannel.clientReceive)
	clientChannel.clientReceiveUnsub = clientReceiveUnsub

	return clientChannel, nil
}

func (self *multiClientChannel) ClientId() Id {
	return self.client.ClientId()
}

func (self *multiClientChannel) IsP2pOnly() bool {
	return self.args.MultiClientGeneratorClientArgs.P2pOnly
}

func (self *multiClientChannel) Tier() int {
	return self.args.DestinationStats.Tier
}

func (self *multiClientChannel) EstimatedByteCountPerSecond() ByteCount {
	return self.args.EstimatedBytesPerSecond
}

func (self *multiClientChannel) setWarning(warning bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.warning = warning
}

func (self *multiClientChannel) isWarning() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.warning
}

// func (self *multiClientChannel) UpdateAffinity() {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	self.affinityCount += 1
// 	self.affinityTime = time.Now()
// }

// func (self *multiClientChannel) ClearAffinity() {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	self.affinityCount = 0
// 	self.affinityTime = time.Time{}
// }

// func (self *multiClientChannel) MostRecentAffinity() (int, time.Time) {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	return self.affinityCount, self.affinityTime
// }

func (self *multiClientChannel) Send(parsedPacket *parsedPacket, timeout time.Duration) bool {
	success, err := self.SendDetailed(parsedPacket, timeout)
	return success && err == nil
}

func (self *multiClientChannel) SendDetailed(parsedPacket *parsedPacket, timeout time.Duration) (bool, error) {
	return self.sendPacketDetailed(parsedPacket.packet, parsedPacket.ipPath, timeout)
}

func (self *multiClientChannel) sendPacketDetailed(packet []byte, ipPath *IpPath, timeout time.Duration) (bool, error) {
	var ack bool
	switch ipPath.Protocol {
	case IpProtocolUdp, IpProtocolIcmp:
		// icmp echo is datagram-like and a measurement tool: unacked
		// transfer lets tunnel loss show honestly as ping loss
		if self.settings.UdpCollapsePrevention {
			ack = false
		} else {
			ack = true
		}
	default:
		ack = true
	}
	return self.sendPacketDetailedWithAck(packet, ipPath, timeout, ack)
}

func (self *multiClientChannel) SendWithAck(parsedPacket *parsedPacket, timeout time.Duration, ack bool) bool {
	success, err := self.SendDetailedWithAck(parsedPacket, timeout, ack)
	return success && err == nil
}

func (self *multiClientChannel) SendDetailedWithAck(parsedPacket *parsedPacket, timeout time.Duration, ack bool) (bool, error) {
	return self.sendPacketDetailedWithAck(parsedPacket.packet, parsedPacket.ipPath, timeout, ack)
}

func (self *multiClientChannel) sendPacketDetailedWithAck(
	packet []byte,
	ipPath *IpPath,
	timeout time.Duration,
	ack bool,
) (bool, error) {
	rawFrame := 2 <= self.settings.ProtocolVersion
	var frame *protocol.Frame
	if !rawFrame {
		var err error
		frame, err = ipPacketToProviderFrame(packet, self.settings.ProtocolVersion)
		if err != nil {
			self.addError(err)
			return false, err
		}
	}

	packetByteCount := ByteCount(len(packet))
	self.addSend(packetByteCount, ipPath)

	var opts []any
	if self.performanceProfile != nil && self.performanceProfile.AllowDirect {
		opts = append(opts, ForceStream())
	}
	if !ack {
		opts = append(opts, NoAck())
	}
	var success bool
	var err error
	if rawFrame {
		success, err = self.client.sendRawMultiHopWithTimeoutDetailed(
			protocol.MessageType_IpIpPacketToProvider,
			packet,
			self.args.Destination,
			self,
			packetByteCount,
			timeout,
			opts...,
		)
	} else {
		// Legacy protocol frames use the public callback API. Modern raw frames
		// carry self and packetByteCount as a sendAckRecord and allocate no
		// closure on the steady-state packet path.
		ackCallback := func(err error) {
			self.sendAckResult(packetByteCount, err)
		}
		success, err = self.client.SendMultiHopWithTimeoutDetailed(
			frame,
			self.args.Destination,
			ackCallback,
			timeout,
			opts...,
		)
	}
	if !success {
		// addSend reserves one outstanding-send record before enqueue because a
		// no-ack write may invoke its completion inline. The transfer ownership
		// contract guarantees that an unsuccessful enqueue invokes no callback,
		// so release that reservation here. Otherwise one transient full queue
		// leaves permanent stale evidence and later removes a healthy route as a
		// blackhole.
		self.addSendReject(packetByteCount)
	}
	// ownership: `packet` is consumed on success and stays with the
	// caller on any failure. The wrapped (!raw) marshal buffer is internal and
	// must be freed on any failure; for raw frames the frame bytes ARE the
	// caller's packet, so they are never freed here on failure.
	if err != nil {
		if !rawFrame {
			MessagePoolReturn(frame.MessageBytes)
		}
		return success, err
	}
	if success {
		if !rawFrame {
			MessagePoolReturn(packet)
		}
	} else if !rawFrame {
		MessagePoolReturn(frame.MessageBytes)
	}
	return success, err
}

func (self *multiClientChannel) SendDetailedMessage(message proto.Message, timeout time.Duration, ackCallback func(error)) (bool, error) {
	if frame, err := ToFrame(message, self.settings.ProtocolVersion); err != nil {
		return false, err
	} else {
		var opts []any
		if self.performanceProfile != nil && self.performanceProfile.AllowDirect {
			opts = append(opts, ForceStream())
		}
		return self.client.SendMultiHopWithTimeoutDetailed(
			frame,
			self.args.Destination,
			ackCallback,
			timeout,
			opts...,
		)
	}
}

func (self *multiClientChannel) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *multiClientChannel) Destination() MultiHopId {
	return self.args.Destination
}

func schedulerPauseDetected(start time.Time, expected time.Duration, tolerance time.Duration) bool {
	if start.IsZero() || expected <= 0 || tolerance <= 0 {
		return false
	}
	return expected+tolerance < time.Since(start)
}

func (self *multiClientChannel) detectBlackhole() {
	// within a timeout window, if there are sent data but none received,
	// error out. This is similar to an ack timeout.
	defer self.cancel()

	pollTimeout := self.settings.BlackholeTimeout / 4
	if pollTimeout <= 0 {
		pollTimeout = time.Second
	}
	lastPollTime := time.Now()
	var recoveryUntil time.Time

	for {
		now := time.Now()
		if schedulerPauseDetected(lastPollTime, pollTimeout, self.settings.SchedulerPauseTolerance) {
			recoveryTimeout := self.settings.SchedulerPauseRecoveryTimeout
			if recoveryTimeout < pollTimeout {
				recoveryTimeout = pollTimeout
			}
			recoveryUntil = now.Add(recoveryTimeout)
			self.log.V(1).Infof("[multi]scheduler pause detected; defer stale blackhole evidence for %s\n", recoveryTimeout)
		}
		lastPollTime = now

		if windowStats, err := self.WindowStats(); err != nil {
			return
		} else {
			blackhole := !time.Now().Before(recoveryUntil) &&
				self.isBlackholeAt(windowStats, time.Now())

			if blackhole {
				// the client has sent data but received nothing back
				// this looks like a blackhole
				if self.log.V(1).Enabled() {
					self.log.Infof("[multi]routing %s blackhole: %d %dB <> %d %dB (%d <> %d)\n",
						self.args.Destination,
						windowStats.sendAckCount,
						windowStats.sendAckByteCount,
						windowStats.receiveAckCount,
						windowStats.receiveAckByteCount,
						windowStats.sendSynCount,
						windowStats.receiveSynCount,
					)
				}
				self.addError(fmt.Errorf("Blackhole (%d %dB)",
					windowStats.sendAckCount,
					windowStats.sendAckByteCount,
				))
				return
			} else {
				if self.log.V(1).Enabled() {
					self.log.Infof(
						"[multi]routing ok %s: %d %dB <> %d %dB (%d <> %d)\n",
						self.args.Destination,
						windowStats.sendAckCount,
						windowStats.sendAckByteCount,
						windowStats.receiveAckCount,
						windowStats.receiveAckByteCount,
						windowStats.sendSynCount,
						windowStats.receiveSynCount,
					)
				}
			}

			select {
			case <-self.ctx.Done():
				return
			case <-self.client.Done():
				return
			case <-time.After(pollTimeout):
			}
		}
	}
}

// isBlackholeAt classifies only evidence that requires a response. A remote
// transfer acknowledgement is proof that an otherwise one-way IP flow reached
// the peer, so the absence of a return IP packet alone is not a blackhole.
//
// When the busy-flow liveness probe is enabled it exclusively owns stale
// reliable-send classification: its positive acknowledgement distinguishes a
// live congested/backpressured peer from a dead one. Running the historical
// passive timeout at the same stale boundary used to cancel the channel just
// as ping began that probe. Keep the passive path only as the compatibility
// fallback when active probing is disabled. TCP SYNs with no SYN response
// retain their independent bounded detection path in either mode.
func (self *multiClientChannel) isBlackholeAt(windowStats *clientWindowStats, now time.Time) bool {
	if self.busyStaleTimeout() <= 0 &&
		!windowStats.firstOutstandingSendTime.IsZero() {
		lastProgressTime := windowStats.firstOutstandingSendTime
		if lastProgressTime.Before(windowStats.lastSendAckTime) {
			lastProgressTime = windowStats.lastSendAckTime
		}
		if lastProgressTime.Before(windowStats.lastReceiveAckTime) {
			lastProgressTime = windowStats.lastReceiveAckTime
		}
		if lastProgressTime.Before(windowStats.lastBusyProbeAckTime) {
			lastProgressTime = windowStats.lastBusyProbeAckTime
		}
		if self.settings.BlackholeTimeout <= now.Sub(lastProgressTime) {
			return true
		}
	}

	// Comparative signal: when sibling window clients are passing traffic, a
	// silent first connect here is this path's own problem, not an outage.
	connectTimeout := self.settings.BlackholeConnectTimeout
	if 0 < self.settings.BlackholeConnectComparativeTimeout &&
		self.settings.BlackholeConnectComparativeTimeout < connectTimeout {
		if windowHealth := self.windowHealth.Load(); windowHealth != nil && (*windowHealth)(self) {
			connectTimeout = self.settings.BlackholeConnectComparativeTimeout
		}
	}
	return !windowStats.firstSendSynTime.IsZero() &&
		connectTimeout <= now.Sub(windowStats.firstSendSynTime) &&
		windowStats.receiveSynCount <= 0
}

// busyStale reports whether an active or outstanding flow's transfer and
// return acknowledgements have stopped making progress — the dead-peer signal
// the idle-only ping misses. A transfer acknowledgement is positive peer
// liveness even for a one-way upload; ignoring it makes a continuously-full
// reliable-send window look stale forever and launches destructive probes
// against a peer that is demonstrably alive.
// A flow with no response history may be legitimately one-way and is left to
// the reliable-send and TCP-connect paths.
// busyStaleTimeout returns the effective busy-stale window: the configured
// CPingBusyStaleTimeout, scaled by DegradedLivenessScale while the host
// reports degraded performance (low power mode, thermal throttling, a weak
// or constrained network). A degraded device answers control pings slowly,
// and a false removal (flow RSTs + reconnect churn) costs far more than the
// extra detection latency. <= 0 keeps the probe disabled.
func (self *multiClientChannel) busyStaleTimeout() time.Duration {
	timeout := self.settings.CPingBusyStaleTimeout
	if timeout <= 0 {
		return timeout
	}
	if degraded := self.settings.DegradedMode; degraded != nil && degraded.Load() {
		if scale := self.settings.DegradedLivenessScale; 1 < scale {
			timeout = time.Duration(float64(timeout) * scale)
		}
	}
	return timeout
}

// idlePingRestTimeout returns the effective rest between idle continuous
// pings: CPingRestTimeout (CPingTimeout when unset), scaled by
// DegradedLivenessScale while the host reports degraded performance. A host
// in low power mode or on a constrained network wants fewer radio wakeups
// from an idle tunnel; the slower idle dead-peer detection is the same
// deliberate trade busyStaleTimeout makes — a false removal costs more than
// late detection — and a busy flow still gets the fast busy-stale probe.
func (self *multiClientChannel) idlePingRestTimeout() time.Duration {
	restTimeout := self.settings.CPingRestTimeout
	if restTimeout <= 0 {
		restTimeout = self.settings.CPingTimeout
	}
	if degraded := self.settings.DegradedMode; degraded != nil && degraded.Load() {
		if scale := self.settings.DegradedLivenessScale; 1 < scale {
			restTimeout = time.Duration(float64(restTimeout) * scale)
		}
	}
	return restTimeout
}

func (self *multiClientChannel) busyStale() bool {
	busyStaleTimeout := self.busyStaleTimeout()
	if busyStaleTimeout <= 0 {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	lastSendTime := self.packetStats.lastSendTime
	outstanding := 0 < self.packetStats.sendNackCount &&
		!self.packetStats.firstOutstandingSendTime.IsZero()
	lastLivenessTime := self.packetStats.lastReceiveAckTime
	if lastLivenessTime.Before(self.packetStats.lastSendAckTime) {
		lastLivenessTime = self.packetStats.lastSendAckTime
	}
	if lastLivenessTime.Before(self.packetStats.lastBusyProbeAckTime) {
		lastLivenessTime = self.packetStats.lastBusyProbeAckTime
	}
	if lastLivenessTime.IsZero() {
		if !outstanding {
			return false
		}
		lastLivenessTime = self.packetStats.firstOutstandingSendTime
	}
	now := time.Now()
	busy := (!lastSendTime.IsZero() &&
		now.Sub(lastSendTime) < busyStaleTimeout) ||
		outstanding
	if !busy {
		return false
	}
	return busyStaleTimeout <= now.Sub(lastLivenessTime)
}

func (self *multiClientChannel) addBusyProbeAck() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.packetStats.lastBusyProbeAckTime = time.Now()
}

// IsSuspect reports whether the channel is currently busy-stale (unacked
// sends with stale return acks; liveness probe outstanding or unanswered).
// New flows prefer non-suspect clients.
func (self *multiClientChannel) IsSuspect() bool {
	return self.suspect.Load()
}

// SetWindowHealth installs the comparative window-health signal (see the
// windowHealth field). Called by the owning window right after construction.
func (self *multiClientChannel) SetWindowHealth(windowHealth func(exclude *multiClientChannel) bool) {
	self.windowHealth.Store(&windowHealth)
}

// recentReceiveActivity reports whether this channel received return traffic
// within the window — the per-sibling half of the comparative health signal.
func (self *multiClientChannel) recentReceiveActivity(window time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	t := self.packetStats.lastReceiveAckTime
	return !t.IsZero() && time.Since(t) < window
}

func (self *multiClientChannel) ping() {
	defer self.cancel()

	// the busy-flow probe polls staleness on a fine cadence decoupled from
	// the idle-ping rest, so a mid-transfer death is caught within
	// ~CPingBusyStaleTimeout + the probe ack wait regardless of the rest
	// interval; the idle ping stays rate-limited to the degraded-aware rest
	// (idlePingRestTimeout).
	var lastIdlePingTime time.Time
	// consecutive busy-stale probes that could not even be queued (the send
	// path is wedged full of the same unacked data the probe is
	// investigating); see the probe send-failure handling below
	busyProbeSendFailures := 0
	for {
		windowStats, err := self.WindowStats()
		if err != nil {
			return
		}
		idle := self.settings.CPingMaxByteCountPerSecond == 0 || windowStats.EffectiveByteCountPerSecond() <= self.settings.CPingMaxByteCountPerSecond

		restTimeout := self.idlePingRestTimeout()

		// probe when idle (the historical continuous ping, rate-limited to the
		// rest interval) OR when busy but the return acks have gone stale (the
		// active liveness probe — see CPingBusyStaleTimeout)
		now := time.Now()
		// Staleness is based on actual recent sends, not the long-window
		// throughput classifier. The latter can label a newly active flow idle
		// (its short burst is diluted across StatsWindowDuration), which used
		// to suppress the fast probe entirely in the recovery kernel.
		busyStale := self.busyStale()
		// mirror the stale state for new-flow routing: a suspect client is
		// skipped by the flow race while any healthy sibling exists
		self.suspect.Store(busyStale)
		if !busyStale {
			// "Consecutive" is scoped to one stale episode. Without this
			// reset, one transient queue-full result could survive a healthy
			// interval and make the next episode cancel after its first
			// unsendable probe.
			busyProbeSendFailures = 0
		}
		idlePing := !busyStale && idle &&
			(lastIdlePingTime.IsZero() || restTimeout <= now.Sub(lastIdlePingTime))
		if idlePing || busyStale {
			if idlePing {
				lastIdlePingTime = now
			}
			// a busy-stale probe confirms liveness on a snappy budget: a live
			// peer answers a control ping within a couple rtts, so waiting the
			// full idle CPingTimeout would defeat the point (fast detection).
			// A live peer here just continues; a dead one is errored.
			pingAckTimeout := self.settings.CPingTimeout
			writeTimeout := self.settings.CPingWriteTimeout
			if busyStale {
				// degraded-aware: on a slow host the probe budgets stretch with
				// the stale window (see busyStaleTimeout)
				busyStaleTimeout := self.busyStaleTimeout()
				if busyStaleTimeout < pingAckTimeout {
					pingAckTimeout = busyStaleTimeout
				}
				// the probe write must fail fast when the send queue is wedged
				// full of the same unacked data the probe is investigating —
				// blocking the loop for the full CPingWriteTimeout here is what
				// used to push detection out to the slower paths
				probeWriteTimeout := max(time.Second, busyStaleTimeout/4)
				if probeWriteTimeout < writeTimeout {
					writeTimeout = probeWriteTimeout
				}
			}
			pingDone := make(chan error)
			success, err := self.SendDetailedMessage(
				&protocol.IpPing{},
				writeTimeout,
				func(err error) {
					defer close(pingDone)
					select {
					case <-self.ctx.Done():
						return
					case pingDone <- err:
					}
				},
			)
			if err != nil || !success {
				close(pingDone)
				if !busyStale {
					// historical idle behavior: exit (the defer cancels the channel)
					return
				}
				// the busy-stale probe could not even be queued while unacked
				// data sits stale. A live congested peer drains the queue
				// between polls (the next probe then queues); a dead one never
				// does — err after a couple consecutive failures instead of
				// exiting silently and leaving detection to the slower paths.
				busyProbeSendFailures += 1
				if 2 <= busyProbeSendFailures {
					self.addError(fmt.Errorf("busy-stale liveness probe unsendable"))
					return
				}
			} else {
				busyProbeSendFailures = 0
				pingWaitStart := time.Now()
				pingTimer := time.NewTimer(pingAckTimeout)
			waitPing:
				for {
					select {
					case <-self.ctx.Done():
						pingTimer.Stop()
						return
					case <-self.client.Done():
						pingTimer.Stop()
						return
					case err := <-pingDone:
						pingTimer.Stop()
						if err != nil {
							self.addError(err)
							return
						}
						if busyStale {
							// A positive control ack is liveness even when the
							// application flow is legitimately one-way. Remember it
							// so the poll loop does not re-probe every cadence.
							self.addBusyProbeAck()
						}
						break waitPing
					case <-pingTimer.C:
						if schedulerPauseDetected(
							pingWaitStart,
							pingAckTimeout,
							self.settings.SchedulerPauseTolerance,
						) {
							// A process/scheduler pause deprived both the peer
							// ack and this waiter of CPU. Keep the SAME probe and
							// give it one fresh normal budget after resume.
							pingWaitStart = time.Now()
							pingTimer.Reset(pingAckTimeout)
							continue
						}
						// probe unanswered within the budget: for an idle ping this
						// is the historical timeout->cancel; for a busy-stale probe
						// it confirms the peer is dead (error so resize removes it
						// and the flow re-races to a survivor)
						if busyStale {
							self.addError(fmt.Errorf("busy-stale liveness probe timed out"))
						}
						return
					}
				}
			}
		}

		// when the busy-flow probe is enabled, poll staleness on a fine
		// cadence (a fraction of its window, capped) so detection latency is
		// ~CPingBusyStaleTimeout + probe wait rather than a full rest later.
		// The idle ping is unaffected — it self-rate-limits via lastIdlePingTime.
		// Degraded-aware: the poll stretches with the scaled window.
		pollTimeout := restTimeout
		if busyStaleTimeout := self.busyStaleTimeout(); 0 < busyStaleTimeout {
			busyPoll := busyStaleTimeout / 4
			if busyPoll < time.Second {
				busyPoll = time.Second
			}
			if busyPoll < pollTimeout {
				pollTimeout = busyPoll
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.client.Done():
			return
		case <-WakeupAfter(pollTimeout, pollTimeout):
		}
	}
}

// addSend records the per-packet send stats (nack, optional syn, and source)
// in a single locked section, so the hot send path takes the channel lock and
// resolves the event bucket once per packet instead of two-three times.
func (self *multiClientChannel) addSend(packetByteCount ByteCount, ipPath *IpPath) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	eventBucket := self.eventBucket()
	now := time.Now()

	if self.packetStats.sendNackCount <= 0 {
		self.packetStats.firstOutstandingSendTime = now
	}
	self.packetStats.sendNackCount += 1
	self.packetStats.sendNackByteCount += packetByteCount
	self.packetStats.lastSendTime = now
	if eventBucket.sendNackCount == 0 {
		eventBucket.sendNackTime = now
	}
	eventBucket.sendNackCount += 1
	eventBucket.sendNackByteCount += packetByteCount

	if ipPath.Syn {
		self.packetStats.sendSynCount += 1
		if eventBucket.sendSynCount == 0 {
			eventBucket.sendSynTime = now
		}
		eventBucket.sendSynCount += 1
	}

	self.addSourceToEventBucketWithLock(eventBucket, ipPath)
}

func (self *multiClientChannel) addSendNack(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	if self.packetStats.sendNackCount <= 0 {
		self.packetStats.firstOutstandingSendTime = now
	}
	self.packetStats.sendNackCount += 1
	self.packetStats.sendNackByteCount += ackByteCount
	self.packetStats.lastSendTime = now

	eventBucket := self.eventBucket()
	if eventBucket.sendNackCount == 0 {
		eventBucket.sendNackTime = now
	}
	eventBucket.sendNackCount += 1
	eventBucket.sendNackByteCount += ackByteCount
}

// addSendReject releases the live outstanding-send reservation for a packet
// that the transfer queue did not accept. The event bucket retains the failed
// attempt for window health, but it cannot become remote-liveness evidence.
func (self *multiClientChannel) addSendReject(packetByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendNackCount -= 1
	self.packetStats.sendNackByteCount -= packetByteCount
	if self.packetStats.sendNackCount <= 0 {
		self.packetStats.sendNackCount = 0
		self.packetStats.sendNackByteCount = 0
		self.packetStats.firstOutstandingSendTime = time.Time{}
	}
}

func (self *multiClientChannel) addSendAck(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	self.packetStats.sendNackCount -= 1
	self.packetStats.sendNackByteCount -= ackByteCount
	self.packetStats.sendAckCount += 1
	self.packetStats.sendAckByteCount += ackByteCount
	self.packetStats.lastSendAckTime = now
	if self.packetStats.sendNackCount <= 0 {
		self.packetStats.firstOutstandingSendTime = time.Time{}
	}

	eventBucket := self.eventBucket()
	if eventBucket.sendAckCount == 0 {
		eventBucket.sendAckTime = now
	}
	eventBucket.sendAckCount += 1
	eventBucket.sendAckByteCount += ackByteCount
}

func (self *multiClientChannel) sendAckResult(packetByteCount ByteCount, err error) {
	if err == nil {
		self.addSendAck(packetByteCount)
	} else {
		self.addError(err)
	}
}

func (self *multiClientChannel) addSendSyn(synCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendSynCount += synCount

	eventBucket := self.eventBucket()
	if eventBucket.sendSynCount == 0 {
		eventBucket.sendSynTime = time.Now()
	}
	eventBucket.sendSynCount += synCount
}

func (self *multiClientChannel) addReceiveAck(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.receiveAckCount += 1
	self.packetStats.receiveAckByteCount += ackByteCount
	self.packetStats.lastReceiveAckTime = time.Now()

	eventBucket := self.eventBucket()
	eventBucket.receiveAckCount += 1
	eventBucket.receiveAckByteCount += ackByteCount
}

func (self *multiClientChannel) addReceiveSyn(synCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.receiveSynCount += synCount

	eventBucket := self.eventBucket()
	eventBucket.receiveSynCount += synCount
}

func (self *multiClientChannel) addSource(ipPath *IpPath) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.addSourceToEventBucketWithLock(self.eventBucket(), ipPath)
}

// must be called with `stateLock`
func (self *multiClientChannel) addSourceToEventBucketWithLock(eventBucket *multiClientEventBucket, ipPath *IpPath) {
	// `ip{4,6}DestinationSourceCount[destination][source]` is a reference count
	// of how many event buckets currently hold the path. `removeEventBucket`
	// decrements it once per path in the bucket's set, so the increment here
	// must also happen exactly once per (bucket, path) — i.e. only when the path
	// is newly added to this bucket's set. doing it on every packet (the prior
	// behavior) both over-counted (the count never returned to zero, so sources
	// were never released — unbounded growth) and did redundant per-packet map
	// writes under the lock. after the first packet of a flow in a bucket, this
	// is a single map read and no writes.
	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()

		if eventBucket.ip4Paths == nil {
			eventBucket.ip4Paths = map[Ip4Path]bool{}
		}
		if eventBucket.ip4Paths[ip4Path] {
			return
		}
		eventBucket.ip4Paths[ip4Path] = true

		source := ip4Path.Source()
		destination := ip4Path.Destination()

		sourceCount, ok := self.ip4DestinationSourceCount[destination]
		if !ok {
			sourceCount = map[Ip4Path]int{}
			self.ip4DestinationSourceCount[destination] = sourceCount
		}
		sourceCount[source] += 1
	case 6:
		ip6Path := ipPath.ToIp6Path()

		if eventBucket.ip6Paths == nil {
			eventBucket.ip6Paths = map[Ip6Path]bool{}
		}
		if eventBucket.ip6Paths[ip6Path] {
			return
		}
		eventBucket.ip6Paths[ip6Path] = true

		source := ip6Path.Source()
		destination := ip6Path.Destination()

		sourceCount, ok := self.ip6DestinationSourceCount[destination]
		if !ok {
			sourceCount = map[Ip6Path]int{}
			self.ip6DestinationSourceCount[destination] = sourceCount
		}
		sourceCount[source] += 1
	default:
		panic(fmt.Errorf("Bad protocol version %d", ipPath.Version))
	}
}

func (self *multiClientChannel) addError(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.endErr == nil {
		self.endErr = err
	}

	eventBucket := self.eventBucket()
	eventBucket.errs = append(eventBucket.errs, err)
}

// must be called with `stateLock`
func (self *multiClientChannel) eventBucket() *multiClientEventBucket {
	now := time.Now()

	var eventBucket *multiClientEventBucket
	if n := len(self.eventBuckets); 0 < n {
		eventBucket = self.eventBuckets[n-1]
	}

	if eventBucket == nil || eventBucket.createTime.Add(self.settings.StatsWindowBucketDuration).Before(now) {
		eventBucket = newMultiClientEventBucket()
		self.eventBuckets = append(self.eventBuckets, eventBucket)
	}

	eventBucket.eventTime = now

	self.coalesceEventBuckets()

	return eventBucket
}

// must be called with `stateLock`
func (self *multiClientChannel) coalesceEventBuckets() {
	// if there is no activity (no new buckets), keep historical buckets around
	minBucketCount := 1 + int(self.settings.StatsWindowDuration/self.settings.StatsWindowBucketDuration)

	windowStart := time.Now().Add(-self.settings.StatsWindowDuration)

	removeEventBucket := func(eventBucket *multiClientEventBucket) {
		self.packetStats.sendAckCount -= eventBucket.sendAckCount
		self.packetStats.sendAckByteCount -= eventBucket.sendAckByteCount
		self.packetStats.sendSynCount -= eventBucket.sendSynCount
		self.packetStats.receiveAckCount -= eventBucket.receiveAckCount
		self.packetStats.receiveAckByteCount -= eventBucket.receiveAckByteCount
		self.packetStats.receiveSynCount -= eventBucket.receiveSynCount

		for ip4Path, _ := range eventBucket.ip4Paths {
			source := ip4Path.Source()
			destination := ip4Path.Destination()

			sourceCount, ok := self.ip4DestinationSourceCount[destination]
			if ok {
				count := sourceCount[source]
				if count-1 <= 0 {
					delete(sourceCount, source)
				} else {
					sourceCount[source] = count - 1
				}
				if len(sourceCount) == 0 {
					delete(self.ip4DestinationSourceCount, destination)
				}
			}
		}

		for ip6Path, _ := range eventBucket.ip6Paths {
			source := ip6Path.Source()
			destination := ip6Path.Destination()

			sourceCount, ok := self.ip6DestinationSourceCount[destination]
			if ok {
				count := sourceCount[source]
				if count-1 <= 0 {
					delete(sourceCount, source)
				} else {
					sourceCount[source] = count - 1
				}
				if len(sourceCount) == 0 {
					delete(self.ip6DestinationSourceCount, destination)
				}
			}
		}
	}

	// remove all events before the window start
	i := 0
	for i < len(self.eventBuckets) && self.eventBuckets[i].eventTime.Before(windowStart) {
		removeEventBucket(self.eventBuckets[i])
		self.eventBuckets[i] = nil
		i += 1
	}
	for i < len(self.eventBuckets) && minBucketCount < len(self.eventBuckets) {
		removeEventBucket(self.eventBuckets[i])
		self.eventBuckets[i] = nil
		i += 1
	}
	if 0 < i {
		self.eventBuckets = self.eventBuckets[i:]
	}
}

func (self *multiClientChannel) WindowStats() (*clientWindowStats, error) {
	return self.windowStatsWithCoalesce(true)
}

func (self *multiClientChannel) windowStatsWithCoalesce(coalesce bool) (*clientWindowStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if coalesce {
		self.coalesceEventBuckets()
	}

	// omit the latest two event buckets since they may be partial
	var eventBuckets []*multiClientEventBucket
	if 2 <= len(self.eventBuckets) {
		eventBuckets = self.eventBuckets[0 : len(self.eventBuckets)-2]
	}

	windowDuration := time.Duration(0)
	if 0 < len(eventBuckets) {
		endTime := eventBuckets[len(eventBuckets)-1].eventTime
		windowDuration = endTime.Sub(eventBuckets[0].createTime)
	}
	var firstSendAckTime time.Time
	for _, eventBucket := range eventBuckets {
		if 0 < eventBucket.sendAckCount {
			firstSendAckTime = eventBucket.sendAckTime
			break
		}
	}
	var firstSendNackTime time.Time
	for _, eventBucket := range eventBuckets {
		if 0 < eventBucket.sendNackCount {
			firstSendNackTime = eventBucket.sendNackTime
			break
		}
	}
	var firstSendSynTime time.Time
	for _, eventBucket := range eventBuckets {
		if 0 < eventBucket.sendSynCount {
			firstSendSynTime = eventBucket.sendSynTime
			break
		}
	}

	// public internet resource ports
	isPublicPort := func(port int) bool {
		switch port {
		case 443:
			return true
		default:
			return false
		}
	}

	netSourceCounts := []int{}
	for ip4Path, sourceCounts := range self.ip4DestinationSourceCount {
		if isPublicPort(ip4Path.DestinationPort) {
			netSourceCounts = append(netSourceCounts, len(sourceCounts))
		}
	}
	for ip6Path, sourceCounts := range self.ip6DestinationSourceCount {
		if isPublicPort(ip6Path.DestinationPort) {
			netSourceCounts = append(netSourceCounts, len(sourceCounts))
		}
	}
	slices.Sort(netSourceCounts)
	maxSourceCount := 0
	selectionIndex := int(math.Ceil(
		self.settings.StatsSourceCountSelection * float64(len(netSourceCounts)-1),
	))
	if selectionIndex < len(netSourceCounts) {
		maxSourceCount = netSourceCounts[selectionIndex]
	}
	netSourceCount := 0
	for _, sourceCounts := range self.ip4DestinationSourceCount {
		netSourceCount += len(sourceCounts)
	}
	for _, sourceCounts := range self.ip6DestinationSourceCount {
		netSourceCount += len(sourceCounts)
	}
	if self.log.V(2).Enabled() {
		for ip4Path, sourceCounts := range self.ip4DestinationSourceCount {
			if isPublicPort(ip4Path.DestinationPort) {
				if len(sourceCounts) == maxSourceCount {
					self.log.Infof("[multi]max source count %d = %v\n", maxSourceCount, ip4Path)
				}
			}
		}
		for ip6Path, sourceCounts := range self.ip6DestinationSourceCount {
			if isPublicPort(ip6Path.DestinationPort) {
				if len(sourceCounts) == maxSourceCount {
					self.log.Infof("[multi]max source count %d = %v\n", maxSourceCount, ip6Path)
				}
			}
		}
	}

	stats := &clientWindowStats{
		log:                      self.log,
		sourceCount:              maxSourceCount,
		netSourceCount:           netSourceCount,
		sendAckCount:             self.packetStats.sendAckCount,
		sendNackCount:            self.packetStats.sendNackCount,
		sendAckByteCount:         self.packetStats.sendAckByteCount,
		sendSynCount:             self.packetStats.sendSynCount,
		sendNackByteCount:        self.packetStats.sendNackByteCount,
		receiveAckCount:          self.packetStats.receiveAckCount,
		receiveAckByteCount:      self.packetStats.receiveAckByteCount,
		receiveSynCount:          self.packetStats.receiveSynCount,
		windowDuration:           windowDuration,
		firstSendAckTime:         firstSendAckTime,
		firstSendNackTime:        firstSendNackTime,
		firstSendSynTime:         firstSendSynTime,
		bucketCount:              len(eventBuckets),
		lastSendTime:             self.packetStats.lastSendTime,
		lastSendAckTime:          self.packetStats.lastSendAckTime,
		lastReceiveAckTime:       self.packetStats.lastReceiveAckTime,
		lastBusyProbeAckTime:     self.packetStats.lastBusyProbeAckTime,
		firstOutstandingSendTime: self.packetStats.firstOutstandingSendTime,
	}
	if 0 < len(eventBuckets) || !self.firstEventTime.IsZero() {
		// var eventTime time.Time
		// if 0 < len(eventBuckets) {
		// 	eventTime = eventBuckets[len(eventBuckets)-1].eventTime
		// } else {
		// 	eventTime = time.Now()
		// }
		eventTime := time.Now()

		if 0 < len(eventBuckets) {
			stats.lastEventTime = eventBuckets[len(eventBuckets)-1].eventTime
		}

		effectiveByteCountPerSecond := stats.EffectiveByteCountPerSecond()
		// scaledEffectiveByteCountPerSecond := ByteCount(self.settings.StatsWindowMaxEffectiveByteCountPerSecondScale * float32(stats.EffectiveByteCountPerSecond()))
		if self.maxEffectiveByteCountPerSecond < effectiveByteCountPerSecond {
			self.maxEffectiveByteCountPerSecond = effectiveByteCountPerSecond
			self.maxEffectiveByteCountPerSecondTime = eventTime
		}

		effectiveSendByteCount, effectiveReceiveByteCount := stats.EffectiveByteCount()
		healthy := (0 < effectiveSendByteCount) == (0 < effectiveReceiveByteCount)
		if healthy {
			if self.lastUnhealthyTime.IsZero() {
				self.lastUnhealthyTime = eventTime
			}

			if !self.healthy {
				self.healthy = true
				if !self.lastHealthyTime.IsZero() {
					self.netUnhealthyDuration += self.lastUnhealthyTime.Sub(self.lastHealthyTime)
				}
			}

			self.lastHealthyTime = eventTime

			stats.healthyDuration = eventTime.Sub(self.lastUnhealthyTime)
		} else {
			if self.lastHealthyTime.IsZero() {
				self.lastHealthyTime = eventTime
			}

			if self.healthy {
				self.healthy = false
				if !self.lastUnhealthyTime.IsZero() {
					self.netHealthyDuration += self.lastHealthyTime.Sub(self.lastUnhealthyTime)
				}
			}

			self.lastUnhealthyTime = eventTime

			stats.unhealthyDuration = eventTime.Sub(self.lastHealthyTime)
		}
		stats.healthy = healthy
		stats.netHealthyDuration = self.netHealthyDuration + stats.healthyDuration
		stats.netUnhealthyDuration = self.netUnhealthyDuration + stats.unhealthyDuration
		if self.firstEventTime.IsZero() {
			self.firstEventTime = eventBuckets[0].createTime
		}
		stats.clientDuration = eventTime.Sub(self.firstEventTime)
		stats.removeTime = self.firstEventTime.Add(self.settings.MaxClientLifetime)
	}
	// if !self.firstEventTime.IsZero() {
	// 	stats.removeTime = self.firstEventTime.Add(self.settings.MaxClientLifetime)
	// }
	if self.settings.StatsWindowGraceperiod < stats.clientDuration {
		stats.estimatedByteCountPerSecond = self.maxEffectiveByteCountPerSecond
	} else {
		stats.estimatedByteCountPerSecond = max(
			min(self.EstimatedByteCountPerSecond(), self.settings.StatsWindowMaxEstimatedByteCountPerSecond),
			self.maxEffectiveByteCountPerSecond,
		)
	}

	err := self.endErr
	if err == nil {
		select {
		case <-self.ctx.Done():
			err = errors.New("Done.")
		case <-self.client.Done():
			err = errors.New("Done.")
		default:
		}
	}

	return stats, err
}

// `connect.ReceiveFunction`
func (self *multiClientChannel) clientReceive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	select {
	case <-self.ctx.Done():
		return
	default:
	}

	// only process frames from the destinations
	// if allow := self.sourceFilter[source]; !allow {
	//     self.log.V(2).Infof("[multi]receive drop %d %s<-\n", len(frames), self.args.DestinationId)
	//     return
	// }

	for _, frame := range frames {
		switch frame.MessageType {
		case protocol.MessageType_TransferResidentMigrate:
			// Drain migration is a platform control instruction. A provider
			// must not be able to make us churn transports by injecting the
			// same protobuf on an ordinary data path.
			if self.transportMigrator == nil || !source.IsControlSource() {
				continue
			}
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			residentMigrate, ok := message.(*protocol.ResidentMigrate)
			if !ok {
				continue
			}
			self.transportMigrator.MigrateClientTransport(
				self.client,
				&self.args.MultiClientGeneratorClientArgs,
				time.UnixMilli(int64(residentMigrate.MigrateTime)),
			)
		case protocol.MessageType_IpIpPacketFromProvider:
			if packet, err := ipPacketFromProviderBytes(frame); err == nil {
				var ipPath IpPath
				_, err := parseIpPathWithPayloadBorrowed(packet, &ipPath)
				if err == nil {
					self.addReceiveAck(ByteCount(len(packet)))
					if ipPath.Syn {
						self.addReceiveSyn(1)
					}
					self.clientReceivePacketCallback(self, source, peer.ProvideMode, ipPath, packet)
				} else if count := self.receiveParseDropCount.Add(1); count&(count-1) == 0 {
					// power-of-two summaries: untrusted, potentially
					// high-rate return input (mirrors the send drop counters)
					self.log.Infof(
						"[multi]receive bad packet %s<- (parse; count=%d) = %s\n",
						self.args.Destination,
						count,
						err,
					)
				}
			} else {
				if self.log.V(2).Enabled() {
					self.log.Infof("[multi]receive drop %s<- = %s\n", self.args.Destination, err)
				}
			}
		default:
			// unknown message, drop
		}
	}
}

func (self *multiClientChannel) Cancel() {
	self.addError(errors.New("Done."))
	self.cancel()
	// unsubscribe even on Cancel so the underlying Client's callback list
	// doesn't retain a dangling reference for channels that are shuffled out
	// without ever going through Close. The lifecycle worker deregisters the
	// platform identity and drains Client resources; keeping Pion Close off
	// this path prevents resize/evaluation from waiting on peer teardown.
	self.clientReceiveUnsub()
}

func (self *multiClientChannel) Close() {
	self.Cancel()
}

func (self *multiClientChannel) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}
