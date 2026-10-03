package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

type multiH1ResidenceWire struct {
	carrier int
	fromA   bool
	bytes   []byte
	at      time.Time
}

type multiH1ResidenceCarrier struct {
	pair         *h1TLSResidencePair
	sendA, sendB Transport
	routes       []Route
	beforeWrite  <-chan struct{}
	writeReached chan struct{}
}

// The clients, logical sequence, peer identity and callback stay the same
// across physical H1 connections. The synthetic transport supplies framing,
// ordered encrypted residence and bounded queues, not host TCP or NAT sockets.
type multiH1ResidenceFixture struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	clientA, clientB       *Client
	settingsA, settingsB   *ClientSettings
	log                    *recordingLogger
	workers                sync.WaitGroup
	carriers               []*multiH1ResidenceCarrier
	errors                 chan error
	wire                   chan multiH1ResidenceWire
	ackRelease             chan struct{}
	ackReleaseOnce         sync.Once
	firstAck               atomic.Bool
	predecessors, requests atomic.Uint64
	pings                  atomic.Uint64
	requestAt              atomic.Int64
	requestFailureAt       atomic.Int64
}

func newMultiH1ResidenceFixture(t *testing.T, certificate tls.Certificate, lifetime time.Duration) *multiH1ResidenceFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &multiH1ResidenceFixture{ctx: ctx, cancel: cancel, log: newRecordingLogger(),
		errors: make(chan error, 16), wire: make(chan multiH1ResidenceWire, 256), ackRelease: make(chan struct{})}
	settings := func(timeout time.Duration) *ClientSettings {
		s := DefaultClientSettings()
		s.Log = f.log
		s.EncryptionSettings.Mode = EncryptionModeOff
		s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		s.SendBufferSettings.AckTimeout = timeout
		s.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(128 * 1024)
		s.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		return s
	}
	f.settingsA, f.settingsB = settings(lifetime), settings(time.Minute)
	f.settingsA.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
		if event.Phase == SendPackLifecyclePhaseTerminal && event.MessageType == protocol.MessageType_IpIpPacketToProvider && event.Err != nil {
			f.requestFailureAt.CompareAndSwap(0, time.Now().UnixNano())
		}
	}
	f.clientA = NewClient(ctx, NewId(), NewNoContractClientOob(), f.settingsA)
	f.clientB = NewClient(ctx, NewId(), NewNoContractClientOob(), f.settingsB)
	ready := false
	defer func() {
		if !ready {
			f.close(t)
		}
	}()
	f.clientA.ContractManager().AddNoContractPeer(f.clientB.ClientId())
	f.clientB.ContractManager().AddNoContractPeer(f.clientA.ClientId())
	f.clientB.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		if source.SourceId != f.clientA.ClientId() {
			f.report(fmt.Errorf("replacement changed application source identity"))
			return
		}
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_IpIpPing {
				f.pings.Add(1)
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			var path IpPath
			payload, parseErr := parseIpPathWithPayloadBorrowed(packet, &path)
			if err != nil || parseErr != nil || path.Protocol != IpProtocolTcp {
				f.report(fmt.Errorf("bad synthetic TCP request: decode=%v parse=%v", err, parseErr))
				continue
			}
			switch string(payload) {
			case "predecessor":
				f.predecessors.Add(1)
			case "request":
				f.requests.Add(1)
				f.requestAt.CompareAndSwap(0, time.Now().UnixNano())
			default:
				f.report(fmt.Errorf("unexpected synthetic TCP payload"))
			}
		}
	})
	f.addCarrier(t, certificate, false)
	ready = true
	return f
}

func (f *multiH1ResidenceFixture) report(err error) {
	if f.ctx.Err() != nil {
		return
	}
	select {
	case f.errors <- err:
	default:
		panic("bounded H1 health error collector overflow")
	}
}

func (f *multiH1ResidenceFixture) releaseAck() {
	f.ackReleaseOnce.Do(func() { close(f.ackRelease) })
}

func (f *multiH1ResidenceFixture) addCarrier(t *testing.T, certificate tls.Certificate, blocked bool) *multiH1ResidenceCarrier {
	t.Helper()
	pair := newH1TLSResidencePair(t, certificate)
	if blocked {
		pair.gate.arm(0)
	}
	index := len(f.carriers)
	c := &multiH1ResidenceCarrier{pair: pair}
	f.carriers = append(f.carriers, c)
	for _, endpoint := range []struct {
		client *Client
		remote Id
		framed *FramedMessageConn
		fromA  bool
	}{{f.clientA, f.clientB.ClientId(), pair.client, true}, {f.clientB, f.clientA.ClientId(), pair.peer, false}} {
		out, in := make(Route, 64), make(Route, 64)
		c.routes = append(c.routes, out, in)
		send := &h1SendClientTransportForGroupTest{NewSendClientTransport(DestinationId(endpoint.remote))}
		if endpoint.fromA {
			c.sendA = send
		} else {
			c.sendB = send
		}
		endpoint.client.RouteManager().UpdateTransport(send, []Route{out})
		endpoint.client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{in})
		f.workers.Add(2)
		go func() {
			defer f.workers.Done()
			for {
				select {
				case <-f.ctx.Done():
					return
				case wire := <-out:
					if !endpoint.fromA && !f.firstAck.Swap(true) {
						// Hold the actual peer-generated predecessor ACK, not a
						// fabricated timestamp or an injected ACK message.
						select {
						case <-f.ackRelease:
						case <-f.ctx.Done():
							MessagePoolReturn(wire)
							return
						}
					}
					select {
					case f.wire <- multiH1ResidenceWire{index, endpoint.fromA, append([]byte(nil), wire...), time.Now()}:
					default:
						f.report(fmt.Errorf("bounded H1 health wire collector overflow"))
					}
					if endpoint.fromA && c.beforeWrite != nil {
						select {
						case c.writeReached <- struct{}{}:
						default:
						}
						select {
						case <-c.beforeWrite:
						case <-f.ctx.Done():
							MessagePoolReturn(wire)
							return
						}
					}
					err := endpoint.framed.WriteMessage(websocket.BinaryMessage, wire)
					MessagePoolReturn(wire)
					if err != nil {
						f.report(err)
						return
					}
				}
			}
		}()
		go func() {
			defer f.workers.Done()
			for {
				_, wire, err := ReadH1PooledMessage(endpoint.framed, 4096)
				if err != nil {
					f.report(err)
					return
				}
				select {
				case in <- wire:
				case <-f.ctx.Done():
					MessagePoolReturn(wire)
					return
				}
			}
		}()
	}
	return c
}

func (f *multiH1ResidenceFixture) close(t *testing.T) {
	t.Helper()
	f.cancel()
	f.releaseAck()
	for _, c := range f.carriers {
		c.pair.close()
	}
	f.workers.Wait()
	for _, client := range []*Client{f.clientA, f.clientB} {
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join real H1 health client: %v", err)
		}
	}
	for _, c := range f.carriers {
		for _, route := range c.routes {
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}
		if c.pair.gate.owned != 0 {
			t.Error("retired physical carrier still owns encrypted bytes")
		}
	}
	for _, settings := range []*ClientSettings{f.settingsA, f.settingsB} {
		budget := settings.SendBufferSettings.ResendQueueBudget
		reserved, released := budget.Counts()
		if budget.UsedByteCount() != 0 || reserved != released {
			t.Errorf("retired H1 health Transfer budget: used=%d reserved=%d released=%d", budget.UsedByteCount(), reserved, released)
		}
	}
}

// A cumulative ACK can name a later BusyProbe Pack after ACK compression.
// Resolve that name against actual outbound wire, not callback success or a
// lexicographic message-id comparison. A selective ACK never covers an earlier
// request, and feedback for another sequence or peer proves nothing here.
func multiH1ResidenceAckCoversRequest(
	request *protocol.Pack,
	outbound map[string]*protocol.Pack,
	frame *protocol.TransferFrame,
	fromA bool,
	client, peer Id,
) bool {
	if request == nil || frame == nil || fromA || frame.TransferPath == nil {
		return false
	}
	ack := frame.GetAck()
	if ack == nil || ack.Selective || len(ack.MissingContractId) != 0 ||
		!bytes.Equal(ack.SequenceId, request.SequenceId) ||
		!bytes.Equal(frame.TransferPath.SourceId, peer.Bytes()) ||
		!bytes.Equal(frame.TransferPath.DestinationId, client.Bytes()) {
		return false
	}
	head := outbound[string(ack.MessageId)]
	return head != nil && !head.Nack &&
		bytes.Equal(head.SequenceId, request.SequenceId) &&
		request.SequenceNumber <= head.SequenceNumber
}

func TestMultiClientH1ResidenceCumulativeAckOracle(t *testing.T) {
	client, peer, sequence := NewId(), NewId(), NewId()
	request := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: sequence.Bytes(), SequenceNumber: 1}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"later_busy_probe_covers_request", true},
		{"exact_request", true},
		{"earlier_busy_probe_does_not_cover", false},
		{"selective_later_probe_does_not_cover", false},
		{"unknown_later_message", false},
		{"different_ack_sequence", false},
		{"different_probe_sequence", false},
		{"different_peer", false},
		{"different_client", false},
		{"wrong_direction", false},
		{"contract_request_not_delivery", false},
		{"no_ack_probe", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: sequence.Bytes(), SequenceNumber: 3,
				Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPing}}}
			ack := &protocol.Ack{MessageId: probe.MessageId, SequenceId: sequence.Bytes()}
			frame := &protocol.TransferFrame{Ack: ack,
				TransferPath: &protocol.TransferPath{SourceId: peer.Bytes(), DestinationId: client.Bytes()}}
			outbound := map[string]*protocol.Pack{string(request.MessageId): request, string(probe.MessageId): probe}
			fromA := false
			switch tc.name {
			case "exact_request":
				ack.MessageId = request.MessageId
			case "earlier_busy_probe_does_not_cover":
				probe.SequenceNumber = 0
			case "selective_later_probe_does_not_cover":
				ack.Selective = true
			case "unknown_later_message":
				ack.MessageId = NewId().Bytes()
			case "different_ack_sequence":
				ack.SequenceId = NewId().Bytes()
			case "different_probe_sequence":
				probe.SequenceId = NewId().Bytes()
			case "different_peer":
				frame.TransferPath.SourceId = NewId().Bytes()
			case "different_client":
				frame.TransferPath.DestinationId = NewId().Bytes()
			case "wrong_direction":
				fromA = true
			case "contract_request_not_delivery":
				ack.MissingContractId = NewId().Bytes()
			case "no_ack_probe":
				probe.Nack = true
			}
			if got := multiH1ResidenceAckCoversRequest(request, outbound, frame, fromA, client, peer); got != tc.want {
				t.Fatalf("wire-backed cumulative request coverage=%t want=%t", got, tc.want)
			}
		})
	}
}

// Pin the exact captured predecessor progress, rather than treating it as an
// end-to-end receipt for the still-held request. The 34.369973417s classifier
// eligibility is not an execution timestamp: the real 1.25s poll owns that.
// BusyProbe remains enabled and sends real IpPing Packs. Its fixed-single-exit
// sibling gate, not a synthetic probe ACK, holds the early 3s watchdog verdict.
func TestMultiClientH1ResidenceHealthAndSamePeerReplacement(t *testing.T) {
	certificate := h1TLSResidenceCertificate(t)
	const predecessorAt = 4369973417 * time.Nanosecond
	const recoveryAt = 36800 * time.Millisecond
	const replacementAt = 8 * time.Second
	for _, tc := range []struct {
		name      string
		lifetime  time.Duration
		replace   bool
		permanent bool
		shared    bool
		writer    bool
	}{
		{"30s_finite", 30 * time.Second, false, false, false, false},
		{"60s_finite_health_still_wins", 60 * time.Second, false, false, false, false},
		{"30s_blackhole", 30 * time.Second, false, true, false, false},
		{"60s_blackhole_health_still_wins", 60 * time.Second, false, true, false, false},
		{"30s_same_peer_replacement_finite", 30 * time.Second, true, false, false, false},
		{"30s_same_peer_replacement_blackhole", 30 * time.Second, true, true, false, false},
		{"30s_same_peer_replacement_shared_congestion", 30 * time.Second, true, false, true, false},
		{"30s_same_peer_replacement_writer_queue", 30 * time.Second, true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				f := newMultiH1ResidenceFixture(t, certificate, tc.lifetime)
				defer f.close(t)
				channel := newPacketTransferTestChannel()
				channel.settings = DefaultMultiClientSettings()
				channel.ctx, channel.cancel = context.WithCancel(f.ctx)
				defer channel.cancel()
				channel.client, channel.log = f.clientA, f.log
				channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.clientB.ClientId()), FixedDestination: true}
				channel.clientReceiveUnsub = func() {}
				channel.flowCountFunc = func(*multiClientChannel) int { return 1 }
				// A stale uplink holds receive-only judgments, but it must not
				// silently suppress reliable no-send-ACK eligibility.
				channel.uplinkGateFunc = func(time.Time) (bool, time.Time) { return true, time.Time{} }
				window := watchdogTestWindow(channel)
				window.ctx, window.settings, window.log = channel.ctx, channel.settings, f.log
				path := rebindTcpPath(12, 60353)
				path.Syn = false
				send := func(sequence uint32, content string) {
					packet := ipOosTcpPacketSequence(path, tcpFlagAck, sequence, []byte(content))
					accepted, err := channel.SendDetailedWithAck(&parsedPacket{packet: packet, ipPath: path}, time.Second, true)
					if !accepted || err != nil {
						MessagePoolReturn(packet)
						t.Fatalf("real MultiClient request admission=%t/%v", accepted, err)
					}
				}
				send(100, "predecessor")
				synctest.Wait()
				if f.predecessors.Load() != 1 || !f.firstAck.Load() {
					t.Fatal("predecessor lacks a real peer receipt and held ACK")
				}
				for len(f.wire) > 0 {
					<-f.wire
				}
				time.Sleep(time.Nanosecond)
				start := time.Now()
				first := f.carriers[0]
				writerRelease := make(chan struct{})
				if tc.writer {
					first.beforeWrite, first.writeReached = writerRelease, make(chan struct{}, 1)
				} else {
					first.pair.gate.arm(0)
				}
				send(200, "request")
				synctest.Wait()
				if len(f.wire) != 1 {
					t.Fatalf("initial request physical writes=%d", len(f.wire))
				}
				requestWire := <-f.wire
				request := decodeSendPackLifecycleWirePack(t, requestWire.bytes)
				if request.Nack || !tc.writer && first.pair.gate.length != len(requestWire.bytes)+26 {
					t.Fatal("held TLS record does not own the exact ACK-required request Pack")
				}
				if tc.writer {
					select {
					case <-first.writeReached:
					default:
						t.Fatal("writer queue gate not reached")
					}
					if first.pair.gate.length != 0 {
						t.Fatal("writer-queued request had a physical TLS write")
					}
				}
				ackTimer := time.AfterFunc(predecessorAt, f.releaseAck)
				defer ackTimer.Stop()
				var healthAt atomic.Int64
				healthDone, stallsDone := make(chan struct{}), make(chan struct{})
				go func() { defer close(healthDone); channel.detectBlackhole(); healthAt.Store(int64(time.Since(start))) }()
				go func() { defer close(stallsDone); window.watchSendStalls() }()
				defer func() { channel.cancel(); <-healthDone; <-stallsDone }()
				synctest.Wait()
				advance := func(elapsed time.Duration) {
					t.Helper()
					if elapsed < time.Since(start) {
						t.Fatalf("test deadline moved backwards: now=%s target=%s", time.Since(start), elapsed)
					}
					time.Sleep(elapsed - time.Since(start))
					synctest.Wait()
				}
				advance(predecessorAt)
				stats, err := channel.WindowStats()
				if err != nil || stats.sendAckCount != 1 || stats.sendNackCount != 1 || !stats.pendingSendTime.Equal(start.Add(predecessorAt)) {
					t.Fatalf("predecessor ACK did not restart only outstanding-send liveness: stats=%+v err=%v", stats, err)
				}
				advance(replacementAt - time.Nanosecond)
				if channel.IsDone() || !channel.hasActiveTransport() || channel.hasActiveUnreliableSendTransport() ||
					!channel.busyProbeAckTime.IsZero() || len(f.log.linesWith("no receiving sibling: uplink unproven")) == 0 {
					t.Fatal("early watchdog was not held by the actual single-exit sibling gate")
				}
				advance(replacementAt)
				if tc.replace {
					// Experimental routing action only: one new framed TLS stream,
					// same clients/provider/Transfer sequence. This is not the
					// PlatformTransport authentication/migration state machine.
					// The fixture does not price a real-network TLS handshake or
					// implement a production recovery trigger.
					f.addCarrier(t, certificate, tc.permanent || tc.shared)
					f.clientA.RouteManager().RemoveTransport(first.sendA)
					f.clientB.RouteManager().RemoveTransport(first.sendB)
					synctest.Wait()
				}
				advance(30*time.Second - time.Nanosecond)
				stats, err = channel.WindowStats()
				if err != nil || channel.IsDone() {
					t.Fatalf("flow terminated before its unchanged lifetime: %v", err)
				}
				survives := tc.replace && !tc.permanent && !tc.shared
				if survives && (f.requests.Load() != 1 || stats.sendNackCount != 0 || stats.sendAckCount != 2) {
					t.Fatalf("independent same-peer stream did not recover the existing request: receipts=%d stats=%+v", f.requests.Load(), stats)
				}
				advance(30 * time.Second)
				if !survives && tc.lifetime == 30*time.Second {
					_, err = channel.WindowStats()
					if err == nil || !strings.Contains(err.Error(), "Send sequence closed.") {
						t.Fatalf("30s logical owner did not terminate: %v", err)
					}
					if got := time.Duration(f.requestFailureAt.Load() - start.UnixNano()); got != 30*time.Second {
						t.Fatalf("physical replacement changed the absolute request lifetime: %s", got)
					}
					// ACK expiry and the 30s poll can be ready together. The
					// callback's 30s verdict is exact; joining health is bounded by
					// one unchanged poll, not by select ordering at the tie.
					advance(30*time.Second + blackholePollInterval)
					if !channel.IsDone() || healthAt.Load() < int64(30*time.Second) || healthAt.Load() > int64(30*time.Second+blackholePollInterval) {
						t.Fatalf("health did not join the terminal request within one poll: at=%s", time.Duration(healthAt.Load()))
					}
				} else if !survives {
					eligibility := predecessorAt + channel.noSendAckTimeout()
					advance(eligibility - time.Nanosecond)
					stats, err = channel.WindowStats()
					if err != nil {
						t.Fatal(err)
					}
					reason, _ := blackholeReasonFromStats(time.Now(), stats, channel.noSendAckTimeout(), 0, time.Hour, blackholeGates{uplinkStale: true})
					if reason != blackholeNone {
						t.Fatalf("early no-send-ACK eligibility: %s", reason)
					}
					advance(eligibility)
					stats, err = channel.WindowStats()
					if err != nil {
						t.Fatal(err)
					}
					reason, held := blackholeReasonFromStats(time.Now(), stats, channel.noSendAckTimeout(), 0, time.Hour, blackholeGates{uplinkStale: true})
					if reason != blackholeNoSendAck || held != blackholeNone || channel.IsDone() {
						t.Fatalf("classifier/poll boundary conflated: reason=%s held=%s done=%t", reason, held, channel.IsDone())
					}
					advance(35 * time.Second)
					_, err = channel.WindowStats()
					if !channel.IsDone() || healthAt.Load() != int64(35*time.Second) || err == nil || !strings.Contains(err.Error(), "Blackhole no-send-ack") {
						t.Fatalf("real poll did not own 35s terminal: done=%t at=%s err=%v", channel.IsDone(), time.Duration(healthAt.Load()), err)
					}
				}
				if !tc.permanent {
					advance(recoveryAt)
					if tc.writer {
						close(writerRelease)
					} else {
						close(first.pair.gate.release)
					}
					if tc.shared {
						close(f.carriers[1].pair.gate.release)
					}
					synctest.Wait()
					if f.requests.Load() != 1 || f.predecessors.Load() != 1 {
						t.Fatalf("old stream release duplicated downstream acceptance: requests=%d predecessor=%d", f.requests.Load(), f.predecessors.Load())
					}
				}
				if len(f.errors) != 0 {
					t.Fatal(<-f.errors)
				}
				if tc.replace && len(f.carriers) != 2 {
					t.Fatalf("manual mechanism control changed carrier count: %d", len(f.carriers))
				}
				if survives {
					stats, err = channel.WindowStats()
					// The predecessor has aged out of the 30s telemetry window;
					// the surviving request's ACK and lifetime state have not.
					if err != nil || channel.IsDone() || stats.sendAckCount < 1 || stats.sendNackCount != 0 {
						t.Fatalf("late old stream changed recovered channel: %v", err)
					}
				}
				requestCopies, replacementCopies, coveringAcks, exactNamingAcks := 0, 0, 0, 0
				outbound := map[string]*protocol.Pack{string(request.MessageId): request}
				type decodedWire struct {
					fromA bool
					frame *protocol.TransferFrame
				}
				decoded := make([]decodedWire, 0, len(f.wire))
				for len(f.wire) > 0 {
					event := <-f.wire
					frame := &protocol.TransferFrame{}
					if err := ProtoUnmarshal(event.bytes, frame); err != nil {
						t.Fatal(err)
					}
					decoded = append(decoded, decodedWire{event.fromA, frame})
					if pack := frame.GetPack(); pack != nil && event.fromA {
						if previous := outbound[string(pack.MessageId)]; previous != nil &&
							(!bytes.Equal(previous.SequenceId, pack.SequenceId) || previous.SequenceNumber != pack.SequenceNumber) {
							t.Fatal("outbound message identity changed across physical retries")
						}
						outbound[string(pack.MessageId)] = pack
					}
					if pack := frame.GetPack(); pack != nil && bytes.Equal(pack.MessageId, request.MessageId) {
						if !bytes.Equal(pack.SequenceId, request.SequenceId) || pack.Nack {
							t.Fatal("replacement changed reliable Pack identity")
						}
						requestCopies++
						if event.carrier == 1 {
							replacementCopies++
						}
					}
				}
				for _, event := range decoded {
					if multiH1ResidenceAckCoversRequest(request, outbound, event.frame, event.fromA, f.clientA.ClientId(), f.clientB.ClientId()) {
						coveringAcks++
						if bytes.Equal(event.frame.GetAck().MessageId, request.MessageId) {
							exactNamingAcks++
						}
					}
				}
				if survives && (replacementCopies == 0 || coveringAcks == 0) {
					t.Fatalf("missing exact request/cumulative ACK coverage: replacement_copies=%d covering_acks=%d", replacementCopies, coveringAcks)
				}
				if !survives && tc.permanent && f.requests.Load() != 0 {
					t.Fatal("permanent blackhole produced data receipt")
				}
				if tc.replace && (tc.shared || tc.permanent) && (requestCopies > 5 || replacementCopies > 4) {
					t.Fatalf("shared stall amplified replay beyond one immediate replacement flight: copies=%d replacement=%d", requestCopies, replacementCopies)
				}
				requestElapsed := time.Duration(0)
				if f.requestAt.Load() != 0 {
					requestElapsed = time.Duration(f.requestAt.Load() - start.UnixNano())
				}
				t.Logf("lifetime=%s replace=%t permanent=%t survived=%t no_send_ack_eligible=%s health_return=%s requests=%d request_receipt=%s same_pack_copies=%d replacement_copies=%d covering_peer_acks=%d exact_naming_peer_acks=%d busy_sibling_holds=%d busy_acquittals=%d", tc.lifetime, tc.replace, tc.permanent, survives, predecessorAt+30*time.Second, time.Duration(healthAt.Load()), f.requests.Load(), requestElapsed, requestCopies, replacementCopies, coveringAcks, exactNamingAcks, len(f.log.linesWith("no receiving sibling: uplink unproven")), len(f.log.linesWith("liveness probe answered")))
			})
		})
	}
}
