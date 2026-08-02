package connect

// First-load timeline instrumentation (PACKETRESEARCH1 §12).
//
// The user-visible cost of "the first page after connect" decomposes into
// stages owned by different subsystems: dns query→answer (the mux's DoH
// pipeline), tcp syn→synack (window formation + the egress provider's dial),
// and first payload byte (the origin's response through the tunnel). This
// component timestamps those stages for the FIRST flows after a connect and
// emits one compact log line per completed measurement, so a slow first load
// attributes to a stage instead of being one opaque delay.
//
// Cost discipline: the hooks sit on the device's hottest paths (every egress
// and ingress packet), so the inactive fast path is a single atomic load and
// branch. The timeline deactivates itself once the flow budget is spent or
// the observation window has passed, after which the per-packet cost is that
// one branch. While active, the send hook reads a handful of fixed-offset
// header bytes (no allocation) and only takes the lock for tcp/443 syn or
// tracked-flow packets.

import (
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// firstLoadMaxFlows is how many tcp/443 flows are measured before the
	// timeline deactivates (the "first load" is the first handful of flows).
	firstLoadMaxFlows = 16
	// firstLoadMaxDnsQueries bounds the measured dns resolutions.
	firstLoadMaxDnsQueries = 24
	// firstLoadWindow is the hard observation deadline after activation;
	// whatever has not completed by then is logged as incomplete and the
	// timeline deactivates.
	firstLoadWindow = 60 * time.Second
	// firstLoadFlowExpiration retires an individual flow that never
	// completed (e.g. a synack that never came).
	firstLoadFlowExpiration = 30 * time.Second
)

// firstLoadFlowKey identifies a tracked egress tcp/443 flow from either
// direction: the remote (server) address and port plus the local (client)
// port.
type firstLoadFlowKey struct {
	remoteAddr netip.Addr
	remotePort uint16
	localPort  uint16
}

// firstLoadFlow is one tracked flow's timestamps (zero until observed).
type firstLoadFlow struct {
	synTime       time.Time
	synAckTime    time.Time
	firstByteTime time.Time
	logged        bool
}

// FirstLoadSample is one completed (or expired) measurement, exported for
// the device stats surface. Durations are -1 when the stage was never
// observed.
type FirstLoadSample struct {
	// Kind is "dns" or "tcp".
	Kind string
	// Target is the domain (dns) or remote addr:port (tcp).
	Target string
	// StartMillis is the sample's start offset from timeline activation.
	StartMillis int64
	// DnsMillis is query→answer (dns samples).
	DnsMillis int64
	// SynAckMillis is syn→synack (tcp samples).
	SynAckMillis int64
	// FirstByteMillis is syn→first payload byte (tcp samples).
	FirstByteMillis int64
}

// firstLoadTimeline measures the first flows after a connect. One per mux
// (the mux is rebuilt per connect, so activation aligns with connect start).
type firstLoadTimeline struct {
	log       Logger
	startTime time.Time

	// active is the per-packet fast-path gate; once false the hooks return
	// on a single atomic load.
	active atomic.Bool

	lock     sync.Mutex
	flows    map[firstLoadFlowKey]*firstLoadFlow
	flowsLru []firstLoadFlowKey
	// trackedFlowCount / dnsCount are the lifetime budgets spent
	trackedFlowCount int
	dnsCount         int
	dnsStarts        map[DohKey]time.Time
	samples          []*FirstLoadSample
}

func newFirstLoadTimeline(log Logger) *firstLoadTimeline {
	self := &firstLoadTimeline{
		log:       loggerOrDefault(log),
		startTime: time.Now(),
		flows:     map[firstLoadFlowKey]*firstLoadFlow{},
		dnsStarts: map[DohKey]time.Time{},
	}
	self.active.Store(true)
	return self
}

// expiredLocked deactivates past the observation window and reports whether
// the timeline is done. Callers hold lock.
func (self *firstLoadTimeline) expiredLocked(now time.Time) bool {
	if firstLoadWindow < now.Sub(self.startTime) {
		self.active.Store(false)
		return true
	}
	return false
}

// doneCheckLocked deactivates once both budgets are spent and every tracked
// flow has been logged (nothing left to observe). Callers hold lock.
func (self *firstLoadTimeline) doneCheckLocked() {
	if self.trackedFlowCount < firstLoadMaxFlows || self.dnsCount < firstLoadMaxDnsQueries {
		return
	}
	for _, flow := range self.flows {
		if !flow.logged {
			return
		}
	}
	self.active.Store(false)
}

// dnsStart marks the start of a dns resolution pipeline for key. Called by
// the mux when a new pipeline starts (coalesced queries share one).
func (self *firstLoadTimeline) dnsStart(key DohKey) {
	if !self.active.Load() {
		return
	}
	now := time.Now()
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.expiredLocked(now) {
		return
	}
	if firstLoadMaxDnsQueries <= self.dnsCount {
		return
	}
	if _, ok := self.dnsStarts[key]; ok {
		return
	}
	self.dnsCount += 1
	self.dnsStarts[key] = now
}

// dnsDone marks the end of a dns resolution pipeline (answer delivered, or
// the pipeline failed with ok=false).
func (self *firstLoadTimeline) dnsDone(key DohKey, ok bool) {
	if !self.active.Load() {
		return
	}
	now := time.Now()
	self.lock.Lock()
	defer self.lock.Unlock()
	startTime, tracked := self.dnsStarts[key]
	if !tracked {
		return
	}
	delete(self.dnsStarts, key)
	dnsMillis := now.Sub(startTime).Milliseconds()
	self.samples = append(self.samples, &FirstLoadSample{
		Kind:            "dns",
		Target:          key.Domain,
		StartMillis:     startTime.Sub(self.startTime).Milliseconds(),
		DnsMillis:       dnsMillis,
		SynAckMillis:    -1,
		FirstByteMillis: -1,
	})
	status := "ok"
	if !ok {
		status = "fail"
	}
	self.log.Infof("[firstload]dns %s %s %dms %s (t+%.1fs)\n",
		key.RecordType, key.Domain, dnsMillis, status, startTime.Sub(self.startTime).Seconds())
	self.doneCheckLocked()
}

// observeSend peeks an egress ip packet for a tcp/443 syn (flow start).
// Allocation-free; the caller gates on Active.
func (self *firstLoadTimeline) observeSend(packet []byte) {
	if !self.active.Load() {
		return
	}
	remoteAddr, remotePort, localPort, flags, _, ok := firstLoadTcpPeek(packet, false)
	if !ok || remotePort != 443 {
		return
	}
	// syn without ack starts a flow
	if flags&0x12 != 0x02 {
		return
	}
	now := time.Now()
	key := firstLoadFlowKey{remoteAddr: remoteAddr, remotePort: remotePort, localPort: localPort}
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.expiredLocked(now) {
		return
	}
	if _, ok := self.flows[key]; ok {
		return // a syn retransmit keeps the original start time
	}
	if firstLoadMaxFlows <= self.trackedFlowCount {
		return
	}
	self.trackedFlowCount += 1
	self.flows[key] = &firstLoadFlow{synTime: now}
	self.flowsLru = append(self.flowsLru, key)
	self.expireFlowsLocked(now)
}

// observeReceive peeks an ingress ip packet for a tracked flow's synack or
// first payload byte. Allocation-free; the caller gates on Active.
func (self *firstLoadTimeline) observeReceive(packet []byte) {
	if !self.active.Load() {
		return
	}
	remoteAddr, remotePort, localPort, flags, payloadLen, ok := firstLoadTcpPeek(packet, true)
	if !ok || remotePort != 443 {
		return
	}
	now := time.Now()
	key := firstLoadFlowKey{remoteAddr: remoteAddr, remotePort: remotePort, localPort: localPort}
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.expiredLocked(now) {
		return
	}
	flow, tracked := self.flows[key]
	if !tracked || flow.logged {
		return
	}
	if flags&0x12 == 0x12 && flow.synAckTime.IsZero() {
		flow.synAckTime = now
		return
	}
	if 0 < payloadLen && flow.firstByteTime.IsZero() {
		flow.firstByteTime = now
		self.logFlowLocked(key, flow)
		self.doneCheckLocked()
	}
}

// expireFlowsLocked logs-and-retires flows that never completed within
// firstLoadFlowExpiration. Called opportunistically from the syn path (new
// flows arriving is what makes old incomplete ones interesting to close out).
func (self *firstLoadTimeline) expireFlowsLocked(now time.Time) {
	for _, key := range self.flowsLru {
		flow, ok := self.flows[key]
		if !ok || flow.logged {
			continue
		}
		if now.Sub(flow.synTime) < firstLoadFlowExpiration {
			break // lru order: the rest are younger
		}
		self.logFlowLocked(key, flow)
	}
}

// logFlowLocked emits the flow's timeline line and records its sample.
// Callers hold lock.
func (self *firstLoadTimeline) logFlowLocked(key firstLoadFlowKey, flow *firstLoadFlow) {
	flow.logged = true
	synAckMillis := int64(-1)
	if !flow.synAckTime.IsZero() {
		synAckMillis = flow.synAckTime.Sub(flow.synTime).Milliseconds()
	}
	firstByteMillis := int64(-1)
	if !flow.firstByteTime.IsZero() {
		firstByteMillis = flow.firstByteTime.Sub(flow.synTime).Milliseconds()
	}
	target := fmt.Sprintf("%s:%d", key.remoteAddr, key.remotePort)
	self.samples = append(self.samples, &FirstLoadSample{
		Kind:            "tcp",
		Target:          target,
		StartMillis:     flow.synTime.Sub(self.startTime).Milliseconds(),
		DnsMillis:       -1,
		SynAckMillis:    synAckMillis,
		FirstByteMillis: firstByteMillis,
	})
	self.log.Infof("[firstload]tcp %s synack %dms firstbyte %dms (t+%.1fs)\n",
		target, synAckMillis, firstByteMillis, flow.synTime.Sub(self.startTime).Seconds())
}

// Samples returns the measurements recorded so far (completed dns
// resolutions and completed/expired flows), oldest first.
func (self *firstLoadTimeline) Samples() []*FirstLoadSample {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]*FirstLoadSample{}, self.samples...)
}

// firstLoadTcpPeek extracts (remote addr, remote port, local port, tcp
// flags, payload length) from a raw ip packet at fixed offsets, without
// allocating. ingress=false reads an egress packet (remote = destination);
// ingress=true reads an ingress packet (remote = source). ok is false for
// non-tcp, short, or extension-header packets (skipped for measurement).
func firstLoadTcpPeek(packet []byte, ingress bool) (remoteAddr netip.Addr, remotePort uint16, localPort uint16, flags byte, payloadLen int, ok bool) {
	if len(packet) < 20 {
		return
	}
	var l4 int
	var totalLen int
	switch packet[0] >> 4 {
	case 4:
		ihl := int(packet[0]&0x0f) * 4
		if ihl < 20 || packet[9] != 6 || len(packet) < ihl+20 {
			return
		}
		totalLen = int(packet[2])<<8 | int(packet[3])
		if totalLen < ihl || len(packet) < totalLen {
			totalLen = len(packet)
		}
		l4 = ihl
		if ingress {
			remoteAddr, _ = netip.AddrFromSlice(packet[12:16])
		} else {
			remoteAddr, _ = netip.AddrFromSlice(packet[16:20])
		}
	case 6:
		if len(packet) < 60 || packet[6] != 6 {
			return
		}
		payloadLen6 := int(packet[4])<<8 | int(packet[5])
		totalLen = 40 + payloadLen6
		if len(packet) < totalLen {
			totalLen = len(packet)
		}
		l4 = 40
		if ingress {
			remoteAddr, _ = netip.AddrFromSlice(packet[8:24])
		} else {
			remoteAddr, _ = netip.AddrFromSlice(packet[24:40])
		}
	default:
		return
	}
	if len(packet) < l4+20 {
		return
	}
	srcPort := uint16(packet[l4])<<8 | uint16(packet[l4+1])
	dstPort := uint16(packet[l4+2])<<8 | uint16(packet[l4+3])
	if ingress {
		remotePort = srcPort
		localPort = dstPort
	} else {
		remotePort = dstPort
		localPort = srcPort
	}
	dataOffset := int(packet[l4+12]>>4) * 4
	if dataOffset < 20 || totalLen < l4+dataOffset {
		return
	}
	flags = packet[l4+13]
	payloadLen = totalLen - l4 - dataOffset
	ok = true
	return
}
