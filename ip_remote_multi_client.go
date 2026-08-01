package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	// "reflect"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
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

type clientReceivePacketFunction func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte)

// dialFailureFunction carries a provider's intercepted dial-failure signal up
// from the channel that received it to the parent, which owns the flow maps.
// The signal is an icmp destination-unreachable that ParseIpPath rejects;
// egressIpPath is the failed flow's original source->destination key recovered
// from the icmp embed by ipParseIcmpUnreachable. Mirrors
// clientReceivePacketFunction, but the parent rebuilds the packet from the path
// (the removeClient teardown convention) rather than carrying raw bytes.
// The bool reports whether the flow was actually unbound for a re-race, so a
// caller holding its own client snapshot knows whether to discard it.
type dialFailureFunction func(sourceClient *multiClientChannel, egressIpPath *IpPath) (reraced bool)

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

func DefaultMultiClientSettings() *MultiClientSettings {
	return &MultiClientSettings{
		SequenceBufferSize:  defaultTransferBufferSize,
		SequenceIdleTimeout: 120 * time.Second,
		// tcp connections are routinely idle between requests and users still
		// consider them open; traditional vpns hold nat state for 5-30 minutes
		TcpSequenceIdleTimeout: 600 * time.Second,

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
		SendRetryTimeout:           2000 * time.Millisecond,
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
		// a lower ack timeout helps cycle through bad providers faster
		AckTimeout:                                30 * time.Second,
		BlackholeTimeout:                          5 * time.Second,
		BlackholeReceiveTimeout:                   20 * time.Second,
		MaxFlowsPerExit:                           16,
		DialFailureRerace:                         true,
		BlackholeConnectTimeout:                   30 * time.Second,
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

		UdpTeardownSignal:        true,
		ClusterAffinityFallback:  true,
		ServerNameAffinityBridge: true,
		// well inside AckTimeout (30s), which is what otherwise bounds recovery
		// from a client that accepts packets and never delivers them, and well
		// outside any normal ack round trip so ordinary latency is not mistaken
		// for a stall
		SendStallTimeout: 3 * time.Second,
		// well under the 30s AckTimeout that previously bounded a stalled
		// flow, and past the ~200ms-1s range of a first tcp rto so a healthy
		// flow's retransmits are still collapsed
		TcpCollapseMaxHold: 1500 * time.Millisecond,

		SecurityPolicyGenerator: DefaultSecurityPolicyWithStats,

		// the epoch for flushing block action and packet stats events to listeners
		EventEpoch:                  1 * time.Second,
		BlockActionDecisionTtl:      30 * time.Second,
		BlockActionDecisionMaxCount: 4096,
		BlockActionAggMaxCount:      1024,
		IpAssocSettings:             DefaultIpAssocSettings(),

		RemoteUserNatMultiClientMonitorSettings: *DefaultRemoteUserNatMultiClientMonitorSettings(),
	}
}

type MultiClientSettings struct {
	// Log, when set, is used by the multi client, its windows, channels, and
	// internal local user nat. nil resolves to `DefaultLogger()`.
	Log Logger

	SequenceBufferSize  int
	SequenceIdleTimeout time.Duration
	// TcpSequenceIdleTimeout is the idle timeout for tcp flows specifically.
	// 0 falls back to SequenceIdleTimeout, restoring the previous single-value
	// behavior. See `sequenceIdleTimeout`.
	TcpSequenceIdleTimeout time.Duration
	WindowSizes            map[WindowType]WindowSizeSettings
	// ClientNackInitialLimit int
	// ClientNackMaxLimit int
	// ClientNackScale float64
	// ClientWriteTimeout time.Duration
	// SendTimeout time.Duration
	// WriteTimeout time.Duration
	SendRetryTimeout           time.Duration
	PingWriteTimeout           time.Duration
	CPingWriteTimeout          time.Duration
	CPingMaxByteCountPerSecond ByteCount
	PingTimeout                time.Duration
	CPingTimeout               time.Duration
	CPingRestTimeout           time.Duration
	AckTimeout                 time.Duration
	BlackholeTimeout           time.Duration
	// BlackholeReceiveTimeout bounds the weaker of the two blackhole signals:
	// the provider is acknowledging our sends, so it is demonstrably alive,
	// but nothing has come back from the destination. That is ambiguous -- a
	// flow waiting on a slow origin looks identical to a provider whose
	// upstream is broken -- and removing an exit is destructive, killing every
	// flow pinned to it rather than just the quiet one. So it gets a longer
	// bar than BlackholeTimeout, which covers the unambiguous case of a
	// provider that has stopped acknowledging anything at all.
	//
	// On mainnet at 5s this fired 44 times out of 44 removals, roughly one
	// every 18s under load, against providers that were acking as much as 602
	// sends / 222KB. 0 disables the check.
	//
	// Bounded above by roughly StatsWindowDuration + StatsWindowBucketDuration
	// (~31s at production constants): the age this is compared against comes
	// from surviving stat buckets, and coalesceEventBuckets drops every bucket
	// older than StatsWindowDuration. A value at or above that ceiling never
	// fires and is silently equivalent to 0 -- no error, no log. The default
	// keeps headroom so a later reduction of StatsWindowDuration does not
	// quietly disable it; TestBlackholeReceiveTimeoutIsReachable fails loudly
	// if that margin is lost.
	BlackholeReceiveTimeout time.Duration
	BlackholeConnectTimeout time.Duration
	// MaxFlowsPerExit bounds how many live flows may be pinned to one exit.
	//
	// Providers are split-tcp, so removing an exit destroys every flow on it.
	// Measured on device: of 14 removals in 28 minutes, 10 cost nothing and
	// one cost 157 connections at once -- a visible 15s stall. Blast radius,
	// not removal rate, is what a user actually feels.
	//
	// The cost is destination affinity: a site whose flows would have shared
	// an exit gets split across two once the first is full, so it sees more
	// than one egress ip. A real trade-off rather than a free win, which is
	// why it is tunable at runtime; 0 restores the previous unbounded
	// behavior.
	//
	// The default comes from observed recovery times, not from the flow counts
	// alone. Over two hours of real use, 21 teardowns carried
	//
	//	1 1 2 4 4 6 7 20 26 35 36 36 43 44 46 53 62 71 101 157 484
	//
	// flows, and the stalls reported against them were: 4-6 flows about 3-5s,
	// 44 about 15s, 157 about 15s, 484 about 35s. Recovery grows with the
	// count sublinearly, with a long plateau through the middle -- so the
	// useful target is keeping events down near the small end, where a hiccup
	// is a few seconds, rather than shaving the tail.
	//
	// A permissive bound misses this entirely. The median teardown here was 36
	// flows, so a cap of 64 would not have touched a single one of the 15s
	// stalls. 16 is chosen to land most events in the range that recovers in
	// a few seconds, accepting more affinity splitting as the price.
	//
	// The cap must never make a flow unroutable -- it bounds blast radius, it
	// is not admission control. When every candidate is full the flow is
	// placed anyway.
	MaxFlowsPerExit int
	// DialFailureRerace, when a provider reports it could not open the
	// upstream for a new flow (see ipOosUnreachable's dial-failure use),
	// silently unbinds the flow and lets the application's own retransmit
	// race it onto another exit -- turning a 3-63s syn-backoff hang into
	// about one second. Off forwards the failure signal to the application
	// instead, which is visible but still fast.
	DialFailureRerace                         bool
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

	// TcpCollapseMaxHold bounds how long TcpCollapsePrevention may keep
	// discarding a sender's retransmits while the committed packet makes no
	// progress. After this long at the same sequence state, one retransmit is
	// admitted per window. 0 disables the bound, restoring the previous
	// behavior where retransmits were dropped until the client was declared
	// dead (up to AckTimeout). Ignored when TcpCollapsePrevention is off.
	TcpCollapseMaxHold time.Duration

	// SendStallTimeout is how long a client may hold outstanding sends without
	// acknowledging any of them before it is treated as failed and removed from
	// the window. A client in that state looks busy rather than broken, so
	// without this it survives until AckTimeout (30s) while every flow pinned to
	// it is frozen. 0 disables the check. See `sendStalled`.
	SendStallTimeout time.Duration

	// ServerNameAffinityBridge lets a new flow whose own affinity group has no
	// donor inherit the client from the destination-scoped group an earlier
	// nameless flow to the same destination joined. Those groups are read, never
	// joined. off restores the previous behavior, where the first connection to
	// a site the mux resolved late stays stranded on a different exit from the
	// rest of the session. See `affinityFallbackIpPathsWithLock`.
	ServerNameAffinityBridge bool

	// ClusterAffinityFallback groups destination ips by their IpAssoc cluster
	// when no server name is known for them, so a site whose flows the dns mux
	// never saw resolved still pins to one client instead of splitting across
	// the window per ip. off restores plain per-ip affinity. Only consulted on
	// the no-server-name path; a known server name always wins.
	ClusterAffinityFallback bool

	// UdpTeardownSignal sends an icmp destination-unreachable to the source
	// when a udp flow is torn down, the way tcp flows already get a rst. off
	// restores the previous behavior, where a udp flow whose exit is removed
	// goes silent and stalls until the application times out. see
	// `ipOosUnreachable`.
	UdpTeardownSignal bool

	SecurityPolicyGenerator func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy

	// the epoch for flushing block action and packet stats events to listeners
	EventEpoch time.Duration
	// how long a cached block action decision stays valid while the overrides
	// and cluster versions are unchanged (server names for a destination can drift)
	BlockActionDecisionTtl      time.Duration
	BlockActionDecisionMaxCount int
	// max distinct block actions aggregated per epoch
	BlockActionAggMaxCount int
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

type RemoteUserNatMultiClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	generator MultiClientGenerator

	receivePacketCallback ReceivePacketFunction

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

	// reliability holds runtime overrides for the reliability knobs, so the
	// developer menu can A/B a live freeze without a rebuild. Unset means "use
	// the values in settings", which is what every non-overridden client does.
	// Read on the packet hot path, so an atomic rather than a lock.
	reliability atomic.Pointer[ReliabilitySettings]

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

	// reliabilityMetrics measures what a provider failure actually costs the
	// user -- how many flows die with an exit, and how long the destinations
	// they served stay unreachable. The reliability knobs above are only
	// A/B-testable against a number, and this is the number.
	reliabilityMetrics *reliabilityMetrics
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

	multiClient := &RemoteUserNatMultiClient{
		ctx:                    cancelCtx,
		cancel:                 cancel,
		log:                    log,
		generator:              generator,
		receivePacketCallback:  receivePacketCallback,
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
		reliabilityMetrics:     newReliabilityMetrics(),
	}
	if settings.IpAssocSettings != nil {
		multiClient.ipAssoc = NewIpAssoc(cancelCtx, settings.IpAssocSettings)
	}
	multiClient.config.Store(&multiClientConfig{
		performanceProfile:  multiClient.overrideAllowDirect(settings.DefaultPerformanceProfile),
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
		multiClient.clientDialFailure,
		multiClient.securityPolicy,
		multiClient.removeClient,
		WindowTypeQuality,
		settings,
		multiClient.reliabilitySettings,
	)
	if _, fixed := generator.FixedDestinationSize(); !fixed {
		multiClient.windows[WindowTypeSpeed] = newMultiClientWindow(
			cancelCtx,
			cancel,
			generator,
			multiClient.clientReceivePacket,
			multiClient.clientDialFailure,
			multiClient.securityPolicy,
			multiClient.removeClient,
			WindowTypeSpeed,
			settings,
			multiClient.reliabilitySettings,
		)
	}
	// else only keep the quality window for fixed destination

	// a trusted same-network peer connection always allows direct (p2p). Force it
	// onto the fresh windows now so the first channels pick it up even before any
	// performance profile is set; SetPerformanceProfile keeps it forced thereafter.
	if provideMode == protocol.ProvideMode_Network {
		multiClient.SetPerformanceProfile(settings.DefaultPerformanceProfile)
	}

	multiClient.localUserNatUnsub = localUserNat.AddReceivePacketCallback(multiClient.localReceivePacket)

	monitors := []MultiClientMonitor{}
	for _, window := range multiClient.windows {
		monitors = append(monitors, window.monitor)
	}
	multiClient.monitor = NewMergedMultiClientMonitor(monitors)

	go HandleError(multiClient.runEventEpoch, cancel)

	return multiClient
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
	self.receivePacketCallback(source, provideMode, ipPath, packet)
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
	if performanceProfile.AllowDirect == allowDirect {
		return performanceProfile
	}
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

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// rebuild the immutable config snapshot under the lock so concurrent
		// setters do not lose each other's field
		prev := self.config.Load()
		self.config.Store(&multiClientConfig{
			performanceProfile:  performanceProfile,
			localSecurityBypass: prev.localSecurityBypass,
			serverNameLookup:    prev.serverNameLookup,
			blocker:             prev.blocker,
		})
	}()
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

// ReliabilitySettings is the runtime-overridable subset of MultiClientSettings:
// the knobs that change how a flow reacts when its exit misbehaves. Each one
// exists so the freeze it addresses can be turned off and back on against a
// live connection, since which cause a given user is hitting is not something
// the code can tell from the outside.
type ReliabilitySettings struct {
	// see the matching MultiClientSettings fields for what each one does
	UdpTeardownSignal        bool
	TcpCollapseMaxHold       time.Duration
	SendStallTimeout         time.Duration
	ClusterAffinityFallback  bool
	ServerNameAffinityBridge bool
	SequenceIdleTimeout      time.Duration
	TcpSequenceIdleTimeout   time.Duration
	BlackholeReceiveTimeout  time.Duration
	MaxFlowsPerExit          int
	DialFailureRerace        bool
}

// ReliabilitySettingsFrom reads the effective values out of a settings struct.
// nil yields the zero value, which is every reliability behavior off -- the
// state before any of this work, and what the bare test fixtures get.
func ReliabilitySettingsFrom(settings *MultiClientSettings) *ReliabilitySettings {
	if settings == nil {
		return &ReliabilitySettings{}
	}
	return &ReliabilitySettings{
		UdpTeardownSignal:        settings.UdpTeardownSignal,
		TcpCollapseMaxHold:       settings.TcpCollapseMaxHold,
		SendStallTimeout:         settings.SendStallTimeout,
		ClusterAffinityFallback:  settings.ClusterAffinityFallback,
		ServerNameAffinityBridge: settings.ServerNameAffinityBridge,
		SequenceIdleTimeout:      settings.SequenceIdleTimeout,
		TcpSequenceIdleTimeout:   settings.TcpSequenceIdleTimeout,
		BlackholeReceiveTimeout:  settings.BlackholeReceiveTimeout,
		MaxFlowsPerExit:          settings.MaxFlowsPerExit,
		DialFailureRerace:        settings.DialFailureRerace,
	}
}

// reliabilitySettings is the effective reliability config: a runtime override
// when one has been set, else whatever the client was constructed with. Safe on
// a bare client, which several test fixtures rely on.
func (self *RemoteUserNatMultiClient) reliabilitySettings() *ReliabilitySettings {
	if overrides := self.reliability.Load(); overrides != nil {
		return overrides
	}
	return ReliabilitySettingsFrom(self.settings)
}

// SetReliabilitySettings installs runtime overrides for the reliability knobs.
// nil clears them, restoring the constructed settings. Takes effect on the next
// packet -- no reconnect needed, which is the point: a freeze can be A/B'd
// while it is happening.
func (self *RemoteUserNatMultiClient) SetReliabilitySettings(reliabilitySettings *ReliabilitySettings) {
	self.reliability.Store(reliabilitySettings)
}

// ReliabilitySettings returns the effective reliability config, for reporting
// the live state back to a developer menu.
func (self *RemoteUserNatMultiClient) ReliabilitySettings() *ReliabilitySettings {
	return self.reliabilitySettings()
}

// clusterAffinityRepresentative picks one stable member of a cluster so every
// ip in it resolves to the same affinity key. The minimum orders
// deterministically, where map iteration would hand back a different member per
// call and defeat the grouping entirely. Empty (an ip in no multi-member
// cluster) yields false, leaving the caller on per-ip affinity.
func clusterAffinityRepresentative(members []netip.Addr) (netip.Addr, bool) {
	var representative netip.Addr
	found := false
	for _, member := range members {
		member = member.Unmap()
		if !found || member.Less(representative) {
			representative = member
			found = true
		}
	}
	return representative, found
}

// destinationAffinityIpPathWithLock builds the destination-scoped affinity key
// for the web ports, or nil when the port has no such key.
//
// No server name means the dns mux never observed a query for this ip: the app
// resolved over its own doh (chrome secure dns, android private dns), or the os
// answered from its cache -- the long-ttl case ReverseTtl's comment calls out.
// Per-ip affinity then splits one cdn-hosted site across the window, since its
// many ips each key separately, so this falls back to the ip association
// cluster, which already groups co-active ips.
//
// Shared by the registration path and the late-name bridge so the two can never
// build the key differently -- if they diverged the bridge would silently stop
// matching and nothing would fail.
//
// called with stateLock
func (self *RemoteUserNatMultiClient) destinationAffinityIpPathWithLock(ipPath *IpPath) *IpPath {
	switch ipPath.DestinationPort {
	case 80, 53, 443:
	default:
		return nil
	}

	destinationIp := ipPath.DestinationIp
	// this function is otherwise a pure function of the config and the path,
	// and is exercised that way from bare clients, so an unset settings falls
	// back to plain per-ip affinity rather than panicking
	if self.reliabilitySettings().ClusterAffinityFallback && self.ipAssoc != nil {
		if addr, ok := ipAssocAddr(ipPath.DestinationIp); ok {
			// GetClusterAddrs is a lock-free atomic load, safe to call with the
			// parent stateLock held
			if rep, ok := clusterAffinityRepresentative(self.ipAssoc.GetClusterAddrs(addr)); ok {
				destinationIp = rep.AsSlice()
			}
		}
	}

	return &IpPath{
		Version:         ipPath.Version,
		DestinationIp:   destinationIp,
		DestinationPort: ipPath.DestinationPort,
	}
}

// affinityFallbackIpPathsWithLock returns the destination-scoped groups a NEW
// flow may inherit a client from when its own affinity group has no donor.
// Consulted, never joined.
//
// The mux learns a server name from the tls ClientHello as well as from dns, and
// the ClientHello is the fourth packet of a connection whose syn already created
// the flow. So for any site the mux did not resolve itself, the ordering is
// always: the first flow is created nameless and keys on the destination, the
// name is learned an rtt later, and every later flow keys on the base domain
// with an empty group -- stranding the first connection, usually the main page
// load, on a different exit from the rest of the session.
//
// Reading the destination group lets those later flows converge onto the exit
// the established flow already uses. Convergence is toward the established
// exit, which is the only direction available: providers terminate tcp, so
// moving a live flow would break it, which is the failure this whole change
// exists to prevent.
//
// Ordering is most specific first -- the exact destination ip, then the cluster
// representative when it differs. Ports outside the web set deliberately return
// nil: their no-name keys are port-only or global, so bridging into them would
// pin every named site to one exit.
//
// called with stateLock
func (self *RemoteUserNatMultiClient) affinityFallbackIpPathsWithLock(ipPath *IpPath) []*IpPath {
	if !self.reliabilitySettings().ServerNameAffinityBridge {
		return nil
	}
	switch ipPath.DestinationPort {
	case 80, 53, 443:
	default:
		return nil
	}
	if ipPath.DestinationIp == nil {
		return nil
	}

	// a window fixed to one client has a single global affinity path already
	if _, windowSize, ok := self.config.Load().performanceProfile.FixedWindow(); ok && windowSize.FixedWindowSize == 1 {
		return nil
	}

	fallbackPaths := []*IpPath{
		{
			Version:         ipPath.Version,
			DestinationIp:   ipPath.DestinationIp,
			DestinationPort: ipPath.DestinationPort,
		},
	}
	// the cluster representative is the broader guess, consulted only if the
	// exact destination group has no usable donor
	if destinationPath := self.destinationAffinityIpPathWithLock(ipPath); destinationPath != nil &&
		!destinationPath.DestinationIp.Equal(ipPath.DestinationIp) {
		fallbackPaths = append(fallbackPaths, destinationPath)
	}
	return fallbackPaths
}

// underFlowCap drops candidates that are already carrying their share of
// flows, preserving the caller's ordering. May return an empty slice when
// every candidate is full: the caller decides the fallback, because only the
// caller knows whether a wider field (another tier) is available to try
// first. See raceCandidates for the fallback order.
//
// NOTE this filter alone does not bound anything. It runs at *selection*, and
// the cap is never re-checked at *assignment*: `sendUpdate` holds stateLock to
// pick a client and releases it, while the matching
// `clientUpdates[client][update] = true` happens in a separate acquisition in
// `sendClientPath`. N concurrent flows in one affinity group all observe the
// same count and all commit, taking an exit to count+N. On device an exit
// reached 79 flows against a cap of 16 with this filter in place.
//
// The real fix is to make check-and-assign atomic at each of the three
// assignment sites. Not attempted here.
func (self *RemoteUserNatMultiClient) underFlowCap(clients []*multiClientChannel) []*multiClientChannel {
	if len(clients) == 0 || self.reliabilitySettings().MaxFlowsPerExit <= 0 {
		return clients
	}

	under := make([]*multiClientChannel, 0, len(clients))
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, client := range clients {
			if !self.clientAtFlowCapWithLock(client) {
				under = append(under, client)
			}
		}
	}()
	return under
}

// raceCandidates assembles the field a new flow is placed over.
//
// The window offers only its best rank (OrderedClients keeps the min tier) so
// traffic does not cross rank until necessary. With one or two exits in the
// top rank -- the normal state on a small provider pool, since the platform
// tiers on measured latency and speed -- that gate used to defeat the flow
// cap outright: the under-cap filter could never keep two candidates, fell
// back to the unfiltered list, and the lowest-rtt winner re-picked the same
// saturated exit for every flow. On device that read as 86 flows on one exit
// with twelve idle spares, which is the exact blast radius MaxFlowsPerExit
// exists to bound.
//
// A min tier with every exit at the cap is the "necessary" the rank gate was
// waiting for. So, in order: the min tier's exits with capacity; only when
// the min tier has none, any tier's exits with capacity; and when everything
// everywhere is full, the least-loaded exits of any tier -- the cap bounds
// blast radius, it is not admission control, and a flow with no exit under
// the cap is still placed, spread toward an even share rather than piled on
// the best rank.
//
// A single under-cap candidate is returned alone, taking the send path's
// no-race branch rather than widening the field: crossing rank while the top
// rank still has capacity would let a nearby lower-rank exit win on rtt and
// split traffic off the rank the platform chose. A no-race placement still
// recovers through the dial-failure and send-error re-race paths.
func (self *RemoteUserNatMultiClient) raceCandidates(window *multiClientWindow) []*multiClientChannel {
	return self.raceCandidatesFrom(window.OrderedClients, window.orderedClientsCrossTier)
}

// raceCandidatesFrom is raceCandidates over explicit list sources, the seam
// the tests drive. Both are pulled lazily: the cross-tier walk only happens
// when the min tier is saturated, and with the cap off the window's rank gate
// is left exactly as it was.
func (self *RemoteUserNatMultiClient) raceCandidatesFrom(
	minTier func() []*multiClientChannel,
	crossTier func() []*multiClientChannel,
) []*multiClientChannel {
	minTierClients := minTier()
	if len(minTierClients) == 0 || self.reliabilitySettings().MaxFlowsPerExit <= 0 {
		return minTierClients
	}
	if under := self.underFlowCap(minTierClients); 0 < len(under) {
		return under
	}
	crossed := crossTier()
	if under := self.underFlowCap(crossed); 0 < len(under) {
		return under
	}
	// every exit of every rank is at the cap: demand exceeds the pool's
	// capacity, and something must exceed the cap. The overflow goes to
	// whoever carries least, not to the min tier -- placing it by rank
	// re-created the single-exit pileup the cap exists to prevent (on device:
	// five exits pinned at 16 while the lone tier-1 exit absorbed 267 flows,
	// a 267-flow teardown waiting to happen). An even share is the best
	// remaining blast-radius bound once capacity is spent.
	if least := self.leastLoadedClients(crossed); 0 < len(least) {
		return least
	}
	return minTierClients
}

// leastLoadedClients keeps the clients carrying the fewest recorded flows --
// the placement of last resort when every exit of every rank is at the flow
// cap. Ties all stay in (the common case at the even-share equilibrium,
// where whole groups sit at the same count), so the multi-exit race is
// preserved exactly when it matters most. Caller ordering is preserved.
func (self *RemoteUserNatMultiClient) leastLoadedClients(clients []*multiClientChannel) []*multiClientChannel {
	if len(clients) < 2 {
		return clients
	}

	least := []*multiClientChannel{}
	minCount := -1
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range clients {
		count := len(self.clientUpdates[client])
		if minCount < 0 || count < minCount {
			minCount = count
			least = append(least[:0], client)
		} else if count == minCount {
			least = append(least, client)
		}
	}
	return least
}

// bindClientFlow records that a flow is now committed to client, which is what
// clientUpdates exists to track.
//
// The send path maintains this bookkeeping only when it notices the committed
// client changed across its callback. A flow that wins its exit from an async
// race never trips that: the first packet leaves with the client still nil, the
// race stores the winner later from the receive path or the race completion,
// and every subsequent send sees no change. Such a flow was therefore never
// entered here at all. (The exit readout no longer relies on this map, so the
// visible "14 exits, flows on one" symptom is fixed independently -- what this
// buys is the three consumers below.)
//
// That map is not cosmetic. clientAtFlowCapWithLock counts it, so uncounted
// flows are also uncapped, and removeClient iterates it to tear flows down, so
// uncounted flows get no teardown and recover only via the slower send-error
// path. Calling this at every assignment site makes all three correct.
//
// Must NOT be called while holding the update's stateLock: that lock is a leaf
// and its holders never take the parent lock. Both race sites call this after
// their locked section returns.
func (self *RemoteUserNatMultiClient) bindClientFlow(update *multiClientChannelUpdate, client *multiClientChannel) {
	if update == nil || client == nil {
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// re-read under the parent lock: the flow may have moved or been torn down
	// between the race storing its winner and this call
	if update.client.Load() != client || client.IsDone() {
		return
	}

	// drop it from any other client's set first -- a re-raced flow moves
	// between exits, and a stale entry would inflate the old exit's count and
	// hand it a teardown for a flow it no longer carries
	for otherClient, updates := range self.clientUpdates {
		if otherClient == client {
			continue
		}
		if _, ok := updates[update]; ok {
			delete(updates, update)
			if len(updates) == 0 {
				delete(self.clientUpdates, otherClient)
			}
		}
	}

	updates, ok := self.clientUpdates[client]
	if !ok {
		updates = map[*multiClientChannelUpdate]bool{}
		self.clientUpdates[client] = updates
	}
	updates[update] = true
}

// clientAtFlowCapWithLock reports whether an exit is already carrying its
// share of flows.
//
// Applied on both assignment paths -- affinity inheritance and the race -- and
// that is the whole point. Affinity assigns update.client directly and never
// reaches the race, so a cap enforced only at client selection would not fire
// for the flows that actually concentrate: a feed opens many connections to
// the same handful of domains, which is exactly what affinity pins together.
//
// called with stateLock
func (self *RemoteUserNatMultiClient) clientAtFlowCapWithLock(client *multiClientChannel) bool {
	maxFlows := self.reliabilitySettings().MaxFlowsPerExit
	if maxFlows <= 0 {
		return false
	}
	return maxFlows <= len(self.clientUpdates[client])
}

// inheritAffinityClient4WithLock adopts the most recently joined healthy client
// in an affinity group. Only ever called with `update.client` nil -- it must
// never repoint a flow that already has a client, since providers terminate tcp
// and a moved flow is a broken flow.
//
// called with stateLock
func (self *RemoteUserNatMultiClient) inheritAffinityClient4WithLock(update *multiClientChannelUpdate, paths map[Ip4Path]time.Time) {
	var mostRecentCreateTime time.Time
	for copyIp4Path, createTime := range paths {
		if copyUpdate, ok := self.ip4PathUpdates[copyIp4Path]; ok {
			if c := copyUpdate.client.Load(); c != nil && !c.IsDone() && !c.isWarning() && !self.clientAtFlowCapWithLock(c) && createTime.After(mostRecentCreateTime) {
				mostRecentCreateTime = createTime
				update.client.Store(c)
			}
		}
	}
}

// inheritAffinityClient6WithLock is the v6 twin of inheritAffinityClient4WithLock
//
// called with stateLock
func (self *RemoteUserNatMultiClient) inheritAffinityClient6WithLock(update *multiClientChannelUpdate, paths map[Ip6Path]time.Time) {
	var mostRecentCreateTime time.Time
	for copyIp6Path, createTime := range paths {
		if copyUpdate, ok := self.ip6PathUpdates[copyIp6Path]; ok {
			if c := copyUpdate.client.Load(); c != nil && !c.IsDone() && !c.isWarning() && !self.clientAtFlowCapWithLock(c) && createTime.After(mostRecentCreateTime) {
				mostRecentCreateTime = createTime
				update.client.Store(c)
			}
		}
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
		} else if destinationPath := self.destinationAffinityIpPathWithLock(ipPath); destinationPath != nil {
			// for these ports, cycle the path per destination ip/port, regardless of protocol
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

func (self *RemoteUserNatMultiClient) sendClientPath(ipPath *IpPath, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
	update, previousClient, currentClient := self.sendUpdate(ipPath)
	callback(update, currentClient)

	// fast path: if the flow's client did not change during the callback, no
	// clientUpdates bookkeeping is needed, so skip the parent lock entirely
	// (client is atomic). this is the steady-state egress path.
	if previousClient == update.client.Load() {
		return
	}

	// re-read (the lock-free check above can race a concurrent client change)
	client := update.client.Load()

	if client != nil {
		// bindClientFlow, not a local add: it clears the flow from every other
		// client's set, which `previousClient` alone cannot do. previousClient
		// is this path's own stale snapshot, and an async race can commit a
		// different client between the snapshot and here -- the single-client
		// send below then stores over it, leaving the flow recorded under the
		// race's winner forever. Nothing ever cleans that: the reaper and
		// removeClient only look under update.client.Load(). The stale entry
		// inflates the abandoned exit's cap count, which makes it look full
		// sooner, which makes the single-client path more likely -- a loop
		// that only tightens.
		self.bindClientFlow(update, client)
		return
	}

	// the flow committed to nothing: drop any entry the previous client held
	if previousClient == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if updates, ok := self.clientUpdates[previousClient]; ok {
		delete(updates, update)
		if len(updates) == 0 {
			delete(self.clientUpdates, previousClient)
		}
	}
}

// waitForIdleUpdate blocks until the flow update has been idle for
// SequenceIdleTimeout, or the update ctx is done. it runs only inside the
// per-flow teardown goroutine; hoisted out of sendUpdate (rather than an inline
// closure) so the per-packet steady-state path does not allocate it.
// sequenceIdleTimeout is how long a flow may sit idle before it is torn down.
//
// A tcp connection is routinely idle between requests -- ssh sessions,
// websockets, push channels -- and the application still considers it open.
// Traditional vpns hold tcp nat state for 5-30 minutes, so a 2 minute bound
// resets connections users have every reason to think are alive. Udp has no
// equivalent notion of an open connection and its mappings are conventionally
// short lived, so it keeps the tighter bound.
func (self *RemoteUserNatMultiClient) sequenceIdleTimeout(ipPath *IpPath) time.Duration {
	reliabilitySettings := self.reliabilitySettings()
	if ipPath != nil && ipPath.Protocol == IpProtocolTcp && 0 < reliabilitySettings.TcpSequenceIdleTimeout {
		return reliabilitySettings.TcpSequenceIdleTimeout
	}
	return reliabilitySettings.SequenceIdleTimeout
}

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

			idleTimeout = update.activityTime.Add(self.sequenceIdleTimeout(update.ipPath)).Sub(time.Now())
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
// teardownSourcePacket builds the packet that tells the source its flow is
// gone: a rst for tcp, and for udp an icmp unreachable when
// `UdpTeardownSignal` is set. `ipPath` is the flow's own direction (source to
// destination); the returned packet is already addressed back toward the
// source. false means there is nothing to send, which is the pre-existing
// behavior for udp and for every non-tcp, non-udp protocol.
func (self *RemoteUserNatMultiClient) teardownSourcePacket(ipPath *IpPath, sourceRstSequence uint32) ([]byte, bool) {
	if packet, ok := ipOosRstSequence(ipPath.Reverse(), sourceRstSequence); ok {
		return packet, true
	}
	if self.reliabilitySettings().UdpTeardownSignal {
		return ipOosUnreachable(ipPath)
	}
	return nil, false
}

func (self *RemoteUserNatMultiClient) rstFlow(ipPath *IpPath, client *multiClientChannel, sourceRstSequence uint32) {
	if client != nil {
		// rst to destination
		if packet, ok := ipOosRst(ipPath); ok {
			client.Send(&parsedPacket{
				packet: packet,
				ipPath: ipPath,
			}, 0)
		}
	}
	// teardown to source
	if packet, ok := self.teardownSourcePacket(ipPath, sourceRstSequence); ok {
		self.receivePacketCallback(TransferPath{}, protocol.ProvideMode_Network, ipPath, packet)
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

	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		var previousClient *multiClientChannel
		update, ok := self.ip4PathUpdates[ip4Path]
		if !ok || update.IsDone() {
			update = newMultiClientChannelUpdate(self.ctx, ipPath)
			self.reliabilityMetrics.flowOpened()
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
							if t := update.activityTime.Add(self.sequenceIdleTimeout(update.ipPath)).Sub(time.Now()); 0 < t {
								// updated since wait for idle
								return false
							}
						}

						client = update.client.Load()
						update.client.Store(nil)

						delete(self.ip4PathUpdates, ip4Path)

						for affinityIp4Path, _ := range update.affinityIp4Paths {
							if paths, ok := self.affinityIp4Paths[affinityIp4Path]; ok {
								delete(paths, ip4Path)
								if len(paths) == 0 {
									delete(self.affinityIp4Paths, affinityIp4Path)
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
					self.rstFlow(ipPath, client, update.sourceRstSequence())
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
					self.inheritAffinityClient4WithLock(update, paths)
				}
			}

			// the flow's own groups had no donor. an established flow to this
			// destination may still exist under the key it was created with
			// before the server name was learned -- read those groups without
			// joining them, so this flow converges onto the exit already in use
			if update.client.Load() == nil {
				for _, fallbackIpPath := range self.affinityFallbackIpPathsWithLock(ipPath) {
					fallbackIp4Path := fallbackIpPath.ToIp4Path()
					if update.affinityIp4Paths[fallbackIp4Path] {
						// already joined and scanned above
						continue
					}
					if paths, ok := self.affinityIp4Paths[fallbackIp4Path]; ok {
						self.inheritAffinityClient4WithLock(update, paths)
						if update.client.Load() != nil {
							break
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
			self.reliabilityMetrics.flowOpened()
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
							if t := update.activityTime.Add(self.sequenceIdleTimeout(update.ipPath)).Sub(time.Now()); 0 < t {
								// updated since wait for idle
								return false
							}
						}

						client = update.client.Load()
						update.client.Store(nil)

						delete(self.ip6PathUpdates, ip6Path)

						for affinityIp6Path, _ := range update.affinityIp6Paths {
							if paths, ok := self.affinityIp6Paths[affinityIp6Path]; ok {
								delete(paths, ip6Path)
								if len(paths) == 0 {
									delete(self.affinityIp6Paths, affinityIp6Path)
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
					self.rstFlow(ipPath, client, update.sourceRstSequence())
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
					self.inheritAffinityClient6WithLock(update, paths)
				}
			}

			// the flow's own groups had no donor. an established flow to this
			// destination may still exist under the key it was created with
			// before the server name was learned -- read those groups without
			// joining them, so this flow converges onto the exit already in use
			if update.client.Load() == nil {
				for _, fallbackIpPath := range self.affinityFallbackIpPathsWithLock(ipPath) {
					fallbackIp6Path := fallbackIpPath.ToIp6Path()
					if update.affinityIp6Paths[fallbackIp6Path] {
						// already joined and scanned above
						continue
					}
					if paths, ok := self.affinityIp6Paths[fallbackIp6Path]; ok {
						self.inheritAffinityClient6WithLock(update, paths)
						if update.client.Load() != nil {
							break
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
	rstPackets := []*receivePacket{}
	// every flow pinned to this client dies with it -- split-tcp means the
	// exit holds the remote end of the connection, so there is nothing to
	// migrate. the count is the blast radius of one provider failure.
	lostDestinations := []recoveryKey{}

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

					// the update's ipPath is egress-oriented, so the remote
					// endpoint the user is waiting on is the destination.
					lostDestinations = append(lostDestinations, newRecoveryKey(
						update.ipPath.DestinationIp,
						update.ipPath.DestinationPort,
					))

					if packet, ok := self.teardownSourcePacket(update.ipPath, update.sourceRstSequence()); ok {
						rstPacket := &receivePacket{
							Source:      TransferPath{},
							ProvideMode: protocol.ProvideMode_Network,
							IpPath:      update.ipPath,
							Packet:      packet,
						}
						rstPackets = append(rstPackets, rstPacket)
					}
				} else {
					self.log.Errorf("[multi]update associated with incorrect client")
				}
			}
		}
	}()

	// recorded outside stateLock -- the metrics take their own lock, and
	// nesting them under the parent lock would put the recovery tracker on the
	// path every flow lookup contends for.
	self.reliabilityMetrics.exitLost(lostDestinations)

	// a client removed while carrying nothing is routine window churn --
	// collapsing the lowest-weighted client, rank-based removal -- and logging
	// those buries the removals that actually cost a user something
	teardownWorthLogging := 0 < len(rstPackets) || 0 < len(lostDestinations)

	select {
	case <-self.ctx.Done():
		// the teardown is dropped when the client is shutting down. worth
		// logging: it is the one case where flows die with no signal at all,
		// and it is otherwise indistinguishable from a teardown that was sent
		// and ignored.
		if teardownWorthLogging {
			self.log.Infof(
				"[multi]teardown skipped, context done: %d packet(s) for %d flow(s) of client %s\n",
				len(rstPackets), len(lostDestinations), client.ClientId(),
			)
		}
	default:
		// whether the peer is told is the difference between a flow that fails
		// fast and one that hangs until the app's own timeout. on device a
		// dropped exit left a download at 0bps rather than erroring, and there
		// was no way to tell whether the teardown was never built, never sent,
		// or sent and ignored. log the emission so those are distinguishable.
		if teardownWorthLogging {
			self.log.Infof(
				"[multi]teardown sending %d packet(s) for %d flow(s) of client %s\n",
				len(rstPackets), len(lostDestinations), client.ClientId(),
			)
		}
		for _, p := range rstPackets {
			if self.log.V(1).Enabled() {
				// IpPath has no String(), so expand the fields -- %s on the
				// struct prints an unreadable blob with the ports and flags
				// mangled, which is useless for the one job this line has
				self.log.Infof(
					"[multi]teardown -> ipv%d p%v %s:%d->%s:%d\n",
					p.IpPath.Version, p.IpPath.Protocol,
					p.IpPath.SourceIp, p.IpPath.SourcePort,
					p.IpPath.DestinationIp, p.IpPath.DestinationPort,
				)
			}
			self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
		}
	}
}

// `SendPacketFunction`
func (self *RemoteUserNatMultiClient) SendPacket(
	source TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	timeout time.Duration,
) bool {
	relationship := egressRelationship(provideMode, self.provideMode)

	ipPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		self.log.Infof("[multi]send bad packet = %s\n", err)
		return false
	}
	r, err := self.securityPolicy.InspectEgress(relationship, ipPath, payload)
	if err != nil {
		self.log.Infof("[multi]send bad packet = %s\n", err)
		return false
	}
	// refresh the flow's activity on the send direction (keeps a download-heavy flow alive)
	self.securityPolicy.RefreshEgress(ipPath)

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
	parsedPacket := &parsedPacket{
		packet:  packet,
		ipPath:  ipPath,
		payload: payload,
	}
	success := self.sendPacket(source, provideMode, parsedPacket, timeout)
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

func (self *RemoteUserNatMultiClient) canSendPacket(sendPacket *parsedPacket, update *multiClientChannelUpdate) (allow bool) {
	ipPath := sendPacket.ipPath
	switch ipPath.Protocol {
	case IpProtocolTcp:
		if self.settings.TcpCollapsePrevention {
			// limit sender tcp collapse
			// as soon as a packet is sent to a client, either the client will eith reliabily transfer the packet,
			// or the client will be dropped
			// retransmits don't need to be sent as soon as the packet is committed to a client
			if ipPath.Syn || ipPath.Rst {
				allow = true
			} else if update.canUpdateSequence(sendPacket) {
				// sequence state is guarded by the per-flow `stateLock`, not
				// the parent `stateLock`
				allow = true
			} else if tcpCollapseMaxHold := self.reliabilitySettings().TcpCollapseMaxHold; 0 < tcpCollapseMaxHold &&
				update.releaseSequenceHold(tcpCollapseMaxHold) {
				// the flow has been pinned at the same sequence state past the
				// hold, so the committed packet is not making progress. let a
				// retransmit through rather than discarding the sender's only
				// recovery mechanism until failure detection catches up
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
	sendPacket *parsedPacket,
	timeout time.Duration,
) (success bool) {
	ipPath := sendPacket.ipPath
	self.sendClientPath(ipPath, func(update *multiClientChannelUpdate, currentClient *multiClientChannel) {
		if !self.canSendPacket(sendPacket, update) {
			return
		}

		enterTime := time.Now()

		if ipPath.Syn || ipPath.Rst {
			// sequence state is guarded by the per-flow `stateLock`
			update.resetSequence(sendPacket)
		}

		// Client-side dial-failure inference. A connection attempt
		// retransmitting on an exit that has answered nothing is the silent
		// form of the failure the provider signal covers: the provider cannot
		// reach the destination, or the destination drops the connection
		// without a word, as anti-bot infrastructure commonly does to
		// datacenter ip ranges. No signal ever arrives for those, so the
		// wait is the only evidence. Feeding the same clientDialFailure path
		// as the explicit signal gets the same guards, the same unbind, and
		// the same dial-strike accounting that warns the exit off new flows --
		// while its established traffic keeps running, which is the entire
		// point: an exit is never torn down for a destination's silence.
		// Covers tcp syns and the request-response udp handshakes (quic,
		// dns) -- see dialProbePacket for why the udp side is port-gated.
		// guard order matters on this path: every egress packet passes here,
		// so the plain field checks go first and the atomic load, settings
		// read, and clock only run for a probe on an unestablished flow.
		if dialProbePacket(ipPath) &&
			currentClient != nil && !update.receivedInbound.Load() &&
			self.reliabilitySettings().DialFailureRerace &&
			update.synWaitExceeded(currentClient, inferredDialFailureTimeout) {
			// treat the flow as unbound locally only if it really was: the
			// guards inside can decline after our check passed (a syn-ack can
			// land in between), and racing while the flow is still committed
			// aborts every attempt until the send times out. when it was
			// unbound, this very retransmit races a fresh exit instead of
			// following the old one into the same silence for another backoff
			// round.
			if self.clientDialFailure(currentClient, ipPath) {
				currentClient = nil
			}
		}

		// `currentClient` is the client snapshot read by `sendClientPath` under
		// the parent lock it already held, so the steady-state send no longer
		// takes the parent lock again just to read `update.client`.
		for client := currentClient; client != nil; {
			var err error
			success, err = client.SendDetailed(sendPacket, timeout)
			if success {
				// sequence state is guarded by the per-flow `stateLock`
				update.updateSequence(sendPacket)
			} else if err != nil {
				// reset the path

				self.log.Infof("[multi]reset error = %s\n", err)

				update.client.Store(nil)

				rstPackets := []*receivePacket{}

				if packet, ok := self.teardownSourcePacket(update.ipPath, update.sourceRstSequence()); ok {
					rstPacket := &receivePacket{
						Source:      TransferPath{},
						ProvideMode: protocol.ProvideMode_Network,
						IpPath:      update.ipPath,
						Packet:      packet,
					}
					rstPackets = append(rstPackets, rstPacket)
				}

				select {
				case <-self.ctx.Done():
				default:
					for _, p := range rstPackets {
						self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
					}
				}
			}
			// else the packet was dropped due to backpressure
			// keep sending to the client until there is an error
			return
		}

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

					// client is atomic; lock-free store
					update.client.Store(client)
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

					p := &parsedPacket{
						packet: MessagePoolShareReadOnly(sendPacket.packet),
						ipPath: update.ipPath,
					}
					sent := client.SendWithAck(p, sendTimeout, true)
					if !sent {
						// a failed attempt retains ownership here: undo this
						// attempt's share or the packet never reaches zero
						// references (the race takes one share per client)
						MessagePoolReturn(p.packet)
					}
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
					} else {
						MessagePoolReturn(p.packet)
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
					orderedClients := self.raceCandidates(window)
					if 0 < len(orderedClients) {
						return orderedClients
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
			raceClients(coalesceOrderedClients(), retryTimeout)
			if success {
				return
			}
			endTime := time.Now()
			retryTimeout -= endTime.Sub(startTime)

			if 0 < retryTimeout {
				select {
				case <-update.ctx.Done():
					return
				case <-time.After(retryTimeout):
				}
			}
		}
	})
	return
}

// clientReceivePacketFunction
func (self *RemoteUserNatMultiClient) clientReceivePacket(
	sourceClient *multiClientChannel,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	r, err := self.securityPolicy.InspectIngress(provideMode, ipPath, nil)
	if err != nil {
		return
	}
	// refresh on the return direction before ipPath is reversed for downstream delivery
	self.securityPolicy.RefreshIngress(ipPath)
	if r != SecurityPolicyResultAllow {
		return
	}

	self.packetStatsCounters.remoteIngressPacketCount.Add(1)
	self.packetStatsCounters.remoteIngressByteCount.Add(int64(len(packet)))

	// traffic from a destination whose flows died with an exit closes out the
	// recovery measurement. also before reverse, so the remote endpoint is
	// still the source. a no-op unless that destination is actually pending.
	self.reliabilityMetrics.destinationReachable(ipPath.SourceIp, ipPath.SourcePort)

	if self.ipAssoc != nil {
		// before reverse, the remote endpoint is the source
		self.ipAssoc.AddIngressPacket(ipPath)
	}

	ipPath = ipPath.Reverse()

	var abandonedClients []*multiClientChannel
	var receivePackets []*receivePacket
	var returnPackets []*receivePacket
	// connectSucceeded is set when this inbound packet is the first to resolve
	// its flow to sourceClient -- a proven upstream connect. Recorded on the
	// channel after receiveClientPath returns, outside every lock, so the
	// channel stateLock never nests under the parent or per-flow stateLock.
	connectSucceeded := false
	// boundUpdate is set when this path commits a flow to sourceClient, so the
	// clientUpdates bookkeeping can be recorded after receiveClientPath
	// returns. Same reason as connectSucceeded above: bindClientFlow takes the
	// parent stateLock, and this closure runs under the per-flow leaf lock.
	var boundUpdate *multiClientChannelUpdate
	success := self.receiveClientPath(ipPath, func(update *multiClientChannelUpdate) {
		// steady-state fast path: the flow is already committed to this client,
		// so deliver without taking any lock (client is atomic). this is the
		// common download path and no longer contends the parent stateLock.
		if update.client.Load() == sourceClient {
			// first inbound packet for this flow marks it established, which
			// gates the dial-failure re-race (a stale signal must not unbind a
			// flow already carrying data).
			if update.receivedInbound.CompareAndSwap(false, true) {
				connectSucceeded = true
			}
			p := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPath,
				Packet:      packet,
			}
			receivePackets = []*receivePacket{p}
			return
		}

		// race / not-yet-committed paths are guarded by the per-flow stateLock
		update.stateLock.Lock()
		defer update.stateLock.Unlock()

		client := update.client.Load()

		if client == sourceClient {
			// committed between the lock-free check and acquiring the lock
			if update.receivedInbound.CompareAndSwap(false, true) {
				connectSucceeded = true
			}
			p := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPath,
				Packet:      packet,
			}
			receivePackets = []*receivePacket{p}
		} else if client != nil {
			// another client already chosen, drop
		} else if race := update.race; race == nil {
			// no race, no client, drop
			self.log.Infof("[multi]receive no race and no client")
		} else if state, ok := race.clientStates[sourceClient]; !ok {
			// this client is not part of the race, drop
			self.log.Infof("[multi]receive client not part of race")
		} else if len(state.packets) < self.settings.MultiRaceClientPacketMaxCount && race.packetCount < self.settings.MultiRacePacketMaxCount {
			packetCopy, pooled := MessagePoolCopyDetailed(packet)
			receivePacket := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPath,
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
			// race buffer limits exceeded, end the race immediately
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
			boundUpdate = update
			if update.receivedInbound.CompareAndSwap(false, true) {
				connectSucceeded = true
			}
			receivePacket := &receivePacket{
				Source:      source,
				ProvideMode: provideMode,
				IpPath:      ipPath,
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
	})
	// a flow's first inbound packet is a proven upstream connect on this
	// channel; it resets the dial-strike window (dialStarved requires zero
	// successes). recorded outside every lock held above.
	if connectSucceeded {
		sourceClient.addConnectSuccess()
	}
	// a race won from the receive path commits the flow without the send path
	// ever noticing a change, so this is the only place the bookkeeping can be
	// recorded for it. outside every lock held above, per bindClientFlow.
	self.bindClientFlow(boundUpdate, sourceClient)
	if success {
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
			self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
		}
		for _, p := range returnPackets {
			MessagePoolReturn(p.Packet)
		}
	} else {
		// incoming packets not in response to outgoing packets
		self.receivePacketCallback(source, provideMode, ipPath, packet)
	}
}

// clientDialFailure handles a provider dial-failure signal intercepted on
// sourceClient's channel: an icmp destination-unreachable whose embed named the
// egress flow egressIpPath. The provider could not open the upstream for that
// flow and answered instead of going silent, so the source is not left in
// syn-retransmit backoff.
//
// It acts only on a flow that is (a) still known, (b) still pinned to the very
// client that reported the failure, and (c) has never received inbound data --
// a flow still waiting on its first upstream connect, not an established one a
// late or stale signal happens to name. When those hold and DialFailureRerace
// is on, it unbinds the flow (the removeClient per-update idiom, minus teardown
// and minus cancelling the update) so the application's own retransmit re-races
// it onto another exit within ~1s. With the knob off it rebuilds the icmp and
// forwards it to the app (removeClient's teardown convention) for a
// visible-but-fast failure. Every intercepted signal, matched or not, is
// counted; an unmatched one is dropped.
// It reports whether the flow was actually unbound, so a caller holding its
// own client snapshot knows whether to discard it. The guards here can decline
// after a caller's own check passed -- a syn-ack can land in between -- and
// treating a still-bound flow as unbound leaves that caller racing against a
// commitment that never clears.
func (self *RemoteUserNatMultiClient) clientDialFailure(sourceClient *multiClientChannel, egressIpPath *IpPath) (reraced bool) {
	// counted for every intercepted signal, matched or not: the gap between
	// this and flowsReraced is failures that named no live flow.
	self.reliabilityMetrics.dialFailureIntercepted()

	rerace := self.reliabilitySettings().DialFailureRerace

	matched := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		var update *multiClientChannelUpdate
		switch egressIpPath.Version {
		case 4:
			update = self.ip4PathUpdates[egressIpPath.ToIp4Path()]
		case 6:
			update = self.ip6PathUpdates[egressIpPath.ToIp6Path()]
		default:
			return
		}

		// act only on the flow this very client owns that has not yet received
		// inbound data. a nil update (gone), a different client (already
		// re-raced onto another exit), or an established flow (receivedInbound)
		// all mean this signal is stale and must not disturb a working flow.
		if update == nil || update.client.Load() != sourceClient || update.receivedInbound.Load() {
			return
		}

		matched = true

		if rerace {
			// unbind exactly like removeClient does per-update, minus teardown
			// and minus cancelling the update: drop it from the client's flow
			// set and clear its client pointer. The flow's own next retransmit
			// (SYN ~1s, QUIC PTO, DNS retry) then sees a nil client in
			// sendPacket and races a fresh exit.
			if updates, ok := self.clientUpdates[sourceClient]; ok {
				delete(updates, update)
				if len(updates) == 0 {
					delete(self.clientUpdates, sourceClient)
				}
			}
			update.client.Store(nil)
		}
	}()

	if !matched {
		// no live flow for this signal: dropped here, already counted above.
		return false
	}

	// strike accounting is per-channel and takes the channel's own stateLock;
	// recorded outside the parent stateLock so the two never nest.
	sourceClient.addDialFailure()

	if rerace {
		self.reliabilityMetrics.flowReraced()
		// swallowed: the icmp is never forwarded. the app's retransmit drives
		// recovery. this is the whole point -- ~1s instead of 3-63s.
		return true
	}

	// rerace disabled: hand the icmp to the app so it fails fast instead of
	// hanging. rebuilt from the egress path with the same builder the provider
	// used, and delivered with the egress ipPath -- exactly the convention
	// removeClient uses for its teardown packets.
	if packet, ok := ipOosUnreachable(egressIpPath); ok {
		self.receivePacketCallback(TransferPath{}, protocol.ProvideMode_Network, egressIpPath, packet)
	}
	return false
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
		// the race winner, recorded for the clientUpdates bookkeeping once the
		// per-flow leaf lock below is released -- bindClientFlow takes the
		// parent lock and must never nest under it
		var boundUpdate *multiClientChannelUpdate
		var boundClient *multiClientChannel
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
						boundUpdate, boundClient = update, client
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
		// the race completion commits the flow with no send-path transition to
		// notice it, so this is the only place its bookkeeping can be recorded
		self.bindClientFlow(boundUpdate, boundClient)
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
			self.receivePacketCallback(p.Source, p.ProvideMode, p.IpPath, p.Packet)
		}
		for _, p := range returnPackets {
			MessagePoolReturn(p.Packet)
		}
	})
}

// ExitInfo is one provider channel in the window, as reported to a developer
// menu: enough to see which exits exist, which the flows are pinned to, and
// which are on their way out.
type ExitInfo struct {
	ClientId   Id
	WindowType WindowType
	// Warning marks a client new flows already avoid -- either unhealthy or
	// past MaxClientLifetime and draining
	Warning bool
	Done    bool
	P2pOnly bool
	// FlowCount is how many live flows are currently pinned to this exit
	FlowCount int
	// DialFailureCount is how many upstream dials this exit has reported
	// failing in the recent window -- the signature of a provider whose own
	// upstream (a resold proxy, an exhausted socket table) is refusing work
	// while the provider itself stays reachable
	DialFailureCount int
	// Tier is the platform's rank for this provider (0 is best). The window
	// races only the best rank present until it is at the flow cap, so this is
	// the field that explains why an exit carries no flows: it is a spare on a
	// higher tier, not a failure
	Tier int
}

// Exits reports the provider channels across every window, with the number of
// flows pinned to each. This is the readout that makes the affinity behavior
// observable: a site split across exits shows up here as flows spread over
// several entries instead of collected on one.
func (self *RemoteUserNatMultiClient) Exits() []*ExitInfo {
	flowCounts := map[Id]int{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// count from the flows themselves, not from the clientUpdates
		// bookkeeping. The bookkeeping is only maintained on the send path
		// when it notices the committed client changed, so a race-won flow --
		// winner stored from the receive path or the race completion, then
		// every later send seeing no change -- is frequently never entered.
		// On device that read as "13 exits, flows on only 2" while traffic
		// plainly moved through others: the two visible were affinity
		// clusters (counted at assignment under this lock), the rest carried
		// flows this map had never heard of. update.client is the ground
		// truth: it is what the egress path actually sends on.
		countClient := func(update *multiClientChannelUpdate) {
			if update == nil || update.IsDone() {
				return
			}
			if c := update.client.Load(); c != nil {
				flowCounts[c.ClientId()] += 1
			}
		}
		for _, update := range self.ip4PathUpdates {
			countClient(update)
		}
		for _, update := range self.ip6PathUpdates {
			countClient(update)
		}
	}()

	exits := []*ExitInfo{}
	for windowType, window := range self.windows {
		for _, client := range window.unorderedClients() {
			clientId := client.ClientId()
			exits = append(exits, &ExitInfo{
				ClientId:         clientId,
				WindowType:       windowType,
				Warning:          client.isWarning(),
				Done:             client.IsDone(),
				P2pOnly:          client.IsP2pOnly(),
				FlowCount:        flowCounts[clientId],
				DialFailureCount: client.dialFailureCount(),
				Tier:             client.Tier(),
			})
		}
	}
	return exits
}

// DropExit cancels a single provider channel, as if that one exit had died.
//
// Shuffle() replaces every exit at once, which is not what a real failure looks
// like -- the interesting case, and the one all of the teardown work addresses,
// is one exit vanishing while the others keep working and flows have to
// discover it. Returns false if no such client is in the window.
func (self *RemoteUserNatMultiClient) DropExit(clientId Id) bool {
	for _, window := range self.windows {
		for _, client := range window.unorderedClients() {
			if client.ClientId() == clientId {
				client.Cancel()
				window.resizeMonitor.NotifyAll()
				return true
			}
		}
	}
	return false
}

// StallExit makes a provider channel stop acknowledging without being cancelled,
// reproducing the state the collapse-prevention bound exists for: a client that
// is neither healthy nor detectably dead, holding its flows' sequence state.
//
// Note the stall is not held indefinitely. The swallowed packet is accounted
// for as an outstanding send, so sendStalled trips after SendStallTimeout (3s
// by default) and the resize pass removes and replaces the exit -- which is
// the behavior being exercised. Expect the stalled exit to disappear from the
// window a few seconds after the button, rather than sit there stalled.
// Before that accounting was in place the exit was invisible to the very
// detector this reproduces the input for, and a stall went unnoticed for 34s
// on device.
//
// This is the only way to exercise that path deliberately -- a real stall
// depends on a provider misbehaving at the right moment. Returns false if no
// such client is in the window.
func (self *RemoteUserNatMultiClient) StallExit(clientId Id, stalled bool) bool {
	for _, window := range self.windows {
		for _, client := range window.unorderedClients() {
			if client.ClientId() == clientId {
				client.setStalled(stalled)
				return true
			}
		}
	}
	return false
}

func (self *RemoteUserNatMultiClient) Shuffle() {
	for _, window := range self.windows {
		window.shuffle()
	}
}

func (self *RemoteUserNatMultiClient) Close() {
	self.cancel()

	// release the server-name-learned subscription so the lookup does not retain
	// this (now-closing) multi-client
	var serverNamesLearnedUnsub func()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		serverNamesLearnedUnsub = self.serverNamesLearnedUnsub
		self.serverNamesLearnedUnsub = nil
	}()
	if serverNamesLearnedUnsub != nil {
		serverNamesLearnedUnsub()
	}

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

	// receivedInbound is set true the first time an inbound (non-icmp) packet
	// resolves this flow to its committed client (the receiveClientPath path).
	// It marks the flow established, which gates the dial-failure re-race: a
	// late or stale dial-failure signal must never unbind a flow that is
	// already carrying data. Written lock-free (atomic) from the ingress path.
	receivedInbound atomic.Bool

	// synWaitStart is when synWaitClient was first asked to open this flow's
	// upstream connection -- the first dial probe (tcp syn, or quic/dns udp:
	// see dialProbePacket) sent while receivedInbound is still false. It backs
	// the client-side dial-failure inference: a provider that silently cannot
	// reach the destination (or a destination that silently drops the
	// connection) produces no failure signal at all, so the wait itself is
	// the only evidence. Keyed to synWaitClient so a re-raced flow judges
	// each exit on its own silence; the pointer is only ever compared, never
	// dereferenced, so a closed channel is harmless here.
	// Both guarded by stateLock.
	synWaitStart  time.Time
	synWaitClient *multiClientChannel
	// synWaitSendCount counts probes sent to synWaitClient since the clock
	// started. Past dialProbeMaxSends the flow is a one-way stream rather
	// than a handshake and the inference leaves it alone. Guarded by
	// stateLock alongside the clock it qualifies.
	synWaitSendCount int

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

	sequencePacketCount int // guarded by stateLock
	// sequenceTime is when the sequence state last advanced. it bounds how long
	// TcpCollapsePrevention may keep discarding a sender's retransmits while
	// the committed packet makes no progress. guarded by stateLock.
	sequenceTime      time.Time
	ackSequenceNumber uint32 // guarded by stateLock
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
	return &multiClientChannelUpdate{
		ctx:              cancelCtx,
		cancel:           cancel,
		ipPath:           ipPath,
		affinityIp4Paths: map[Ip4Path]bool{},
		affinityIp6Paths: map[Ip6Path]bool{},
	}
}

// inferredDialFailureTimeout is how long a flow's initial connect may go
// unanswered on one exit before the wait itself is treated as a dial failure.
//
// TCP retransmits its syn at roughly 1s, 2s, 4s -- so 3s means the second or
// third retransmit re-races instead of following the same path into the same
// silence. Genuine slow dials overwhelmingly complete inside 3s, and the cost
// of a wrong inference is one syn racing a different exit, not a teardown: if
// the destination is merely slow, the connect still completes wherever the
// race lands it.
const inferredDialFailureTimeout = 3 * time.Second

// dialProbeMaxSends bounds how many sends into silence still look like a
// handshake. No sane transport sends more than its initial window before the
// first response -- quic's initial cwnd is 10 packets, a tcp syn retransmits
// at most ~7 times in a minute -- so a flow past this count with nothing back
// is not dialing, it is a one-way stream (a udp pump, send-heavy telemetry on
// a watched port), and re-racing it would drop its in-flight responses for no
// diagnostic gain. Streams into silence are the blackhole detector's job.
// Caught by TestMultiClientUdp6, whose long-lived udp/53 stream was churned
// by the inference whenever the first echo ran past the wait under load.
const dialProbeMaxSends = 16

// dialProbePacket reports whether this egress packet is a connection attempt
// whose continued silence is diagnostic of a dead path: a pure tcp syn, or a
// packet of a request-response udp protocol -- quic on 443, dns on 53 --
// on a flow that has never received a byte.
//
// The port gate is what keeps the inference honest for udp. Tcp declares its
// intent in the syn flag, but a udp datagram carries no handshake marker, and
// send-only protocols (telemetry, some game and logging traffic) legitimately
// never hear back -- re-racing those on silence would bounce them between
// exits forever and strike healthy exits for a silence that is normal. Quic
// and dns always expect an answer, are the overwhelming mass of udp here
// (56% of traffic is quic), and are exactly the flows observed pinned to a
// dead exit for the full blackhole bound: on device, three exits held 63
// flows in syn-and-quic silence for 29s -- 22-28 unanswered syns each, 0
// bytes received -- because only their tcp minority could escape early.
//
// The caller layers the remaining guards: flow unestablished, wait exceeded
// per exit, and the same clientDialFailure gate the provider's explicit
// signal uses, so quic's established-flow protections are identical to tcp's.
// A moved quic flow survives the exit change by design: quic keys the
// connection on its connection id, not the 4-tuple.
func dialProbePacket(ipPath *IpPath) bool {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		return ipPath.Syn && !ipPath.Ack
	case IpProtocolUdp:
		switch ipPath.DestinationPort {
		case 443, 53:
			return true
		}
	}
	return false
}

// synWaitExceeded starts the connect-wait clock on the first dial probe (a
// tcp syn or a udp handshake packet, see dialProbePacket) a given exit
// carries for this flow, and reports whether it has run past timeout on later
// probes through that same exit. The clock is keyed to the exit: a flow that
// re-races restarts the wait, so each exit is judged on its own silence rather
// than inheriting the previous one's -- otherwise the first probe through a
// fresh exit would strike it immediately. When it trips, the clock restarts
// for the same reason.
func (self *multiClientChannelUpdate) synWaitExceeded(client *multiClientChannel, timeout time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	if self.synWaitClient != client || self.synWaitStart.IsZero() {
		self.synWaitClient = client
		self.synWaitStart = now
		self.synWaitSendCount = 1
		return false
	}
	self.synWaitSendCount += 1
	// past an initial window of sends with nothing back this flow is a
	// one-way stream, not a handshake -- see dialProbeMaxSends. It stays
	// exempt on this exit; a re-race re-keys the clock and the count.
	if dialProbeMaxSends < self.synWaitSendCount {
		return false
	}
	if timeout <= now.Sub(self.synWaitStart) {
		self.synWaitStart = now
		return true
	}
	return false
}

func (self *multiClientChannelUpdate) resetSequence(sendPacket *parsedPacket) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	ipPath := sendPacket.ipPath

	self.ackSequenceNumber = ipPath.AckSequenceNumber
	self.sequenceNumber = ipPath.SequenceNumber
	self.sequencePacketCount = 0
	self.sequenceTime = time.Now()
}

func (self *multiClientChannelUpdate) updateSequence(sendPacket *parsedPacket) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	ipPath := sendPacket.ipPath
	update := false

	nextAckSequenceNumber := ipPath.AckSequenceNumber
	if self.ackSequenceNumber != nextAckSequenceNumber {
		self.ackSequenceNumber = nextAckSequenceNumber
		update = true
	}

	// modular uint32 add wraps correctly across the 4 GB boundary
	nextSequenceNumber := ipPath.SequenceNumber + uint32(len(sendPacket.payload))
	// signed-delta comparison is wraparound-tolerant: > 0 means nextSequenceNumber
	// is later in TCP sequence space than self.sequenceNumber
	if 0 < int32(nextSequenceNumber-self.sequenceNumber) {
		self.sequenceNumber = nextSequenceNumber
		update = true
	}

	if update {
		self.sequencePacketCount += 1
		self.sequenceTime = time.Now()
	}
}

// releaseSequenceHold reports whether the flow has sat at the same sequence
// state for at least maxHold, and when it has, restarts the window.
//
// TcpCollapsePrevention discards a sender's retransmits on the premise that the
// packet already committed to a client will either be delivered reliably or the
// client will be dropped. When a client stalls without yet being declared dead,
// that premise fails: retransmits -- the sender's only recovery mechanism --
// are discarded for as long as failure detection takes (up to AckTimeout, 30s),
// and the flow is frozen the whole time.
//
// Restarting the window on release means at most one retransmit is admitted per
// maxHold rather than the whole backlog, so collapse prevention still holds
// during a normal stall while a genuinely stuck flow keeps a way to recover.
func (self *multiClientChannelUpdate) releaseSequenceHold(maxHold time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// nothing committed yet: canUpdateSequence already allows these
	if self.sequencePacketCount == 0 || self.sequenceTime.IsZero() {
		return false
	}

	now := time.Now()
	if now.Sub(self.sequenceTime) < maxHold {
		return false
	}
	self.sequenceTime = now
	return true
}

// sourceRstSequence is the sequence a reset toward the source must carry to be
// accepted: the sequence the source expects next from the destination, which is
// the ack number it has been sending. See `ipOosRstSequence`.
func (self *multiClientChannelUpdate) sourceRstSequence() uint32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ackSequenceNumber
}

func (self *multiClientChannelUpdate) canUpdateSequence(sendPacket *parsedPacket) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.sequencePacketCount == 0 {
		return true
	}

	ipPath := sendPacket.ipPath

	if self.ackSequenceNumber != ipPath.AckSequenceNumber {
		return true
	}

	nextSequenceNumber := ipPath.SequenceNumber + uint32(len(sendPacket.payload))
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
	packet  []byte
	ipPath  *IpPath
	payload []byte
}

func newParsedPacket(packet []byte) (*parsedPacket, error) {
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		return nil, err
	}
	return &parsedPacket{
		packet: packet,
		ipPath: ipPath,
	}, nil
}

type multiClientWindow struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	generator                   MultiClientGenerator
	clientReceivePacketCallback clientReceivePacketFunction
	dialFailureCallback         dialFailureFunction
	ingressSecurityPolicy       SecurityPolicy
	clientRemoveCallback        func(client *multiClientChannel)
	windowType                  WindowType

	settings *MultiClientSettings
	// reliabilitySettingsFunc reads the parent's effective reliability config,
	// which the developer menu can override at runtime. A callback rather than a
	// back-reference so the window keeps its existing lifetime and the bare
	// window fixtures in the suite stay constructible. nil falls back to
	// `settings`.
	reliabilitySettingsFunc func() *ReliabilitySettings

	clientChannelArgs chan *multiClientChannelArgs

	monitor *RemoteUserNatMultiClientMonitor

	contractStatusCallbacks *CallbackList[*contractStatusCallbackWorker]
	contractStatsCallbacks  *CallbackList[ContractStatsFunction]
	// relayed from every window client's encryption session manager
	peerIdentityChangeCallbacks *CallbackList[func()]

	stateLock          sync.Mutex
	clients            map[Id]*multiClientChannel
	performanceProfile *PerformanceProfile

	generatorMonitor *Monitor
	resizeMonitor    *Monitor
}

func newMultiClientWindow(
	ctx context.Context,
	cancel context.CancelFunc,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
	dialFailureCallback dialFailureFunction,
	ingressSecurityPolicy SecurityPolicy,
	clientRemoveCallback func(client *multiClientChannel),
	windowType WindowType,
	settings *MultiClientSettings,
	reliabilitySettingsFunc func() *ReliabilitySettings,
) *multiClientWindow {
	window := &multiClientWindow{
		ctx:                         ctx,
		cancel:                      cancel,
		log:                         loggerOrDefault(settings.Log),
		generator:                   generator,
		clientReceivePacketCallback: clientReceivePacketCallback,
		dialFailureCallback:         dialFailureCallback,
		ingressSecurityPolicy:       ingressSecurityPolicy,
		clientRemoveCallback:        clientRemoveCallback,
		windowType:                  windowType,
		settings:                    settings,
		reliabilitySettingsFunc:     reliabilitySettingsFunc,
		clientChannelArgs:           make(chan *multiClientChannelArgs),
		monitor:                     NewRemoteUserNatMultiClientMonitor(&settings.RemoteUserNatMultiClientMonitorSettings),
		contractStatusCallbacks:     NewCallbackList[*contractStatusCallbackWorker](),
		contractStatsCallbacks:      NewCallbackList[ContractStatsFunction](),
		peerIdentityChangeCallbacks: NewCallbackList[func()](),
		clients:                     map[Id]*multiClientChannel{},
		generatorMonitor:            NewMonitor(),
		resizeMonitor:               NewMonitor(),
	}

	go HandleError(window.randomEnumerateClientArgs, cancel)
	go HandleError(window.resize, cancel)
	go HandleError(window.watchSendStalls, cancel)

	return window
}

// sendStallPollTimeout is how often the stall watchdog looks.
//
// A fraction of the stall timeout, so a stall is noticed within roughly its own
// timeout rather than a multiple of it, with a floor so a very small timeout
// cannot turn the watchdog into a busy loop. When stall detection is disabled
// the watchdog idles at the resize cadence instead of exiting, so enabling it
// at runtime from the developer menu is picked up without a reconnect.
func sendStallPollTimeout(stallTimeout time.Duration, resizeTimeout time.Duration) time.Duration {
	if stallTimeout <= 0 {
		return resizeTimeout
	}
	return max(stallTimeout/3, 250*time.Millisecond)
}

// watchSendStalls wakes the resize pass as soon as a client stops delivering.
//
// The stall check itself lives in resize, which otherwise runs on
// WindowResizeTimeout -- 15s. Detecting a stall at 3s is worth nothing if it is
// only consulted every 15s, and device testing showed exactly that: a stalled
// exit took 15-30s to recover rather than the intended 3. This polls on a
// fraction of the stall timeout and notifies the monitor, so the pass that
// removes the client runs promptly instead of on the next scheduled sweep.
//
// Only a notification -- the decision to remove stays in resize, so there is
// one place that classifies a client.
func (self *multiClientWindow) watchSendStalls() {
	for {
		stallTimeout := self.reliabilitySettings().SendStallTimeout

		pollTimeout := sendStallPollTimeout(stallTimeout, self.settings.WindowResizeTimeout)

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(pollTimeout):
		}

		if stallTimeout <= 0 {
			continue
		}
		for _, client := range self.unorderedClients() {
			if client.sendStalled(stallTimeout) {
				self.resizeMonitor.NotifyAll()
				break
			}
		}
	}
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
	callbackId := self.contractStatsCallbacks.Add(contractStatsCallback)
	return func() {
		self.contractStatsCallbacks.Remove(callbackId)
	}
}

// registered on every window client's contract manager.
// the manager's epoch worker calls this off the packet paths
func (self *multiClientWindow) contractStats(contractStatsEvents []*ContractStatsEvent) {
	for _, contractStatsCallback := range self.contractStatsCallbacks.Get() {
		HandleError(func() {
			contractStatsCallback(contractStatsEvents)
		})
	}
}

func (self *multiClientWindow) AddPeerIdentityChangeCallback(callback func()) func() {
	callbackId := self.peerIdentityChangeCallbacks.Add(callback)
	return func() {
		self.peerIdentityChangeCallbacks.Remove(callbackId)
	}
}

// registered on every window client's encryption session manager
// (and fired once more when a window client is removed)
func (self *multiClientWindow) peerIdentityChanged() {
	for _, callback := range self.peerIdentityChangeCallbacks.Get() {
		HandleError(callback)
	}
}

// the performance profile will take effect at the next `resize` iteration
func (self *multiClientWindow) SetPerformanceProfile(performanceProfile *PerformanceProfile) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.performanceProfile = performanceProfile
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

		destinations, err := self.generator.NextDestinations(
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
					var clientArgs *MultiClientGeneratorClientArgs
					var err error
					if destinationGenerator, ok := self.generator.(MultiClientGeneratorWithDestination); ok {
						clientArgs, err = destinationGenerator.NewClientArgsForDestination(destination)
					} else {
						clientArgs, err = self.generator.NewClientArgs()
					}
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
			if profileWindowType, profileWindowSize, ok := self.performanceProfile.FixedWindow(); ok {
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

			// a client holding unacked sends with no progress is failed, however
			// healthy its history looks. it is not erroring and not idle, so
			// nothing else here classifies it until AckTimeout, and every flow
			// pinned to it is frozen until then
			sendStalled := client.sendStalled(self.reliabilitySettings().SendStallTimeout)

			healthy := !sendStalled && stats.unhealthyDuration < self.settings.StatsWindowMaxUnhealthyDuration

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
			// a stalled client is removed regardless of rank. rank-keeping exists
			// to ride out transient badness in the healthiest clients, but this
			// one is provably delivering nothing, and keeping it holds its flows
			// frozen -- removal is what lets them re-race onto a working client
			if sendStalled {
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
						// a dial-starved provider -- its own upstream refusing
						// connects -- warns out of new-flow selection so fresh
						// flows avoid it, while its established flows keep
						// working. This is the warning site ONLY: it must never
						// reach the remove decision above, because those flows
						// are the provider's only working asset.
						//
						// only when there is somewhere else to go. warning the
						// sole exit of a fixed window blocks every new flow
						// while helping none of them, and with the client-side
						// inference feeding strikes, one silently-dead polled
						// destination could oscillate a single-exit window
						// between warned and not indefinitely.
						if ulimit || (1 < len(clientStats) && client.dialStarved()) {
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

		var windowSizeMin int
		var targetWindowSize int
		if fixedDestinationSize, fixed := self.generator.FixedDestinationSize(); fixed {
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

		addedCount := 0
		if len(clients) < targetWindowSize {
			// expand
			n := targetWindowSize - len(clients)
			self.monitor.AddWindowExpandEvent(
				windowSizeMin <= len(clients),
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
				windowSizeMin <= len(clients)+addedCount,
				windowSize.WindowSizeHardMax,
			)
			collapseLowestWeighted(max(0, windowSize.WindowSizeHardMax-addedCount))
			if self.log.V(1).Enabled() {
				self.log.Infof("[multi]window collapse -%d ->%d\n", (len(clients)+len(warnedClients)+addedCount)-windowSize.WindowSizeHardMax, windowSize.WindowSizeHardMax)
			}
		} else {
			self.monitor.AddWindowExpandEvent(
				windowSizeMin <= len(clients)+addedCount,
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

func (self *multiClientWindow) expand(
	windowSize WindowSizeSettings,
	currentWindowSize int,
	currentP2pOnlyWindowSize int,
	targetWindowSize int,
	windowSizeMin int,
	n int,
) (returnPingSuccess int) {
	mutex := sync.Mutex{}
	pendingPingDones := []context.Context{}
	added := 0
	addedP2pOnly := 0
	pingSuccess := 0

	defer func() {
		mutex.Lock()
		defer mutex.Unlock()

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
					mutex.Lock()
					defer mutex.Unlock()

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
				self.dialFailureCallback,
				self.ingressSecurityPolicy,
				self.contractStatus,
				self.contractStats,
				self.peerIdentityChanged,
				self.performanceProfile,
				self.settings,
				self.reliabilitySettings,
			)
			if err != nil {
				self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
				self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
			} else {

				// send an initial ping on the client and let the ack timeout close it
				pingDone, pingCancel := context.WithCancel(self.ctx)
				pendingPingDones = append(pendingPingDones, pingDone)

				// must be called with mutex
				fail := func() {
					select {
					case <-pingDone.Done():
						// already done
						return
					default:
					}

					pingCancel()
					client.Cancel()
					self.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
					self.monitor.AddProviderEvent(args.ClientId, ProviderStateEvaluationFailed)
				}

				go HandleError(func() {
					mutex.Lock()
					defer mutex.Unlock()

					select {
					case <-pingDone.Done():
						// already done
						return
					default:
					}

					added += 1
					if client.IsP2pOnly() {
						addedP2pOnly += 1
					}

					self.monitor.AddProviderEvent(args.ClientId, ProviderStateInEvaluation)

					success, err := client.SendDetailedMessage(
						&protocol.IpPing{},
						self.settings.PingWriteTimeout,
						func(err error) {
							mutex.Lock()
							defer mutex.Unlock()

							select {
							case <-pingDone.Done():
								// already done
								return
							default:
							}

							if err == nil {
								self.log.V(1).Infof("[multi]expand new client\n")

								var replacedClient *multiClientChannel
								func() {
									self.stateLock.Lock()
									defer self.stateLock.Unlock()
									clientId := client.ClientId()
									replacedClient = self.clients[clientId]
									self.clients[clientId] = client
								}()
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
								pingSuccess += 1
								pingCancel()
							} else {
								if self.log.V(1).Enabled() {
									self.log.Infof("[multi]create ping error = %s\n", err)
								}
								fail()
							}
						},
					)
					if err != nil {
						self.log.Infof("[multi]create client ping error = %s\n", err)
						fail()
					} else if !success {
						fail()
					} else {
						// async wait for the ping
						go HandleError(func() {
							select {
							case <-pingDone.Done():
							case <-time.After(self.settings.PingTimeout):
								self.log.V(2).Infof("[multi]expand window timeout waiting for ping\n")
								func() {
									mutex.Lock()
									defer mutex.Unlock()
									fail()
								}()
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

// reliabilitySettings is the effective reliability config for this window: the
// parent's runtime override when one is installed, else what the window was
// constructed with. Safe on a bare window, which the suite's fixtures rely on.
func (self *multiClientWindow) reliabilitySettings() *ReliabilitySettings {
	if self.reliabilitySettingsFunc != nil {
		return self.reliabilitySettingsFunc()
	}
	return ReliabilitySettingsFrom(self.settings)
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

// OrderedClients is the window's offer to the race: its healthy clients,
// weighted-shuffled, narrowed to the best rank present ("min tier") so
// traffic does not cross rank until necessary.
func (self *multiClientWindow) OrderedClients() []*multiClientChannel {
	return self.orderedClients(false)
}

// orderedClientsCrossTier is OrderedClients without the min-tier gate, for
// the caller that has decided crossing rank IS now necessary -- every min-tier
// exit sitting at the flow cap. See RemoteUserNatMultiClient.raceCandidates.
func (self *multiClientWindow) orderedClientsCrossTier() []*multiClientChannel {
	return self.orderedClients(true)
}

func (self *multiClientWindow) orderedClients(crossTier bool) []*multiClientChannel {
	var windowSize WindowSizeSettings
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if _, profileWindowSize, ok := self.performanceProfile.FixedWindow(); ok {
			windowSize = profileWindowSize
		} else {
			windowSize = self.settings.WindowSizes[self.windowType]
		}
	}()

	clients := []*multiClientChannel{}
	lruTimes := map[*multiClientChannel]time.Time{}
	weights := map[*multiClientChannel]float32{}

	for _, client := range self.unorderedClients() {
		if stats, err := client.WindowStats(); err == nil && !client.isWarning() {
			clients = append(clients, client)
			if !stats.lastEventTime.IsZero() {
				lruTimes[client] = stats.lastEventTime
			}
			weights[client] = float32(1 + stats.ExpectedByteCountPerSecond())
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

	if crossTier {
		return clients
	}

	// use only the top n items from the window
	// if 0 < windowSize.WindowSizeUseMax {
	// 	clients = clients[:min(len(clients), windowSize.WindowSizeUseMax)]
	// }

	// use only clients in the min tier
	// this prevents the window from crossing rank until necessary
	return minTierClients(clients)
}

// minTierClients keeps only the clients of the best (lowest) rank present,
// preserving order. Empty input yields empty output.
func minTierClients(clients []*multiClientChannel) []*multiClientChannel {
	if len(clients) == 0 {
		return clients
	}
	minTier := clients[0].Tier()
	for _, client := range clients[1:] {
		minTier = min(minTier, client.Tier())
	}
	kept := []*multiClientChannel{}
	for _, client := range clients {
		if client.Tier() == minTier {
			kept = append(kept, client)
		}
	}
	return kept
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
	dialFailureCallback         dialFailureFunction
	ingressSecurityPolicy       SecurityPolicy
	performanceProfile          *PerformanceProfile
	createTime                  time.Time

	settings *MultiClientSettings
	// reliabilitySettingsFunc reads the parent's effective reliability config,
	// so the blackhole bound can be retuned on a live connection. nil on
	// channels built directly by tests; see reliabilitySettings().
	reliabilitySettingsFunc func() *ReliabilitySettings

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

	warning bool

	// pendingSendTime is when the current run of unacked sends began, reset on
	// every ack. With sendNackCount > 0 it is the age of the oldest unmade
	// progress, which is what sendStalled tests. Guarded by stateLock.
	pendingSendTime time.Time

	// stalled is a test/diagnostic hook, set only by StallExit. Read on the
	// send hot path, so an atomic rather than the state lock.
	stalled atomic.Bool

	// dialFailureTimes and connectSuccessTimes are this channel's sliding-window
	// strike record: a timestamp appended on each intercepted dial failure, and
	// on each flow that first receives inbound data (a proven upstream connect).
	// Both are pruned to dialStrikeWindow on access. Guarded by stateLock. A
	// provider whose own upstream is refusing work -- a resold proxy over its
	// concurrency cap, an exhausted socket table -- shows failures with no
	// successes here, which dialStarved reports so the resize pass warns it out
	// of new-flow selection without destroying its established flows.
	dialFailureTimes    []time.Time
	connectSuccessTimes []time.Time
}

func newMultiClientChannel(
	ctx context.Context,
	args *multiClientChannelArgs,
	generator MultiClientGenerator,
	clientReceivePacketCallback clientReceivePacketFunction,
	dialFailureCallback dialFailureFunction,
	ingressSecurityPolicy SecurityPolicy,
	contractStatusCallback ContractStatusFunction,
	contractStatsCallback ContractStatsFunction,
	peerIdentityChangeCallback func(),
	performanceProfile *PerformanceProfile,
	settings *MultiClientSettings,
	// reliabilitySettingsFunc reads the parent's effective reliability config
	// so the blackhole bound can be retuned on a live connection. nil falls
	// back to settings, which the suite's bare-channel fixtures rely on.
	reliabilitySettingsFunc func() *ReliabilitySettings,
) (*multiClientChannel, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientSettings := generator.NewClientSettings()
	clientSettings.SendBufferSettings.AckTimeout = settings.AckTimeout
	if performanceProfile != nil && performanceProfile.PostQuantumEncryption {
		// pqe: opportunistic per-peer e2e sessions (post-quantum key
		// exchange). A provider without session support falls back to
		// plaintext at this layer.
		if clientSettings.EncryptionSettings == nil {
			clientSettings.EncryptionSettings = DefaultEncryptionSettings()
		}
		clientSettings.EncryptionSettings.Encrypt = true
	}

	client, err := generator.NewClient(
		cancelCtx,
		&args.MultiClientGeneratorClientArgs,
		clientSettings,
	)
	if err != nil {
		cancel()
		return nil, err
	}
	contractStatusSub := client.ContractManager().AddContractStatusCallback(contractStatusCallback)
	contractStatsSub := client.ContractManager().AddContractStatsCallback(contractStatsCallback)
	peerIdentitySub := client.EncryptionSessionManager().AddPeerIdentityChangeCallback(peerIdentityChangeCallback)
	go HandleError(func() {
		select {
		case <-cancelCtx.Done():
		case <-client.Done():
		}
		// fire the contract-close events for this client's still-open contracts
		// while the stats listener below is still attached, BEFORE cancelling the
		// client (which stops the epoch worker without emitting). Otherwise a
		// removed peer's contracts linger open forever in the contract-details UI.
		client.CloseContractStats()
		client.Cancel()
		contractStatusSub()
		contractStatsSub()
		peerIdentitySub()
		// the removed client's established peers leave the aggregate set
		peerIdentityChangeCallback()
		generator.RemoveClientWithArgs(client, &args.MultiClientGeneratorClientArgs)
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
		dialFailureCallback:         dialFailureCallback,
		ingressSecurityPolicy:       ingressSecurityPolicy,
		performanceProfile:          performanceProfile,
		createTime:                  time.Now(),
		settings:                    settings,
		// sourceFilter: sourceFilter,
		client:                    client,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: loggerOrDefault(settings.Log)},
		reliabilitySettingsFunc:   reliabilitySettingsFunc,
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
	// bare fixture channels have no args; rank them best rather than panic on
	// the selection path
	if self.args == nil {
		return 0
	}
	return self.args.DestinationStats.Tier
}

func (self *multiClientChannel) EstimatedByteCountPerSecond() ByteCount {
	return self.args.EstimatedBytesPerSecond
}

// reliabilitySettings is the effective reliability config for this channel:
// the parent's runtime override when one is installed, else what the channel
// was constructed with. Safe on a channel built without the parent, which the
// suite's fixtures rely on.
func (self *multiClientChannel) reliabilitySettings() *ReliabilitySettings {
	if self.reliabilitySettingsFunc != nil {
		return self.reliabilitySettingsFunc()
	}
	return ReliabilitySettingsFrom(self.settings)
}

// setStalled makes the channel swallow packets without acknowledging or
// erroring. See RemoteUserNatMultiClient.StallExit.
func (self *multiClientChannel) setStalled(stalled bool) {
	self.stalled.Store(stalled)
}

// sendStalled reports whether this client has outstanding sends that have made
// no progress for at least stallTimeout: bytes committed, nothing acknowledged.
//
// This is the state a client is in when it accepts packets and never delivers
// them. Failure detection otherwise waits for AckTimeout (30s), because the
// client is neither erroring nor going quiet -- it looks busy. Every flow
// pinned to it is frozen for that whole window, and releasing the sender's
// retransmits does not help: they go to the same black hole. The flow can only
// recover once this client is out of the window, so the useful signal is "in
// flight, no acks", and it is available within seconds.
//
// 0 disables the check, restoring the previous AckTimeout-bounded behavior.
func (self *multiClientChannel) sendStalled(stallTimeout time.Duration) bool {
	if stallTimeout <= 0 {
		return false
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// nothing outstanding means nothing to be stalled on -- an idle client is
	// not a broken one
	if self.packetStats.sendNackCount <= 0 || self.pendingSendTime.IsZero() {
		return false
	}
	return stallTimeout <= time.Since(self.pendingSendTime)
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

// dialStrikeWindow is how long a dial failure or a connect success counts
// toward the starvation decision. Matches the recovery tracker's horizon: long
// enough that a burst of failures is not forgotten between resize passes, short
// enough that a provider that recovers stops being warned within a minute.
const dialStrikeWindow = 60 * time.Second

// dialStarvedFailureThreshold is how many intercepted dial failures (with zero
// successes) mark a channel starved. A couple of failures are noise -- a site
// that is genuinely down, a transient blip; a sustained run with nothing
// connecting is the resold-proxy-over-cap signature this warns on.
const dialStarvedFailureThreshold = 3

// pruneStrikeTimes drops timestamps strictly before horizon. The slices are
// append-only and time-ordered, so a prefix scan is sufficient.
func pruneStrikeTimes(times []time.Time, horizon time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(horizon) {
		i++
	}
	return times[i:]
}

// addDialFailure records one intercepted dial failure for this channel.
func (self *multiClientChannel) addDialFailure() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	self.dialFailureTimes = append(pruneStrikeTimes(self.dialFailureTimes, now.Add(-dialStrikeWindow)), now)
}

// addConnectSuccess records one proven upstream connect for this channel (a
// flow that received its first inbound data). A single success in the window
// clears dialStarved.
func (self *multiClientChannel) addConnectSuccess() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	self.connectSuccessTimes = append(pruneStrikeTimes(self.connectSuccessTimes, now.Add(-dialStrikeWindow)), now)
}

// dialStarved reports whether this channel's upstream is refusing work: at
// least dialStarvedFailureThreshold intercepted dial failures and zero proven
// connects inside the sliding window. It gates new-flow selection only (the
// resize pass warning); it must never feed the removal decision, because a
// dial-starved provider's established flows are its only working asset and
// destroying them is the bug this whole design exists to avoid.
func (self *multiClientChannel) dialStarved() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	horizon := time.Now().Add(-dialStrikeWindow)
	self.dialFailureTimes = pruneStrikeTimes(self.dialFailureTimes, horizon)
	self.connectSuccessTimes = pruneStrikeTimes(self.connectSuccessTimes, horizon)
	return dialStarvedFailureThreshold <= len(self.dialFailureTimes) && len(self.connectSuccessTimes) == 0
}

// dialFailureCount is the number of intercepted dial failures in the current
// window, for ExitInfo.DialFailureCount. Prunes on access like dialStarved.
func (self *multiClientChannel) dialFailureCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.dialFailureTimes = pruneStrikeTimes(self.dialFailureTimes, time.Now().Add(-dialStrikeWindow))
	return len(self.dialFailureTimes)
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
	var ack bool
	switch parsedPacket.ipPath.Protocol {
	case IpProtocolUdp:
		if self.settings.UdpCollapsePrevention {
			ack = false
		} else {
			ack = true
		}
	default:
		ack = true
	}
	return self.SendDetailedWithAck(parsedPacket, timeout, ack)
}

func (self *multiClientChannel) SendWithAck(parsedPacket *parsedPacket, timeout time.Duration, ack bool) bool {
	success, err := self.SendDetailedWithAck(parsedPacket, timeout, ack)
	return success && err == nil
}

func (self *multiClientChannel) SendDetailedWithAck(parsedPacket *parsedPacket, timeout time.Duration, ack bool) (bool, error) {
	if frame, err := ipPacketToProviderFrame(parsedPacket.packet, self.settings.ProtocolVersion); err != nil {
		self.addError(err)
		return false, err
	} else {
		packetByteCount := ByteCount(len(parsedPacket.packet))
		self.addSend(packetByteCount, parsedPacket.ipPath)

		// a stalled exit swallows the packet: reported sent, never acknowledged,
		// and crucially no error -- an error would reset the flow immediately,
		// which is the opposite of the state being reproduced. see StallExit.
		//
		// this must come *after* addSend. addSend is what starts the stall
		// clock (pendingSendTime) and counts the send as outstanding, and
		// sendStalled treats a client with nothing outstanding as idle rather
		// than broken. returning before it made a stalled exit invisible to
		// the detector built to catch stalled exits: on device, a stall went
		// unnoticed for 34s while the flows on it were dead. a provider that
		// really blackholes takes this path and is accounted for, so swallowing
		// here is also the faithful simulation -- the packet is committed and
		// simply never acknowledged.
		if self.stalled.Load() {
			return true, nil
		}

		ackCallback := func(err error) {
			if err == nil {
				self.addSendAck(packetByteCount)
			} else {
				self.addError(err)
			}
		}

		var opts []any
		if self.performanceProfile != nil && self.performanceProfile.AllowDirect {
			opts = append(opts, ForceStream())
		}
		if !ack {
			opts = append(opts, NoAck())
		}
		success, err := self.client.SendMultiHopWithTimeoutDetailed(
			frame,
			self.args.Destination,
			ackCallback,
			timeout,
			opts...,
		)
		// ownership: `parsedPacket.packet` is consumed on success and stays with the
		// caller on any failure. The wrapped (!raw) marshal buffer is internal and
		// must be freed on any failure; for raw frames the frame bytes ARE the
		// caller's packet, so they are never freed here on failure.
		if err != nil {
			if !frame.Raw {
				MessagePoolReturn(frame.MessageBytes)
			}
			return success, err
		}
		if success {
			if !frame.Raw {
				MessagePoolReturn(parsedPacket.packet)
			}
		} else {
			if !frame.Raw {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
		return success, err
	}
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

// blackholeReason names which signal removed a provider. The three used to
// report an identical error string, so a field capture could not tell them
// apart -- the discriminating counts live only on a V(1) line that is off in
// the field. Naming them makes the next capture decisive.
type blackholeReason string

const (
	blackholeNone blackholeReason = ""
	// the provider acknowledges nothing: it is not there
	blackholeNoSendAck blackholeReason = "no-send-ack"
	// the provider acknowledges our sends but no destination data comes back
	blackholeNoReceiveAck blackholeReason = "no-receive-ack"
	// no connection was ever established back
	blackholeNoReceiveSyn blackholeReason = "no-receive-syn"
)

// blackholeReasonFromStats decides whether a provider looks like a blackhole,
// and by which signal.
//
// The signals differ in strength, so they get different bars:
//
//   - The provider acknowledges nothing. It is not there. Unambiguous, acted
//     on at blackholeTimeout.
//   - The provider acknowledges our sends but no destination data comes back.
//     It is demonstrably alive, and may simply be carrying a flow waiting on a
//     slow origin. Removing an exit destroys every flow pinned to it, not just
//     the quiet one, so this needs receiveTimeout -- a longer bar.
//
// Sharing one 5s bound removed 44 providers out of 44 on mainnet, about one
// every 18s under load, every one still acknowledging sends (up to 602 sends /
// 222KB). receiveTimeout of 0 disables the weaker check, leaving only the
// unambiguous one.
//
// IMPORTANT: receiveTimeout has a hard ceiling of roughly
// StatsWindowDuration + StatsWindowBucketDuration (~31s at production
// constants). firstSendNackTime is derived from surviving buckets, and
// coalesceEventBuckets drops every bucket older than StatsWindowDuration, so
// sendNackAge can never exceed that. A receiveTimeout above the ceiling is
// silently equivalent to 0 -- it does not error, it just never fires. See
// TestBlackholeReceiveTimeoutIsReachable.
//
// Split out from detectBlackhole so the decision can be tested against real
// window stats rather than restated in a test, where it could drift from what
// actually ships.
func blackholeReasonFromStats(
	now time.Time,
	windowStats *clientWindowStats,
	blackholeTimeout time.Duration,
	receiveTimeout time.Duration,
	connectTimeout time.Duration,
) blackholeReason {
	if !windowStats.firstSendNackTime.IsZero() {
		sendNackAge := now.Sub(windowStats.firstSendNackTime)

		if blackholeTimeout <= sendNackAge && windowStats.sendAckCount <= 0 {
			return blackholeNoSendAck
		}
		if 0 < receiveTimeout && receiveTimeout <= sendNackAge && windowStats.receiveAckCount <= 0 {
			return blackholeNoReceiveAck
		}
	}

	if !windowStats.firstSendSynTime.IsZero() {
		// unanswered syns alone must not remove an exit whose established
		// traffic is flowing. the syn-acks here are built by the provider only
		// after its upstream dial succeeds, so "syns out, none back" conflates
		// three cases the counter cannot tell apart: the provider cannot dial,
		// the destination silently drops connections (datacenter ip ranges are
		// widely dropped by anti-bot infrastructure), or the destination is
		// merely slow. in two of those the exit is innocent -- and in all
		// three, its established flows are its only working asset. on device
		// this branch removed an exit moving 48 packets / 8.7KB of return
		// traffic because ~18 syns (a handful of destinations, retransmitting)
		// went unanswered, destroying 276 working connections.
		//
		// connect trouble on a live exit is instead handled per-flow: the
		// unanswered flow re-races onto another exit (dial-failure signalling
		// and the client-side inference that feeds the same path), and the
		// dial-strike warning stops new flows choosing the exit. removal here
		// is reserved for an exit that has established nothing at all.
		if connectTimeout <= now.Sub(windowStats.firstSendSynTime) &&
			windowStats.receiveSynCount <= 0 &&
			windowStats.receiveAckCount <= 0 {
			return blackholeNoReceiveSyn
		}
	}

	return blackholeNone
}

func (self *multiClientChannel) detectBlackhole() {
	// within a timeout window, if there are sent data but none received,
	// error out. This is similar to an ack timeout.
	defer self.cancel()

	for {
		if windowStats, err := self.WindowStats(); err != nil {
			return
		} else {
			reason := blackholeReasonFromStats(
				time.Now(),
				windowStats,
				self.settings.BlackholeTimeout,
				self.reliabilitySettings().BlackholeReceiveTimeout,
				self.settings.BlackholeConnectTimeout,
			)
			blackhole := reason != blackholeNone

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
				// Everything needed to judge the verdict goes in the error,
				// because this is the only line that survives into a field log.
				// The V(1) diagnostic above carries the same counts but glog
				// verbosity is pinned to 0 in sdk.go with no runtime control,
				// so in practice it never prints.
				//
				// This matters for telling a real blackhole from a false
				// positive. On a small network of known-good providers a
				// blackhole verdict is far more likely to be our accounting
				// than a broken exit, and the counts say which: sends
				// acknowledged with nothing received can simply be a quiet
				// destination, while an unacked-send age far past the bound
				// with no acks at all is a provider that really went away.
				// Once per removal, so the cost is nothing.
				self.addError(fmt.Errorf(
					"Blackhole %s (send %d/%dB recv %d/%dB syn %d/%d nackAge %s synAge %s)",
					reason,
					windowStats.sendAckCount,
					windowStats.sendAckByteCount,
					windowStats.receiveAckCount,
					windowStats.receiveAckByteCount,
					windowStats.sendSynCount,
					windowStats.receiveSynCount,
					blackholeAgeString(windowStats.firstSendNackTime),
					blackholeAgeString(windowStats.firstSendSynTime),
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
			case <-time.After(self.settings.BlackholeTimeout / 4):
			}
		}
	}
}

func (self *multiClientChannel) ping() {
	defer self.cancel()

	for {
		if windowStats, err := self.WindowStats(); err != nil {
			return
		} else if self.settings.CPingMaxByteCountPerSecond == 0 || windowStats.EffectiveByteCountPerSecond() <= self.settings.CPingMaxByteCountPerSecond {
			pingDone := make(chan error)
			success, err := self.SendDetailedMessage(
				&protocol.IpPing{},
				self.settings.CPingWriteTimeout,
				func(err error) {
					defer close(pingDone)
					select {
					case <-self.ctx.Done():
						return
					case pingDone <- err:
					}
				},
			)
			if err != nil {
				close(pingDone)
				return
			} else if !success {
				close(pingDone)
				return
			} else {
				select {
				case <-self.ctx.Done():
					return
				case <-self.client.Done():
					return
				case err := <-pingDone:
					if err != nil {
						self.addError(err)
						return
					}
				case <-time.After(self.settings.CPingTimeout):
					return
				}
			}
		}

		// rest between pings. `CPingRestTimeout` is decoupled from the ack wait
		// so a dead idle client is detected promptly (rest + ack wait), with a
		// fallback to `CPingTimeout` for settings that predate the split
		restTimeout := self.settings.CPingRestTimeout
		if restTimeout <= 0 {
			restTimeout = self.settings.CPingTimeout
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.client.Done():
			return
		case <-WakeupAfter(restTimeout, restTimeout):
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

	if self.packetStats.sendNackCount == 0 {
		// first outstanding send, so start the stall clock. see sendStalled
		self.pendingSendTime = time.Now()
	}
	self.packetStats.sendNackCount += 1
	self.packetStats.sendNackByteCount += packetByteCount
	if eventBucket.sendNackCount == 0 {
		eventBucket.sendNackTime = time.Now()
	}
	eventBucket.sendNackCount += 1
	eventBucket.sendNackByteCount += packetByteCount

	if ipPath.Syn {
		self.packetStats.sendSynCount += 1
		if eventBucket.sendSynCount == 0 {
			eventBucket.sendSynTime = time.Now()
		}
		eventBucket.sendSynCount += 1
	}

	self.addSourceToEventBucketWithLock(eventBucket, ipPath)
}

func (self *multiClientChannel) addSendNack(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendNackCount += 1
	self.packetStats.sendNackByteCount += ackByteCount

	eventBucket := self.eventBucket()
	if eventBucket.sendNackCount == 0 {
		eventBucket.sendNackTime = time.Now()
	}
	eventBucket.sendNackCount += 1
	eventBucket.sendNackByteCount += ackByteCount
}

func (self *multiClientChannel) addSendAck(ackByteCount ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.packetStats.sendNackCount -= 1
	self.packetStats.sendNackByteCount -= ackByteCount
	self.packetStats.sendAckCount += 1
	self.packetStats.sendAckByteCount += ackByteCount
	// an ack is progress, so the stall clock restarts here
	self.pendingSendTime = time.Now()

	eventBucket := self.eventBucket()
	if eventBucket.sendAckCount == 0 {
		eventBucket.sendAckTime = time.Now()
	}
	eventBucket.sendAckCount += 1
	eventBucket.sendAckByteCount += ackByteCount
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
		log:                 self.log,
		sourceCount:         maxSourceCount,
		netSourceCount:      netSourceCount,
		sendAckCount:        self.packetStats.sendAckCount,
		sendNackCount:       self.packetStats.sendNackCount,
		sendAckByteCount:    self.packetStats.sendAckByteCount,
		sendSynCount:        self.packetStats.sendSynCount,
		sendNackByteCount:   self.packetStats.sendNackByteCount,
		receiveAckCount:     self.packetStats.receiveAckCount,
		receiveAckByteCount: self.packetStats.receiveAckByteCount,
		receiveSynCount:     self.packetStats.receiveSynCount,
		windowDuration:      windowDuration,
		firstSendAckTime:    firstSendAckTime,
		firstSendNackTime:   firstSendNackTime,
		firstSendSynTime:    firstSendSynTime,
		bucketCount:         len(eventBuckets),
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
		case protocol.MessageType_IpIpPacketFromProvider:
			if ipPacketFromProvider_, err := FromFrame(frame); err == nil {
				ipPacketFromProvider := ipPacketFromProvider_.(*protocol.IpPacketFromProvider)

				packet := ipPacketFromProvider.IpPacket.PacketBytes

				ipPath, err := ParseIpPath(packet)
				if err == nil {
					self.addReceiveAck(ByteCount(len(packet)))
					if ipPath.Syn {
						self.addReceiveSyn(1)
					}
					self.clientReceivePacketCallback(self, source, peer.ProvideMode, ipPath, packet)
				} else if egressIpPath, ok := ipParseIcmpUnreachable(packet); ok {
					// a provider dial-failure signal: an icmp
					// destination-unreachable that ParseIpPath rejects ("No
					// support for protocol 1"), carrying the failed flow's
					// egress key in its embed. Hand it to the parent to re-race.
					//
					// Deliberately NOT counted as received data: the
					// addReceiveAck / addReceiveSyn above are in the err == nil
					// branch only, and this is the parse-failure branch, so an
					// intercepted icmp never touches the receive counters. A
					// provider that answers every dial with a failure must not
					// look healthy to detectBlackhole.
					if self.dialFailureCallback != nil {
						self.dialFailureCallback(self, egressIpPath)
					}
				}
				// else not an ip packet, drop
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
	self.client.Cancel()
	// unsubscribe even on Cancel so the underlying Client's callback list
	// doesn't retain a dangling reference for channels that are shuffled out
	// without ever going through Close. unsub is idempotent.
	self.clientReceiveUnsub()
}

func (self *multiClientChannel) Close() {
	self.addError(errors.New("Done."))
	self.cancel()
	self.client.Close()

	self.clientReceiveUnsub()
}

func (self *multiClientChannel) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}
