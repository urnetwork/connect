package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The provider egress hook, C5 of the sidecar design's connect changes
// (sidecar design §4.6 and §17.6). A provider LocalUserNat asks its hook about
// each new flow, in the flow's own goroutine and before any upstream socket
// exists: pass it to its destination, map it to another address, or deny it.
// Generic, additive and off by default. A NAT without a hook behaves exactly as
// before, and its one cost is a nil check when a flow is created.
//
// The source's identity reaches the flow on every lane. Each packet carries an
// `egressSource`, built from the receive callback's `Peer`, down the ordinary,
// reliable and fallback lanes into the parsed packet that opens a flow, as the
// packet's TTL rides today. A lane that dropped it would read as a source with
// no seals, no contract and no session, which a hook denies.
//
// A flow registers its decision before it dials, opens its socket or sends its
// first echo, all under its own context, so `SetEgressHook` can close every
// flow its new hook did not decide.

// What a hook decides for one flow.
type EgressVerdict int

const (
	// The zero value, so a decision a hook leaves unset refuses its flow. A
	// denied TCP flow gets a reset, a UDP flow an unreachable, and an ICMP echo
	// nothing. A deny is never a security incident and never reports abuse.
	EgressVerdictDeny EgressVerdict = iota
	// Dial the destination as sent.
	EgressVerdictPass
	// Dial `EgressDecision.Target` in place of the destination. Replies keep
	// the original destination as their source, so a map is a destination NAT
	// with no rewrite of any packet.
	EgressVerdictMap
)

func (self EgressVerdict) String() string {
	switch self {
	case EgressVerdictDeny:
		return "deny"
	case EgressVerdictPass:
		return "pass"
	case EgressVerdictMap:
		return "map"
	default:
		return "unknown(" + strconv.Itoa(int(self)) + ")"
	}
}

// A new flow as its hook sees it. The seals are the platform's statements, a
// consistency check only; `Principal` is what the hook itself verified for the
// flow's contract, through `EgressHook.VerifyPrincipal`.
type EgressFlow struct {
	// The source client, as the provider's receive callback named it.
	Source TransferPath
	// The mode the frames that opened the flow arrived under.
	ProvideMode protocol.ProvideMode
	// The contract class C13 sealed; `ContractClassUnset` when the contract
	// sealed none or the flow arrived without a contract.
	ContractClass protocol.ContractClass
	// C13's sealed source subject; zero when unsealed.
	SourceSubject Id
	// C13's sealed source network, in every mode; zero when unsealed.
	SourceNetwork Id
	// The hook's own answer for this flow's contract, from its
	// `WindowDelegation`; nil for none. Not `Peer.Principal`, the contract
	// signer's string.
	Principal *BridgePrincipal
	// The frame that opened the flow came through an end-to-end session whose
	// peer client key the session verified (`Peer.VerifiedClientPublicKey`).
	// A client without `EncryptionSettings.ExposeVerifiedPeerClientKey` never
	// sees a key, so every flow it hosts reads as unsealed.
	Sealed   bool
	Protocol IpProtocol
	// The flow's source and destination as the packet carried them. A
	// v4-mapped v6 address arrives as the v4 address it maps, so a hook judges
	// it as the security policy does (`policyAddress`). ICMP has no ports: both
	// are 0 for an echo, so a rule naming a port never admits one.
	From netip.AddrPort
	To   netip.AddrPort
}

// The verified subject of a contract, from a window client's customer-signed
// delegation (sidecar design §17.9).
type BridgePrincipal struct {
	Subject Id
	Network Id
	// Zero on a same-network link.
	ShareId Id
	// The principal lapses at this time; the hook rechecks it per flow.
	NotAfter time.Time
}

// A hook's answer for one flow.
type EgressDecision struct {
	Verdict EgressVerdict
	// Where a map dials. Port 0 keeps the destination's port. A target that is
	// not a valid address denies the flow, and so does an ICMP map that
	// changes the address family, since an echo cannot cross families.
	Target netip.AddrPort
	// Names the (subject, rule) pair the decision came from, so
	// `LocalUserNat.CloseFlows` can end that pair's flows.
	RuleTag uint64
	// The published policy generation the decision used.
	PolicyGeneration uint64
	// This flow's idle timeout in place of its NAT's. Zero keeps the NAT's.
	IdleTimeout time.Duration
}

// The policy a provider LocalUserNat consults before each new flow. Every
// method may be called from any flow's goroutine at once.
type EgressHook interface {
	// Called once per contract a source presents, in the sequence of the
	// first flow it opens, before that flow's dial. The NAT keeps the answer,
	// nil included, for that source's later flows on the same contract, and
	// asks again on a new contract. A nil answer is asked again only after
	// the published generation has moved. `peer` carries the contract seals,
	// the window delegation and the verified session key of the frame that
	// opened the flow; its roles, principal and delivery are not carried.
	VerifyPrincipal(source TransferPath, peer Peer) *BridgePrincipal
	// Decides one flow, inline: it must not block, wait for I/O or take a
	// lock that a packet path holds.
	ResolveEgress(flow *EgressFlow) EgressDecision
	// The published generation, one atomic load. A flow whose decision used
	// an older one is resolved again before its first byte.
	PolicyGeneration() uint64
	// C18: true hands FriendsAndFamily flows to private destinations to
	// `ResolveEgress` in place of the public-only rules, admits the replies of
	// those it passes or maps, and sheds foreign flows first when a flow table
	// is full. Asked once per provider callback and per flow, so like
	// `PolicyGeneration` it must be one load.
	FriendsAndFamilyEgress() bool
}

// The refusal a router answers a packet it will not forward with, addressed
// back to the packet's source as if from its destination: a reset for TCP, and
// for anything else an ICMP destination unreachable, administratively
// prohibited (v4 type 3 code 13, v6 type 1 code 1), quoting the packet's start.
// Nil for a packet that must get no answer (RFC 1122 §3.2.2): a reset, an ICMP
// error, a fragment, a packet from or to an address that is not a single host,
// or anything malformed.
//
// Borrows the packet. The result is a message pool buffer the caller owns.
func AdministrativeRejectPacket(packet []byte) []byte {
	if len(packet) == 0 {
		return nil
	}
	switch packet[0] >> 4 {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		if !ok || !administrativeRejectHosts(sourceIp, destinationIp) {
			return nil
		}
		switch ipProtocol {
		case ipProtocolNumberTcp:
			return tcpRstForPolicyReject(packet)
		case ipProtocolNumberIcmp4:
			if len(transport) < IcmpHeaderSize || icmp4ErrorType(transport[0]) {
				return nil
			}
		}
		// rfc 792: the header and the first 8 bytes of what it carried
		headerByteCount := int(packet[0]&0x0f) * 4
		quoted := packet[:headerByteCount+min(len(transport), icmp4UnreachableQuotedTransport)]
		reply := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + IcmpHeaderSize + len(quoted))
		writeIpv4Header(reply, ipProtocolNumberIcmp4, destinationIp, sourceIp)
		icmp := reply[Ipv4HeaderSizeWithoutExtensions:]
		icmp[0] = icmp4TypeDestinationUnreachable
		icmp[1] = icmp4CodeAdministrativelyProhibited
		clear(icmp[2:IcmpHeaderSize])
		copy(icmp[IcmpHeaderSize:], quoted)
		binary.BigEndian.PutUint16(icmp[2:4], checksumFinish(checksumAdd(0, icmp)))
		return reply
	case 6:
		ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv6(packet)
		if !ok || !administrativeRejectHosts(sourceIp, destinationIp) {
			return nil
		}
		switch ipProtocol {
		case ipProtocolNumberTcp:
			return tcpRstForPolicyReject(packet)
		case ipProtocolNumberIcmp6:
			// rfc 4443 §2.1: types below 128 are errors
			if len(transport) < IcmpHeaderSize || transport[0] < 128 {
				return nil
			}
		}
		// rfc 4443 §2.4: as much of the packet as fits in the minimum mtu
		quotedByteCount := min(len(packet), icmp6MinMtu-Ipv6HeaderSize-IcmpHeaderSize)
		reply := MessagePoolGet(Ipv6HeaderSize + IcmpHeaderSize + quotedByteCount)
		writeIpv6Header(reply, ipProtocolNumberIcmp6, destinationIp, sourceIp)
		icmp := reply[Ipv6HeaderSize:]
		icmp[0] = icmp6TypeDestinationUnreachable
		icmp[1] = icmp6CodeAdministrativelyProhibited
		clear(icmp[2:IcmpHeaderSize])
		copy(icmp[IcmpHeaderSize:], packet[:quotedByteCount])
		binary.BigEndian.PutUint16(icmp[2:4], transportChecksum(ipProtocolNumberIcmp6, destinationIp, sourceIp, icmp))
		return reply
	default:
		return nil
	}
}

const (
	// rfc 1812 §5.2.7.1
	icmp4CodeAdministrativelyProhibited = byte(13)
	// rfc 4443 §3.1
	icmp6CodeAdministrativelyProhibited = byte(1)
)

// The ICMPv4 error types (rfc 1122 §3.2.2): destination unreachable, source
// quench, redirect, time exceeded and parameter problem. No error answers one.
func icmp4ErrorType(icmpType byte) bool {
	switch icmpType {
	case 3, 4, 5, 11, 12:
		return true
	default:
		return false
	}
}

// Whether a packet's endpoints admit an error answer: the source a single host
// to send it to, the destination a single host that could have refused it.
func administrativeRejectHosts(sourceIp net.IP, destinationIp net.IP) bool {
	single := func(ip net.IP) bool {
		return !ip.IsUnspecified() && !ip.IsMulticast() && !ip.Equal(net.IPv4bcast)
	}
	return single(sourceIp) && single(destinationIp)
}

// What a provider knows of the sender of a packet, carried with the packet from
// the receive callback to the flow it opens: the contract seals, the contract
// id and the session's verified client key, from `Peer` without its delivery
// capability. The zero value is a packet a caller handed the NAT directly, with
// no peer: no seals, no contract and no session key.
type egressSource struct {
	contractClass     protocol.ContractClass
	sourceSubject     Id
	sourceNetwork     Id
	contractId        Id
	windowDelegation  []byte
	verifiedClientKey *[ed25519.PublicKeySize]byte
}

func newEgressSource(peer Peer) egressSource {
	return egressSource{
		contractClass:     peer.ContractClass,
		sourceSubject:     peer.SourceSubject,
		sourceNetwork:     peer.SourceNetwork,
		contractId:        peer.contractId,
		windowDelegation:  peer.WindowDelegation,
		verifiedClientKey: peer.verifiedClientKey,
	}
}

// The peer `EgressHook.VerifyPrincipal` sees: the carried seals, delegation and
// session key, without roles, principal or delivery.
func (self *egressSource) peer(provideMode protocol.ProvideMode, transferKey TransferKey) Peer {
	return Peer{
		ProvideMode:       provideMode,
		ContractClass:     self.contractClass,
		SourceSubject:     self.sourceSubject,
		SourceNetwork:     self.sourceNetwork,
		WindowDelegation:  self.windowDelegation,
		TransferKey:       transferKey,
		verifiedClientKey: self.verifiedClientKey,
		contractId:        self.contractId,
	}
}

// One installed hook, boxed so a single atomic pointer swaps it. A decision and
// a principal answer remember the box they came from, so nothing one hook
// decided outlives its replacement.
type egressHookBox struct {
	hook EgressHook
}

// The principal answers are kept per source and contract.
type egressPrincipalKey struct {
	source     TransferPath
	contractId Id
}

// One contract's principal. `done` closes once `principal` and `generation`
// are set; the first flow to ask answers, and the others wait for it.
type egressPrincipalAnswer struct {
	box        *egressHookBox
	done       chan struct{}
	principal  *BridgePrincipal
	generation uint64
}

// Lists the flows of one send shard's buffers that `match` selects. `match`
// runs under each buffer's mutex and may only read atomics.
type egressShardScan func(match func(*egressFlowState) bool, handles []egressFlowHandle) []egressFlowHandle

// A flow sequence, as a scan of the shards lists it.
type egressFlowHandle interface {
	egressState() *egressFlowState
	// Ends the flow, first answering its source when `answer` is set: a reset
	// for TCP, an unreachable for UDP, nothing for an ICMP echo. False when the
	// flow had already been ended by its own deny or by another close.
	closeEgress(answer bool) bool
}

// A NAT's part in the egress hook, shared by its send shards and their flows.
// The hook is one atomic load. The principal answers, the shard registry and
// the hook's binding sit under `stateLock`, which no packet path takes: a flow
// takes it when it resolves, before its dial, and `SetEgressHook`,
// `CloseFlows` and the NAT's close take it off the packet path. Safe for
// concurrent use.
type localUserNatEgress struct {
	settings *LocalUserNatSettings
	hook     atomic.Pointer[egressHookBox]
	// The hook's `FriendsAndFamilyEgress` as last asked, at its install and at
	// each flow's resolve, for a flow table at its cap, which must not call
	// the hook under its mutex (shedFirst).
	foreignFirst atomic.Bool

	stateLock       sync.Mutex
	principals      map[egressPrincipalKey]*egressPrincipalAnswer
	nextShardScanId uint64
	shardScans      map[uint64]egressShardScan
	// the installed hook's `EgressHookBinder` unbind; nil when it has none
	unbind func()
	// the NAT has ended, and binds no hook again
	closed bool
}

func newLocalUserNatEgress(settings *LocalUserNatSettings) *localUserNatEgress {
	return &localUserNatEgress{
		settings:   settings,
		principals: map[egressPrincipalKey]*egressPrincipalAnswer{},
		shardScans: map[uint64]egressShardScan{},
	}
}

// Whether a hook is installed now. A flow that read false may still meet a
// hook installed after, which closes it (`SetEgressHook`).
func (self *localUserNatEgress) hooked() bool {
	return self != nil && self.hook.Load() != nil
}

// Whether the installed hook takes FriendsAndFamily flows to private
// destinations (C18). Nil-safe, for a NAT built without one.
func (self *localUserNatEgress) friendsAndFamilyEgress() bool {
	if self == nil {
		return false
	}
	box := self.hook.Load()
	return box != nil && box.hook.FriendsAndFamilyEgress()
}

func (self *localUserNatEgress) lockStateWithLock() {
	self.stateLock.Lock()
	if self.settings.afterEgressStateLockForTest != nil {
		self.settings.afterEgressStateLockForTest()
	}
}

// Swaps in `hook`, nil removing it, drops every principal answer the old hook
// gave, and moves the binding: the old hook's binder is unbound and the new one
// bound to `closer` (`EgressHookBinder`). Binders run outside every lock, and a
// bind that a later install or the NAT's end overtook is unbound at once.
// Returns the new box, nil when the hook was removed.
func (self *localUserNatEgress) install(closer EgressFlowCloser, hook EgressHook) *egressHookBox {
	var box *egressHookBox
	if hook != nil {
		box = &egressHookBox{hook: hook}
	}
	self.foreignFirst.Store(hook != nil && hook.FriendsAndFamilyEgress())
	var previousUnbind func()
	func() {
		self.lockStateWithLock()
		defer self.stateLock.Unlock()
		self.hook.Store(box)
		clear(self.principals)
		previousUnbind = self.unbind
		self.unbind = nil
	}()
	if previousUnbind != nil {
		previousUnbind()
	}
	if binder, ok := hook.(EgressHookBinder); ok {
		unbind := binder.BindFlowCloser(closer)
		if unbind != nil {
			func() {
				self.lockStateWithLock()
				defer self.stateLock.Unlock()
				if !self.closed && self.hook.Load() == box {
					self.unbind = unbind
					unbind = nil
				}
			}()
			if unbind != nil {
				unbind()
			}
		}
	}
	return box
}

// Unbinds the installed hook when the NAT ends.
func (self *localUserNatEgress) close() {
	var unbind func()
	func() {
		self.lockStateWithLock()
		defer self.stateLock.Unlock()
		self.closed = true
		unbind = self.unbind
		self.unbind = nil
	}()
	if unbind != nil {
		unbind()
	}
}

// Registers one send shard's scan; the returned function removes it.
func (self *localUserNatEgress) addShardScan(scan egressShardScan) func() {
	self.lockStateWithLock()
	defer self.stateLock.Unlock()
	self.nextShardScanId += 1
	shardScanId := self.nextShardScanId
	self.shardScans[shardScanId] = scan
	return func() {
		self.lockStateWithLock()
		defer self.stateLock.Unlock()
		delete(self.shardScans, shardScanId)
	}
}

// The live flows `match` selects, across every shard. Each buffer is scanned
// under its own mutex, and no lock is held between buffers or after the scan.
func (self *localUserNatEgress) flows(match func(*egressFlowState) bool) []egressFlowHandle {
	var shardScans []egressShardScan
	func() {
		self.lockStateWithLock()
		defer self.stateLock.Unlock()
		for _, shardScan := range self.shardScans {
			shardScans = append(shardScans, shardScan)
		}
	}()
	var handles []egressFlowHandle
	for _, shardScan := range shardScans {
		handles = shardScan(match, handles)
	}
	return handles
}

// The flow's principal: the contract's kept answer, or the hook's, asked once
// per source and contract. Concurrent flows of one contract wait for the first
// one's answer under their own contexts. A nil answer is asked again once the
// published generation has moved, since it may only mean the hook did not yet
// know the subject. A flow with no contract asks every time, which costs
// nothing: with no contract there is no delegation to verify.
func (self *localUserNatEgress) verifyPrincipal(
	ctx context.Context,
	box *egressHookBox,
	source TransferPath,
	transferKey TransferKey,
	provideMode protocol.ProvideMode,
	egressSource *egressSource,
) *BridgePrincipal {
	peer := egressSource.peer(provideMode, transferKey)
	if egressSource.contractId == (Id{}) {
		return box.hook.VerifyPrincipal(source, peer)
	}
	key := egressPrincipalKey{source: source, contractId: egressSource.contractId}
	for {
		answer, owner := self.takePrincipalAnswer(box, key)
		if owner {
			return self.answerPrincipal(box, key, answer, source, peer)
		}
		select {
		case <-answer.done:
		default:
			if self.settings.beforeEgressPrincipalWaitForTest != nil {
				self.settings.beforeEgressPrincipalWaitForTest()
			}
			select {
			case <-answer.done:
			case <-ctx.Done():
				return nil
			}
		}
		if answer.principal != nil || answer.generation == box.hook.PolicyGeneration() {
			return answer.principal
		}
		// a refusal under an older policy: ask the hook again
		self.dropPrincipalAnswer(key, answer)
	}
}

// The kept answer for `key` from this box, or a new pending one this caller
// must give (owner).
func (self *localUserNatEgress) takePrincipalAnswer(
	box *egressHookBox,
	key egressPrincipalKey,
) (answer *egressPrincipalAnswer, owner bool) {
	self.lockStateWithLock()
	defer self.stateLock.Unlock()
	if answer = self.principals[key]; answer != nil && answer.box == box {
		return answer, false
	}
	answer = &egressPrincipalAnswer{box: box, done: make(chan struct{})}
	self.principals[key] = answer
	if maxCount := self.settings.EgressPrincipalMaxCount; 0 < maxCount && maxCount < len(self.principals) {
		// arbitrary eviction of finished answers, as the provider bounds its
		// per-source maps; an evicted contract asks again on its next flow
		for evictKey, evictAnswer := range self.principals {
			if len(self.principals) <= maxCount {
				break
			}
			select {
			case <-evictAnswer.done:
				delete(self.principals, evictKey)
			default:
			}
		}
	}
	return answer, true
}

// Asks the hook and publishes the answer to every flow waiting on it. A hook
// that panics leaves no kept answer, and its waiters read a refusal.
func (self *localUserNatEgress) answerPrincipal(
	box *egressHookBox,
	key egressPrincipalKey,
	answer *egressPrincipalAnswer,
	source TransferPath,
	peer Peer,
) *BridgePrincipal {
	answered := false
	defer func() {
		if !answered || self.settings.EgressPrincipalMaxCount <= 0 {
			self.dropPrincipalAnswer(key, answer)
		}
		close(answer.done)
	}()
	answer.generation = box.hook.PolicyGeneration()
	answer.principal = box.hook.VerifyPrincipal(source, peer)
	answered = true
	return answer.principal
}

func (self *localUserNatEgress) dropPrincipalAnswer(key egressPrincipalKey, answer *egressPrincipalAnswer) {
	self.lockStateWithLock()
	defer self.stateLock.Unlock()
	if self.principals[key] == answer {
		delete(self.principals, key)
	}
}

// Forgets a retired source's answers, so a returning source verifies afresh.
func (self *localUserNatEgress) dropSource(sourceId Id) {
	self.lockStateWithLock()
	defer self.stateLock.Unlock()
	for key := range self.principals {
		if key.source.SourceId == sourceId {
			delete(self.principals, key)
		}
	}
}

// Ends a flow its hook denied or a close ended. Never an incident.
var errEgressRefused = errors.New("egress refused")

// What a flow does after it asks its hook.
type egressOutcome int

const (
	// no hook: dial the destination as before hooks existed
	egressOutcomeLegacy egressOutcome = iota
	// dial the destination, or the returned map target when it is valid
	egressOutcomeDial
	// the hook refused: answer the source, then end the flow
	egressOutcomeDeny
	// end the flow without an answer: its context ended, or a close ended it
	egressOutcomeEnd
)

// A decision as its flow registered it.
type egressRegistration struct {
	box       *egressHookBox
	decision  EgressDecision
	principal *BridgePrincipal
	// what the flow dials: the destination, or the map's target with its port
	dial netip.AddrPort
}

// One flow's part in the egress hook, kept on its sequence: the identity its
// first packet carried and the decision it registered. The flow's goroutine
// writes the registration once, before its dial, and everything else reads it
// with one atomic load; no packet path touches it.
type egressFlowState struct {
	// nil for a flow outside any NAT's egress (a sequence a caller built
	// directly), which resolves as a NAT without a hook does
	nat    *localUserNatEgress
	source egressSource
	// the flow's decision; nil until it resolves under a hook
	registration atomic.Pointer[egressRegistration]
	// the flow reached its decision, with or without a hook, so its first
	// packet has been taken in and a close may answer its source
	resolved atomic.Bool
	// set by whichever ends the flow first, its own deny or a close from
	// outside, so the source gets at most one answer
	closed atomic.Bool
}

// Asks the hook for the flow's decision and registers it, in the flow's own
// goroutine before its dial. A hook that panics ends the flow there, before any
// socket exists: the panic reaches the sequence's own handler.
func (self *egressFlowState) resolve(
	ctx context.Context,
	source TransferPath,
	transferKey TransferKey,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
) (egressOutcome, netip.AddrPort) {
	self.resolved.Store(true)
	if self.nat == nil {
		return egressOutcomeLegacy, netip.AddrPort{}
	}
	box := self.nat.hook.Load()
	if box == nil {
		return egressOutcomeLegacy, netip.AddrPort{}
	}
	self.nat.foreignFirst.Store(box.hook.FriendsAndFamilyEgress())
	flow := EgressFlow{
		Source:        source,
		ProvideMode:   provideMode,
		ContractClass: self.source.contractClass,
		SourceSubject: self.source.sourceSubject,
		SourceNetwork: self.source.sourceNetwork,
		Sealed:        self.source.verifiedClientKey != nil,
		Protocol:      ipPath.Protocol,
		From:          egressAddrPort(ipPath.SourceIp, ipPath.SourcePort, ipPath.Protocol),
		To:            egressAddrPort(ipPath.DestinationIp, ipPath.DestinationPort, ipPath.Protocol),
	}
	flow.Principal = self.nat.verifyPrincipal(ctx, box, source, transferKey, provideMode, &self.source)
	decision := box.hook.ResolveEgress(&flow)
	dial, ok := egressDial(&flow, decision, ipPath.Version)
	if !ok {
		return egressOutcomeDeny, netip.AddrPort{}
	}
	// the flow's own path, before its first reply: a provider admits a
	// share's replies only for a flow its hook passed or mapped (C18)
	ipPath.egressAdmitted = true
	self.registration.Store(&egressRegistration{
		box:       box,
		decision:  decision,
		principal: flow.Principal,
		dial:      dial,
	})
	if ctx.Err() != nil || self.closed.Load() {
		return egressOutcomeEnd, netip.AddrPort{}
	}
	if decision.Verdict == EgressVerdictPass {
		// the destination exactly as a flow without a hook dials it
		return egressOutcomeDial, netip.AddrPort{}
	}
	return egressOutcomeDial, dial
}

// What a decision dials, or false when it refuses the flow: a deny, an unknown
// verdict, a map to no address, or an echo mapped across address families.
func egressDial(flow *EgressFlow, decision EgressDecision, ipVersion int) (netip.AddrPort, bool) {
	switch decision.Verdict {
	case EgressVerdictPass:
		return flow.To, true
	case EgressVerdictMap:
		addr := decision.Target.Addr().Unmap()
		if !addr.IsValid() || addr.IsUnspecified() {
			return netip.AddrPort{}, false
		}
		if flow.Protocol == IpProtocolIcmp && addr.Is4() != (ipVersion == 4) {
			return netip.AddrPort{}, false
		}
		port := decision.Target.Port()
		if port == 0 {
			port = flow.To.Port()
		}
		return netip.AddrPortFrom(addr, port), true
	default:
		return netip.AddrPort{}, false
	}
}

// An address and port as a hook sees them: a v4-mapped v6 address as its v4
// address, and no port for ICMP.
func egressAddrPort(ip net.IP, port int, ipProtocol IpProtocol) netip.AddrPort {
	addr, _ := netip.AddrFromSlice(ip)
	if ipProtocol == IpProtocolIcmp {
		port = 0
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port))
}

// Installs, replaces or removes this NAT's hook while it runs. The hook decides
// every flow opened after the call, and every live flow it did not decide is
// closed: one that has taken in its first packet answers its source with a
// reset or an unreachable, and one still opening ends before it dials. So no
// flow opened under an earlier hook, or under none, outlives the change
// (plan-BRIDGE-small-readings #4). Removing the hook leaves live flows open.
//
// Safe for concurrent use, but not from a packet path: it scans every flow.
func (self *LocalUserNat) SetEgressHook(hook EgressHook) {
	if self.egress.install(self, hook) == nil {
		return
	}
	// judged against the hook installed when each flow is scanned, so a later
	// install's flows are never closed by an earlier one's sweep
	undecided := self.egress.flows(func(state *egressFlowState) bool {
		current := self.egress.hook.Load()
		if current == nil {
			return false
		}
		registration := state.registration.Load()
		return registration == nil || registration.box != current
	})
	for _, handle := range undecided {
		handle.closeEgress(handle.egressState().resolved.Load())
	}
}

// Appends this buffer's flows that `match` selects, under its mutex.
func (self *TcpBuffer[BufferId]) appendEgressFlows(
	match func(*egressFlowState) bool,
	handles []egressFlowHandle,
) []egressFlowHandle {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, sequence := range self.sequences {
		if match(&sequence.egressFlow) {
			handles = append(handles, sequence)
		}
	}
	return handles
}

// Appends this buffer's flows that `match` selects, under its mutex.
func (self *UdpBuffer[BufferId]) appendEgressFlows(
	match func(*egressFlowState) bool,
	handles []egressFlowHandle,
) []egressFlowHandle {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, sequence := range self.sequences {
		if match(&sequence.egressFlow) {
			handles = append(handles, sequence)
		}
	}
	return handles
}

// Appends this buffer's flows that `match` selects, under its mutex.
func (self *IcmpBuffer[BufferId]) appendEgressFlows(
	match func(*egressFlowState) bool,
	handles []egressFlowHandle,
) []egressFlowHandle {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, sequence := range self.sequences {
		if match(&sequence.egressFlow) {
			handles = append(handles, sequence)
		}
	}
	return handles
}
