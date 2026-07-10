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
// except ControlId. Control-plane frames (addressed to ControlId) are routed
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

// controlBlackholeTransport sinks send traffic addressed to ControlId. The
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
	a.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if m, err := FromFrame(frame); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					receivesA <- sm.Content
				}
			}
		}
	})
	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
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

	// Drive both directions in parallel: A and B each initiate to the other at
	// the same time, so each client holds four sequences for the peer —
	// send/receive on its client role (its own outbound handshake) and
	// send/receive on its server role (the peer's). Each send reuses its
	// per-role send sequence, so the handshake runs once and completes (rather
	// than restarting per burst) while keeping the per-peer sessions referenced
	// through it. Encryption is confirmed by (a) the per-peer client cipher
	// coming up in both directions and (b) messages continuing to round-trip
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
		sess := client.EncryptionSessionManager().Lookup(peer, sequenceTlsRoleClient, false)
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

// TestEncryptedCompanionSessionsCreateSeparateContracts verifies that the
// per-peer encryption session key includes the companion bit: when A and B each
// send to the other in both companion and non-companion mode, the four
// application flows must run over four cleanly separated client sessions, and
// each peer's two server-role EncryptedControl reply carriers (one echoing the
// companion initiator, one the non-companion initiator) must not collapse onto
// a shared session/contract.
//
// Concretely there are eight send sequences:
//
//	A: client(companion=false)  client(companion=true)  server(companion=false)  server(companion=true)
//	B: client(companion=false)  client(companion=true)  server(companion=false)  server(companion=true)
//
// where each client-role sequence carries A/B's own application data + its
// ClientHello, and each server-role sequence is the reply carrier for the
// peer's corresponding flow. Each send sequence owns a distinct ContractKey
// (and therefore opens its own contract), so exactly four distinct contract
// keys appear in each client's local stats — eight total.
//
// Before companion was added to the session/sequence/contract keys, the two
// server-role reply carriers shared a key (same role, same
// EncryptionControlUseCompanion contract) and collapsed to three keys per
// client — this test pins that they stay separated.
func TestEncryptedCompanionSessionsCreateSeparateContracts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()

	aSend := make(chan []byte)
	bSend := make(chan []byte)

	aConditioner, bReceive := newConditioner(ctx, aSend)
	bConditioner, aReceive := newConditioner(ctx, bSend)
	aConditioner.update(func() { aConditioner.randomDelay = 20 * time.Millisecond })
	bConditioner.update(func() { bConditioner.randomDelay = 20 * time.Millisecond })

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
		// Long send idle so all eight sequences stay alive (and keep their
		// contracts open) through the assertion — this test is about how many
		// distinct sequences/contracts exist, not contract-flush churn.
		s.SendBufferSettings.IdleTimeout = 60 * time.Second
		s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
		s.ReceiveBufferSettings.SequenceBufferSize = 0
		s.ReceiveBufferSettings.GapTimeout = 60 * time.Second
		s.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
		s.ForwardBufferSettings.SequenceBufferSize = 0
		s.ForwardBufferSettings.IdleTimeout = 1 * time.Second
		s.ContractManagerSettings.LegacyCreateContract = false
		s.EncryptionSettings.Encrypt = true
		s.EncryptionSettings.TlsTimeout = 30 * time.Second
		// Server reply carriers ride regular (non-companion) contracts. This is
		// the case that exercises the ContractKey companion split: both server
		// carriers then share Destination/role/CompanionContract and differ only
		// by the session identity companion.
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

	// send drives one application message; companion=true rides a companion
	// contract (and so a companion-keyed per-peer session).
	send := func(client *Client, dst Id, label string, companion bool) {
		m := &protocol.SimpleMessage{Content: label}
		frame, err := ToFrame(m, DefaultProtocolVersion)
		if err != nil {
			panic(err)
		}
		if companion {
			client.SendWithTimeout(frame, DestinationId(dst), func(error) {}, 1*time.Second, CompanionContract())
		} else {
			client.SendWithTimeout(frame, DestinationId(dst), func(error) {}, 1*time.Second)
		}
	}

	// Drive all four flows continuously so every handshake starts (and its
	// peer's server reply carrier opens a contract), and so all eight sequences
	// stay referenced.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i += 1 {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			default:
			}
			send(a, bClientId, fmt.Sprintf("a%d", i), false)
			send(a, bClientId, fmt.Sprintf("ac%d", i), true)
			send(b, aClientId, fmt.Sprintf("b%d", i), false)
			send(b, aClientId, fmt.Sprintf("bc%d", i), true)
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	// distinctContractKeys returns the set of distinct ContractKeys for which
	// this client currently holds an open send contract — one per live send
	// sequence. Control-plane traffic (the EncryptedKey publish to ControlId)
	// is NoContract, so it never appears here.
	distinctContractKeys := func(client *Client) map[ContractKey]bool {
		keys := map[ContractKey]bool{}
		for _, key := range client.ContractManager().LocalStats().ContractOpenKeys {
			keys[key] = true
		}
		return keys
	}

	// Wait until each client has opened its four distinct send-sequence
	// contracts. If the companion sessions collapsed, a client would stall at
	// three (its two server reply carriers sharing one key) and this times out.
	deadline := time.After(45 * time.Second)
	for {
		aKeys := distinctContractKeys(a)
		bKeys := distinctContractKeys(b)
		if 4 <= len(aKeys) && 4 <= len(bKeys) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf(
				"expected 4 distinct send-sequence contracts per client (8 total); got a=%d b=%d — the companion and non-companion server reply carriers collapsed onto a shared contract",
				len(aKeys), len(bKeys),
			)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Confirm exactly four per client (eight total) with the expected
	// (role, companion) breakdown: for each role, one companion and one
	// non-companion sequence.
	type roleCompanion struct {
		role      sequenceTlsRole
		companion bool
	}
	assertBreakdown := func(client *Client, peer Id, tag string) {
		keys := distinctContractKeys(client)
		if len(keys) != 4 {
			t.Fatalf("%s: expected exactly 4 distinct contract keys, got %d: %+v", tag, len(keys), keys)
		}
		combos := map[roleCompanion]int{}
		for key := range keys {
			if key.Destination.DestinationId != peer {
				t.Fatalf("%s: unexpected contract destination %s (want peer %s)", tag, key.Destination.DestinationId, peer)
			}
			combos[roleCompanion{role: key.EncryptionRole, companion: key.EncryptionCompanion}] += 1
		}
		for _, role := range []sequenceTlsRole{sequenceTlsRoleClient, sequenceTlsRoleServer} {
			for _, companion := range []bool{false, true} {
				rc := roleCompanion{role: role, companion: companion}
				if combos[rc] != 1 {
					t.Fatalf("%s: expected exactly one contract for %v companion=%t, got %d (combos=%v)", tag, role, companion, combos[rc], combos)
				}
			}
		}
	}
	assertBreakdown(a, bClientId, "a")
	assertBreakdown(b, aClientId, "b")
}
