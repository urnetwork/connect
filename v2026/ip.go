package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	// "runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// implements user-space NAT (UNAT) and packet inspection
// The UNAT emulates a raw socket using user-space sockets.

// use 0 for deadlock testing
const defaultIpBufferSize = 1024

// packets are written directly into the receiver tap/tun interface,
// so the packet size must not exceed the device interface mtu (1440).
// this is a contract with the devices and must not be raised.
const DefaultMtu = 1440
const Ipv4HeaderSizeWithoutExtensions = 20
const Ipv6HeaderSize = 40
const UdpHeaderSize = 8
const TcpHeaderSizeWithoutExtensions = 20

const debugVerifyHeaders = false

// send from a raw socket
// note `ipProtocol` is not supplied. The implementation must do a packet inspection to determine protocol
// `provideMode` is the relationship between the source and this device
type SendPacketFunction func(provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool

// receive into a raw socket. ipPath is the canonical outbound flow identity
// (client→server), while packet retains its actual wire direction
// (server→client on a normal return). A callee that needs the packet's source
// or destination must inspect packet rather than infer its direction from
// ipPath. Both are read-only for the duration of the callback. A callee that
// retains packet must call MessagePoolShareReadOnly; a callee that needs a
// mutable path must clone it.
type ReceivePacketFunction func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte)

// receive a batch of packets from one flow (same source, provideMode, ipPath)
// in a single call, so a consumer can coalesce them (see the provider return
// path, which packs the batch into one wire Pack). The packet buffers are
// read-only and valid only for the duration of the callback. A callee that
// retains one must call MessagePoolShareReadOnly before returning.
type ReceivePacketsFunction func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packets [][]byte)

// the internal batch delivery hook plumbed from the nat into the per-flow
// read-loops: reports whether a batch consumer took the batch, so the loop
// falls back to per-packet delivery when none is registered
type receivePacketsBatchFunction func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packets [][]byte) (batched bool)

type UserNatClient interface {
	// `SendPacketFunction`
	SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool
	Close()
	Shuffle()

	SecurityPolicyStats(reset bool) SecurityPolicyStats

	// allow traffic that fails the security policy of the peers to stay local
	SetLocalSecurityBypass(localSecurityBypass bool)
}

// the per flow channel depths dominate the nat's per flow memory (three
// channels per flow at ~8-88 bytes per slot of backing array), so the
// defaults are scaled by the memory budget. udp tolerates drops, so its
// queues are short; tcp depth must cover the max window in mtu packets so a
// full window burst is never dropped (the nat implements no retransmit
// toward the socket).
const defaultUdpFlowBufferSize = 256
const defaultTcpFlowBufferSize = 1024

func DefaultUdpBufferSettings() *UdpBufferSettings {
	return DefaultUdpBufferSettingsWithBufferSize(MemoryScaledCount(defaultUdpFlowBufferSize, 32))
}

func DefaultUdpBufferSettingsWithBufferSize(bufferSize int) *UdpBufferSettings {
	globalLimit := 0
	if 0 < MemoryBudget() {
		globalLimit = MemoryScaledCount(2048, 256)
	}
	return &UdpBufferSettings{
		ReadTimeout:  300 * time.Second,
		WriteTimeout: 15 * time.Second,
		// short idle reap, standard for udp nats: single round trip flows
		// (dns, quic probes) dominate udp flow counts, and each idle flow
		// pins its channels, read buffer, and goroutines until reaped
		IdleTimeout:         60 * time.Second,
		Mtu:                 DefaultMtu,
		ReadBufferByteCount: DefaultMtu,
		SequenceBufferSize:  bufferSize,
		WriteBatchSize:      64,
		// Flows are assigned round-robin to a bounded number of ordered
		// receive workers. Workers start lazily, so one active flow still uses
		// one receive worker; five or five thousand flows use at most four.
		ReceiveShardCount: 4,
		UserLimit:         0,
		// A process with an explicit memory budget is a constrained device and
		// gets a scaled hard cap. Unbudgeted server/provider callers are not
		// silently assigned the phone cap; provider entry points select their
		// explicit profile below.
		GlobalLimit:     globalLimit,
		MaxWindowSize:   uint32(MemoryScaledByteCount(mib(1), kib(256))),
		ConnectSettings: *DefaultConnectSettings(),
	}
}

func DefaultTcpBufferSettings() *TcpBufferSettings {
	return DefaultTcpBufferSettingsWithBufferSize(MemoryScaledCount(defaultTcpFlowBufferSize, 192))
}

func DefaultTcpBufferSettingsWithBufferSize(bufferSize int) *TcpBufferSettings {
	minWindowSize := uint32(kib(64))
	globalLimit := 0
	if 0 < MemoryBudget() {
		globalLimit = MemoryScaledCount(512, 64)
	}
	tcpBufferSettings := &TcpBufferSettings{
		// ConnectTimeout:     60 * time.Second,
		ReadTimeout: 300 * time.Second,
		// WriteTimeout bounds ZERO-progress time on the upstream socket (the
		// write pipeline re-arms it on partial progress). A closed window on
		// the tunnel side legitimately parks the whole pipeline — the device's
		// acks can starve for tens of seconds behind a saturated tunnel — and
		// a kill here tears down an alive flow with an RST. A dead upstream
		// peer normally surfaces as RST/EPIPE rather than eternal silence, and
		// the idle reaper (IdleTimeout/ReadTimeout) still bounds a truly
		// wedged flow, so patience is cheap.
		WriteTimeout:       60 * time.Second,
		AckCompressTimeout: 50 * time.Millisecond,
		IdleTimeout:        300 * time.Second,
		SequenceBufferSize: bufferSize,
		Mtu:                DefaultMtu,
		// large socket reads are split into mtu-sized data packets by `DataPackets`
		ReadBufferByteCount: int(MemoryScaledByteCount(kib(64), kib(16))),
		WriteBatchSize:      64,
		MinWindowSize:       minWindowSize,
		MaxWindowSize:       scaledPow2WindowSize(uint32(mib(1)), minWindowSize, uint32(kib(128))),
		UserLimit:           0,
		// See the UDP profile above. In particular, an unbudgeted provider
		// must not inherit a 512-flow phone cap and reset established users.
		GlobalLimit:        globalLimit,
		EnableOrphanRst:    true,
		OrphanRstPerSecond: 256,
		// on by default: the benchmark range is reserved (RFC 2544) and the
		// synthetic server only ever answers flows explicitly addressed to it
		EnableSyntheticSpeed: true,
		ConnectSettings:      *DefaultConnectSettings(),
	}
	return tcpBufferSettings
}

// scaledPow2WindowSize scales `maxWindowSize` by the memory budget with a
// floor of `floorWindowSize`, rounded down to a power of 2 multiple of
// `minWindowSize` to preserve the `MaxWindowSize` contract (the window
// doubling ladder must land exactly on the max)
func scaledPow2WindowSize(maxWindowSize uint32, minWindowSize uint32, floorWindowSize uint32) uint32 {
	scaledWindowSize := uint32(MemoryScaledByteCount(ByteCount(maxWindowSize), ByteCount(floorWindowSize)))
	windowSize := minWindowSize
	for windowSize*2 <= scaledWindowSize {
		windowSize *= 2
	}
	return windowSize
}

func DefaultLocalUserNatSettings() *LocalUserNatSettings {
	return &LocalUserNatSettings{
		// scaled by the memory budget: slots on the dispatch channel pin
		// in-flight pool buffers under backpressure
		SequenceBufferSize: MemoryScaledCount(defaultIpBufferSize, 256),
		SendShardCount:     1,
		// BufferTimeout:      15 * time.Second,
		UdpBufferSettings:  DefaultUdpBufferSettings(),
		TcpBufferSettings:  DefaultTcpBufferSettings(),
		IcmpBufferSettings: DefaultIcmpBufferSettings(),
	}
}

// providerUdpIdleTimeout is the udp idle reap for an unbudgeted
// provider/egress. The general 60s udp idle (tuned for constrained devices,
// where single round trip flows dominate and each idle flow pins memory) is
// too aggressive for an egress provider: long-lived plain-udp sessions
// (wireguard, voip, games) legitimately go quiet for minutes, and reaping
// their NAT bindings breaks the flow for the user. 300s is the historical
// provider default from before the idle reap was shortened.
const providerUdpIdleTimeout = 300 * time.Second

// per-flow byte cost model for deriving the provider flow caps from the
// provider memory target: the expected in-process bytes one flow pins
// (channel backing arrays and queued packets, read buffer occupancy, window
// bookkeeping). Kernel socket buffers are excluded — they live outside the
// process footprint. These are the calibration levers for the provider
// target; validate changes against the provider load measurement
// (sdk TestDeviceLocalProviderMemoryUnderLoad).
const providerUdpFlowByteCount = 2 * 1024
const providerTcpFlowByteCount = 8 * 1024

// the icmp item of the same model: the backend read and write buffers (one
// mtu each on unix; the windows transactor's bounded outstanding reply
// buffers are comparable), the send queue backing array, flow bookkeeping,
// and the packet-class pool buffer a live flow keeps in circulation.
// Calibrated to the marginal heap of a live flow (~7 KiB measured, see
// TestIcmpFlowMemoryFootprint), rounded up. Like its udp and tcp siblings
// this figure is heap attributable: goroutine stacks (~10 KiB for a flow's
// two goroutines) and kernel socket buffers live outside it.
const providerIcmpFlowByteCount = 8 * 1024

// the icmp share of the nat target. icmp is an additive item above the
// udp/tcp split (see the provider profile below), so the divisor bounds the
// overcommit rather than carving the other tables.
const providerIcmpTargetDivisor = 16

// A cold public page can fan out across more than 100 origins while the
// browser keeps prior http/2 and quic connections alive. The target-derived
// 4 MiB mobile provider profile used to cap one source at 51 tcp and 153 udp
// flows, so an ordinary page evicted live handshakes and fell onto the
// browser's 17/33/65 second syn retry ladder. These are bounded functional
// floors, not unlimited exceptions: the aggregate caps still contain the
// complete table, and larger provider targets continue to scale from the
// byte-cost model above.
const providerMinUdpUserLimit = 256
const providerMinUdpGlobalLimit = 512
const providerMinTcpUserLimit = 256
const providerMinTcpGlobalLimit = 512

// icmp functional floors, in the style of the udp/tcp floors above: echo
// flows are one per ping process, so even a tiny target keeps a working
// ping table (see ICMP.md).
const providerMinIcmpUserLimit = 64
const providerMinIcmpGlobalLimit = 128

// DefaultProviderLocalUserNatSettings is the explicit provider/egress profile.
// A process that installed a memory budget is a constrained device and gets
// the scaled per-source/aggregate caps. An unbudgeted desktop/server provider
// preserves the historical unlimited flow counts: silently assigning it the
// phone's 512-TCP cap resets established provider traffic under ordinary
// server-scale load. Keeping this choice here makes provider policy explicit
// without changing every generic LocalUserNat caller.
func DefaultProviderLocalUserNatSettings() *LocalUserNatSettings {
	return DefaultProviderLocalUserNatSettingsWithMemoryTarget(0)
}

// DefaultProviderLocalUserNatSettingsWithMemoryTarget sizes the provider
// profile from the owner's provider memory target (the per-device share, see
// the sdk device wiring). 0 keeps the legacy behavior: process-budget-scaled
// caps, or unlimited flow counts for an unbudgeted server/desktop provider.
func DefaultProviderLocalUserNatSettingsWithMemoryTarget(targetByteCount ByteCount) *LocalUserNatSettings {
	settings := DefaultLocalUserNatSettings()
	if 0 < targetByteCount {
		// the provider target has its own gate, independent of the process
		// budget. Half the target sizes the egress nat flow tables (60% udp /
		// 40% tcp by bytes, converted to flow counts by the per-flow cost
		// model above, keeping the historical user:global ratios and floors);
		// the other half sizes the provider client's transfer budgets (see
		// the sdk device wiring). Flows over a cap evict the
		// least-recently-active flow, so the tables remain predictably
		// bounded under load. The functional fanout floors above may exceed a
		// very small target's byte-derived count.
		natTarget := targetByteCount / 2
		udpGlobalLimit := max(providerMinUdpGlobalLimit, int(natTarget*3/5/providerUdpFlowByteCount))
		tcpGlobalLimit := max(providerMinTcpGlobalLimit, int(natTarget*2/5/providerTcpFlowByteCount))
		settings.UdpBufferSettings.UserLimit = max(providerMinUdpUserLimit, udpGlobalLimit/4)
		settings.UdpBufferSettings.GlobalLimit = udpGlobalLimit
		settings.TcpBufferSettings.UserLimit = max(providerMinTcpUserLimit, tcpGlobalLimit/2)
		settings.TcpBufferSettings.GlobalLimit = tcpGlobalLimit
		// icmp is its own budget item, additive above the udp/tcp split: an
		// idle icmp path must not shrink the udp/tcp tables, so its bound is
		// not carved from their shares. The caps are ceilings — an unused
		// table holds zero bytes — and while icmp flows exist the worst-case
		// overcommit of the nat target is bounded by the divisor (like the
		// functional floors above at small targets).
		icmpGlobalLimit := max(
			providerMinIcmpGlobalLimit,
			int(natTarget/providerIcmpTargetDivisor/providerIcmpFlowByteCount),
		)
		settings.IcmpBufferSettings.UserLimit = max(providerMinIcmpUserLimit, icmpGlobalLimit/2)
		settings.IcmpBufferSettings.GlobalLimit = icmpGlobalLimit

		// The generic process profile leaves headroom above one full tcp
		// window. A provider can hold hundreds of simultaneous page flows, so
		// multiplying that spare channel capacity by every flow wastes
		// several MiB. Retain exactly enough slots for a max-window burst of
		// full-size IPv6 tcp packets; the lossless source contract remains
		// intact while active provider memory becomes proportional to the
		// advertised window instead of the unrelated process-wide default.
		tcpPayloadByteCount := max(
			1,
			settings.TcpBufferSettings.Mtu-Ipv6HeaderSize-TcpHeaderSizeWithoutExtensions,
		)
		windowPacketCount := int(
			(settings.TcpBufferSettings.MaxWindowSize +
				uint32(tcpPayloadByteCount) - 1) /
				uint32(tcpPayloadByteCount),
		)
		settings.TcpBufferSettings.SequenceBufferSize = min(
			settings.TcpBufferSettings.SequenceBufferSize,
			max(1, windowPacketCount),
		)
		return settings
	}
	if MemoryBudget() <= 0 {
		// unbudgeted: keep the unlimited flow counts and give plain-udp NAT
		// bindings the provider-tuned idle instead of the general short reap
		settings.UdpBufferSettings.IdleTimeout = providerUdpIdleTimeout
		return settings
	}
	settings.UdpBufferSettings.UserLimit = MemoryScaledCount(512, 64)
	settings.UdpBufferSettings.GlobalLimit = MemoryScaledCount(2048, 256)
	settings.TcpBufferSettings.UserLimit = MemoryScaledCount(256, 32)
	settings.TcpBufferSettings.GlobalLimit = MemoryScaledCount(512, 64)
	settings.IcmpBufferSettings.UserLimit = MemoryScaledCount(128, 16)
	settings.IcmpBufferSettings.GlobalLimit = MemoryScaledCount(256, 32)
	return settings
}

// DefaultLocalUserNatSettingsWithBufferSize applies `bufferSize` verbatim to
// the nat dispatch channel and every per flow channel (no memory budget
// scaling of the depths; callers that pass an explicit size own the depth
// choice). The window and read buffer defaults still scale.
func DefaultLocalUserNatSettingsWithBufferSize(bufferSize int) *LocalUserNatSettings {
	return &LocalUserNatSettings{
		SequenceBufferSize: bufferSize,
		SendShardCount:     1,
		UdpBufferSettings:  DefaultUdpBufferSettingsWithBufferSize(bufferSize),
		TcpBufferSettings:  DefaultTcpBufferSettingsWithBufferSize(bufferSize),
		IcmpBufferSettings: DefaultIcmpBufferSettingsWithBufferSize(bufferSize),
	}
}

type LocalUserNatSettings struct {
	SequenceBufferSize int
	// the number of send dispatch shards.
	// flows are pinned to a shard by their address tuple, which preserves
	// the per flow lossless in-order assumption.
	// the default is 1, since the per flow sequences already parallelize the
	// heavy work and sharding measured neutral at eight parallel flows.
	// note the tcp/udp user limits apply per shard.
	SendShardCount int
	// BufferTimeout      time.Duration
	UdpBufferSettings *UdpBufferSettings
	TcpBufferSettings *TcpBufferSettings
	// nil resolves to the defaults, so settings constructed before icmp
	// support keep working
	IcmpBufferSettings *IcmpBufferSettings

	// Log, when set, is used by the local user nat and its udp/tcp/icmp
	// buffers and sequences (used for a buffer whose settings `Log` is nil,
	// via a private copy — the caller's settings are never mutated).
	// nil resolves to `DefaultLogger()`.
	Log Logger
}

// forwards packets using user space sockets
// this assumes transfer between the packet source and this is lossless and in order,
// so the protocol stack implementations do not implement any retransmit logic
type LocalUserNat struct {
	ctx       context.Context
	cancel    context.CancelFunc
	clientTag string
	log       Logger

	sendPackets chan *SendPacket

	settings *LocalUserNatSettings

	// receive callback
	receiveCallbacks *CallbackList[ReceivePacketFunction]
	// batch receive callback (see receivePackets)
	receivePacketsCallbacks *CallbackList[ReceivePacketsFunction]
}

func NewLocalUserNatWithDefaults(ctx context.Context, clientTag string) *LocalUserNat {
	return NewLocalUserNat(ctx, clientTag, DefaultLocalUserNatSettings())
}

func NewLocalUserNat(ctx context.Context, clientTag string, settings *LocalUserNatSettings) *LocalUserNat {
	cancelCtx, cancel := context.WithCancel(ctx)

	log := loggerOrDefault(settings.Log)
	// propagate so a nat-level logger covers the udp/tcp/icmp buffers and
	// sequences. Copy instead of writing through the caller's settings: the
	// caller may share them with other components or concurrent
	// constructions (see the platform transport framer settings for the
	// same rule).
	{
		copied := *settings
		if copied.UdpBufferSettings != nil && copied.UdpBufferSettings.Log == nil {
			udpCopied := *copied.UdpBufferSettings
			udpCopied.Log = log
			copied.UdpBufferSettings = &udpCopied
		}
		if copied.TcpBufferSettings != nil && copied.TcpBufferSettings.Log == nil {
			tcpCopied := *copied.TcpBufferSettings
			tcpCopied.Log = log
			copied.TcpBufferSettings = &tcpCopied
		}
		if copied.IcmpBufferSettings == nil {
			copied.IcmpBufferSettings = DefaultIcmpBufferSettings()
		}
		if copied.IcmpBufferSettings.Log == nil {
			icmpCopied := *copied.IcmpBufferSettings
			icmpCopied.Log = log
			copied.IcmpBufferSettings = &icmpCopied
		}
		settings = &copied
	}
	localUserNat := &LocalUserNat{
		ctx:                     cancelCtx,
		cancel:                  cancel,
		clientTag:               clientTag,
		log:                     log,
		sendPackets:             make(chan *SendPacket, settings.SequenceBufferSize),
		settings:                settings,
		receiveCallbacks:        NewCallbackList[ReceivePacketFunction](),
		receivePacketsCallbacks: NewCallbackList[ReceivePacketsFunction](),
	}
	go HandleError(localUserNat.Run)

	return localUserNat
}

func (self *LocalUserNat) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return SecurityPolicyStats{}
}

func (self *LocalUserNat) SendPacketWithTimeout(source TransferPath, provideMode protocol.ProvideMode,
	packet []byte, timeout time.Duration) bool {
	return self.SendPacketsWithTimeout(source, provideMode, [][]byte{packet}, timeout)
}

// `SendPacketWithTimeout` for a batch of packets from one source.
// queueing a batch is one channel operation, which reduces wakeups when the
// caller already has multiple packets in hand.
// on success the local user nat owns `packets` and each packet in it.
// on failure the caller keeps ownership of all of the packets.
func (self *LocalUserNat) SendPacketsWithTimeout(source TransferPath, provideMode protocol.ProvideMode,
	packets [][]byte, timeout time.Duration) bool {
	sendPacket := &SendPacket{
		source:      source,
		provideMode: provideMode,
		packets:     packets,
	}
	// fast path without arming a timer
	select {
	case self.sendPackets <- sendPacket:
		return true
	default:
	}
	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendPackets <- sendPacket:
			return true
		}
	} else if 0 == timeout {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendPackets <- sendPacket:
			return true
		default:
			// full
			return false
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendPackets <- sendPacket:
			return true
		case <-time.After(timeout):
			// full
			return false
		}
	}
}

// `SendPacketFunction`
func (self *LocalUserNat) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	return self.SendPacketWithTimeout(source, provideMode, packet, timeout)
}

// `SendPackets` for a batch of packets from one source. see `SendPacketsWithTimeout`.
func (self *LocalUserNat) SendPackets(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) bool {
	return self.SendPacketsWithTimeout(source, provideMode, packets, timeout)
}

// func (self *LocalUserNat) ReceiveN(source TransferPath, provideMode protocol.ProvideMode, packet []byte, n int) {
//     self.Receive(source, provideMode, packet[0:n])
// }

func (self *LocalUserNat) AddReceivePacketCallback(receiveCallback ReceivePacketFunction) func() {
	callbackId := self.receiveCallbacks.Add(receiveCallback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

// func (self *LocalUserNat) RemoveReceivePacketCallback(receiveCallback ReceivePacketFunction) {
//     self.receiveCallbacks.Remove(receiveCallback)
// }

// `ReceivePacketFunction`
func (self *LocalUserNat) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		HandleError(func() {
			receiveCallback(source, provideMode, ipPath, packet)
		})
	}
}

// receivePackets fans a flow's packet batch to the registered batch
// callbacks (the provider return path coalesces it into one wire Pack).
// Batch consumers take the batch exclusively: when any is registered, the
// per-packet callbacks do not see these packets (the flow read-loop only
// falls back to per-packet delivery when receivePackets reports
// not-batched). A nat therefore has either batch consumers (the provider
// exit nat) or per-packet consumers (the device local nats), not both.
// The batch callback shares read-only what it retains; the read-loop keeps
// buffer ownership.
func (self *LocalUserNat) receivePackets(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packets [][]byte) (batched bool) {
	batchCallbacks := self.receivePacketsCallbacks.Get()
	for _, batchCallback := range batchCallbacks {
		HandleError(func() {
			batchCallback(source, provideMode, ipPath, packets)
		})
		batched = true
	}
	return batched
}

func (self *LocalUserNat) hasReceivePacketsCallback() bool {
	return 0 < len(self.receivePacketsCallbacks.Get())
}

// AddReceivePacketsCallback registers a batch receive callback (see
// receivePackets). A batch callback borrows the flow's drained packet buffers
// for the duration of the call; the read loop retains ownership.
func (self *LocalUserNat) AddReceivePacketsCallback(receiveCallback ReceivePacketsFunction) func() {
	callbackId := self.receivePacketsCallbacks.Add(receiveCallback)
	return func() {
		self.receivePacketsCallbacks.Remove(callbackId)
	}
}

func (self *LocalUserNat) Run() {
	defer self.cancel()

	shardCount := max(1, self.settings.SendShardCount)
	if shardCount == 1 {
		self.runSendShard(self.sendPackets)
		return
	}

	// shard the send dispatch. flows are pinned to a shard by their address
	// tuple, which preserves the per flow lossless in-order assumption.
	shardSendPackets := make([]chan *SendPacket, shardCount)
	for i := 0; i < shardCount; i += 1 {
		sendPackets := make(chan *SendPacket, self.settings.SequenceBufferSize)
		shardSendPackets[i] = sendPackets
		go HandleError(func() {
			self.runSendShard(sendPackets)
		}, self.cancel)
	}

	forward := func(shard int, sendPacket *SendPacket) bool {
		select {
		case <-self.ctx.Done():
			for _, packet := range sendPacket.packets {
				MessagePoolReturn(packet)
			}
			return false
		case shardSendPackets[shard] <- sendPacket:
			return true
		}
	}

	route := func(sendPacket *SendPacket) bool {
		if len(sendPacket.packets) == 0 {
			return true
		}
		// common case: all packets in the batch are for one shard
		shard := sendShard(sendPacket.packets[0], shardCount)
		split := false
		for _, packet := range sendPacket.packets[1:] {
			if sendShard(packet, shardCount) != shard {
				split = true
				break
			}
		}
		if !split {
			return forward(shard, sendPacket)
		}

		shardPackets := make([][][]byte, shardCount)
		for _, packet := range sendPacket.packets {
			packetShard := sendShard(packet, shardCount)
			shardPackets[packetShard] = append(shardPackets[packetShard], packet)
		}
		for packetShard, packets := range shardPackets {
			if len(packets) == 0 {
				continue
			}
			shardSendPacket := &SendPacket{
				source:      sendPacket.source,
				provideMode: sendPacket.provideMode,
				packets:     packets,
			}
			if !forward(packetShard, shardSendPacket) {
				for _, returnPackets := range shardPackets[packetShard+1:] {
					for _, packet := range returnPackets {
						MessagePoolReturn(packet)
					}
				}
				return false
			}
		}
		return true
	}

send:
	for {
		select {
		case <-self.ctx.Done():
			return
		case sendPacket := <-self.sendPackets:
			if !route(sendPacket) {
				return
			}
			// opportunistically drain queued packets to reduce wakeups
			for {
				select {
				case <-self.ctx.Done():
					return
				case sendPacket := <-self.sendPackets:
					if !route(sendPacket) {
						return
					}
				default:
					continue send
				}
			}
		}
	}
}

// pins a packet to a send shard by its address tuple.
// all packets of a flow map to the same shard.
func sendShard(ipPacket []byte, shardCount int) int {
	// fnv-1a over the address tuple
	hash := uint32(2166136261)
	hashBytes := func(b []byte) {
		for _, c := range b {
			hash = (hash ^ uint32(c)) * 16777619
		}
	}
	if 0 < len(ipPacket) {
		switch uint8(ipPacket[0]) >> 4 {
		case 4:
			if Ipv4HeaderSizeWithoutExtensions <= len(ipPacket) {
				hashBytes(ipPacket[12:20])
				headerByteCount := int(ipPacket[0]&0xf) * 4
				if ipProtocolNumber(ipPacket[9]) == ipProtocolNumberIcmp4 {
					// the first transport bytes are type/code/checksum, which
					// vary per packet; the echo identifier is the flow identity
					if headerByteCount+6 <= len(ipPacket) {
						hashBytes(ipPacket[headerByteCount+4 : headerByteCount+6])
					}
				} else if headerByteCount+4 <= len(ipPacket) {
					hashBytes(ipPacket[headerByteCount : headerByteCount+4])
				}
			}
		case 6:
			if Ipv6HeaderSize+4 <= len(ipPacket) {
				hashBytes(ipPacket[8:40])
				if ipProtocolNumber(ipPacket[6]) == ipProtocolNumberIcmp6 {
					if Ipv6HeaderSize+6 <= len(ipPacket) {
						hashBytes(ipPacket[Ipv6HeaderSize+4 : Ipv6HeaderSize+6])
					}
				} else {
					hashBytes(ipPacket[Ipv6HeaderSize : Ipv6HeaderSize+4])
				}
			}
		}
	}
	return int(hash % uint32(shardCount))
}

func (self *LocalUserNat) runSendShard(sendPackets chan *SendPacket) {
	defer self.cancel()

	udp4Buffer := NewUdp4Buffer(self.ctx, self.receive, self.settings.UdpBufferSettings)
	udp6Buffer := NewUdp6Buffer(self.ctx, self.receive, self.settings.UdpBufferSettings)
	tcp4Buffer := NewTcp4Buffer(self.ctx, self.receive, self.settings.TcpBufferSettings)
	tcp6Buffer := NewTcp6Buffer(self.ctx, self.receive, self.settings.TcpBufferSettings)
	icmp4Buffer := NewIcmp4Buffer(self.ctx, self.receive, self.settings.IcmpBufferSettings)
	icmp6Buffer := NewIcmp6Buffer(self.ctx, self.receive, self.settings.IcmpBufferSettings)
	// the per-flow read-loops route their drained batch through
	// receivePackets, which coalesces it into one wire Pack when a batch
	// consumer is registered (the provider return path) and otherwise
	// reports not-batched so the loop falls back to per-packet delivery.
	// The check is live (per batch), since the provider registers its batch
	// callback after this nat's Run starts.
	udp4Buffer.receivePacketsCallback = self.receivePackets
	udp6Buffer.receivePacketsCallback = self.receivePackets
	tcp4Buffer.receivePacketsCallback = self.receivePackets
	tcp6Buffer.receivePacketsCallback = self.receivePackets
	icmp4Buffer.receivePacketsCallback = self.receivePackets
	icmp6Buffer.receivePacketsCallback = self.receivePackets

	// parsed per-packet views. these are copied by value into the send items,
	// so the locals can be reused across packets.
	var udpPacket parsedUdp
	var tcpPacket parsedTcp
	var icmpPacket parsedIcmp

	handleIpPacket := func(source TransferPath, provideMode protocol.ProvideMode, ipPacket []byte) {
		if len(ipPacket) == 0 {
			MessagePoolReturn(ipPacket)
			return
		}
		ipVersion := uint8(ipPacket[0]) >> 4
		switch ipVersion {
		case 4:
			ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(ipPacket)
			if !ok {
				// malformed, drop
				MessagePoolReturn(ipPacket)
				return
			}
			switch ipProtocol {
			case ipProtocolNumberUdp:
				if !parseUdpPacket(sourceIp, destinationIp, transport, &udpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := udp4Buffer.send(
						source,
						provideMode,
						&udpPacket,
						self.settings.UdpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				delivered := false
				if self.log.V(2).Enabled() {
					delivered = TraceWithReturn(
						fmt.Sprintf("[lnr]send udp4 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					delivered = c()
				}
				if !delivered {
					// full/timeout drop: the sequence never took ownership
					MessagePoolReturn(ipPacket)
				}
			case ipProtocolNumberTcp:
				if !parseTcpPacket(sourceIp, destinationIp, transport, &tcpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := tcp4Buffer.send(
						source,
						provideMode,
						&tcpPacket,
						self.settings.TcpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				delivered := false
				if self.log.V(2).Enabled() {
					delivered = TraceWithReturn(
						fmt.Sprintf("[lnr]send tcp4 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					delivered = c()
				}
				if !delivered {
					// full/timeout drop: the sequence never took ownership
					MessagePoolReturn(ipPacket)
				}
			case ipProtocolNumberIcmp4:
				if !parseIcmpPacket(4, sourceIp, destinationIp, transport, &icmpPacket) {
					// unsupported type or malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				if !icmpPacket.echoRequest {
					// an outbound echo reply is always an orphan: unsolicited
					// inbound pings cannot reach a source, and the egress
					// backends cannot send one anyway (see ICMP.md)
					MessagePoolReturn(ipPacket)
					return
				}
				icmpPacket.ttl = ipPacket[8]
				c := func() bool {
					success, err := icmp4Buffer.send(
						source,
						provideMode,
						&icmpPacket,
						self.settings.IcmpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				delivered := false
				if self.log.V(2).Enabled() {
					delivered = TraceWithReturn(
						fmt.Sprintf("[lnr]send icmp4 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					delivered = c()
				}
				if !delivered {
					// full/timeout drop: the sequence never took ownership
					MessagePoolReturn(ipPacket)
				}
			default:
				// no support for this protocol, drop
				MessagePoolReturn(ipPacket)
			}
		case 6:
			ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv6(ipPacket)
			if !ok {
				// malformed, drop
				MessagePoolReturn(ipPacket)
				return
			}
			switch ipProtocol {
			case ipProtocolNumberUdp:
				if !parseUdpPacket(sourceIp, destinationIp, transport, &udpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := udp6Buffer.send(
						source,
						provideMode,
						&udpPacket,
						self.settings.UdpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				delivered := false
				if self.log.V(2).Enabled() {
					delivered = TraceWithReturn(
						fmt.Sprintf("[lnr]send udp6 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					delivered = c()
				}
				if !delivered {
					// full/timeout drop: the sequence never took ownership
					MessagePoolReturn(ipPacket)
				}
			case ipProtocolNumberTcp:
				if !parseTcpPacket(sourceIp, destinationIp, transport, &tcpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := tcp6Buffer.send(
						source,
						provideMode,
						&tcpPacket,
						self.settings.TcpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				delivered := false
				if self.log.V(2).Enabled() {
					delivered = TraceWithReturn(
						fmt.Sprintf("[lnr]send tcp6 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					delivered = c()
				}
				if !delivered {
					// full/timeout drop: the sequence never took ownership
					MessagePoolReturn(ipPacket)
				}
			case ipProtocolNumberIcmp6:
				if !parseIcmpPacket(6, sourceIp, destinationIp, transport, &icmpPacket) {
					// unsupported type or malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				if !icmpPacket.echoRequest {
					// an outbound echo reply is always an orphan (see the v4
					// case)
					MessagePoolReturn(ipPacket)
					return
				}
				// hop limit
				icmpPacket.ttl = ipPacket[7]
				c := func() bool {
					success, err := icmp6Buffer.send(
						source,
						provideMode,
						&icmpPacket,
						self.settings.IcmpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				delivered := false
				if self.log.V(2).Enabled() {
					delivered = TraceWithReturn(
						fmt.Sprintf("[lnr]send icmp6 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					delivered = c()
				}
				if !delivered {
					// full/timeout drop: the sequence never took ownership
					MessagePoolReturn(ipPacket)
				}
			default:
				// no support for this protocol, drop
				MessagePoolReturn(ipPacket)
			}
		default:
			// unknown IP version, drop
			MessagePoolReturn(ipPacket)
		}
	}

	handleSendPacket := func(sendPacket *SendPacket) {
		for _, ipPacket := range sendPacket.packets {
			handleIpPacket(sendPacket.source, sendPacket.provideMode, ipPacket)
		}
	}

send:
	for {
		select {
		case <-self.ctx.Done():
			return
		case sendPacket := <-sendPackets:
			handleSendPacket(sendPacket)
			// opportunistically drain queued packets to reduce wakeups
			for {
				select {
				case <-self.ctx.Done():
					return
				case sendPacket := <-sendPackets:
					handleSendPacket(sendPacket)
				default:
					continue send
				}
			}
		}
	}
}

func (self *LocalUserNat) Close() {
	self.cancel()
}

// a batch of packets from one source
type SendPacket struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	packets     [][]byte
}

// comparable
type BufferId4 struct {
	source          TransferPath
	sourceIp        [4]byte
	sourcePort      int
	destinationIp   [4]byte
	destinationPort int
}

func NewBufferId4(source TransferPath, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) BufferId4 {
	return BufferId4{
		source:          source,
		sourceIp:        [4]byte(sourceIp),
		sourcePort:      sourcePort,
		destinationIp:   [4]byte(destinationIp),
		destinationPort: destinationPort,
	}
}

// comparable
type BufferId6 struct {
	source          TransferPath
	sourceIp        [16]byte
	sourcePort      int
	destinationIp   [16]byte
	destinationPort int
}

func NewBufferId6(source TransferPath, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) BufferId6 {
	return BufferId6{
		source:          source,
		sourceIp:        [16]byte(sourceIp),
		sourcePort:      sourcePort,
		destinationIp:   [16]byte(destinationIp),
		destinationPort: destinationPort,
	}
}

type UdpBufferSettings struct {
	// nil resolves to the local user nat `Log`
	Log                 Logger
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	Mtu                 int
	ReadBufferByteCount int
	SequenceBufferSize  int
	// the maximum number of payloads to process under a single write deadline.
	// udp datagrams cannot be coalesced, so each payload is one socket write.
	WriteBatchSize int
	// Number of ordered receive-dispatch shards shared by every UDP flow in
	// this address-family buffer. Zero retains the default of four for custom
	// settings built before this field existed. Each shard queue has
	// SequenceBufferSize slots, making aggregate user-space receive
	// backpressure independent of flow count.
	ReceiveShardCount int
	// the number of open sockets per user
	// uses an lru cleanup where new sockets over the limit close old sockets
	UserLimit int
	// the number of open sockets across all users (per address family
	// buffer), an aggregate flow state and fd ceiling for hosts with a hard
	// process memory budget. uses the same lru cleanup as `UserLimit`.
	// 0 (the default) is no limit.
	GlobalLimit   int
	MaxWindowSize uint32

	ConnectSettings
}

// iana ip protocol numbers, as carried in the ipv4 protocol and ipv6 next
// header fields. tcp, udp, and icmp echo are handled; other protocols are
// dropped. the icmp numbers also label the flow-teardown unreachable
// messages built by `ipOosUnreachable`, which are injected toward the
// source.
type ipProtocolNumber uint8

const (
	ipProtocolNumberIcmp4 = ipProtocolNumber(1)
	ipProtocolNumberTcp   = ipProtocolNumber(6)
	ipProtocolNumberUdp   = ipProtocolNumber(17)
	ipProtocolNumberIcmp6 = ipProtocolNumber(58)
)

// minimal parsed views of packets on the send path.
// these avoid per-packet decode allocations in the hot dispatch.
// all slices alias the backing ip packet, which stays valid while the
// owning send item holds the packet.

type parsedUdp struct {
	sourceIp        net.IP
	destinationIp   net.IP
	sourcePort      uint16
	destinationPort uint16
	payload         []byte
}

type parsedTcp struct {
	sourceIp        net.IP
	destinationIp   net.IP
	sourcePort      uint16
	destinationPort uint16
	fin             bool
	syn             bool
	rst             bool
	psh             bool
	ack             bool
	seq             uint32
	ackNumber       uint32
	windowSize      uint16
	options         []byte
	payload         []byte
}

func (self *parsedTcp) flagsString() string {
	flags := []string{}
	if self.fin {
		flags = append(flags, "FIN")
	}
	if self.syn {
		flags = append(flags, "SYN")
	}
	if self.rst {
		flags = append(flags, "RST")
	}
	if self.psh {
		flags = append(flags, "PSH")
	}
	if self.ack {
		flags = append(flags, "ACK")
	}
	return strings.Join(flags, ", ")
}

// parses the ipv4 header. the returned slices alias `ipPacket`.
func parseIpv4(ipPacket []byte) (ipProtocol ipProtocolNumber, sourceIp net.IP, destinationIp net.IP, transport []byte, ok bool) {
	if len(ipPacket) < Ipv4HeaderSizeWithoutExtensions {
		return
	}
	headerByteCount := int(ipPacket[0]&0xf) * 4
	totalByteCount := int(binary.BigEndian.Uint16(ipPacket[2:4]))
	if headerByteCount < Ipv4HeaderSizeWithoutExtensions || totalByteCount < headerByteCount || len(ipPacket) < totalByteCount {
		return
	}
	// fragments are not reassembled: a non-first fragment has no transport
	// header and a first fragment has a truncated payload, so either would
	// misparse payload bytes as transport fields. one 16 bit load covers mf
	// plus the whole offset field (0x3fff); df and the reserved bit pass.
	if binary.BigEndian.Uint16(ipPacket[6:8])&0x3fff != 0 {
		return
	}
	ipProtocol = ipProtocolNumber(ipPacket[9])
	sourceIp = net.IP(ipPacket[12:16])
	destinationIp = net.IP(ipPacket[16:20])
	transport = ipPacket[headerByteCount:totalByteCount]
	ok = true
	return
}

// parses the ipv6 header. the returned slices alias `ipPacket`.
// extension headers are not walked, matching the previous decode behavior
// which dropped non tcp/udp next headers.
func parseIpv6(ipPacket []byte) (ipProtocol ipProtocolNumber, sourceIp net.IP, destinationIp net.IP, transport []byte, ok bool) {
	if len(ipPacket) < Ipv6HeaderSize {
		return
	}
	payloadByteCount := int(binary.BigEndian.Uint16(ipPacket[4:6]))
	if len(ipPacket) < Ipv6HeaderSize+payloadByteCount {
		return
	}
	ipProtocol = ipProtocolNumber(ipPacket[6])
	sourceIp = net.IP(ipPacket[8:24])
	destinationIp = net.IP(ipPacket[24:40])
	transport = ipPacket[Ipv6HeaderSize : Ipv6HeaderSize+payloadByteCount]
	ok = true
	return
}

// parses a udp packet into `udp`. the slices alias the backing packet.
func parseUdpPacket(sourceIp net.IP, destinationIp net.IP, transport []byte, udp *parsedUdp) bool {
	if len(transport) < UdpHeaderSize {
		return false
	}
	udpByteCount := int(binary.BigEndian.Uint16(transport[4:6]))
	if udpByteCount < UdpHeaderSize || len(transport) < udpByteCount {
		return false
	}
	udp.sourceIp = sourceIp
	udp.destinationIp = destinationIp
	udp.sourcePort = binary.BigEndian.Uint16(transport[0:2])
	udp.destinationPort = binary.BigEndian.Uint16(transport[2:4])
	udp.payload = transport[UdpHeaderSize:udpByteCount]
	return true
}

// parses a tcp packet into `tcp`. the slices alias the backing packet.
func parseTcpPacket(sourceIp net.IP, destinationIp net.IP, transport []byte, tcp *parsedTcp) bool {
	if len(transport) < TcpHeaderSizeWithoutExtensions {
		return false
	}
	headerByteCount := int(transport[12]>>4) * 4
	if headerByteCount < TcpHeaderSizeWithoutExtensions || len(transport) < headerByteCount {
		return false
	}
	flags := transport[13]
	tcp.sourceIp = sourceIp
	tcp.destinationIp = destinationIp
	tcp.sourcePort = binary.BigEndian.Uint16(transport[0:2])
	tcp.destinationPort = binary.BigEndian.Uint16(transport[2:4])
	tcp.seq = binary.BigEndian.Uint32(transport[4:8])
	tcp.ackNumber = binary.BigEndian.Uint32(transport[8:12])
	tcp.fin = (flags & 0x01) != 0
	tcp.syn = (flags & 0x02) != 0
	tcp.rst = (flags & 0x04) != 0
	tcp.psh = (flags & 0x08) != 0
	tcp.ack = (flags & 0x10) != 0
	tcp.windowSize = binary.BigEndian.Uint16(transport[14:16])
	tcp.options = transport[TcpHeaderSizeWithoutExtensions:headerByteCount]
	tcp.payload = transport[headerByteCount:]
	return true
}

// tcp flag bits
const (
	tcpFlagFin = byte(0x01)
	tcpFlagSyn = byte(0x02)
	tcpFlagRst = byte(0x04)
	tcpFlagPsh = byte(0x08)
	tcpFlagAck = byte(0x10)
)

// ones' complement sum in the style of rfc 1071.
// an odd final byte is padded high.
func checksumAdd(sum uint32, b []byte) uint32 {
	i := 0
	for ; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if i < len(b) {
		sum += uint32(b[i]) << 8
	}
	return sum
}

func checksumFinish(sum uint32) uint16 {
	for 0xffff < sum {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// computes the transport checksum with the ipv4 or ipv6 pseudo header.
// the two pseudo headers sum identically for transport lengths that fit
// in 16 bits.
func transportChecksum(ipProtocol ipProtocolNumber, packetSourceIp net.IP, packetDestinationIp net.IP, transport []byte) uint16 {
	sum := checksumAdd(0, packetSourceIp)
	sum = checksumAdd(sum, packetDestinationIp)
	sum += uint32(ipProtocol)
	sum += uint32(len(transport))
	return checksumFinish(checksumAdd(sum, transport))
}

// writes an ipv4 header with no options.
// `packet` must be sized to the full packet.
func writeIpv4Header(packet []byte, ipProtocol ipProtocolNumber, packetSourceIp net.IP, packetDestinationIp net.IP) {
	// version 4, header length 5 words
	packet[0] = 0x45
	// tos
	packet[1] = 0
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	// id, flags, fragment offset
	packet[4] = 0
	packet[5] = 0
	packet[6] = 0
	packet[7] = 0
	// ttl
	packet[8] = 64
	packet[9] = byte(ipProtocol)
	// checksum, set below
	packet[10] = 0
	packet[11] = 0
	copy(packet[12:16], packetSourceIp)
	copy(packet[16:20], packetDestinationIp)
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:Ipv4HeaderSizeWithoutExtensions])))
}

// writes an ipv6 header with no extensions.
// `packet` must be sized to the full packet.
func writeIpv6Header(packet []byte, ipProtocol ipProtocolNumber, packetSourceIp net.IP, packetDestinationIp net.IP) {
	// version 6, traffic class and flow label zero
	packet[0] = 0x60
	packet[1] = 0
	packet[2] = 0
	packet[3] = 0
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-Ipv6HeaderSize))
	packet[6] = byte(ipProtocol)
	// hop limit
	packet[7] = 64
	copy(packet[8:24], packetSourceIp)
	copy(packet[24:40], packetDestinationIp)
}

// udpReceiveDispatcher replaces one receive channel and one receive goroutine
// per UDP flow with a small set of shared FIFO shards. A flow is permanently
// assigned to one shard, and its socket has exactly one read producer, so its
// callback order is unchanged. The aggregate user-space queue is now
// shardCount*SequenceBufferSize rather than flowCount*SequenceBufferSize.
//
// A callback can still block unrelated flows in the same shard. Four shards
// bound that failure domain without reintroducing per-flow stacks/queues; the
// socket kernel buffers provide the next bounded absorption layer.
const defaultUdpReceiveShardCount = 4

type udpReceiveDispatchItem struct {
	sequence *UdpSequence
	packet   []byte
}

type udpReceiveDispatchShard struct {
	ctx       context.Context
	startOnce sync.Once
	items     chan udpReceiveDispatchItem
	batchSize int
}

type udpReceiveDispatcher struct {
	ctx       context.Context
	shards    []udpReceiveDispatchShard
	nextShard int
}

func newUdpReceiveDispatcher(ctx context.Context, settings *UdpBufferSettings) *udpReceiveDispatcher {
	shardCount := settings.ReceiveShardCount
	if shardCount <= 0 {
		shardCount = defaultUdpReceiveShardCount
	}
	dispatcher := &udpReceiveDispatcher{
		ctx:    ctx,
		shards: make([]udpReceiveDispatchShard, shardCount),
	}
	for i := range dispatcher.shards {
		dispatcher.shards[i] = udpReceiveDispatchShard{
			ctx:       ctx,
			items:     make(chan udpReceiveDispatchItem, max(0, settings.SequenceBufferSize)),
			batchSize: max(1, settings.WriteBatchSize),
		}
	}
	return dispatcher
}

// assignShard is called under UdpBuffer.mutex when a flow is created.
func (dispatcher *udpReceiveDispatcher) assignShard() int {
	shard := dispatcher.nextShard
	dispatcher.nextShard = (dispatcher.nextShard + 1) % len(dispatcher.shards)
	return shard
}

func (dispatcher *udpReceiveDispatcher) enqueue(sequence *UdpSequence, packet []byte) bool {
	if dispatcher == nil || sequence == nil || len(dispatcher.shards) == 0 {
		return false
	}
	shard := &dispatcher.shards[sequence.receiveShard]
	shard.startOnce.Do(func() {
		go HandleError(shard.run)
	})
	select {
	case <-dispatcher.ctx.Done():
		return false
	case <-sequence.ctx.Done():
		return false
	case shard.items <- udpReceiveDispatchItem{sequence: sequence, packet: packet}:
		return true
	}
}

func (shard *udpReceiveDispatchShard) run() {
	batch := make([][]byte, 0, shard.batchSize)
	var pending udpReceiveDispatchItem
	hasPending := false

	releaseQueued := func() {
		if hasPending {
			MessagePoolReturn(pending.packet)
			pending = udpReceiveDispatchItem{}
			hasPending = false
		}
		for {
			select {
			case item := <-shard.items:
				MessagePoolReturn(item.packet)
			default:
				return
			}
		}
	}

	for {
		var item udpReceiveDispatchItem
		if hasPending {
			item = pending
			pending = udpReceiveDispatchItem{}
			hasPending = false
		} else {
			select {
			case <-shard.ctx.Done():
				releaseQueued()
				return
			case item = <-shard.items:
			}
		}

		sequence := item.sequence
		batch = append(batch[:0], item.packet)

		// Coalesce only adjacent packets from the same flow. Holding one
		// different-flow item as pending preserves the shard's total FIFO
		// order while retaining the existing two-frame wire batching win.
	fill:
		for len(batch) < cap(batch) {
			select {
			case next := <-shard.items:
				if next.sequence != sequence {
					pending = next
					hasPending = true
					break fill
				}
				batch = append(batch, next.packet)
			default:
				break fill
			}
		}
		sequence.receiveBatch(batch)
	}
}

type Udp4Buffer struct {
	UdpBuffer[BufferId4]
}

func NewUdp4Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings) *Udp4Buffer {
	return &Udp4Buffer{
		UdpBuffer: *newUdpBuffer[BufferId4](ctx, receiveCallback, udpBufferSettings),
	}
}

func (self *Udp4Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	udp *parsedUdp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId4(
		source,
		udp.sourceIp, int(udp.sourcePort),
		udp.destinationIp, int(udp.destinationPort),
	)

	return self.udpSend(
		bufferId,
		source,
		provideMode,
		4,
		udp,
		timeout,
		ipPacket,
	)
}

type Udp6Buffer struct {
	UdpBuffer[BufferId6]
}

func NewUdp6Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings) *Udp6Buffer {
	return &Udp6Buffer{
		UdpBuffer: *newUdpBuffer[BufferId6](ctx, receiveCallback, udpBufferSettings),
	}
}

func (self *Udp6Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	udp *parsedUdp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId6(
		source,
		udp.sourceIp, int(udp.sourcePort),
		udp.destinationIp, int(udp.destinationPort),
	)

	return self.udpSend(
		bufferId,
		source,
		provideMode,
		6,
		udp,
		timeout,
		ipPacket,
	)
}

type UdpBuffer[BufferId comparable] struct {
	ctx                    context.Context
	log                    Logger
	receiveCallback        ReceivePacketFunction
	receivePacketsCallback receivePacketsBatchFunction
	udpBufferSettings      *UdpBufferSettings
	receiveDispatcher      *udpReceiveDispatcher

	mutex sync.Mutex

	sequences       map[BufferId]*UdpSequence
	sourceSequences map[TransferPath]map[BufferId]*UdpSequence
}

func newUdpBuffer[BufferId comparable](
	ctx context.Context,
	receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings,
) *UdpBuffer[BufferId] {
	return &UdpBuffer[BufferId]{
		ctx:               ctx,
		log:               loggerOrDefault(udpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		udpBufferSettings: udpBufferSettings,
		receiveDispatcher: newUdpReceiveDispatcher(ctx, udpBufferSettings),
		sequences:         map[BufferId]*UdpSequence{},
		sourceSequences:   map[TransferPath]map[BufferId]*UdpSequence{},
	}
}

func (self *UdpBuffer[BufferId]) udpSend(
	bufferId BufferId,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	udp *parsedUdp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error) {
	initSequence := func(skip *UdpSequence) *UdpSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		sequence, ok := self.sequences[bufferId]
		if ok {
			if skip == nil || skip != sequence {
				return sequence
			} else {
				sequence.Cancel()
				delete(self.sequences, bufferId)
				sourceSequences := self.sourceSequences[sequence.source]
				delete(sourceSequences, bufferId)
				if 0 == len(sourceSequences) {
					delete(self.sourceSequences, sequence.source)
				}
			}
		}

		if 0 < self.udpBufferSettings.UserLimit {
			// limit the total connections per source to avoid blowing up the ulimit
			if sourceSequences := self.sourceSequences[source]; self.udpBufferSettings.UserLimit <= len(sourceSequences) {
				applyLruMapLimit(sourceSequences, self.udpBufferSettings.UserLimit-1, func(bufferId BufferId, sequence *UdpSequence) bool {
					if self.log.V(1).Enabled() {
						self.log.Infof(
							"[lnr]udp limit source %s->%s\n",
							source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					self.removeSequenceWithLock(bufferId, sequence)
					return true
				})
			}
		}
		if 0 < self.udpBufferSettings.GlobalLimit {
			// limit the total connections across all sources, an aggregate
			// flow state and fd ceiling
			if self.udpBufferSettings.GlobalLimit <= len(self.sequences) {
				applyLruMapLimit(self.sequences, self.udpBufferSettings.GlobalLimit-1, func(bufferId BufferId, sequence *UdpSequence) bool {
					if self.log.V(1).Enabled() {
						self.log.Infof(
							"[lnr]udp limit global %s->%s\n",
							sequence.source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					self.removeSequenceWithLock(bufferId, sequence)
					return true
				})
			}
		}

		// TODO
		// limit the number of new connections per second per source
		// self.sourceLimiter[source].Limit()

		sourceIpCopy := make(net.IP, len(udp.sourceIp))
		copy(sourceIpCopy, udp.sourceIp)

		destinationIpCopy := make(net.IP, len(udp.destinationIp))
		copy(destinationIpCopy, udp.destinationIp)

		sequence = NewUdpSequence(
			self.ctx,
			self.receiveCallback,
			source,
			provideMode,
			ipVersion,
			sourceIpCopy,
			udp.sourcePort,
			destinationIpCopy,
			udp.destinationPort,
			self.udpBufferSettings,
		)
		sequence.receivePacketsCallback = self.receivePacketsCallback
		sequence.receiveDispatcher = self.receiveDispatcher
		sequence.receiveShard = self.receiveDispatcher.assignShard()
		self.sequences[bufferId] = sequence
		sourceSequences := self.sourceSequences[source]
		if sourceSequences == nil {
			sourceSequences = map[BufferId]*UdpSequence{}
			self.sourceSequences[source] = sourceSequences
		}
		sourceSequences[bufferId] = sequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sequence.Close()
				// clean up
				if sequence == self.sequences[bufferId] {
					delete(self.sequences, bufferId)
					sourceSequences := self.sourceSequences[sequence.source]
					delete(sourceSequences, bufferId)
					if 0 == len(sourceSequences) {
						delete(self.sourceSequences, sequence.source)
					}
				}
			}()
			sequence.Run()
		})
		return sequence
	}

	sendItem := &UdpSendItem{
		provideMode: provideMode,
		udp:         *udp,
		ipPacket:    ipPacket,
	}
	sequence := initSequence(nil)
	if success, err := sequence.send(sendItem, timeout); err == nil {
		return success, nil
	} else {
		// sequence closed
		return initSequence(sequence).send(sendItem, timeout)
	}
}

// removeSequenceWithLock removes a UDP sequence from both indexes before
// canceling it. The sequence goroutine's deferred cleanup is identity-checked,
// so eager removal is safe and makes the configured cap exact under bursts.
// The caller holds mutex.
func (self *UdpBuffer[BufferId]) removeSequenceWithLock(bufferId BufferId, sequence *UdpSequence) {
	if self.sequences[bufferId] != sequence {
		return
	}
	delete(self.sequences, bufferId)
	sourceSequences := self.sourceSequences[sequence.source]
	delete(sourceSequences, bufferId)
	if len(sourceSequences) == 0 {
		delete(self.sourceSequences, sequence.source)
	}
	sequence.Cancel()
}

type UdpSequence struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	log                    Logger
	receiveCallback        ReceivePacketFunction
	receivePacketsCallback receivePacketsBatchFunction
	receiveDispatcher      *udpReceiveDispatcher
	receiveShard           int
	udpBufferSettings      *UdpBufferSettings

	sendMutex sync.Mutex
	sendItems chan *UdpSendItem
	// Lazily allocated only when the bounded send queue actually blocks.
	// sendMutex serializes Reset/Stop.
	sendTimer *time.Timer

	idleCondition *IdleCondition

	StreamState
}

func NewUdpSequence(ctx context.Context, receiveCallback ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	sourceIp net.IP, sourcePort uint16,
	destinationIp net.IP, destinationPort uint16,
	udpBufferSettings *UdpBufferSettings) *UdpSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &UdpSequence{
		ctx:               cancelCtx,
		cancel:            cancel,
		log:               loggerOrDefault(udpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		sendItems:         make(chan *UdpSendItem, udpBufferSettings.SequenceBufferSize),
		udpBufferSettings: udpBufferSettings,
		idleCondition:     NewIdleCondition(),
		StreamState: StreamState{
			source:          source,
			provideMode:     provideMode,
			ipVersion:       ipVersion,
			sourceIp:        sourceIp,
			sourcePort:      sourcePort,
			destinationIp:   destinationIp,
			destinationPort: destinationPort,
			userLimited: userLimited{
				lastActivityTime: time.Now(),
			},
		},
	}
}

func (self *UdpSequence) send(sendItem *UdpSendItem, timeout time.Duration) (bool, error) {
	self.sendMutex.Lock()
	defer self.sendMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, nil
	}
	defer self.idleCondition.UpdateClose()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	// fast path without arming a timer
	select {
	case self.sendItems <- sendItem:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		default:
			return false, nil
		}
	} else {
		timeoutChan := resetOrCreateTimer(&self.sendTimer, timeout)
		select {
		case <-self.ctx.Done():
			self.sendTimer.Stop()
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			self.sendTimer.Stop()
			return true, nil
		case <-timeoutChan:
			return false, nil
		}
	}
}

func (self *UdpSequence) receivePacket(packet []byte) {
	self.receiveCallback(self.source, self.provideMode, self.IpPath(), packet)
	MessagePoolReturn(packet)
}

// receiveBatch delivers a flow's drained batch in one call when a batch
// consumer is registered (coalesced into one wire Pack), else per packet. The
// dispatcher owns the buffers; the consumer shares read-only what it retains.
func (self *UdpSequence) receiveBatch(packets [][]byte) {
	if self.receivePacketsCallback != nil &&
		self.receivePacketsCallback(self.source, self.provideMode, self.IpPath(), packets) {
		for _, packet := range packets {
			MessagePoolReturn(packet)
		}
		return
	}
	for _, packet := range packets {
		self.receivePacket(packet)
	}
}

func (self *UdpSequence) Run() {
	defer func() {
		self.cancel()

		func() {
			self.sendMutex.Lock()
			defer self.sendMutex.Unlock()
			close(self.sendItems)
		}()

		// drain the channel
		func() {
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					MessagePoolReturn(sendItem.ipPacket)
				default:
					return
				}
			}
		}()
	}()

	self.log.V(2).Infof("[init]udp connect\n")
	socket, err := self.udpBufferSettings.DialContext(
		self.ctx,
		"udp",
		self.IpPath().DestinationHostPort(),
	)
	if err != nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[init]udp connect error = %s\n", err)
		}
		// answer the source instead of going silent: udp "dial" cannot yield a
		// meaningful refusal at connect time, so always send the capacity-class
		// unreachable through the same receive callback, then return -- no
		// socket exists.
		if _, signal := classifyDialFailure(self.IpPath(), err); signal != nil {
			self.receivePacket(MessagePoolCopy(signal))
		}
		return
	}
	defer socket.Close()
	self.UpdateLastActivityTime()
	self.log.V(2).Infof("[init]connect success\n")

	if udpConn, ok := socket.(*net.UDPConn); ok {
		// size the kernel buffers to the max window.
		// the os may silently cap these at system limits.
		udpConn.SetReadBuffer(int(self.udpBufferSettings.MaxWindowSize))
		udpConn.SetWriteBuffer(int(self.udpBufferSettings.MaxWindowSize))
	}
	// f, _ := udpConn.File()
	// fd := SocketHandle(f.Fd())
	// syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU, self.udpBufferSettings.Mtu)

	go HandleError(func() {
		// The dispatcher owns every successfully enqueued packet. Canceling
		// the sequence closes the socket/main send loop, but already queued
		// packets remain valid and drain in shard FIFO order.
		defer self.cancel()

		buffer := make([]byte, self.udpBufferSettings.ReadBufferByteCount)

		for forwardIter := uint64(0); ; forwardIter += 1 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			readTimeout := time.Now().Add(self.udpBufferSettings.ReadTimeout)
			socket.SetReadDeadline(readTimeout)
			n, err := socket.Read(buffer)

			if err != nil {
				if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]udp receive err = %s\n", forwardIter, err)
				}
			}

			if 0 < n {
				self.UpdateLastActivityTime()

				packets, packetsErr := self.DataPackets(buffer, n, self.udpBufferSettings.Mtu)
				if packetsErr != nil {
					self.log.Infof("[f%d]udp receive packets error = %s\n", forwardIter, packetsErr)
					return
				}
				if 1 < len(packets) {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]udp receive segemented packets = %d\n", forwardIter, len(packets))
					}
				}
				for _, packet := range packets {
					if self.log.V(1).Enabled() {
						self.log.Infof("[f%d]udp receive %d\n", forwardIter, len(packet))
					}
					if self.receiveDispatcher == nil {
						// Directly constructed sequences (primarily focused
						// tests) retain synchronous ordered delivery.
						self.singleDataPacket[0] = packet
						self.receiveBatch(self.singleDataPacket[:])
					} else if !self.receiveDispatcher.enqueue(self, packet) {
						MessagePoolReturn(packet)
					}
				}
			}

			if err != nil {
				if err == io.EOF {
					return
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if self.log.V(1).Enabled() {
						self.log.Infof("[f%d]timeout\n", forwardIter)
					}
					return
				} else {
					// some other error
					return
				}
			}
		}
	}, self.cancel)

	sendIter := uint64(0)
	// The sequence goroutine owns the outbound socket writes directly. The
	// former write worker plus writePayloads channel duplicated both one
	// goroutine and one SequenceBufferSize queue per UDP flow. sendItems is
	// already the bounded producer/consumer queue, and net.Conn permits one
	// concurrent reader plus one writer, so the extra stage added memory,
	// scheduling, and a handoff without adding parallelism.
	writeBatch := make([]*UdpSendItem, 0, self.udpBufferSettings.WriteBatchSize)
	writeSendItems := func(first *UdpSendItem) bool {
		writeBatch = append(writeBatch[:0], first)
	drain:
		for len(writeBatch) < cap(writeBatch) {
			select {
			case sendItem, ok := <-self.sendItems:
				if !ok {
					break drain
				}
				writeBatch = append(writeBatch, sendItem)
			default:
				break drain
			}
		}

		socket.SetWriteDeadline(time.Now().Add(self.udpBufferSettings.WriteTimeout))
		var writeErr error
		for _, sendItem := range writeBatch {
			payload := sendItem.udp.payload
			if writeErr == nil && 0 < len(payload) {
				// Each payload is one datagram; writes cannot be coalesced, but
				// the drained batch shares one deadline and one scheduler wake.
				n, err := socket.Write(payload)
				if err == nil {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]udp forward %d\n", sendIter, n)
					}
				} else if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]udp forward %d error = %s", sendIter, n, err)
				}
				if 0 < n {
					self.UpdateLastActivityTime()
				}
				writeErr = err
			}
			MessagePoolReturn(sendItem.ipPacket)
			sendIter += 1
		}
		return writeErr == nil
	}

	// reusable idle timer: this send loop wakes per datagram, so a per-iteration
	// time.After allocated a timer per packet (the dominant alloc in the udp
	// egress profile). hot-path timer reuse per CODESTYLE.
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.udpBufferSettings.IdleTimeout)
		select {
		case <-self.ctx.Done():
			return
		case sendItem, ok := <-self.sendItems:
			if !ok {
				return
			}
			if !writeSendItems(sendItem) {
				return
			}
		case <-idleTimer.C:
			done := false
			func() {
				self.sendMutex.Lock()
				defer self.sendMutex.Unlock()
				if self.idleCondition.Close(checkpointId) {
					// close the sequence
					done = true
				}
			}()
			if done {
				// close the sequence
				return
			}
			// else there pending updates
		}
	}
}

func (self *UdpSequence) Cancel() {
	self.cancel()
}

func (self *UdpSequence) Close() {
	self.cancel()
}

type UdpSendItem struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	udp         parsedUdp
	ipPacket    []byte
}

type StreamState struct {
	source          TransferPath
	provideMode     protocol.ProvideMode
	ipVersion       int
	sourceIp        net.IP
	sourcePort      uint16
	destinationIp   net.IP
	destinationPort uint16
	userLimited

	// cached immutable ip path for this stream (see IpPath). primed by the
	// first call, which happens at sequence setup (DialContext) before the
	// per-packet goroutines start, so it is written once and then read-only.
	ipPath *IpPath

	// reusable backing for the common single-datagram DataPackets result.
	// DataPackets is called from one goroutine and its result is consumed
	// before the next call, so the backing can be reused; fragmented payloads
	// allocate a fresh slice.
	singleDataPacket [1][]byte
}

// IpPath returns the immutable ip path for this stream. The path is built once
// and cached; the stream identity (version, ips, ports) never changes.
func (self *StreamState) IpPath() *IpPath {
	if self.ipPath == nil {
		self.ipPath = &IpPath{
			Version:         self.ipVersion,
			Protocol:        IpProtocolUdp,
			SourceIp:        self.sourceIp,
			SourcePort:      int(self.sourcePort),
			DestinationIp:   self.destinationIp,
			DestinationPort: int(self.destinationPort),
		}
	}
	return self.ipPath
}

// this must only be called from one goroutine
// this is called from the writer only and does not need to syncrhronize with the reader state
func (self *StreamState) DataPackets(payload []byte, n int, mtu int) ([][]byte, error) {
	var headerByteCount int
	switch self.ipVersion {
	case 4:
		headerByteCount = Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize
	case 6:
		headerByteCount = Ipv6HeaderSize + UdpHeaderSize
	}

	packetByteCount := mtu - headerByteCount
	if n <= packetByteCount {
		// reuse the single-packet backing for the common unfragmented case
		// (see singleDataPacket); the result is consumed before the next call.
		self.singleDataPacket[0] = self.udpPacket(payload[0:n])
		return self.singleDataPacket[:], nil
	}
	// fragment into separate datagrams
	packets := make([][]byte, 0, (n+packetByteCount-1)/packetByteCount)
	for i := 0; i < n; {
		j := min(i+packetByteCount, n)
		packets = append(packets, self.udpPacket(payload[i:j]))
		i = j
	}
	return packets, nil
}

// builds a udp packet from the stream destination to the stream source
// into a single pool buffer
func (self *StreamState) udpPacket(payload []byte) []byte {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + UdpHeaderSize + len(payload))
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, ipProtocolNumberUdp, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, ipProtocolNumberUdp, self.destinationIp, self.sourceIp)
	}

	udp := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(udp[0:2], uint16(self.destinationPort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(self.sourcePort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	// checksum, set below
	udp[6] = 0
	udp[7] = 0
	copy(udp[UdpHeaderSize:], payload)
	checksum := transportChecksum(ipProtocolNumberUdp, self.destinationIp, self.sourceIp, udp)
	if checksum == 0 {
		// zero means no checksum
		checksum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], checksum)
	return packet
}

type TcpBufferSettings struct {
	// nil resolves to the local user nat `Log`
	Log Logger
	// ConnectTimeout     time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// coalesce pure acks for up to this duration.
	// an ack is sent sooner when the unacked byte count reaches half the window.
	// zero sends a pure ack on every send seq advance.
	AckCompressTimeout time.Duration
	// ReadPollTimeout time.Duration
	// WritePollTimeout time.Duration
	IdleTimeout         time.Duration
	ReadBufferByteCount int
	// the maximum number of payloads to coalesce into a single socket write
	WriteBatchSize     int
	SequenceBufferSize int
	Mtu                int
	// the window size is the max amount of packet data in memory for each sequence
	// `WindowSize / 2^WindowScale` must fit in uint16
	// see https://datatracker.ietf.org/doc/html/rfc1323#page-8
	WindowScale uint32
	// the initial window size
	MinWindowSize uint32
	// `MaxWindowSize` should be a power of 2 multiple of `MinWindowSize`
	MaxWindowSize uint32
	// the number of open sockets per user
	// uses an lru cleanup where new sockets over the limit close old sockets
	UserLimit int
	// the number of open sockets across all users (per address family
	// buffer), an aggregate flow state and fd ceiling for hosts with a hard
	// process memory budget. uses the same lru cleanup as `UserLimit`.
	// 0 (the default) is no limit.
	GlobalLimit int
	// EnableOrphanRst replies with a RST to a non-SYN packet that matches no
	// sequence (PROXYDRAIN1.md §3.5). Without it a source whose flow state
	// was lost here (e.g. the source client restarted with a fresh identity,
	// orphaning its provider-side flows) retransmits into silence and the
	// application hangs to its own timeout; the RST makes it reconnect
	// immediately. Rate limited by `OrphanRstPerSecond`.
	EnableOrphanRst bool
	// OrphanRstPerSecond bounds orphan RST generation per buffer (an abuse
	// valve: orphan packets are attacker-influenceable). <= 0 is unlimited.
	OrphanRstPerSecond int

	// EnableSyntheticSpeed terminates TCP flows to the RFC 2544 benchmark
	// range 198.18.0.0/15 at an in-memory synthetic speed server instead of
	// dialing upstream (see ip_synthetic_speed.go). The range is never
	// publicly routable, so no real destination is shadowed. Serves
	// measurement of the tunnel path itself, isolated from origin and
	// upstream network variability.
	EnableSyntheticSpeed bool

	ConnectSettings
}

type Tcp4Buffer struct {
	TcpBuffer[BufferId4]
}

func NewTcp4Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings) *Tcp4Buffer {
	return &Tcp4Buffer{
		TcpBuffer: *newTcpBuffer[BufferId4](ctx, receiveCallback, tcpBufferSettings),
	}
}

func (self *Tcp4Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	tcp *parsedTcp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId4(
		source,
		tcp.sourceIp, int(tcp.sourcePort),
		tcp.destinationIp, int(tcp.destinationPort),
	)

	return self.tcpSend(
		bufferId,
		source,
		provideMode,
		4,
		tcp,
		timeout,
		ipPacket,
	)
}

type Tcp6Buffer struct {
	TcpBuffer[BufferId6]
}

func NewTcp6Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings) *Tcp6Buffer {
	return &Tcp6Buffer{
		TcpBuffer: *newTcpBuffer[BufferId6](ctx, receiveCallback, tcpBufferSettings),
	}
}

func (self *Tcp6Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	tcp *parsedTcp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId6(
		source,
		tcp.sourceIp, int(tcp.sourcePort),
		tcp.destinationIp, int(tcp.destinationPort),
	)

	return self.tcpSend(
		bufferId,
		source,
		provideMode,
		6,
		tcp,
		timeout,
		ipPacket,
	)
}

type TcpBuffer[BufferId comparable] struct {
	log                    Logger
	ctx                    context.Context
	receiveCallback        ReceivePacketFunction
	receivePacketsCallback receivePacketsBatchFunction
	tcpBufferSettings      *TcpBufferSettings

	mutex sync.Mutex

	sequences       map[BufferId]*TcpSequence
	sourceSequences map[TransferPath]map[BufferId]*TcpSequence

	// orphan rst rate limiting (guarded by mutex; see EnableOrphanRst)
	orphanRstWindowStart time.Time
	orphanRstWindowCount int
}

func newTcpBuffer[BufferId comparable](
	ctx context.Context,
	receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings,
) *TcpBuffer[BufferId] {
	return &TcpBuffer[BufferId]{
		log:               loggerOrDefault(tcpBufferSettings.Log),
		ctx:               ctx,
		receiveCallback:   receiveCallback,
		tcpBufferSettings: tcpBufferSettings,
		sequences:         map[BufferId]*TcpSequence{},
		sourceSequences:   map[TransferPath]map[BufferId]*TcpSequence{},
	}
}

// allowOrphanRstWithLock applies the orphan rst rate limit. The caller must
// hold `mutex`.
func (self *TcpBuffer[BufferId]) allowOrphanRstWithLock() bool {
	limit := self.tcpBufferSettings.OrphanRstPerSecond
	if limit <= 0 {
		return true
	}
	now := time.Now()
	if 1*time.Second <= now.Sub(self.orphanRstWindowStart) {
		self.orphanRstWindowStart = now
		self.orphanRstWindowCount = 0
	}
	if limit <= self.orphanRstWindowCount {
		return false
	}
	self.orphanRstWindowCount += 1
	return true
}

func (self *TcpBuffer[BufferId]) tcpSend(
	bufferId BufferId,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	tcp *parsedTcp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error) {
	var orphanRst []byte
	var orphanSourceIp net.IP
	var orphanDestinationIp net.IP
	initSequence := func() *TcpSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if sequence, ok := self.sequences[bufferId]; ok {
			if tcp.rst {
				// drop the packet
				sequence.Cancel()
				delete(self.sequences, bufferId)
				sourceSequences := self.sourceSequences[sequence.source]
				delete(sourceSequences, bufferId)
				if 0 == len(sourceSequences) {
					delete(self.sourceSequences, sequence.source)
				}
				// Return false without consuming ipPacket; the LocalUserNat
				// dispatcher owns and returns every packet a sequence does not
				// accept.
				return nil
			}
			if !tcp.syn || sequence.ctx.Err() == nil && tcp.seq == sequence.initialSynSeq {
				return sequence
			}

			// A source may reuse a four-tuple before the previous sequence's
			// goroutine reaches deferred map cleanup. A fresh SYN sent to that
			// old sequence is rejected by its established sequence-number
			// check, leaving the replacement connection silent until timeout.
			// Keep an exact live SYN retransmission on the current generation;
			// a different initial sequence number, or a canceled generation,
			// atomically replaces it.
			self.removeSequenceWithLock(bufferId, sequence)
		}

		if !tcp.syn {
			// drop the packet; only create a new sequence on SYN.
			// Reply with a RST so the source fails fast instead of
			// retransmitting into silence (PROXYDRAIN1.md §3.5) — sent
			// outside the lock, below. Never reset a reset.
			if self.tcpBufferSettings.EnableOrphanRst && !tcp.rst && self.allowOrphanRstWithLock() {
				orphanRst = tcpRstForOrphan(ipVersion, tcp)
				// parseIpv4/parseIpv6 return views into ipPacket, which is
				// returned below before the callback runs. Preserve the path
				// independently of that pooled backing. Use one allocation for
				// both address slices.
				ipBacking := make(net.IP, len(tcp.sourceIp)+len(tcp.destinationIp))
				sourceIpByteCount := copy(ipBacking, tcp.sourceIp)
				copy(ipBacking[sourceIpByteCount:], tcp.destinationIp)
				orphanSourceIp = ipBacking[:sourceIpByteCount:sourceIpByteCount]
				orphanDestinationIp = ipBacking[sourceIpByteCount:]
			}
			if self.log.V(2).Enabled() {
				self.log.Infof("[lnr]tcp drop no syn (%s)\n", tcp.flagsString())
			}
			// As above, false means ownership stays with the dispatcher.
			return nil
		}

		// else new sequence
		// if sequence, ok := self.sequences[bufferId]; ok {
		// 	sequence.Cancel()
		// 	delete(self.sequences, bufferId)
		// 	sourceSequences := self.sourceSequences[sequence.source]
		// 	delete(sourceSequences, bufferId)
		// 	if 0 == len(sourceSequences) {
		// 		delete(self.sourceSequences, sequence.source)
		// 	}
		// }
		if 0 < self.tcpBufferSettings.UserLimit {
			// limit the total connections per source to avoid blowing up the ulimit
			if sourceSequences := self.sourceSequences[source]; self.tcpBufferSettings.UserLimit <= len(sourceSequences) {
				applyLruMapLimit(sourceSequences, self.tcpBufferSettings.UserLimit-1, func(bufferId BufferId, sequence *TcpSequence) bool {
					if self.log.V(1).Enabled() {
						self.log.Infof(
							"[lnr]tcp limit source %s->%s\n",
							source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					self.removeSequenceWithLock(bufferId, sequence)
					return true
				})
			}
		}
		if 0 < self.tcpBufferSettings.GlobalLimit {
			// limit the total connections across all sources, an aggregate
			// flow state and fd ceiling
			if self.tcpBufferSettings.GlobalLimit <= len(self.sequences) {
				applyLruMapLimit(self.sequences, self.tcpBufferSettings.GlobalLimit-1, func(bufferId BufferId, sequence *TcpSequence) bool {
					if self.log.V(1).Enabled() {
						self.log.Infof(
							"[lnr]tcp limit global %s->%s\n",
							sequence.source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					self.removeSequenceWithLock(bufferId, sequence)
					return true
				})
			}
		}

		// TODO
		// limit the number of new connections per second per source
		// self.sourceLimiter[source].Limit()

		sourceIpCopy := make(net.IP, len(tcp.sourceIp))
		copy(sourceIpCopy, tcp.sourceIp)

		destinationIpCopy := make(net.IP, len(tcp.destinationIp))
		copy(destinationIpCopy, tcp.destinationIp)

		sequence := NewTcpSequence(
			self.ctx,
			self.receiveCallback,
			source,
			provideMode,
			ipVersion,
			sourceIpCopy,
			tcp.sourcePort,
			destinationIpCopy,
			tcp.destinationPort,
			tcp.seq,
			self.tcpBufferSettings,
		)
		sequence.receivePacketsCallback = self.receivePacketsCallback
		self.sequences[bufferId] = sequence
		sourceSequences := self.sourceSequences[source]
		if sourceSequences == nil {
			sourceSequences = map[BufferId]*TcpSequence{}
			self.sourceSequences[source] = sourceSequences
		}
		sourceSequences[bufferId] = sequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sequence.Close()
				// clean up
				if sequence == self.sequences[bufferId] {
					delete(self.sequences, bufferId)
					sourceSequences := self.sourceSequences[sequence.source]
					delete(sourceSequences, bufferId)
					if 0 == len(sourceSequences) {
						delete(self.sourceSequences, sequence.source)
					}
				}
			}()
			sequence.Run()
		})
		return sequence
	}
	sendItem := &TcpSendItem{
		provideMode: provideMode,
		tcp:         *tcp,
		ipPacket:    ipPacket,
	}
	if sequence := initSequence(); sequence == nil {
		if orphanRst != nil {
			// outside the buffer lock: the receive callback can block on the
			// return send path
			self.receiveCallback(
				source,
				provideMode,
				&IpPath{
					Version:         ipVersion,
					Protocol:        IpProtocolTcp,
					SourceIp:        orphanSourceIp,
					SourcePort:      int(tcp.sourcePort),
					DestinationIp:   orphanDestinationIp,
					DestinationPort: int(tcp.destinationPort),
				},
				orphanRst,
			)
			MessagePoolReturn(orphanRst)
		}
		// sequence does not exist and not a syn packet, drop
		return false, nil
	} else {
		return sequence.send(sendItem, timeout)
	}
}

// removeSequenceWithLock is the TCP counterpart of the UDP helper above.
// Eager index removal keeps the cap exact even when canceled sequence
// goroutines have not reached their deferred cleanup yet. The caller holds
// mutex.
func (self *TcpBuffer[BufferId]) removeSequenceWithLock(bufferId BufferId, sequence *TcpSequence) {
	if self.sequences[bufferId] != sequence {
		return
	}
	delete(self.sequences, bufferId)
	sourceSequences := self.sourceSequences[sequence.source]
	delete(sourceSequences, bufferId)
	if len(sourceSequences) == 0 {
		delete(self.sourceSequences, sequence.source)
	}
	sequence.Cancel()
}

// tcpRstForOrphan builds the RFC 793 reset for a segment that matched no
// sequence, addressed back to the segment's source: with ACK set the reset
// carries seq = segment.ack and no ack flag; otherwise seq 0 and
// ack = segment.seq + payload length (+1 each for syn/fin), with RST|ACK.
func tcpRstForOrphan(ipVersion int, tcp *parsedTcp) []byte {
	var ipHeaderByteCount int
	switch ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	default:
		return nil
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions)
	switch ipVersion {
	case 4:
		writeIpv4Header(packet, ipProtocolNumberTcp, tcp.destinationIp, tcp.sourceIp)
	case 6:
		writeIpv6Header(packet, ipProtocolNumberTcp, tcp.destinationIp, tcp.sourceIp)
	}

	t := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(t[0:2], tcp.destinationPort)
	binary.BigEndian.PutUint16(t[2:4], tcp.sourcePort)
	var seq uint32
	var ackNumber uint32
	flags := byte(tcpFlagRst)
	if tcp.ack {
		seq = tcp.ackNumber
	} else {
		ackNumber = tcp.seq + uint32(len(tcp.payload))
		if tcp.syn {
			ackNumber += 1
		}
		if tcp.fin {
			ackNumber += 1
		}
		flags |= tcpFlagAck
	}
	binary.BigEndian.PutUint32(t[4:8], seq)
	binary.BigEndian.PutUint32(t[8:12], ackNumber)
	// data offset, no options
	t[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	t[13] = flags
	// window
	t[14] = 0
	t[15] = 0
	// checksum, set below
	t[16] = 0
	t[17] = 0
	// urgent
	t[18] = 0
	t[19] = 0
	binary.BigEndian.PutUint16(t[16:18], transportChecksum(ipProtocolNumberTcp, tcp.destinationIp, tcp.sourceIp, t))
	return packet
}

/*
** Important implementation note **
In this implementation, packet flow from the UNAT to the source
is assumed to never require retransmits. The retrasmit logic
is not implemented.
This is a safe assumption when moving packets from local raw socket
to the UNAT via `transfer`, which is lossless and in-order.
*/
// writeWithProgressDeadline writes to an upstream socket, bounding
// ZERO-PROGRESS time rather than total time: the deadline re-arms whenever a
// write advances, so a peer applying ordinary flow control (accepting data
// steadily but slowly) is not failed, while a peer that accepts nothing for a
// full timeout still is.
//
// A single deadline for the whole batch killed alive flows under sustained
// bulk transfer: the return path's acks stall behind saturated tunnel queues
// long enough that the upstream socket drains slower than one batch, and the
// partial write at the deadline tore the sequence down (observed as mid-stream
// resets in the full-stack tcp test).
func writeWithProgressDeadline(
	socket net.Conn,
	buffers net.Buffers,
	timeout time.Duration,
) (int64, error) {
	n := int64(0)
	for {
		socket.SetWriteDeadline(time.Now().Add(timeout))
		wn, err := buffers.WriteTo(socket)
		n += wn
		if err == nil {
			return n, nil
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() && 0 < wn {
			// progress inside this deadline window: the peer is alive and
			// draining, so give the remainder a fresh window
			continue
		}
		return n, err
	}
}

type TcpSequence struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	receiveCallback        ReceivePacketFunction
	receivePacketsCallback receivePacketsBatchFunction

	tcpBufferSettings *TcpBufferSettings

	sendMutex sync.Mutex
	sendItems chan *TcpSendItem
	// Lazily allocated only when the bounded send queue actually blocks.
	// sendMutex serializes Reset/Stop.
	sendTimer *time.Timer

	idleCondition *IdleCondition
	// immutable generation identity used to distinguish a retransmitted SYN
	// from four-tuple reuse while the previous flow is still being reaped
	initialSynSeq uint32

	ConnectionState
}

func NewTcpSequence(ctx context.Context, receiveCallback ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	sourceIp net.IP, sourcePort uint16,
	destinationIp net.IP, destinationPort uint16,
	initialSynSeq uint32,
	tcpBufferSettings *TcpBufferSettings) *TcpSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &TcpSequence{
		ctx:               cancelCtx,
		cancel:            cancel,
		log:               loggerOrDefault(tcpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		tcpBufferSettings: tcpBufferSettings,
		sendItems:         make(chan *TcpSendItem, tcpBufferSettings.SequenceBufferSize),
		idleCondition:     NewIdleCondition(),
		initialSynSeq:     initialSynSeq,
		ConnectionState: ConnectionState{
			source:          source,
			provideMode:     provideMode,
			ipVersion:       ipVersion,
			sourceIp:        sourceIp,
			sourcePort:      sourcePort,
			destinationIp:   destinationIp,
			destinationPort: destinationPort,
			// prime the cached ip path before the sequence goroutines start
			// (see IpPath)
			ipPath: &IpPath{
				Version:         ipVersion,
				Protocol:        IpProtocolTcp,
				SourceIp:        sourceIp,
				SourcePort:      int(sourcePort),
				DestinationIp:   destinationIp,
				DestinationPort: int(destinationPort),
			},
			// the window size starts at the fixed value
			enableWindowScale: false,
			// FIXME start this at initial window size, and it grows up to max window size
			// FIXME initial window size should be ~4k, set max window size as a 2^amount multiplier of initial size
			windowSize:  tcpBufferSettings.MinWindowSize,
			windowScale: 0,
			userLimited: userLimited{
				lastActivityTime: time.Now(),
			},
		},
	}
}

func (self *TcpSequence) send(sendItem *TcpSendItem, timeout time.Duration) (bool, error) {
	self.sendMutex.Lock()
	defer self.sendMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, nil
	}
	defer self.idleCondition.UpdateClose()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	// fast path without arming a timer
	select {
	case self.sendItems <- sendItem:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		default:
			return false, nil
		}
	} else {
		timeoutChan := resetOrCreateTimer(&self.sendTimer, timeout)
		select {
		case <-self.ctx.Done():
			self.sendTimer.Stop()
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			self.sendTimer.Stop()
			return true, nil
		case <-timeoutChan:
			return false, nil
		}
	}
}

// initializeSynWithLock establishes sequence and window state from the first
// SYN. The sequence mutex must be held.
func (self *TcpSequence) initializeSynWithLock(tcp *parsedTcp) {
	// SYN and FIN consume one sequence number.
	self.sendSeq = tcp.seq + 1
	// The synthetic return sequence may start at the sender's sequence because
	// there is no transport-security boundary between the two sides.
	self.receiveSeq = tcp.seq
	self.receiveSeqAck = tcp.seq

	parseWindowScaleOpts := func() (bool, uint32) {
		options := tcp.options
		for optionIndex := 0; optionIndex < len(options); {
			switch options[optionIndex] {
			case 0:
				return false, 0
			case 1:
				optionIndex += 1
			default:
				if len(options) < optionIndex+2 {
					return false, 0
				}
				optionByteCount := int(options[optionIndex+1])
				if optionByteCount < 2 || len(options) < optionIndex+optionByteCount {
					return false, 0
				}
				if options[optionIndex] == 3 && optionByteCount == 3 {
					return true, min(uint32(options[optionIndex+2]), 14)
				}
				optionIndex += optionByteCount
			}
		}
		return false, 0
	}

	self.enableWindowScale, self.receiveWindowScale = parseWindowScaleOpts()
	// RFC 7323 applies the negotiated scale only after the handshake. The
	// Window field in a SYN is always the literal, unscaled value.
	self.receiveWindowSize = uint32(tcp.windowSize)
	self.receiveWindowEnd = self.receiveSeqAck + self.receiveWindowSize
	self.receiveWindowEndSet = true
	if self.enableWindowScale {
		bits := math.Log2(float64(self.tcpBufferSettings.MaxWindowSize) / float64(math.MaxUint16))
		if 0 <= bits {
			self.windowScale = uint32(math.Ceil(bits))
		} else {
			self.windowScale = 0
		}
	} else {
		self.windowScale = 0
	}
}

func (self *TcpSequence) Run() {
	defer func() {
		self.cancel()

		func() {
			self.sendMutex.Lock()
			defer self.sendMutex.Unlock()
			close(self.sendItems)
		}()

		// drain the channel
		func() {
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					MessagePoolReturn(sendItem.ipPacket)
				default:
					return
				}
			}
		}()
	}()

	// One timer serves the initial SYN wait and the steady-state idle wait.
	// The send loop wakes per segment, so time.After here previously allocated
	// a timer per packet even when the send channel was immediately ready.
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	// note receive is called from multiple goroutines
	// tcp packets with ack may be reordered due to being written in parallel
	receive := func(packet []byte) {
		self.receiveCallback(self.source, self.provideMode, self.IpPath(), packet)
		MessagePoolReturn(packet)
	}
	// receiveBatch: coalesce a drained batch into one return-path wire Pack
	// when a batch consumer is registered, else deliver per packet. The
	// read-loop owns the buffers; the consumer shares read-only what it
	// retains, so we free our owning ref for each (same ownership as
	// `receive`).
	receiveBatch := func(packets [][]byte) {
		if self.receivePacketsCallback != nil &&
			self.receivePacketsCallback(self.source, self.provideMode, self.IpPath(), packets) {
			for _, packet := range packets {
				MessagePoolReturn(packet)
			}
			return
		}
		for _, packet := range packets {
			receive(packet)
		}
	}

	// f, _ := tcpConn.File()
	// fd := SocketHandle(f.Fd())
	// syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU, self.tcpBufferSettings.Mtu)

	var packet []byte
	var packetErr error
	for syn := false; !syn; {
		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.tcpBufferSettings.ConnectTimeout)
		select {
		case <-self.ctx.Done():
			idleTimer.Stop()
			return
		case sendItem := <-self.sendItems:
			idleTimer.Stop()
			if self.log.V(2).Enabled() {
				self.log.Infof("[init]send(%d)\n", len(sendItem.tcp.payload))
			}
			// the first packet must be a syn
			if sendItem.tcp.syn {
				self.log.V(2).Infof("[init]SYN\n")

				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					self.initializeSynWithLock(&sendItem.tcp)
					if self.log.V(2).Enabled() {
						self.log.Infof("[init]window=%d/%d, receive=%d/%d\n", self.windowSize, self.windowScale, self.receiveWindowSize, self.receiveWindowScale)
					}

					packet, packetErr = self.SynAck(self.tcpBufferSettings.Mtu)
					self.receiveSeq += 1
				}()

				syn = true
			} else {
				// an ACK here could be for a previous FIN
				if self.log.V(2).Enabled() {
					self.log.Infof("[init]waiting for SYN (%s)\n", sendItem.tcp.flagsString())
				}
			}
			MessagePoolReturn(sendItem.ipPacket)
		case <-idleTimer.C:
			if self.idleCondition.Close(checkpointId) {
				// close the sequence
				self.log.V(2).Infof("[init]connect timeout\n")
				return
			}
			// else there pending updates
		}
	}

	if packetErr != nil {
		return
	}

	// connect to upstream before sending the syn+ack
	self.log.V(2).Infof("[init]tcp connect\n")
	var socket net.Conn
	var err error
	if self.tcpBufferSettings.EnableSyntheticSpeed && isSyntheticSpeedIp(self.IpPath().DestinationIp) {
		// benchmark-range destination: terminate at the in-memory synthetic
		// speed server (see ip_synthetic_speed.go)
		if self.log.V(1).Enabled() {
			self.log.Infof("[init]tcp connect synthetic %s\n", self.IpPath().DestinationHostPort())
		}
		socket = newSyntheticSpeedConn()
	} else {
		socket, err = self.tcpBufferSettings.DialContext(
			self.ctx,
			"tcp",
			self.IpPath().DestinationHostPort(),
		)
	}
	if err != nil {
		// The source has not seen our synthetic syn-ack yet. Abandoning the
		// sequence silently makes it retransmit the syn on an exponential
		// schedule (17/33/65 seconds in Chromium under repeated provider dial
		// failure). Release the unsent syn-ack and reject the flow so the
		// source can fail or retry immediately. A parent cancellation is
		// teardown, not an upstream refusal, and must not generate new output.
		MessagePoolReturn(packet)
		if self.log.V(1).Enabled() {
			self.log.Infof("[init]tcp connect error = %s\n", err)
		}
		// answer the source instead of going silent: a refused destination gets
		// an honest RST+ACK, everything else a capacity-class unreachable. both
		// ride the same receive callback the SynAck uses (from-destination
		// orientation); then return as before -- no socket exists. the ctx
		// guard implements the comment above: classifyDialFailure cannot tell
		// a canceled parent from a capacity failure, and teardown must not
		// generate new output.
		if self.ctx.Err() == nil {
			switch action, signal := classifyDialFailure(self.IpPath(), err); action {
			case dialFailureRst:
				var rstPacket []byte
				var rstErr error
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()
					rstPacket, rstErr = self.RstAck()
				}()
				if rstErr == nil {
					receive(rstPacket)
				}
			case dialFailureUnreachable:
				receive(MessagePoolCopy(signal))
			}
		}
		return
	}
	self.UpdateLastActivityTime()
	self.log.V(2).Infof("[init]connect success\n")

	defer socket.Close()
	if tcpConn, ok := socket.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetNoDelay(true)
		// size the kernel buffers to the max window.
		// the os may silently cap these at system limits.
		tcpConn.SetReadBuffer(int(self.tcpBufferSettings.MaxWindowSize))
		tcpConn.SetWriteBuffer(int(self.tcpBufferSettings.MaxWindowSize))
	}

	self.log.V(2).Infof("[init]receive SYN+ACK\n")
	receive(packet)

	/*
		if v, ok := socket.(*net.TCPConn); ok {
			if err := v.SetWriteBuffer(int(self.windowSize)); err != nil {
				self.log.Infof("[init]could not set write buffer = %d\n", self.windowSize)
			}
			// if err := v.SetReadBuffer(int(self.receiveWindowSize)); err != nil {
			// 	self.log.Infof("[init]could not set read buffer = %d\n", self.receiveWindowSize)
			// }
		}
	*/

	receiveAckCond := sync.NewCond(&self.mutex)
	ackCond := sync.NewCond(&self.mutex)
	defer func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		receiveAckCond.Broadcast()
		ackCond.Broadcast()
	}()

	// signals the ack pipeline to send a coalesced ack now
	ackSignal := make(chan struct{}, 1)

	var ackedSendSeq uint32
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		ackedSendSeq = self.sendSeq
	}()

	// pipelines

	type writePayload struct {
		sendIter         uint64
		ipPacket         []byte
		payloadOffset    uint16
		payloadByteCount uint16
	}

	writePayloads := make(chan writePayload, self.tcpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		// best effort return of queued payloads after cancel
		defer func() {
			for {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(writePayload.ipPacket)
				default:
					return
				}
			}
		}()
		defer self.cancel()

		batch := make([]writePayload, 0, self.tcpBufferSettings.WriteBatchSize)
		bufferStorage := make([][]byte, 0, self.tcpBufferSettings.WriteBatchSize)

		for {
			batch = batch[:0]
			closed := false
			select {
			case <-self.ctx.Done():
				return
			case writePayload, ok := <-writePayloads:
				if !ok {
					return
				}
				batch = append(batch, writePayload)
			}
			// opportunistically coalesce queued payloads into a single socket write
		drain:
			for len(batch) < self.tcpBufferSettings.WriteBatchSize {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						closed = true
						break drain
					}
					batch = append(batch, writePayload)
				default:
					break drain
				}
			}

			bufferStorage = bufferStorage[:0]
			byteCount := 0
			for _, writePayload := range batch {
				payloadStart := int(writePayload.payloadOffset)
				payloadEnd := payloadStart + int(writePayload.payloadByteCount)
				payload := writePayload.ipPacket[payloadStart:payloadEnd]
				bufferStorage = append(bufferStorage, payload)
				byteCount += len(payload)
			}
			// `net.Buffers` uses a single vectored write when the socket supports it.
			// `WriteTo` retries partial writes until fully written, a timeout, or an error.
			buffers := net.Buffers(bufferStorage)

			n, err := writeWithProgressDeadline(
				socket,
				buffers,
				self.tcpBufferSettings.WriteTimeout,
			)

			if err == nil {
				if self.log.V(2).Enabled() {
					self.log.Infof("[f%d]tcp forward %d/%d\n", batch[0].sendIter, n, byteCount)
				}
			} else {
				if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]tcp forward %d/%d error = %s\n", batch[0].sendIter, n, byteCount, err)
				}
			}
			if 0 < n {
				self.UpdateLastActivityTime()
			}
			for _, writePayload := range batch {
				MessagePoolReturn(writePayload.ipPacket)
			}
			if err != nil {
				// timeout or socket error
				return
			}
			if closed {
				return
			}
		}
	}, self.cancel)

	// Keep at most one callback batch of read-ahead per flow. The former
	// SequenceBufferSize queue reserved a full send window again even though
	// the socket reader already enforces that window. One batch preserves the
	// measured socket-read/delivery overlap while a stalled callback still
	// applies bounded backpressure to its own flow.
	readQueueSize := min(
		self.tcpBufferSettings.SequenceBufferSize,
		max(1, self.tcpBufferSettings.WriteBatchSize),
	)
	readPackets := make(chan []byte, readQueueSize)
	go HandleError(func() {
		defer self.cancel()

		defer func() {
			// drain to the close so that ordered data and any final
			// fin/rst reach the source on teardown. the socket read side
			// always closes `readPackets` on exit, which is unblocked by
			// the deferred socket close in `Run`
			for packet := range readPackets {
				receive(packet)
			}
		}()

		// reused across drains; receiveBatch consumes it before the next
		// drain reuses it
		batch := make([][]byte, 0, self.tcpBufferSettings.WriteBatchSize)

	read:
		for {
			select {
			case <-self.ctx.Done():
				return
			case packet, ok := <-readPackets:
				if !ok {
					return
				}
				batch = append(batch[:0], packet)
				// opportunistically drain queued packets into one batch to
				// reduce wakeups and coalesce the return-path wire Pack
				for len(batch) < cap(batch) {
					select {
					case packet, ok := <-readPackets:
						if !ok {
							receiveBatch(batch)
							return
						}
						batch = append(batch, packet)
					default:
						receiveBatch(batch)
						continue read
					}
				}
				receiveBatch(batch)
			}
		}
	}, self.cancel)

	go HandleError(func() {
		fin := false
		defer func() {
			// close without cancel so that the receive pipeline drains all
			// queued packets before the sequence cancels.
			// the receive pipeline cancels after the drain.
			if !fin {
				var packet []byte
				var err error
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					packet, err = self.RstAck()
				}()
				if err == nil {
					select {
					case readPackets <- packet:
						fin = true
					}
				}
			}

			close(readPackets)
		}()

		buffer := make([]byte, self.tcpBufferSettings.ReadBufferByteCount)

		for forwardIter := uint64(0); ; forwardIter += 1 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			readTimeout := time.Now().Add(self.tcpBufferSettings.ReadTimeout)
			socket.SetReadDeadline(readTimeout)

			n, err := socket.Read(buffer)

			if err != nil {
				if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]tcp receive error = %s\n", forwardIter, err)
				}
			}

			if 0 < n {
				self.UpdateLastActivityTime()

				// since the transfer from local to remove is lossless and preserves order,
				// do not worry about retransmits.
				// packetize and emit one window-sized chunk at a time, so that a
				// read larger than the receive window cannot stall. each chunk
				// must be emitted before waiting for window room for the next
				// chunk, since the window only opens as the source acks
				// emitted data.
				stop := false
				packetCount := 0
				for i := 0; i < n && !stop; {
					var chunkPackets [][]byte
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						for {
							select {
							case <-self.ctx.Done():
								stop = true
								return
							default:
							}

							windowByteCount := int(int64(self.receiveWindowSize) - int64(self.receiveSeq-self.receiveSeqAck))
							if 0 < windowByteCount {
								j := min(i+windowByteCount, n)
								var err error
								chunkPackets, err = self.DataPackets(buffer[i:j], j-i, self.tcpBufferSettings.Mtu)
								if err != nil {
									self.log.Infof("[f%d]tcp receive packets error = %s\n", forwardIter, err)
									stop = true
									return
								}
								self.receiveSeq += uint32(j - i)
								ackedSendSeq = self.sendSeq
								i = j
								return
							}

							if self.log.V(2).Enabled() {
								self.log.Infof("[f%d]tcp receive window wait\n", forwardIter)
							}
							receiveAckCond.Wait()
						}
					}()
					for _, packet := range chunkPackets {
						if stop {
							MessagePoolReturn(packet)
						} else {
							select {
							case <-self.ctx.Done():
								MessagePoolReturn(packet)
								stop = true
							case readPackets <- packet:
								packetCount += 1
							}
						}
					}
				}
				if stop {
					return
				}

				if 1 < packetCount {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]tcp receive segmented packets %d\n", forwardIter, packetCount)
					}
				}
				if self.log.V(2).Enabled() {
					self.log.Infof("[f%d]tcp receive %d %d\n", forwardIter, n, packetCount)
				}
			}

			if err != nil {
				if err == io.EOF {
					// closed (FIN)
					// propagate the FIN and close the sequence
					self.log.V(2).Infof("[final]FIN\n")
					var finPacket []byte
					var finErr error
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						finPacket, finErr = self.FinAck()
						self.receiveSeq += 1
					}()
					if finErr == nil {
						select {
						case <-self.ctx.Done():
							MessagePoolReturn(finPacket)
						case readPackets <- finPacket:
							fin = true
						}
					}
					return
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]timeout\n", forwardIter)
					}
					return
				} else {
					// some other error
					return
				}
			}
		}
	}, self.cancel)

	go HandleError(func() {
		defer self.cancel()

		// reusable ack-compress timer (avoids a per-iteration time.After alloc
		// on the hot ack coalescing path)
		ackCompressTimer := time.NewTimer(0)
		defer ackCompressTimer.Stop()

		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			var packet []byte
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				select {
				case <-self.ctx.Done():
					return
				default:
				}

				for self.sendSeq == ackedSendSeq {
					ackCond.Wait()
					select {
					case <-self.ctx.Done():
						return
					default:
					}
				}

				var err error
				packet, err = self.PureAck()
				if err != nil {
					self.log.Infof("[r]ack err = %s\n", err)
				}
				ackedSendSeq = self.sendSeq
			}()
			if packet == nil {
				return
			}

			select {
			case <-self.ctx.Done():
				return
			default:
			}

			receive(packet)

			if 0 < self.tcpBufferSettings.AckCompressTimeout {
				// coalesce acks up to the timeout.
				// the send loop signals to ack sooner when the unacked byte
				// count reaches half the window, so the source never stalls
				// on a full window waiting for the timeout.
				ackCompressTimer.Reset(self.tcpBufferSettings.AckCompressTimeout)
				select {
				case <-ackCompressTimer.C:
				case <-ackSignal:
				case <-self.ctx.Done():
					return
				}
			}
		}
	}, self.cancel)

	// window scaling depends on `nonBlockingByteCount` and `blockingByteCount` per `self.windowSize`
	nonBlockingByteCount := uint32(0)
	blockingByteCount := uint32(0)
	fin := false
	sendIter := uint64(0)
	// returns false when the send loop must stop:
	// rst or cancel with `fin` false, or fin flush with `fin` true
	handleSendItem := func(sendItem *TcpSendItem) bool {
		if self.log.V(2).Enabled() {
			if "ACK" != sendItem.tcp.flagsString() {
				self.log.Infof("[r%d]receive(%d %s)\n", sendIter, len(sendItem.tcp.payload), sendItem.tcp.flagsString())
			}
		}

		if sendItem.tcp.rst {
			// a RST typically appears for a bad TCP segment
			if self.log.V(2).Enabled() {
				self.log.Infof("[r%d]RST\n", sendIter)
			}
			MessagePoolReturn(sendItem.ipPacket)
			// FIXME
			return false
			// continue
		}

		drop := false
		// seq := uint32(0)

		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			var receiveAckUpdated bool
			drop, receiveAckUpdated = self.applySendItemWithLock(&sendItem.tcp)
			if receiveAckUpdated {
				receiveAckCond.Broadcast()
			}
		}()

		if drop {
			MessagePoolReturn(sendItem.ipPacket)
			return true
		}

		if sendItem.tcp.fin {
			if self.log.V(2).Enabled() {
				self.log.Infof("[r%d]FIN\n", sendIter)
			}
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				self.sendSeq += 1
				ackCond.Broadcast()
			}()
		}

		payload := sendItem.tcp.payload
		if 0 < len(payload) {
			// seq += uint32(len(payload))
			ipHeaderByteCount := Ipv6HeaderSize
			if sendItem.ipPacket[0]>>4 == 4 {
				ipHeaderByteCount = int(sendItem.ipPacket[0]&0xf) * 4
			}
			writePayload := writePayload{
				sendIter:         sendIter,
				ipPacket:         sendItem.ipPacket,
				payloadOffset:    uint16(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions + len(sendItem.tcp.options)),
				payloadByteCount: uint16(len(payload)),
			}
			// FIXME count the number of non-blocking versus blocking channel adds
			// FIXME every window size, check the count:
			// FIXME - if 0 blocking, double window size
			// FIXME - if >half blocking, half the window size
			// FIXME else leave the window size unchanged
			select {
			case writePayloads <- writePayload:
				nonBlockingByteCount += uint32(len(payload))
			default:
				select {
				case writePayloads <- writePayload:
					blockingByteCount += uint32(len(payload))
				case <-self.ctx.Done():
					MessagePoolReturn(sendItem.ipPacket)
					return false
				}
			}
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				// self.log.Infof("[r%d]eval window size (%d, %d, %d)\n", sendIter, self.windowSize, nonBlockingByteCount, blockingByteCount)
				if self.windowSize <= blockingByteCount+nonBlockingByteCount {
					if self.windowSize <= nonBlockingByteCount {
						nextWindowSize := min(self.windowSize*2, self.tcpBufferSettings.MaxWindowSize)
						if self.windowSize != nextWindowSize {
							if self.log.V(1).Enabled() {
								self.log.Infof("[r%d]increase window size %d -> %d\n", sendIter, self.windowSize, nextWindowSize)
							}
							self.windowSize = nextWindowSize
						}
					} else if self.windowSize/2 <= blockingByteCount {
						nextWindowSize := max(self.windowSize/2, self.tcpBufferSettings.MinWindowSize)
						if self.windowSize != nextWindowSize {
							if self.log.V(1).Enabled() {
								self.log.Infof("[r%d]decrease window size %d -> %d\n", sendIter, self.windowSize, nextWindowSize)
							}
							self.windowSize = nextWindowSize
						}
					}
					// else no change to the window
					// reset the stats
					nonBlockingByteCount = uint32(0)
					blockingByteCount = uint32(0)
				}

				self.sendSeq += uint32(len(payload))
				ackCond.Broadcast()
				if self.windowSize/2 <= self.sendSeq-ackedSendSeq {
					select {
					case ackSignal <- struct{}{}:
					default:
					}
				}
			}()
		} else {
			MessagePoolReturn(sendItem.ipPacket)
		}

		// if 0 < seq {
		// 	func() {
		// 		self.mutex.Lock()
		// 		defer self.mutex.Unlock()

		// 		self.sendSeq += seq
		// 		ackCond.Broadcast()
		// 	}()
		// }

		if sendItem.tcp.fin {
			// flush the write channel to propage the FIN and close the sequence
			close(writePayloads)
			fin = true
			return false
		}

		return true
	}

send:
	for {
		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.tcpBufferSettings.IdleTimeout)
		select {
		case <-self.ctx.Done():
			idleTimer.Stop()
			return
		case sendItem := <-self.sendItems:
			idleTimer.Stop()
			if !handleSendItem(sendItem) {
				if !fin {
					return
				}
				break send
			}
			sendIter += 1
			// opportunistically drain queued send items to reduce wakeups
			for {
				select {
				case sendItem := <-self.sendItems:
					if !handleSendItem(sendItem) {
						if !fin {
							return
						}
						break send
					}
					sendIter += 1
				default:
					continue send
				}
			}
		case <-idleTimer.C:
			done := false
			func() {
				self.sendMutex.Lock()
				defer self.sendMutex.Unlock()
				if self.idleCondition.Close(checkpointId) {
					// close the sequence
					done = true
				}
			}()
			if done {
				// close the sequence
				if self.log.V(2).Enabled() {
					self.log.Infof("[r%d]timeout\n", sendIter)
				}
				return
			}
			// else there pending updates
		}
	}

	// wait for `writePayloads` to finish
	select {
	case <-self.ctx.Done():
	}
}

// applySendItemWithLock updates the return-path acknowledgment/window and
// reports whether an established packet's sequence-bearing portion is in
// order. The caller holds mutex.
func (self *TcpSequence) applySendItemWithLock(tcp *parsedTcp) (drop bool, receiveAckUpdated bool) {
	if tcp.ack &&
		0 <= int32(tcp.ackNumber-self.receiveSeqAck) &&
		0 <= int32(self.receiveSeq-tcp.ackNumber) {
		// ACK generation and upload packetization may run concurrently in the
		// source TCP stack. A pure ACK can therefore arrive with a sequence
		// just ahead of or behind upload payload already delivered here. Its
		// ACK field is independent and must still reopen the return window.
		// Bound it by receiveSeq so a corrupt/future ACK cannot acknowledge
		// bytes this sequence has not emitted.
		if !self.receiveWindowEndSet {
			self.receiveWindowEnd = self.receiveSeqAck + self.receiveWindowSize
			self.receiveWindowEndSet = true
		}

		// A TCP receiver never shrinks the right edge it has already
		// advertised. ACK generation and application reads can produce
		// duplicate window updates concurrently, so their callback order is
		// not a reliable chronology. Preserve the greatest advertised edge:
		// this accepts zero-window closure (the ACK advances to the existing
		// edge), accepts a later reopen (the edge grows), and ignores a stale
		// smaller-window duplicate that arrives after the reopen.
		receiveWindowEnd := tcp.ackNumber + (uint32(tcp.windowSize) << self.receiveWindowScale)
		if 0 < int32(receiveWindowEnd-self.receiveWindowEnd) {
			self.receiveWindowEnd = receiveWindowEnd
		}

		receiveSeqAck := tcp.ackNumber
		receiveWindowSize := uint32(0)
		if 0 < int32(self.receiveWindowEnd-receiveSeqAck) {
			receiveWindowSize = self.receiveWindowEnd - receiveSeqAck
		}
		if self.receiveSeqAck != receiveSeqAck || self.receiveWindowSize != receiveWindowSize {
			self.receiveSeqAck = receiveSeqAck
			self.receiveWindowSize = receiveWindowSize
			receiveAckUpdated = true
		}
	}

	// Pure ACKs consume no sequence space and tolerate the full-duplex
	// scheduling described above. Payload, FIN, and an unexpected established
	// SYN remain strictly in order because the user-NAT does not reorder or
	// retransmit that direction.
	if tcp.syn || (0 < len(tcp.payload) || tcp.fin) && int32(tcp.seq-self.sendSeq) != 0 {
		drop = true
	}
	return
}

func (self *TcpSequence) Cancel() {
	self.cancel()
}

func (self *TcpSequence) Close() {
	self.cancel()
}

type TcpSendItem struct {
	provideMode protocol.ProvideMode
	tcp         parsedTcp
	ipPacket    []byte
}

type ConnectionState struct {
	source          TransferPath
	provideMode     protocol.ProvideMode
	ipVersion       int
	sourceIp        net.IP
	sourcePort      uint16
	destinationIp   net.IP
	destinationPort uint16

	mutex sync.Mutex

	sendSeq             uint32
	receiveSeq          uint32
	receiveSeqAck       uint32
	receiveWindowSize   uint32
	receiveWindowEnd    uint32
	receiveWindowEndSet bool
	receiveWindowScale  uint32
	enableWindowScale   bool
	windowSize          uint32
	windowScale         uint32
	// encodedWindowSize  uint16

	// cached immutable ip path for this connection (see IpPath). primed at
	// construction, before the sequence goroutines start, so it is written
	// once and then read-only
	ipPath *IpPath

	// reusable backing for the common single-packet DataPackets result.
	// DataPackets is called from one goroutine and its result is consumed
	// before the next call, so the backing can be reused; segmented payloads
	// allocate a fresh slice.
	singleDataPacket [1][]byte

	userLimited
}

// IpPath returns the immutable ip path for this connection. The path is
// primed at construction and cached; the connection identity (version, ips,
// ports) never changes. A zero-value state (tests) builds a fresh uncached
// path, since the sequence goroutines may race a lazy write.
func (self *ConnectionState) IpPath() *IpPath {
	if self.ipPath != nil {
		return self.ipPath
	}
	return &IpPath{
		Version:         self.ipVersion,
		Protocol:        IpProtocolTcp,
		SourceIp:        self.sourceIp,
		SourcePort:      int(self.sourcePort),
		DestinationIp:   self.destinationIp,
		DestinationPort: int(self.destinationPort),
	}
}

func (self *ConnectionState) encodedWindowSize() uint16 {
	return uint16(min(
		uint32(self.windowSize>>self.windowScale),
		uint32(math.MaxUint16),
	))
}

// SynAck builds the syn-ack for the connect handshake into a single pool
// buffer, advertising the mss and, when enabled, the window scale.
func (self *ConnectionState) SynAck(mtu int) ([]byte, error) {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	// mss (kind 2, length 4) plus window scale (kind 3, length 3) when
	// enabled, zero padded to a 4 byte header word boundary
	optionsByteCount := 4
	if self.enableWindowScale {
		optionsByteCount += 3
	}
	paddedOptionsByteCount := (optionsByteCount + 3) &^ 3

	tcpHeaderByteCount := TcpHeaderSizeWithoutExtensions + paddedOptionsByteCount
	packet := MessagePoolGet(ipHeaderByteCount + tcpHeaderByteCount)
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, ipProtocolNumberTcp, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, ipProtocolNumberTcp, self.destinationIp, self.sourceIp)
	}

	tcp := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(tcp[0:2], uint16(self.destinationPort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(self.sourcePort))
	binary.BigEndian.PutUint32(tcp[4:8], self.receiveSeq)
	binary.BigEndian.PutUint32(tcp[8:12], self.sendSeq)
	tcp[12] = byte(tcpHeaderByteCount/4) << 4
	tcp[13] = tcpFlagSyn | tcpFlagAck
	binary.BigEndian.PutUint16(tcp[14:16], self.encodedWindowSize())
	// checksum, set below
	tcp[16] = 0
	tcp[17] = 0
	// urgent
	tcp[18] = 0
	tcp[19] = 0
	options := tcp[TcpHeaderSizeWithoutExtensions:]
	clear(options)
	// advertise the mss so the source does not segment to a conservative default
	options[0] = 2
	options[1] = 4
	binary.BigEndian.PutUint16(options[2:4], uint16(mtu-ipHeaderByteCount-TcpHeaderSizeWithoutExtensions))
	if self.enableWindowScale {
		options[4] = 3
		options[5] = 3
		options[6] = byte(self.windowScale)
	}
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(ipProtocolNumberTcp, self.destinationIp, self.sourceIp, tcp))
	return packet, nil
}

func (self *ConnectionState) PureAck() ([]byte, error) {
	return self.tcpPacket(tcpFlagAck, self.receiveSeq, nil), nil
}

func (self *ConnectionState) FinAck() ([]byte, error) {
	return self.tcpPacket(tcpFlagAck|tcpFlagFin, self.receiveSeq, nil), nil
}

func (self *ConnectionState) RstAck() ([]byte, error) {
	return self.tcpPacket(tcpFlagAck|tcpFlagRst, self.receiveSeq, nil), nil
}

func (self *ConnectionState) DataPackets(payload []byte, n int, mtu int) ([][]byte, error) {
	var headerByteCount int
	switch self.ipVersion {
	case 4:
		headerByteCount = Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions
	case 6:
		headerByteCount = Ipv6HeaderSize + TcpHeaderSizeWithoutExtensions
	}

	packetByteCount := mtu - headerByteCount
	if n <= packetByteCount {
		// reuse the single-packet backing for the common unsegmented case
		// (see singleDataPacket); the result is consumed before the next call
		self.singleDataPacket[0] = self.tcpPacket(tcpFlagAck, self.receiveSeq, payload[0:n])
		return self.singleDataPacket[:], nil
	}
	// segment
	packets := make([][]byte, 0, (n+packetByteCount-1)/packetByteCount)
	for i := 0; i < n; {
		j := min(i+packetByteCount, n)
		packets = append(packets, self.tcpPacket(tcpFlagAck, self.receiveSeq+uint32(i), payload[i:j]))
		i = j
	}
	return packets, nil
}

// builds a tcp packet from the stream destination to the stream source
// into a single pool buffer. the ack number is always set.
func (self *ConnectionState) tcpPacket(flags byte, seq uint32, payload []byte) []byte {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions + len(payload))
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, ipProtocolNumberTcp, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, ipProtocolNumberTcp, self.destinationIp, self.sourceIp)
	}

	tcp := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(tcp[0:2], uint16(self.destinationPort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(self.sourcePort))
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], self.sendSeq)
	// data offset, no options
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], self.encodedWindowSize())
	// checksum, set below
	tcp[16] = 0
	tcp[17] = 0
	// urgent
	tcp[18] = 0
	tcp[19] = 0
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(ipProtocolNumberTcp, self.destinationIp, self.sourceIp, tcp))
	return packet
}

func DefaultRemoteUserNatProviderSettings() *RemoteUserNatProviderSettings {
	return DefaultRemoteUserNatProviderSettingsWithMemoryTarget(0)
}

// DefaultRemoteUserNatProviderSettingsWithMemoryTarget sizes the provider
// wrapper from the owner's provider memory target. 0 keeps the legacy
// process-budget-scaled bounds.
func DefaultRemoteUserNatProviderSettingsWithMemoryTarget(targetByteCount ByteCount) *RemoteUserNatProviderSettings {
	// bounds the per-source return provide mode map (see
	// `recordSourceProvideMode`): derived from the provider memory target
	// when set (~1 KiB of target per tracked source), else scaled by the
	// process memory budget
	maxSourceCount := MemoryScaledCount(8192, 1024)
	if 0 < targetByteCount {
		maxSourceCount = max(1024, int(targetByteCount/kib(1)))
	}
	return &RemoteUserNatProviderSettings{
		WriteTimeout:            30 * time.Second,
		ProtocolVersion:         DefaultProtocolVersion,
		SecurityPolicyGenerator: DefaultProviderSecurityPolicyWithStats,
		EventEpoch:              1 * time.Second,
		MaxSourceCount:          maxSourceCount,
		IngressDispatchTimeout:  5 * time.Millisecond,
	}
}

type RemoteUserNatProviderSettings struct {
	WriteTimeout time.Duration

	ProtocolVersion int

	SecurityPolicyGenerator func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy

	// epoch to flush packet stats events to listeners
	EventEpoch time.Duration

	// the maximum number of sources tracked for return provide modes.
	// 0 is no limit.
	MaxSourceCount int

	// IngressDispatchTimeout bounds how long ClientReceive waits for a full
	// nat dispatch channel before dropping a pack's packets. Bounded
	// backpressure (not drop-on-full): a burst that instantaneously fills
	// the dispatch would otherwise discard whole packs, corrupting per-flow
	// tcp state (the nat implements no retransmit toward the socket) and
	// stalling flows to their deadlines — measured as a run-collapse under
	// burst. The wait runs on the per-source receive sequence goroutine, so
	// one source's burst delays only its own receive processing. 0 restores
	// drop-on-full.
	IngressDispatchTimeout time.Duration
}

type RemoteUserNatProvider struct {
	ctx               context.Context
	client            *Client
	cancel            context.CancelFunc
	localUserNat      *LocalUserNat
	securityPolicy    SecurityPolicy
	settings          *RemoteUserNatProviderSettings
	localUserNatUnsub func()
	clientUnsub       func()

	// cumulative packet counts relayed for remote clients, in the same
	// direction convention as the contracts: ingress is traffic received from
	// the tunnel (remote clients' egress), egress is the return into the tunnel
	packetStatsCounters  *packetStatsCounters
	packetStatsCallbacks *CallbackList[PacketStatsFunction]

	// the return provide mode recorded per source (see recordSourceProvideMode),
	// so the return path can echo it. A source on the same network sends under
	// ProvideMode_Network and its return traffic should also be network mode,
	// which skips the public security rules and forgoes the companion contract.
	stateLock         sync.Mutex
	sourceProvideMode map[Id]protocol.ProvideMode
	// sourceP2pPriorityRefresh rate-limits Network-peer admission promotion
	// on the provider packet path. Entries are bounded with
	// sourceProvideMode and removed on the same arbitrary safe eviction.
	sourceP2pPriorityRefresh map[Id]time.Time
	// the packet stats epoch worker started (on the first callback)
	packetStatsStarted bool
}

func NewRemoteUserNatProviderWithDefaults(
	client *Client,
	localUserNat *LocalUserNat,
) *RemoteUserNatProvider {
	return NewRemoteUserNatProvider(client, localUserNat, DefaultRemoteUserNatProviderSettings())
}

func NewRemoteUserNatProvider(
	client *Client,
	localUserNat *LocalUserNat,
	settings *RemoteUserNatProviderSettings,
) *RemoteUserNatProvider {
	// the security policy runs a background scan goroutine; scope it to this provider (a child of
	// the client ctx) so Close stops it, rather than leaking it for the life of the client
	cancelCtx, cancel := context.WithCancel(client.Ctx())
	userNatProvider := &RemoteUserNatProvider{
		ctx:                      cancelCtx,
		client:                   client,
		cancel:                   cancel,
		localUserNat:             localUserNat,
		securityPolicy:           settings.SecurityPolicyGenerator(cancelCtx, DefaultSecurityPolicyStatsCollector()),
		settings:                 settings,
		packetStatsCounters:      &packetStatsCounters{},
		packetStatsCallbacks:     NewCallbackList[PacketStatsFunction](),
		sourceProvideMode:        map[Id]protocol.ProvideMode{},
		sourceP2pPriorityRefresh: map[Id]time.Time{},
	}

	// Register both return paths. No-contract peers can take the NAT's drained
	// batch directly; contract-bearing peers are fanned back to Receive so the
	// transfer sequence can leave contract heads unbatched and only coalesce
	// already-queued frames when the current contract has room. Synthesized
	// control packets always arrive through the per-packet callback.
	localUserNatBatchUnsub := localUserNat.AddReceivePacketsCallback(userNatProvider.ReceiveBatch)
	localUserNatPacketUnsub := localUserNat.AddReceivePacketCallback(userNatProvider.Receive)
	localUserNatUnsub := func() {
		localUserNatBatchUnsub()
		localUserNatPacketUnsub()
	}
	userNatProvider.localUserNatUnsub = localUserNatUnsub
	clientUnsub := client.AddReceiveCallback(userNatProvider.ClientReceive)
	userNatProvider.clientUnsub = clientUnsub

	return userNatProvider
}

func (self *RemoteUserNatProvider) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

// PacketStats returns the cumulative packet counts relayed for remote clients.
// remote ingress is traffic received from the tunnel (remote clients' egress),
// remote egress is the return traffic sent back into the tunnel, and blocked is
// the traffic dropped by the provider security policy
func (self *RemoteUserNatProvider) PacketStats() *PacketStats {
	return self.packetStatsCounters.snapshot()
}

// AddPacketStatsCallback registers a listener fired on the event epoch when the
// stats change. the epoch worker starts on the first callback
func (self *RemoteUserNatProvider) AddPacketStatsCallback(packetStatsCallback PacketStatsFunction) func() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.packetStatsStarted {
			self.packetStatsStarted = true
			go HandleError(self.runPacketStats, self.cancel)
		}
	}()
	callbackId := self.packetStatsCallbacks.Add(packetStatsCallback)
	return func() {
		self.packetStatsCallbacks.Remove(callbackId)
	}
}

// flushes packet stats events to listeners on the event epoch
func (self *RemoteUserNatProvider) runPacketStats() {
	lastPacketStats := PacketStats{}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.EventEpoch):
		}

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

// recordSourceProvideMode remembers the provide mode to echo on a source's
// return path. ProvideMode is a set of flags, not an ordered scale, so this is a
// per-case choice, never a numeric min: prefer the same-Network relationship once
// a source has used it — its Network contract is verified, so the return traffic
// can ride the network relationship and skip the public ingress rules — otherwise
// remember the source's (non-Network) mode so the echo rides a companion contract.
func (self *RemoteUserNatProvider) recordSourceProvideMode(sourceId Id, provideMode protocol.ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch existing, ok := self.sourceProvideMode[sourceId]; {
	case !ok:
		// bound the map: evict an arbitrary entry at the cap. Eviction is
		// safe — the return path falls back to the packet's carried provide
		// mode (see `sourceReturnProvideMode`), and an active source
		// re-records on its next inbound packet.
		if maxCount := self.settings.MaxSourceCount; 0 < maxCount && maxCount <= len(self.sourceProvideMode) {
			for evictSourceId := range self.sourceProvideMode {
				delete(self.sourceProvideMode, evictSourceId)
				delete(self.sourceP2pPriorityRefresh, evictSourceId)
				break
			}
		}
		self.sourceProvideMode[sourceId] = provideMode
	case existing == protocol.ProvideMode_Network:
		// already network; keep it
	case provideMode == protocol.ProvideMode_Network:
		self.sourceProvideMode[sourceId] = protocol.ProvideMode_Network
	default:
		self.sourceProvideMode[sourceId] = provideMode
	}
}

const providerP2pPriorityRefreshInterval = 5 * time.Second

// refreshP2pPriority promotes an explicitly selected same-network source out
// of the provider's bounded public-peer admission lottery. The provider sees
// the relationship on relayed data before P2P is established, so it can
// reclaim one existing reservation without requiring new StreamOpen protocol
// fields or increasing the fixed memory ceiling.
func (self *RemoteUserNatProvider) refreshP2pPriority(sourceId Id, provideMode protocol.ProvideMode) {
	if provideMode != protocol.ProvideMode_Network || sourceId == (Id{}) {
		return
	}
	now := time.Now()
	shouldRefresh := false
	self.stateLock.Lock()
	if self.sourceP2pPriorityRefresh == nil {
		self.sourceP2pPriorityRefresh = map[Id]time.Time{}
	}
	if _, ok := self.sourceP2pPriorityRefresh[sourceId]; !ok {
		if maxCount := self.settings.MaxSourceCount; 0 < maxCount && maxCount <= len(self.sourceP2pPriorityRefresh) {
			for evictSourceId := range self.sourceP2pPriorityRefresh {
				delete(self.sourceP2pPriorityRefresh, evictSourceId)
				break
			}
		}
	}
	if next := self.sourceP2pPriorityRefresh[sourceId]; !now.Before(next) {
		self.sourceP2pPriorityRefresh[sourceId] = now.Add(providerP2pPriorityRefreshInterval)
		shouldRefresh = true
	}
	self.stateLock.Unlock()
	if shouldRefresh {
		self.client.webRtcManager.PrioritizePeer(sourceId)
	}
}

// sourceReturnProvideMode returns the recorded return provide mode for a source
// (see recordSourceProvideMode), falling back to the provide mode of the current
// return packet (carried back through the local nat conntrack) if the source is
// not yet tracked
func (self *RemoteUserNatProvider) sourceReturnProvideMode(sourceId Id, fallback protocol.ProvideMode) protocol.ProvideMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if provideMode, ok := self.sourceProvideMode[sourceId]; ok {
		return provideMode
	}
	return fallback
}

// providerReturnTransferOption keeps every provider-to-client frame on the
// same sender sequence as the rest of that relationship. Network-peer data
// and active WebRTC signaling use ForceStream. ForceStream is part of the
// sender's sequence key but is not carried on the wire, so returning Network
// traffic with the default option would fork a second live sequence that the
// receiver cannot distinguish from the first. Whichever sequence id arrived
// second would supersede the other and could leave its acknowledged traffic
// permanently dropped as "older". Non-Network returns continue to ride the
// companion contract that the remote source grants.
func providerReturnTransferOptions(
	defaultOptions TransferOptions,
	provideMode protocol.ProvideMode,
) TransferOptions {
	if provideMode == protocol.ProvideMode_Network {
		defaultOptions.CompanionContract = false
		defaultOptions.ForceStream = true
		defaultOptions.NetworkPeer = true
	} else {
		defaultOptions.CompanionContract = true
		defaultOptions.ForceStream = false
		defaultOptions.NetworkPeer = false
	}
	return defaultOptions
}

// `ReceivePacketFunction`
// providerReturnBatchMaxFrames / providerReturnBatchMaxBytes bound one
// coalesced return Pack so the complete transfer frame stays below the
// platform and resident transport's 4 KiB message limit. Two mtu packets
// plus the Pack/TransferFrame envelope fit; three do not.
const providerReturnBatchMaxFrames = sendPackBatchMaxFrames
const providerReturnBatchMaxBytes = sendPackBatchMaxMessageByteCount

// `ReceivePacketsFunction`
// ReceiveBatch coalesces a flow's drained packet batch into one wire Pack
// (chunked to the frame/byte bounds), collapsing the per-packet
// route/transport handoffs to one per chunk. All packets share the flow's
// source/ipPath, so the egress policy is evaluated once. Ownership mirrors
// the per-packet Receive: each packet is shared read-only into a frame; the
// share/marshal buffers are freed on the same raw/wrapped rules.
func (self *RemoteUserNatProvider) ReceiveBatch(
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packets [][]byte,
) {
	if len(packets) == 0 {
		return
	}
	if self.client.ClientId() == source.SourceId {
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof("drop remote user nat provider s packet ->%s\n", source.SourceId)
		}
		return
	}
	// A contract frame is attached to a sequence head and at contract
	// boundaries. This layer cannot see that overhead, so only pre-batch when
	// the peer is explicitly no-contract. Contract-bearing traffic returns to
	// the per-packet path; SendSequence performs envelope-safe coalescing once
	// a contract is established and an earlier item is still outstanding.
	if !self.client.ContractManager().SendNoContract(source.SourceId) {
		for _, packet := range packets {
			self.Receive(source, provideMode, ipPath, packet)
		}
		return
	}
	// flow-level egress policy (ipPath is constant across the batch)
	r, err := self.securityPolicy.InspectEgress(provideMode, ipPath, nil)
	if err != nil {
		return
	}
	self.securityPolicy.RefreshEgress(ipPath)
	if r != SecurityPolicyResultAllow {
		var blockedBytes int64
		for _, packet := range packets {
			blockedBytes += int64(len(packet))
		}
		self.packetStatsCounters.blockEgressPacketCount.Add(int64(len(packets)))
		self.packetStatsCounters.blockEgressByteCount.Add(blockedBytes)
		return
	}

	returnProvideMode := self.sourceReturnProvideMode(source.SourceId, provideMode)
	returnOption := providerReturnTransferOptions(
		self.client.settings.DefaultTransferOpts,
		returnProvideMode,
	)
	destination := source.Reverse()

	// build frames, flushing a chunk when the frame/byte bound is hit
	frames := make([]*protocol.Frame, 0, providerReturnBatchMaxFrames)
	wrappedShares := make([][]byte, 0, providerReturnBatchMaxFrames)
	var chunkBytes int64
	var chunkPacketBytes int64

	flush := func() {
		if len(frames) == 0 {
			return
		}
		packetBytes := chunkPacketBytes
		frameCount := len(frames)
		// the pack references the slice asynchronously until the send
		// sequence marshals it — hand it ownership and start a fresh slice
		// for the next chunk (never reuse the backing array)
		sendFrames := frames
		frames = make([]*protocol.Frame, 0, providerReturnBatchMaxFrames)
		sent := self.client.SendMultiWithTimeout(
			sendFrames,
			destination,
			func(err error) {},
			self.settings.WriteTimeout,
			returnOption,
		)
		if sent {
			self.packetStatsCounters.remoteEgressPacketCount.Add(int64(frameCount))
			self.packetStatsCounters.remoteEgressByteCount.Add(packetBytes)
		} else {
			// the send did not take the frames: free their message bytes
			// (raw shares or wrapped marshal buffers)
			for _, frame := range sendFrames {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
		// wrapped frames carry a separate share buffer, freed unconditionally
		// (mirrors the per-packet `!Raw` defer); not referenced by the pack,
		// so the local slice can be reused
		for _, share := range wrappedShares {
			MessagePoolReturn(share)
		}
		wrappedShares = wrappedShares[:0]
		chunkBytes = 0
		chunkPacketBytes = 0
	}

	for _, packet := range packets {
		packetShare := MessagePoolShareReadOnly(packet)
		frame, err := ipPacketFromProviderFrame(packetShare, self.settings.ProtocolVersion)
		if err != nil {
			MessagePoolReturn(packetShare)
			if self.client.log.V(2).Enabled() {
				self.client.log.Infof("drop remote user nat provider s packet ->%s = %s\n", source.SourceId, err)
			}
			panic(err)
		}
		if !frame.Raw {
			wrappedShares = append(wrappedShares, packetShare)
		}
		frames = append(frames, frame)
		chunkBytes += int64(len(frame.MessageBytes))
		chunkPacketBytes += int64(len(packet))
		if providerReturnBatchMaxFrames <= len(frames) || providerReturnBatchMaxBytes <= chunkBytes {
			flush()
		}
	}
	flush()
}

func (self *RemoteUserNatProvider) Receive(
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	// self.client.log.Infof("[trace]provider return packet for %s\n", source.SourceId)

	if self.client.ClientId() == source.SourceId {
		// locally generated traffic should use a separate local user nat
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof("drop remote user nat provider s packet ->%s\n", source.SourceId)
		}
		return
	}

	// the provider's egress is the return into the tunnel (destination->client); the reversed
	// provider policy applies the client-ingress source check here, then refreshes the flow so an
	// active download isn't reclaimed while the outbound side is quiet
	r, err := self.securityPolicy.InspectEgress(provideMode, ipPath, nil)
	if err != nil {
		return
	}
	self.securityPolicy.RefreshEgress(ipPath)
	if r != SecurityPolicyResultAllow {
		self.packetStatsCounters.blockEgressPacketCount.Add(1)
		self.packetStatsCounters.blockEgressByteCount.Add(int64(len(packet)))
		return
	}

	packetShare := MessagePoolShareReadOnly(packet)

	// echo the recorded return provide mode for the source. A same-network source
	// sends under ProvideMode_Network; its return traffic is also network mode,
	// which uses the network relationship (no companion contract) so the device
	// receives it as network mode and skips the public ingress rules. Other
	// modes ride a companion contract (verified as Stream) as before.
	returnProvideMode := self.sourceReturnProvideMode(source.SourceId, provideMode)
	returnOption := providerReturnTransferOptions(
		self.client.settings.DefaultTransferOpts,
		returnProvideMode,
	)
	// note udp is sent with ack because because otherwise the delivery reliability will mulitply with the egress
	c := func() bool {
		var sent bool
		if 2 <= self.settings.ProtocolVersion {
			sent, _ = self.client.sendRawWithTimeoutDetailed(
				protocol.MessageType_IpIpPacketFromProvider,
				packetShare,
				source.Reverse(),
				nil,
				0,
				self.settings.WriteTimeout,
				returnOption,
			)
		} else {
			frame, err := ipPacketFromProviderFrame(packetShare, self.settings.ProtocolVersion)
			if err != nil {
				MessagePoolReturn(packetShare)
				if self.client.log.V(2).Enabled() {
					self.client.log.Infof("drop remote user nat provider s packet ->%s = %s\n", source.SourceId, err)
				}
				panic(err)
			}
			// Legacy marshal bytes are owned by the send. The packet share is
			// independent and is released after the synchronous enqueue.
			sent = self.client.SendWithTimeout(
				frame,
				source.Reverse(),
				func(err error) {},
				self.settings.WriteTimeout,
				returnOption,
			)
			MessagePoolReturn(packetShare)
			if !sent {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
		if sent {
			self.packetStatsCounters.remoteEgressPacketCount.Add(1)
			self.packetStatsCounters.remoteEgressByteCount.Add(int64(len(packet)))
		} else if 2 <= self.settings.ProtocolVersion {
			// the send did not take the frame: free it. For raw frames this undoes
			// the packet share above; for wrapped frames it frees the marshal buffer.
			MessagePoolReturn(packetShare)
		}
		// if sent {
		// 	self.client.log.Infof("[trace]provider return packet sent for %s\n", source.SourceId)
		// }
		return sent
	}
	if self.client.log.V(2).Enabled() {
		TraceWithReturn(
			fmt.Sprintf("[unps]%s %s->%s s(%s)", ipPath.Protocol, self.client.ClientTag(), source.SourceId, source.StreamId),
			c,
		)
	} else {
		c()
	}

}

// `connect.ReceiveFunction`
func (self *RemoteUserNatProvider) ClientReceive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	// receive functions should be non-blocking
	// clients should manage their own congestion protocols on top to avoid overflowing the sequence queues

	provideMode := peer.ProvideMode
	// One observation per delivered Pack is enough. The former call inside
	// every packet case serialized a large coalesced return batch on this
	// provider-global lock even though every frame has the same source and
	// relationship.
	self.recordSourceProvideMode(source.SourceId, provideMode)
	self.refreshP2pPriority(source.SourceId, provideMode)

	// collect the allowed packets and queue them into the local user nat as one batch
	var packets [][]byte
	var packetsByteCount ByteCount
	for _, frame := range frames {
		switch frame.MessageType {
		case protocol.MessageType_IpIpPing:
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[ip]provider ping <- %s(%d)\n", source, provideMode)
			}
			// Receive callback frames are borrowed and valid only until this
			// callback returns. The send queue is asynchronous, so forwarding
			// `frame` itself lets the decoded-frame pool reset/reuse it before
			// SendSequence serializes it. Besides corrupting the echo, that can
			// make contract accounting charge the original empty IpPing and
			// later acknowledge whatever larger frame reused the object,
			// terminating the whole send sequence with "Bad accounting".
			//
			// Keep the payload alive with a read-only share and give the send
			// its own frame object. IpPing is rare, so the one small object is
			// preferable to letting a callback-lifetime object escape.
			echoBytes := MessagePoolShareReadOnly(frame.MessageBytes)
			echoFrame := &protocol.Frame{
				MessageType:  frame.MessageType,
				MessageBytes: echoBytes,
				Raw:          frame.Raw,
			}
			// echo the recorded return provide mode for the source, like the
			// provider's other return traffic. A same-network source pings
			// under ProvideMode_Network; its echo is also network mode (no
			// companion contract). For other modes the source only provides
			// ProvideMode_Stream, so a forward contract would be rejected
			// (no permission); the echo rides a companion contract instead.
			returnProvideMode := self.sourceReturnProvideMode(source.SourceId, provideMode)
			if !self.client.SendWithTimeout(
				echoFrame,
				source.Reverse(),
				func(err error) {},
				0,
				providerReturnTransferOptions(
					self.client.settings.DefaultTransferOpts,
					returnProvideMode,
				),
			) {
				MessagePoolReturn(echoBytes)
			}
		case protocol.MessageType_IpIpPacketToProvider:
			packetBytes, err := ipPacketToProviderBytes(frame)
			if err != nil {
				panic(err)
			}

			var ipPath IpPath
			payload, err := parseIpPathWithPayloadBorrowed(packetBytes, &ipPath)
			if err == nil {
				// the provider's ingress is the remote client's egress (outbound, received from the
				// tunnel); the reversed provider policy applies the client-egress DPI here
				r, err := inspectAndRefreshIngressBorrowed(self.securityPolicy, provideMode, ipPath, payload)
				if err == nil {
					switch r {
					case SecurityPolicyResultAllow:
						var packet []byte
						if frame.Raw {
							packet = MessagePoolShareReadOnly(packetBytes)
						} else {
							packet = MessagePoolCopy(packetBytes)
						}
						packets = append(packets, packet)
						packetsByteCount += ByteCount(len(packet))
					default:
						// drop or incident: blocked by the provider security policy
						self.packetStatsCounters.blockIngressPacketCount.Add(1)
						self.packetStatsCounters.blockIngressByteCount.Add(int64(len(packetBytes)))
						if r == SecurityPolicyResultIncident {
							self.client.ReportAbuse(source)
						}
					}
				}
			}
		}
	}

	if 0 < len(packets) {
		c := func() bool {
			success := self.localUserNat.SendPacketsWithTimeout(
				source,
				provideMode,
				packets,
				// bounded backpressure for burst safety
				// (see `IngressDispatchTimeout`)
				self.settings.IngressDispatchTimeout,
			)
			if success {
				self.packetStatsCounters.remoteIngressPacketCount.Add(int64(len(packets)))
				self.packetStatsCounters.remoteIngressByteCount.Add(int64(packetsByteCount))
			} else {
				for _, packet := range packets {
					MessagePoolReturn(packet)
				}
			}
			return success
		}
		if self.client.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[unpr]%d %s<-%s s(%s)", len(packets), self.client.ClientTag(), source.SourceId, source.StreamId),
				c,
			)
		} else {
			c()
		}
	}
}

func (self *RemoteUserNatProvider) Close() {
	// self.client.RemoveReceiveCallback(self.clientCallbackId)
	// self.localUserNat.RemoveReceivePacketCallback(self.localUserNatCallbackId)
	self.cancel()
	self.clientUnsub()
	self.localUserNatUnsub()
}

// this is a basic implementation. See `RemoteUserNatWindowedClient` for a more robust implementation
type RemoteUserNatClient struct {
	client                *Client
	cancel                context.CancelFunc
	receivePacketCallback ReceivePacketFunction
	securityPolicy        SecurityPolicy
	pathTable             *pathTable
	// the provide mode of the source packets
	// for locally generated packets this is `ProvideMode_Network`
	provideMode       protocol.ProvideMode
	localUserNat      *LocalUserNat
	closeCallback     func()
	clientUnsub       func()
	localUserNatUnsub func()

	stateLock           sync.Mutex
	allowDirect         bool
	localSecurityBypass bool
}

func NewRemoteUserNatClient(
	client *Client,
	receivePacketCallback ReceivePacketFunction,
	destinations []MultiHopId,
	provideMode protocol.ProvideMode,
) *RemoteUserNatClient {
	return NewRemoteUserNatClientWithClose(client, receivePacketCallback, destinations, provideMode, nil)
}

func NewRemoteUserNatClientWithClose(
	client *Client,
	receivePacketCallback ReceivePacketFunction,
	destinations []MultiHopId,
	provideMode protocol.ProvideMode,
	closeCallback func(),
) *RemoteUserNatClient {
	pathTable := newPathTable(destinations)

	localUserNatSettings := DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := NewLocalUserNat(client.Ctx(), "remote local", localUserNatSettings)

	// the security policy runs a background scan goroutine; scope it to this client (a child of
	// the client ctx) so Close stops it rather than leaking it for the life of the client
	cancelCtx, cancel := context.WithCancel(client.Ctx())
	userNatClient := &RemoteUserNatClient{
		client:                client,
		cancel:                cancel,
		receivePacketCallback: receivePacketCallback,
		securityPolicy:        DefaultSecurityPolicy(cancelCtx),
		pathTable:             pathTable,
		provideMode:           provideMode,
		localUserNat:          localUserNat,
		closeCallback:         closeCallback,
	}

	clientUnsub := client.AddReceiveCallback(userNatClient.ClientReceive)
	userNatClient.clientUnsub = clientUnsub

	userNatClient.localUserNatUnsub = localUserNat.AddReceivePacketCallback(receivePacketCallback)

	return userNatClient
}

func (self *RemoteUserNatClient) Destinations() []MultiHopId {
	return self.pathTable.Destinations()
}

func (self *RemoteUserNatClient) DestinationIds() []Id {
	return self.pathTable.DestinationIds()
}

func (self *RemoteUserNatClient) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

// `SendPacketFunction`
func (self *RemoteUserNatClient) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	relationship := egressRelationship(provideMode, self.provideMode)

	var ipPath IpPath
	payload, err := parseIpPathWithPayloadBorrowed(packet, &ipPath)
	if err != nil {
		return false
	}
	r, err := inspectAndRefreshEgressBorrowed(self.securityPolicy, relationship, ipPath, payload)
	if err != nil {
		return false
	}

	switch r {
	case SecurityPolicyResultAllow:
		destination, err := self.pathTable.SelectDestination(packet)
		if err != nil {
			// drop
			return false
		}

		// The default v2 path embeds the raw frame in the pooled SendPack. It
		// consumes packet only when the send accepts it and avoids both a frame
		// allocation and the extra share that previously left the caller's
		// original reference outstanding after a successful send.
		if 2 <= DefaultProtocolVersion {
			success, _ := self.client.sendRawMultiHopWithTimeoutDetailed(
				protocol.MessageType_IpIpPacketToProvider,
				packet,
				destination,
				nil,
				0,
				timeout,
			)
			return success
		}

		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			panic(err)
		}

		// the sender will control transfer
		// note udp is sent with ack because because otherwise the delivery reliability will mulitply with the egress
		success := self.client.SendMultiHopWithTimeout(frame, destination, func(err error) {}, timeout)
		if success {
			// Legacy serialization copied the packet into frame.MessageBytes;
			// consume the caller's packet only after the queue accepts the copy.
			MessagePoolReturn(packet)
		} else {
			MessagePoolReturn(frame.MessageBytes)
		}
		return success
	case SecurityPolicyResultDrop:
		if self.LocalSecurityBypass() {
			return self.localUserNat.SendPacket(source, provideMode, packet, 0)
		} else {
			return false
		}
	default:
		return false
	}
}

// `connect.ReceiveFunction`
func (self *RemoteUserNatClient) ClientReceive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	// only process frames from the destinations
	// if allow := self.sourceFilter[source]; !allow {
	//     return
	// }

	for _, frame := range frames {
		// self.client.log.Infof("[trace]receive frame %s\n", frame.MessageType)
		switch frame.MessageType {
		case protocol.MessageType_IpIpPacketFromProvider:
			packet, err := ipPacketFromProviderBytes(frame)
			if err != nil {
				panic(err)
			}

			ipPath, err := ParseIpPath(packet)
			if err == nil {
				self.securityPolicy.RefreshIngress(ipPath)
				HandleError(func() {
					self.receivePacketCallback(
						source,
						peer.ProvideMode,
						ipPath,
						packet,
					)
				})
			}
			// else not an ip packet, drop
		}
	}
}

func (self *RemoteUserNatClient) Shuffle() {
}

func (self *RemoteUserNatClient) Close() {
	// self.client.RemoveReceiveCallback(self.clientCallbackId)
	self.cancel()
	self.localUserNat.Close()
	self.localUserNatUnsub()
	self.clientUnsub()
	if self.closeCallback != nil {
		self.closeCallback()
	}
}

func (self *RemoteUserNatClient) SetAllowDirect(allowDirect bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.allowDirect = allowDirect
}

func (self *RemoteUserNatClient) AllowDirect() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.allowDirect
}

func (self *RemoteUserNatClient) SetLocalSecurityBypass(localSecurityBypass bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.localSecurityBypass = localSecurityBypass
}

func (self *RemoteUserNatClient) LocalSecurityBypass() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.localSecurityBypass
}

type pathTable struct {
	destinations []MultiHopId

	// TODO clean up entries that haven't been used in some time
	paths4 map[Ip4Path]MultiHopId
	paths6 map[Ip6Path]MultiHopId
}

func newPathTable(destinations []MultiHopId) *pathTable {
	return &pathTable{
		destinations: destinations,
		paths4:       map[Ip4Path]MultiHopId{},
		paths6:       map[Ip6Path]MultiHopId{},
	}
}

func (self *pathTable) Destinations() []MultiHopId {
	return slices.Clone(self.destinations)
}

func (self *pathTable) DestinationIds() []Id {
	var clientIds []Id
	for _, destination := range self.destinations {
		clientIds = append(clientIds, destination.Tail())
	}
	return clientIds
}

func (self *pathTable) SelectDestination(packet []byte) (MultiHopId, error) {
	if len(self.destinations) == 0 {
		return MultiHopId{}, fmt.Errorf("No destinations")
	}
	if len(self.destinations) == 1 {
		return self.destinations[0], nil
	}

	ipPath, err := ParseIpPath(packet)
	if err != nil {
		return MultiHopId{}, err
	}
	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		if destination, ok := self.paths4[ip4Path]; ok {
			return destination, nil
		}
		i := mathrand.Intn(len(self.destinations))
		destination := self.destinations[i]
		self.paths4[ip4Path] = destination
		return destination, nil
	case 6:
		ip6Path := ipPath.ToIp6Path()
		if destination, ok := self.paths6[ip6Path]; ok {
			return destination, nil
		}
		i := mathrand.Intn(len(self.destinations))
		destination := self.destinations[i]
		self.paths6[ip6Path] = destination
		return destination, nil
	default:
		// no support for this version
		return MultiHopId{}, fmt.Errorf("No support for ip version %d", ipPath.Version)
	}
}

type IpProtocol int

const (
	IpProtocolUnknown IpProtocol = 0
	IpProtocolTcp     IpProtocol = 1
	IpProtocolUdp     IpProtocol = 2
	IpProtocolIcmp    IpProtocol = 3
)

func (self IpProtocol) String() string {
	switch self {
	case IpProtocolTcp:
		return "tcp"
	case IpProtocolUdp:
		return "udp"
	case IpProtocolIcmp:
		return "icmp"
	default:
		return "unknown"
	}
}

type IpPath struct {
	Version         int
	Protocol        IpProtocol
	SourceIp        net.IP
	SourcePort      int
	DestinationIp   net.IP
	DestinationPort int

	SequenceNumber    uint32
	AckSequenceNumber uint32
	Syn               bool
	Rst               bool
	Ack               bool

	ServerName string
}

func ParseIpPath(ipPacket []byte) (*IpPath, error) {
	ipPath, _, err := ParseIpPathWithPayload(ipPacket)
	return ipPath, err
}

func ParseIpPathWithPayload(ipPacket []byte) (*IpPath, []byte, error) {
	var ipPath IpPath
	payload, err := parseIpPathWithPayloadBorrowed(ipPacket, &ipPath)
	if err != nil {
		return nil, nil, err
	}
	// The public result owns its addresses. Keep this copy outside the borrowed
	// parser so escape analysis does not conservatively apply this allocation
	// branch to every synchronous borrowed parse.
	ipBacking := make(net.IP, len(ipPath.SourceIp)+len(ipPath.DestinationIp))
	sourceByteCount := copy(ipBacking, ipPath.SourceIp)
	copy(ipBacking[sourceByteCount:], ipPath.DestinationIp)
	ipPath.SourceIp = ipBacking[:sourceByteCount:sourceByteCount]
	ipPath.DestinationIp = ipBacking[sourceByteCount:]
	return &ipPath, payload, nil
}

// parseIpPathWithPayloadBorrowed fills ipPath without allocating address
// copies. The address slices alias ipPacket and are valid only while ipPacket
// is valid. It is for synchronous packet-policy hot paths; anything retaining
// an IpPath must use ParseIpPathWithPayload instead.
func parseIpPathWithPayloadBorrowed(ipPacket []byte, ipPath *IpPath) ([]byte, error) {
	if len(ipPacket) == 0 {
		return nil, fmt.Errorf("Empty packet.")
	}
	ipVersion := uint8(ipPacket[0]) >> 4
	var ipProtocol ipProtocolNumber
	var sourceIp net.IP
	var destinationIp net.IP
	var transport []byte
	var ok bool
	switch ipVersion {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv4(ipPacket)
	case 6:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv6(ipPacket)
	default:
		// no support for this version
		return nil, fmt.Errorf("No support for ip version %d", ipVersion)
	}
	if !ok {
		return nil, fmt.Errorf("Malformed ip packet.")
	}

	switch ipProtocol {
	case ipProtocolNumberUdp:
		var udp parsedUdp
		if !parseUdpPacket(sourceIp, destinationIp, transport, &udp) {
			return nil, fmt.Errorf("Malformed udp packet.")
		}

		*ipPath = IpPath{
			Version:         int(ipVersion),
			Protocol:        IpProtocolUdp,
			SourceIp:        sourceIp,
			SourcePort:      int(udp.sourcePort),
			DestinationIp:   destinationIp,
			DestinationPort: int(udp.destinationPort),
		}
		return udp.payload, nil
	case ipProtocolNumberTcp:
		var tcp parsedTcp
		if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			return nil, fmt.Errorf("Malformed tcp packet.")
		}

		*ipPath = IpPath{
			Version:           int(ipVersion),
			Protocol:          IpProtocolTcp,
			SourceIp:          sourceIp,
			SourcePort:        int(tcp.sourcePort),
			DestinationIp:     destinationIp,
			DestinationPort:   int(tcp.destinationPort),
			SequenceNumber:    tcp.seq,
			AckSequenceNumber: tcp.ackNumber,
			Syn:               tcp.syn,
			Rst:               tcp.rst,
			Ack:               tcp.ack,
		}
		return tcp.payload, nil
	default:
		// icmp and the unsupported protocols are parsed out of line: keeping
		// this switch at its original two hot cases preserves the tcp and udp
		// dispatch (adding cases here measurably slowed the udp parse)
		return parseIcmpIpPathBorrowed(ipVersion, ipProtocol, sourceIp, destinationIp, transport, ipPath)
	}
}

// parseIcmpIpPathBorrowed is the cold tail of parseIpPathWithPayloadBorrowed:
// icmp echo, else the unsupported protocol error. Split out so the hot tcp/udp
// dispatch above is unchanged by icmp support.
//
//go:noinline
func parseIcmpIpPathBorrowed(
	ipVersion uint8,
	ipProtocol ipProtocolNumber,
	sourceIp net.IP,
	destinationIp net.IP,
	transport []byte,
	ipPath *IpPath,
) ([]byte, error) {
	switch ipProtocol {
	case ipProtocolNumberIcmp4, ipProtocolNumberIcmp6:
	default:
		return nil, fmt.Errorf("No support for protocol %d", ipProtocol)
	}
	if (ipProtocol == ipProtocolNumberIcmp4) != (ipVersion == 4) {
		// the icmp variant must match the ip version
		return nil, fmt.Errorf("No support for protocol %d", ipProtocol)
	}
	var icmp parsedIcmp
	if !parseIcmpPacket(int(ipVersion), sourceIp, destinationIp, transport, &icmp) {
		return nil, fmt.Errorf("Unsupported or malformed icmp packet.")
	}

	// the echo identifier stands in for the port on the client side of the
	// flow: the source side of a request, the destination side of a reply,
	// so Reverse aligns the two directions (see ICMP.md)
	*ipPath = IpPath{
		Version:       int(ipVersion),
		Protocol:      IpProtocolIcmp,
		SourceIp:      sourceIp,
		DestinationIp: destinationIp,
	}
	if icmp.echoRequest {
		ipPath.SourcePort = int(icmp.identifier)
	} else {
		ipPath.DestinationPort = int(icmp.identifier)
	}
	return icmp.payload, nil
}

func (self *IpPath) SourceHostPort() string {
	return net.JoinHostPort(
		self.SourceIp.String(),
		strconv.Itoa(self.SourcePort),
	)
}

func (self *IpPath) DestinationHostPort() string {
	return net.JoinHostPort(
		self.DestinationIp.String(),
		strconv.Itoa(self.DestinationPort),
	)
}

func (self *IpPath) ToIp4Path() Ip4Path {
	var sourceIp [4]byte
	if self.SourceIp != nil {
		if sourceIp4 := self.SourceIp.To4(); sourceIp4 != nil {
			sourceIp = [4]byte(sourceIp4)
		}
	}
	var destinationIp [4]byte
	if self.DestinationIp != nil {
		if destinationIp4 := self.DestinationIp.To4(); destinationIp4 != nil {
			destinationIp = [4]byte(destinationIp4)
		}
	}
	return Ip4Path{
		Protocol:        self.Protocol,
		SourceIp:        sourceIp,
		SourcePort:      self.SourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: self.DestinationPort,
		ServerName:      self.ServerName,
	}
}

func (self *IpPath) ToIp6Path() Ip6Path {
	var sourceIp [16]byte
	if self.SourceIp != nil {
		if sourceIp6 := self.SourceIp.To16(); sourceIp6 != nil {
			sourceIp = [16]byte(sourceIp6)
		}
	}
	var destinationIp [16]byte
	if self.DestinationIp != nil {
		if destinationIp6 := self.DestinationIp.To16(); destinationIp6 != nil {
			destinationIp = [16]byte(destinationIp6)
		}
	}
	return Ip6Path{
		Protocol:        self.Protocol,
		SourceIp:        sourceIp,
		SourcePort:      self.SourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: self.DestinationPort,
		ServerName:      self.ServerName,
	}
}

// dialFailureAction classifies a failed upstream DialContext into the signal
// the provider sends back to the source in place of the SynAck. Without a
// signal the source sits in syn-retransmit backoff (3s..63s) and the blackhole
// timeout eventually removes the provider along with its working flows -- the
// bug this replaces.
type dialFailureAction int

const (
	// dialFailureNone: send nothing (nil path / unsupported ip version).
	dialFailureNone dialFailureAction = iota
	// dialFailureRst: the destination itself refused the connection. The caller
	// answers with ConnectionState.RstAck() -- honest tcp semantics, forwarded
	// to the app.
	dialFailureRst
	// dialFailureUnreachable: capacity-class failure. The caller delivers the
	// icmp destination-unreachable packet returned alongside this action.
	dialFailureUnreachable
)

// classifyDialFailure maps an upstream DialContext error to the signal the
// provider should return in place of a SynAck. It is pure -- no live socket, no
// ConnectionState -- so the errno logic is unit-testable.
//
// For dialFailureUnreachable the built icmp packet is returned as well (a fresh
// non-pool buffer from ipOosUnreachable; the caller copies it into the pool
// before delivery). For dialFailureRst and dialFailureNone the packet is nil --
// a RST needs the ConnectionState's sequence state, so the caller builds it.
//
// tcp + ECONNREFUSED means the destination refused: RST+ACK. Anything else on
// tcp (timeouts, EMFILE/ENFILE, EADDRNOTAVAIL, unreachable, unrecognized) is
// capacity-class and gets an unreachable; defaulting unrecognized errors to the
// capacity class is deliberate -- new clients intercept the signal and old ones
// drop it at ParseIpPath, so a misclassification is cheap. errors.Is folds the
// Windows WSAECONNREFUSED onto syscall.ECONNREFUSED, but the providers this runs
// on are almost all linux, so linux errno semantics are what matter. udp "dial"
// cannot produce a meaningful ECONNREFUSED at connect time, so udp is always
// capacity-class.
func classifyDialFailure(ipPath *IpPath, err error) (dialFailureAction, []byte) {
	if ipPath == nil {
		return dialFailureNone, nil
	}
	switch ipPath.Protocol {
	case IpProtocolTcp:
		if errors.Is(err, syscall.ECONNREFUSED) {
			return dialFailureRst, nil
		}
		// otherwise fall through to the unreachable build below
	case IpProtocolUdp:
		// always capacity-class; fall through
	default:
		return dialFailureNone, nil
	}
	if packet, ok := ipOosUnreachable(ipPath); ok {
		return dialFailureUnreachable, packet
	}
	return dialFailureNone, nil
}

func (self *IpPath) Source() *IpPath {
	return &IpPath{
		Protocol:   self.Protocol,
		Version:    self.Version,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *IpPath) Destination() *IpPath {
	return &IpPath{
		Protocol:        self.Protocol,
		Version:         self.Version,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

// ReverseValue returns the flow tuple in the opposite direction without
// allocating. Sequence/flag state is intentionally omitted, matching Reverse:
// synthetic packets built from a reversed path are out of sequence.
func (self *IpPath) ReverseValue() IpPath {
	return IpPath{
		Protocol:        self.Protocol,
		Version:         self.Version,
		SourceIp:        self.DestinationIp,
		SourcePort:      self.DestinationPort,
		DestinationIp:   self.SourceIp,
		DestinationPort: self.SourcePort,
	}
}

func (self *IpPath) Reverse() *IpPath {
	reversed := self.ReverseValue()
	return &reversed
}

// comparable
type Ip4Path struct {
	Protocol        IpProtocol
	SourceIp        [4]byte
	SourcePort      int
	DestinationIp   [4]byte
	DestinationPort int
	ServerName      string
}

func (self *Ip4Path) Source() Ip4Path {
	return Ip4Path{
		Protocol:   self.Protocol,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *Ip4Path) Destination() Ip4Path {
	return Ip4Path{
		Protocol:        self.Protocol,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

// comparable
type Ip6Path struct {
	Protocol        IpProtocol
	SourceIp        [16]byte
	SourcePort      int
	DestinationIp   [16]byte
	DestinationPort int
	ServerName      string
}

func (self *Ip6Path) Source() Ip6Path {
	return Ip6Path{
		Protocol:   self.Protocol,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *Ip6Path) Destination() Ip6Path {
	return Ip6Path{
		Protocol:        self.Protocol,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

type UserLimited interface {
	LastActivityTime() time.Time
	Cancel()
}

type userLimited struct {
	mutex            sync.Mutex
	lastActivityTime time.Time
}

func newUserLimited() *userLimited {
	return &userLimited{
		lastActivityTime: time.Now(),
	}
}

func (self *userLimited) LastActivityTime() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.lastActivityTime
}

func (self *userLimited) UpdateLastActivityTime() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.lastActivityTime = time.Now()
}

const lruEvictionSampleSize = 32

// applyLruMapLimit bounds eviction work independently of table cardinality.
// Go map iteration starts at a pseudo-random bucket, so choosing the oldest of
// the first fixed-size sample is an approximate LRU without collecting and
// sorting the entire flow table under its dispatch lock. limitCallback must
// remove an accepted resource from resources; it owns any cancellation.
func applyLruMapLimit[K comparable, R UserLimited](
	resources map[K]R,
	limit int,
	limitCallback func(K, R) bool,
) {
	for limit < len(resources) {
		var oldestKey K
		var oldest R
		var oldestTime time.Time
		found := false
		sampled := 0
		for key, resource := range resources {
			activityTime := resource.LastActivityTime()
			if !found || activityTime.Before(oldestTime) {
				oldestKey = key
				oldest = resource
				oldestTime = activityTime
				found = true
			}
			sampled += 1
			if lruEvictionSampleSize <= sampled {
				break
			}
		}
		if !found || !limitCallback(oldestKey, oldest) {
			return
		}
	}
}
