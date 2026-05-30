package connect

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// grantingClientOob is a test ClientOob that answers CreateContract requests
// by issuing a signed contract from sourceId to the requested destination,
// using the destination's provide secret key (so the contract verifies on the
// receiver). Unlike NewNoContractClientOob (which fails CreateContract), this
// exercises the real contract-negotiation path — required to reproduce
// contract-coupled encryption behavior, e.g. the per-peer EncryptedControl
// carrier negotiating its own contracts alongside normal application data.
type grantingClientOob struct {
	sourceId Id
	settings *ContractManagerSettings
	// destSecretKey returns the provide secret key the destination uses to
	// sign contracts addressed to it. Looked up lazily so it can reference a
	// peer client created after this oob.
	destSecretKey func(destinationId Id) ([]byte, bool)
	// destClientPublicKey returns the destination's long-lived Ed25519 client
	// public key, sealed into the contract so the sender can verify the
	// destination's per-peer-session identity proof (establishing the cipher).
	destClientPublicKey func(destinationId Id) []byte
}

func (self *grantingClientOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	var out []*protocol.Frame
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		createContract, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		destinationId, err := IdFromBytes(createContract.DestinationId)
		if err != nil {
			continue
		}
		secretKey, ok := self.destSecretKey(destinationId)
		if !ok {
			continue
		}
		storedContract := &protocol.StoredContract{
			ContractId:        NewId().Bytes(),
			TransferByteCount: createContract.TransferByteCount,
			SourceId:          self.sourceId.Bytes(),
			DestinationId:     destinationId.Bytes(),
		}
		if self.destClientPublicKey != nil {
			storedContract.DestinationClientPublicKey = self.destClientPublicKey(destinationId)
		}
		storedContractBytes, err := ProtoMarshal(storedContract)
		if err != nil {
			continue
		}
		hmac := SignStoredContract(self.settings, secretKey, storedContractBytes)
		result := &protocol.CreateContractResult{
			Contract: &protocol.Contract{
				StoredContractBytes: storedContractBytes,
				StoredContractHmac:  hmac,
				ProvideMode:         protocol.ProvideMode_Network,
			},
		}
		resultFrame, err := ToFrame(result, DefaultProtocolVersion)
		if err != nil {
			continue
		}
		out = append(out, resultFrame)
	}
	if callback != nil {
		HandleError(func() {
			callback(out, nil)
		})
	}
}

// dataGatewayTransport is a send gateway that matches every destination
// EXCEPT ControlId. Control-plane frames (addressed to ControlId) are routed
// to controlBlackholeTransport instead, so they are never forwarded
// peer-to-peer by a catch-all gateway (which, with no platform to terminate
// them, would loop forever).
type dataGatewayTransport struct {
	transportId Id
}

func newDataGatewayTransport() *dataGatewayTransport {
	return &dataGatewayTransport{transportId: NewId()}
}

func (self *dataGatewayTransport) TransportId() Id { return self.transportId }
func (self *dataGatewayTransport) Priority() int   { return 100 }
func (self *dataGatewayTransport) Weight() float32 { return 0 }
func (self *dataGatewayTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}
func (self *dataGatewayTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	return 1.0 / float32(1+len(remainingStats))
}
func (self *dataGatewayTransport) MatchesSend(destination TransferPath) bool {
	return destination.DestinationId != ControlId
}
func (self *dataGatewayTransport) MatchesReceive(destination TransferPath) bool { return false }
func (self *dataGatewayTransport) Downgrade(source TransferPath)                {}

// controlBlackholeTransport sinks SEND traffic addressed to ControlId. The
// two-client test harness has no platform to terminate control-plane frames
// (e.g. the EncryptedKey publish, which correctly retries via ControlSync), so
// without a sink those frames would be forwarded peer-to-peer forever by the
// catch-all gateway transports. Routing them to a drained channel terminates
// them deterministically — no reliance on packet loss to break the loop.
type controlBlackholeTransport struct {
	transportId Id
}

func newControlBlackholeTransport() *controlBlackholeTransport {
	return &controlBlackholeTransport{transportId: NewId()}
}

func (self *controlBlackholeTransport) TransportId() Id { return self.transportId }
func (self *controlBlackholeTransport) Priority() int   { return TransportMaxPriority }
func (self *controlBlackholeTransport) Weight() float32 { return 0 }
func (self *controlBlackholeTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}
func (self *controlBlackholeTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	return 1.0
}
func (self *controlBlackholeTransport) MatchesSend(destination TransferPath) bool {
	return destination.DestinationId == ControlId
}
func (self *controlBlackholeTransport) MatchesReceive(destination TransferPath) bool { return false }
func (self *controlBlackholeTransport) Downgrade(source TransferPath)                {}

// blackholeControlId registers a controlBlackholeTransport on routeManager and
// drains it, sinking all ControlId-addressed traffic for the lifetime of ctx.
func blackholeControlId(ctx context.Context, routeManager *RouteManager) {
	transport := newControlBlackholeTransport()
	sink := make(chan []byte)
	routeManager.UpdateTransport(transport, []Route{sink})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sink:
			}
		}
	}()
}

// TestSendReceiveEncryptedWithContracts exercises bidirectional encrypted
// transfer when contracts are required and supplied on demand by a
// contract-granting oob. The send idle timeout is short so send sequences
// exit and flush their contract queue between bursts — the condition that, in
// the integration test, starved the EncryptedControl carrier of contracts
// when both per-peer encryption send sequences shared one ContractKey. With
// per-role ContractKeys the two sequences own separate queues, so one's
// exit-flush no longer discards the other's contracts and the handshake
// completes.
func TestSendReceiveEncryptedWithContracts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()

	aSend := make(chan []byte)
	bSend := make(chan []byte)

	aConditioner, bReceive := newConditioner(ctx, aSend)
	bConditioner, aReceive := newConditioner(ctx, bSend)
	aConditioner.update(func() { aConditioner.randomDelay = 20 * time.Millisecond })
	bConditioner.update(func() { bConditioner.randomDelay = 20 * time.Millisecond })

	// Data gateways match every destination except ControlId; ControlId
	// traffic is sunk by a blackhole (see blackholeControlId below) so the
	// EncryptedKey publish doesn't loop peer-to-peer in this platform-less
	// harness.
	aSendTransport := newDataGatewayTransport()
	aReceiveTransport := NewReceiveGatewayTransport()
	bSendTransport := newDataGatewayTransport()
	bReceiveTransport := NewReceiveGatewayTransport()

	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}

	makeSettings := func() *ClientSettings {
		s := DefaultClientSettings()
		s.SendBufferSettings.SequenceBufferSize = 0
		s.SendBufferSettings.AckBufferSize = 0
		s.SendBufferSettings.AckTimeout = 60 * time.Second
		// Short send idle so a send sequence exits (and flushes its contract
		// queue) between bursts.
		s.SendBufferSettings.IdleTimeout = 1 * time.Second
		s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
		s.ReceiveBufferSettings.SequenceBufferSize = 0
		s.ReceiveBufferSettings.GapTimeout = 60 * time.Second
		s.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
		s.ForwardBufferSettings.SequenceBufferSize = 0
		s.ForwardBufferSettings.IdleTimeout = 1 * time.Second
		s.ContractManagerSettings.LegacyCreateContract = false
		s.EncryptionSettings.Encrypt = true
		s.EncryptionSettings.TlsTimeout = 30 * time.Second
		// Exercise the symmetric (non-companion) EncryptedControl path.
		s.EncryptionSettings.EncryptionControlUseCompanion = false
		return s
	}

	var a, b *Client
	aOob := &grantingClientOob{
		sourceId: aClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return b.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return b.ClientKeyManager().PublicKey()
		},
	}
	bOob := &grantingClientOob{
		sourceId: bClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return a.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return a.ClientKeyManager().PublicKey()
		},
	}

	a = NewClient(ctx, aClientId, aOob, makeSettings())
	defer a.Cancel()
	a.RouteManager().UpdateTransport(aSendTransport, []Route{aSend})
	a.RouteManager().UpdateTransport(aReceiveTransport, []Route{aReceive})
	blackholeControlId(ctx, a.RouteManager())
	a.ContractManager().SetProvideModes(provideModes)

	b = NewClient(ctx, bClientId, bOob, makeSettings())
	defer b.Cancel()
	b.RouteManager().UpdateTransport(bSendTransport, []Route{bSend})
	b.RouteManager().UpdateTransport(bReceiveTransport, []Route{bReceive})
	blackholeControlId(ctx, b.RouteManager())
	b.ContractManager().SetProvideModes(provideModes)

	receivesA := make(chan string, 1024)
	receivesB := make(chan string, 1024)
	a.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ protocol.ProvideMode) {
		for _, frame := range frames {
			if m, err := FromFrame(frame); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					receivesA <- sm.Content
				}
			}
		}
	})
	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ protocol.ProvideMode) {
		for _, frame := range frames {
			if m, err := FromFrame(frame); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					receivesB <- sm.Content
				}
			}
		}
	})

	send := func(client *Client, dst Id, label string) {
		m := &protocol.SimpleMessage{Content: label}
		frame, err := ToFrame(m, DefaultProtocolVersion)
		if err != nil {
			panic(err)
		}
		client.Send(frame, DestinationId(dst), func(error) {})
	}

	// Drive BOTH directions in parallel: A and B each initiate to the other at
	// the same time, so each client holds four sequences for the peer —
	// send/receive on its client role (its own outbound handshake) and
	// send/receive on its server role (the peer's). Each send reuses its
	// per-role send sequence, so the handshake runs once and completes (rather
	// than restarting per burst) while keeping the per-peer sessions referenced
	// through it. Encryption is confirmed by (a) the per-peer client cipher
	// coming up in BOTH directions and (b) messages continuing to round-trip
	// after that — encrypted messages whose receipt necessarily waited for the
	// handshake.
	stop := make(chan struct{})
	defer close(stop)
	var gotA, gotB int64
	go func() {
		for i := 0; ; i += 1 {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			default:
			}
			send(a, bClientId, fmt.Sprintf("a%d", i))
			send(b, aClientId, fmt.Sprintf("b%d", i))
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-receivesA:
				atomic.AddInt64(&gotA, 1)
			case <-receivesB:
				atomic.AddInt64(&gotB, 1)
			}
		}
	}()

	cipherUp := func(client *Client, peer Id) bool {
		sess := client.EncryptionSessionManager().Lookup(peer, sequenceTlsRoleClient)
		return sess != nil && sess.Cipher() != nil
	}

	// Wait for the per-peer client cipher to establish in both directions.
	deadline := time.After(45 * time.Second)
	for !(cipherUp(a, bClientId) && cipherUp(b, aClientId)) {
		select {
		case <-deadline:
			t.Fatalf("encryption did not establish: a->b cipher=%t, b->a cipher=%t (a received %d, b received %d)",
				cipherUp(a, bClientId), cipherUp(b, aClientId), atomic.LoadInt64(&gotA), atomic.LoadInt64(&gotB))
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Both ciphers up: confirm encrypted messages keep round-tripping (sent now,
	// after the cipher, so they are wrapped).
	baseA, baseB := atomic.LoadInt64(&gotA), atomic.LoadInt64(&gotB)
	deadline2 := time.After(20 * time.Second)
	for atomic.LoadInt64(&gotA) < baseA+3 || atomic.LoadInt64(&gotB) < baseB+3 {
		select {
		case <-deadline2:
			t.Fatalf("messages stopped after cipher established: a received %d (want %d), b received %d (want %d)",
				atomic.LoadInt64(&gotA), baseA+3, atomic.LoadInt64(&gotB), baseB+3)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
