package connect

// IpMux is a composable packet stage between a downstream packet source and an
// upstream UserNat. It conforms to the UserNat send/receive shapes so muxes chain
// transparently:
//
//   - SendPacket is called by the downstream. A concrete mux's onSend hook may
//     claim (and locally terminate) a packet; anything not claimed is forwarded to
//     the wrapped upstream send.
//   - Receive is installed as the upstream's receive callback. Packets addressed to
//     the mux's internal Tun address are delivered into the Tun (they are replies to
//     the Tun's own upstream connections); everything else is dispatched to the
//     registered receivers (toward the downstream / OS TUN).
//   - The internal Tun "sends out of the upstream": packets the userspace stack
//     emits are forwarded to the upstream send.
//
// The internal Tun reserves one address from the shared local-address pool, so a
// mux address never collides with the tunnel address or another mux.

import (
	"context"
	"net"
	"net/netip"
	"runtime/debug"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// IpMuxSend matches the UserNat send signature.
type IpMuxSend = func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool

// ipMuxOnSend lets a concrete mux claim and terminate a send-path packet. It
// returns true if it handled the packet, or false to forward it upstream.
type ipMuxOnSend = func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool

// ipMuxOnPump lets a concrete mux intercept a packet the internal stack emits
// before it is forwarded upstream — e.g. to un-NAT a locally-terminated connection's
// reply and deliver it downstream. It returns true if it handled the packet, or false
// to forward it upstream as usual.
type ipMuxOnPump = func(packet []byte) bool

type IpMux struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	tun            *Tun
	localAddresses []netip.Addr

	onSend ipMuxOnSend
	onPump ipMuxOnPump

	// source/provideMode/sendTimeout stamp the Tun's upstream-originated packets
	// (the local servers' own upstream connections).
	source      TransferPath
	provideMode protocol.ProvideMode
	sendTimeout time.Duration

	stateLock sync.Mutex
	upstream  IpMuxSend

	receivers *CallbackList[ReceivePacketFunction]
}

func NewIpMux(
	ctx context.Context,
	tun *Tun,
	source TransferPath,
	provideMode protocol.ProvideMode,
	sendTimeout time.Duration,
	onSend ipMuxOnSend,
	onPump ipMuxOnPump,
	initialReceiver ReceivePacketFunction,
	log Logger,
) *IpMux {
	cancelCtx, cancel := context.WithCancel(ctx)
	receivers := NewCallbackList[ReceivePacketFunction]()
	if initialReceiver != nil {
		receivers.Add(initialReceiver)
	}
	self := &IpMux{
		ctx:            cancelCtx,
		cancel:         cancel,
		log:            loggerOrDefault(log),
		tun:            tun,
		localAddresses: tun.LocalAddresses(),
		onSend:         onSend,
		onPump:         onPump,
		source:         source,
		provideMode:    provideMode,
		sendTimeout:    sendTimeout,
		receivers:      receivers,
	}
	go HandleError(self.pump)
	return self
}

// SetUpstream wires the wrapped upstream send. It is settable after construction
// because the upstream (e.g. a RemoteUserNatMultiClient) is created with the mux's
// Receive as its callback, so the two have a circular dependency.
func (self *IpMux) SetUpstream(upstream IpMuxSend) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.upstream = upstream
}

func (self *IpMux) getUpstream() IpMuxSend {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.upstream
}

// AddReceiver registers a downstream receiver; returns an unregister closure.
func (self *IpMux) AddReceiver(receiver ReceivePacketFunction) func() {
	callbackId := self.receivers.Add(receiver)
	return func() {
		self.receivers.Remove(callbackId)
	}
}

// Tun exposes the internal userspace stack so a concrete mux can run local
// servers (ListenTCP/ListenUDP), dial upstream (DialContext), and resolve (DohCache).
func (self *IpMux) Tun() *Tun {
	return self.tun
}

// SendPacket is the downstream entry point: a claimed packet is terminated locally
// by the concrete mux; otherwise it is forwarded to the upstream.
func (self *IpMux) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	if self.onSend != nil && self.onSend(source, provideMode, packet, timeout) {
		return true
	}
	upstream := self.getUpstream()
	if upstream == nil {
		return false
	}
	return upstream(source, provideMode, packet, timeout)
}

// Receive is installed as the upstream's receive callback. Packets addressed to the
// mux's Tun address are delivered into the internal stack; the rest go downstream.
func (self *IpMux) Receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	if ipPath != nil && self.isLocalDestination(ipPath.DestinationIp) {
		self.tun.Write(packet)
		return
	}
	self.deliverDownstream(source, provideMode, ipPath, packet)
}

// deliverDownstream dispatches a packet to the registered receivers. A concrete mux
// also uses this to inject locally-generated replies (e.g. DNS responses) toward
// the downstream.
func (self *IpMux) deliverDownstream(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	for _, receiver := range self.receivers.Get() {
		safeReceive(receiver, source, provideMode, ipPath, packet)
	}
}

// safeReceive calls one receiver with the panic isolation that wrapping it in
// HandleError would give, but without allocating a closure. This runs once per
// received packet per receiver (the whole inbound path), so it must not allocate:
// the deferred recover closure captures no variables, so it is a static func value
// (see safeAck).
func safeReceive(receiver ReceivePacketFunction, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	defer func() {
		if r := recover(); r != nil {
			if IsDoneError(r) {
				// the context was canceled and raised; standard pattern, do not log
			} else {
				DefaultLogger().Warningf("Unexpected error: %s\n", ErrorJson(r, debug.Stack()))
			}
		}
	}()
	receiver(source, provideMode, ipPath, packet)
}

func (self *IpMux) isLocalDestination(dst net.IP) bool {
	addr, ok := netIPAddr(dst)
	if !ok {
		return false
	}
	for _, local := range self.localAddresses {
		if local == addr {
			return true
		}
	}
	return false
}

// pump forwards packets the internal stack emits (the local servers' upstream
// connections) out via the upstream send.
//
// NOTE: this is correct while the only stack-originated traffic is upstream-bound
// (DNS resolution in step 3 via the Tun's DohCache/DialContext). Step 4 (HTTP), which
// terminates client TCP on the stack, will also emit client-bound replies here and
// will need to route stack output by destination.
func (self *IpMux) pump() {
	for {
		packet, err := self.tun.Read()
		if err != nil {
			// tun.Read only fails once the tun ctx is canceled (Close); the pump is done
			return
		}
		// isolate per-packet processing: a panic handling one packet (a parse/rewrite edge
		// case, or deep in the upstream send) must not kill the pump. Otherwise the tun's
		// stack-originated traffic (DoH resolution, HTTP upgrade) would stop being forwarded
		// while the tun stays open, and DNS would break until the mux is recreated (restart).
		HandleError(func() {
			if self.onPump != nil && self.onPump(packet) {
				return
			}
			if upstream := self.getUpstream(); upstream != nil {
				upstream(self.source, self.provideMode, packet, self.sendTimeout)
			}
		})
	}
}

func (self *IpMux) Close() {
	self.cancel()
	self.tun.Close()
}
