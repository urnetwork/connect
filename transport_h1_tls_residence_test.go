package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// This socket admits bounded encrypted writes, in order, while withholding
// one TLS record's final byte. It models ordered carrier residence, not TCP's
// retransmission algorithm. The independent real-gVisor ten-drop control in
// Server proves that finite TCP recovery can cross 30 seconds. Neither test
// retrospectively identifies the ciphertext that held historical Pack 72.
type h1TLSResidenceWrite struct {
	bytes   []byte
	hold    bool
	drained chan struct{}
}

type h1TLSResidenceConn struct {
	net.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	writes  chan h1TLSResidenceWrite
	done    chan struct{}
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	mutex   sync.Mutex
	armed   bool
	held    bool
	minimum int
	length  int
	owned   int
	peak    int
}

func newH1TLSResidenceConn(connection net.Conn) *h1TLSResidenceConn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &h1TLSResidenceConn{Conn: connection, ctx: ctx, cancel: cancel,
		writes: make(chan h1TLSResidenceWrite, 64), done: make(chan struct{}),
		reached: make(chan struct{}), release: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer func() {
			for {
				select {
				case <-c.writes:
				default:
					c.mutex.Lock()
					c.owned = 0
					c.mutex.Unlock()
					return
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case write := <-c.writes:
				if write.drained != nil {
					close(write.drained)
					continue
				}
				prefix := write.bytes
				if write.hold {
					prefix = prefix[:len(prefix)-1]
				}
				if _, err := io.Copy(c.Conn, bytes.NewReader(prefix)); err != nil {
					return
				}
				if write.hold {
					close(c.reached)
					select {
					case <-ctx.Done():
						return
					case <-c.release:
					}
					if _, err := c.Conn.Write(write.bytes[len(write.bytes)-1:]); err != nil {
						return
					}
				}
				c.mutex.Lock()
				c.owned -= len(write.bytes)
				c.mutex.Unlock()
			}
		}
	}()
	return c
}

func (c *h1TLSResidenceConn) Write(p []byte) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	if 64*1024-c.owned < len(p) {
		return 0, errors.New("test encrypted residence byte bound exceeded")
	}
	hold := c.armed && !c.held && c.minimum <= len(p)
	if hold && (len(p) < 6 || p[0] != 23 || int(binary.BigEndian.Uint16(p[3:5]))+5 != len(p)) {
		return 0, errors.New("test gate requires one complete TLS application record")
	}
	write := h1TLSResidenceWrite{bytes: append([]byte(nil), p...), hold: hold}
	select {
	case c.writes <- write:
		c.owned += len(p)
		c.peak = max(c.peak, c.owned)
		if hold {
			c.held, c.length = true, len(p)
		}
		return len(p), nil
	default:
		return 0, errors.New("test encrypted residence message bound exceeded")
	}
}

func (c *h1TLSResidenceConn) arm(minimum int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// Handshake allocations are not measured as retained application bytes.
	if c.owned != 0 {
		panic("H1 residence gate armed before TLS handshake writes drained")
	}
	c.peak = 0
	c.armed, c.minimum = true, minimum
}

func (c *h1TLSResidenceConn) Close() error {
	c.once.Do(func() {
		c.mutex.Lock()
		c.cancel()
		c.mutex.Unlock()
		_ = c.Conn.Close()
	})
	<-c.done
	return nil
}

type h1TLSResidencePair struct {
	client, peer *FramedMessageConn
	gate         *h1TLSResidenceConn
	peerRaw      net.Conn
}

func h1TLSResidenceCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := selfSign([]string{"127.0.0.1"}, "hermetic-H1-residence", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

// No listening socket, host network, or identity is used. Verification is
// intentionally bypassed only for the synthetic certificate inside fake time.
func newH1TLSResidencePair(t *testing.T, certificate tls.Certificate) *h1TLSResidencePair {
	t.Helper()
	left, right := net.Pipe()
	gate := newH1TLSResidenceConn(left)
	pair := &h1TLSResidencePair{gate: gate, peerRaw: right}
	succeeded := false
	defer func() {
		if !succeeded {
			pair.close()
		}
	}()
	client := tls.Client(gate, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	peer := tls.Server(right, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate}, SessionTicketsDisabled: true, NextProtos: []string{"http/1.1"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	peerHandshake := make(chan error, 1)
	go func() { peerHandshake <- peer.HandshakeContext(ctx) }()
	clientErr, peerErr := client.HandshakeContext(ctx), <-peerHandshake
	if clientErr != nil || peerErr != nil {
		t.Fatalf("real TLS handshake: client=%v peer=%v", clientErr, peerErr)
	}
	var err error
	pair.client, err = NewFramedMessageConn(client, H1FramerProtocol, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	pair.peer, err = NewFramedMessageConn(peer, H1FramerProtocol, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Join the exact preceding handshake writes, rather than invoking the
	// bubble-wide Wait (this constructor may run in a recovery worker).
	drained := make(chan struct{})
	gate.writes <- h1TLSResidenceWrite{drained: drained}
	<-drained
	succeeded = true
	return pair
}

func (pair *h1TLSResidencePair) close() {
	// Closing raw endpoints first cannot wait for a TLS close-notify queued
	// behind the deliberately retained ciphertext. Join the writer owner too.
	_ = pair.peerRaw.Close()
	_ = pair.gate.Close()
}

// A whole early H1 frame is not readable until its TLS record authenticates.
// Separate flushes isolate a later record's tail, but cannot bypass a TCP hole
// in the first record. This pins that distinction without changing batching.
func TestH1TLSRecordTailOwnsCoalescedFrameReceipt(t *testing.T) {
	certificate := h1TLSResidenceCertificate(t)
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("separate_flushes_%t", separate), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				pair := newH1TLSResidencePair(t, certificate)
				defer pair.close()
				first, later := bytes.Repeat([]byte{0x71}, 64), bytes.Repeat([]byte{0x72}, 512)
				pair.gate.arm(256) // combined first record, or the separate later record
				type result struct {
					body []byte
					err  error
				}
				reads := make(chan result, 2)
				joined := make(chan struct{})
				go func() {
					defer close(joined)
					for range 2 {
						_, body, err := pair.peer.ReadMessage()
						reads <- result{body, err}
						if err != nil {
							return
						}
					}
				}()
				defer func() { pair.close(); <-joined }()
				if separate {
					for _, body := range [][]byte{first, later} {
						if err := pair.client.WriteMessage(websocket.BinaryMessage, body); err != nil {
							t.Fatal(err)
						}
					}
				} else if err := pair.client.WriteMessages([][]byte{first, later}); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-pair.gate.reached:
				default:
					t.Fatal("encrypted tail gate did not receive the record prefix")
				}
				wantReady := 0
				if separate {
					wantReady = 1
				}
				if len(reads) != wantReady {
					t.Fatalf("TLS tail owner: ready=%d want=%d", len(reads), wantReady)
				}
				wantRecord := 4 + len(later) + 22 // H1 header + TLS1.3 header/tag/type
				if !separate {
					wantRecord += 4 + len(first)
				}
				if pair.gate.length != wantRecord {
					t.Fatalf("unexpected record boundary: got=%d want=%d", pair.gate.length, wantRecord)
				}
				close(pair.gate.release)
				synctest.Wait()
				for _, want := range [][]byte{first, later} {
					got := <-reads
					if got.err != nil || !bytes.Equal(got.body, want) {
						t.Fatalf("finite record recovery changed frame: err=%v", got.err)
					}
				}
				t.Logf("separate=%t early_frames_before_tail=%d held_record_bytes=%d no_extra_flush=true", separate, wantReady, wantRecord)
			})
		})
	}
}

// The two real Transfer clients own receipt and the cumulative ACK. The
// fixture never fabricates an ACK or treats Write success as peer acceptance.
// A longer per-instance lifetime is a diagnostic arm, not a default change:
// the permanent-silence cases report Transfer's explicit extra blackhole delay.
// Platform heartbeat and MultiClient health owners are deliberately absent;
// their independent terminal behavior requires the existing liveness controls.
func TestH1TLSFiniteResidenceTransferAckOwner(t *testing.T) {
	certificate := h1TLSResidenceCertificate(t)
	for _, tc := range []struct {
		name             string
		lifetime, resume time.Duration
	}{
		{"30s_recovers_at_29s", 30 * time.Second, 29 * time.Second},
		{"30s_expires_before_36_8s_recovery", 30 * time.Second, 36800 * time.Millisecond},
		{"60s_recovers_at_36_8s", 60 * time.Second, 36800 * time.Millisecond},
		{"30s_permanent_silence", 30 * time.Second, 0},
		{"60s_permanent_silence", 60 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				pair := newH1TLSResidencePair(t, certificate)
				defer pair.close()
				ctx, cancel := context.WithCancel(context.Background())
				log := newRecordingLogger()
				settings := func(lifetime time.Duration) *ClientSettings {
					s := DefaultClientSettings()
					s.Log = log
					s.EncryptionSettings.Mode = EncryptionModeOff
					s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
					s.SendBufferSettings.AckTimeout = lifetime
					// Charge the exact retained owner, not the ordinary per-flow
					// uncharged floor, for this bounded memory control.
					s.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(32 * 1024)
					s.SendBufferSettings.ResendQueueRetainedByteAccounting = true
					return s
				}
				aSettings, bSettings := settings(tc.lifetime), settings(60*time.Second)
				a := NewClient(ctx, NewId(), NewNoContractClientOob(), aSettings)
				b := NewClient(ctx, NewId(), NewNoContractClientOob(), bSettings)
				a.ContractManager().AddNoContractPeer(b.ClientId())
				b.ContractManager().AddNoContractPeer(a.ClientId())
				outA, inA, outB, inB := make(Route, 64), make(Route, 64), make(Route, 64), make(Route, 64)
				for _, endpoint := range []struct {
					client  *Client
					remote  Id
					out, in Route
				}{{a, b.ClientId(), outA, inA}, {b, a.ClientId(), outB, inB}} {
					endpoint.client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{NewSendClientTransport(DestinationId(endpoint.remote))}, []Route{endpoint.out})
					endpoint.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{endpoint.in})
				}
				var workers sync.WaitGroup
				errs := make(chan error, 8)
				report := func(err error) {
					if ctx.Err() == nil {
						select {
						case errs <- err:
						default:
							panic("H1 residence error collector overflow")
						}
					}
				}
				firstWire := make(chan []byte, 1)
				ackWire := make(chan []byte, 64)
				var writes, received, ackWrites atomic.Uint64
				for _, endpoint := range []struct {
					framed  *FramedMessageConn
					out, in Route
					sender  bool
				}{{pair.client, outA, inA, true}, {pair.peer, outB, inB, false}} {
					workers.Add(2)
					go func() {
						defer workers.Done()
						for {
							select {
							case <-ctx.Done():
								return
							case wire := <-endpoint.out:
								if endpoint.sender && writes.Add(1) == 1 {
									firstWire <- append([]byte(nil), wire...)
								}
								if !endpoint.sender {
									ackWrites.Add(1)
									select {
									case ackWire <- append([]byte(nil), wire...):
									default:
										report(errors.New("peer ACK evidence exceeded bound"))
									}
								}
								err := endpoint.framed.WriteMessage(websocket.BinaryMessage, wire)
								MessagePoolReturn(wire)
								if err != nil {
									report(err)
									return
								}
							}
						}
					}()
					go func() {
						defer workers.Done()
						for {
							_, wire, err := ReadH1PooledMessage(endpoint.framed, 4096)
							if err != nil {
								report(err)
								return
							}
							select {
							case endpoint.in <- wire:
							case <-ctx.Done():
								MessagePoolReturn(wire)
								return
							}
						}
					}()
				}
				defer func() {
					cancel()
					pair.close()
					workers.Wait()
					for _, client := range []*Client{a, b} {
						if err := client.CloseAndWait(context.Background()); err != nil {
							t.Errorf("join Transfer client: %v", err)
						}
					}
					for _, route := range []Route{outA, inA, outB, inB} {
						for len(route) > 0 {
							MessagePoolReturn(<-route)
						}
					}
					if pair.gate.owned != 0 {
						t.Error("closed encrypted carrier retained bytes")
					}
					for _, settings := range []*ClientSettings{aSettings, bSettings} {
						budget := settings.SendBufferSettings.ResendQueueBudget
						reserved, released := budget.Counts()
						if budget.UsedByteCount() != 0 || reserved != released {
							t.Errorf("Transfer retained ownership after join: used=%d reserved=%d released=%d", budget.UsedByteCount(), reserved, released)
						}
					}
				}()
				const content = "exact request owned by finite TLS recovery"
				b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
					for _, frame := range frames {
						message, err := FromFrame(frame)
						simple, ok := message.(*protocol.SimpleMessage)
						if err != nil || !ok || simple.Content != content || source.SourceId != a.ClientId() {
							report(fmt.Errorf("wrong peer-owned application receipt: %v", err))
							return
						}
						received.Add(1)
					}
				})
				acks := make(chan error, 2)
				pair.gate.arm(0)
				start := time.Now()
				frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
				if !a.SendWithTimeout(frame, b.ClientId(), func(err error) { acks <- err }, time.Second) {
					MessagePoolReturn(frame.MessageBytes)
					t.Fatal("real Transfer did not admit request")
				}
				synctest.Wait()
				if len(firstWire) != 1 {
					t.Fatal("initial exact Pack did not reach H1 writer")
				}
				wire := <-firstWire
				pack := decodeSendPackLifecycleWirePack(t, wire)
				if pack.Nack || len(pack.Frames) != 1 || pack.Frames[0].MessageType != protocol.MessageType_TestSimpleMessage {
					t.Fatal("fixture request lost its ACK-required Pack owner")
				}
				select {
				case <-pair.gate.reached:
				default:
					t.Fatal("exact Pack ciphertext did not reach held TLS tail")
				}
				if pair.gate.length != len(wire)+4+22 {
					t.Fatalf("exact Pack is not the held complete record: record=%d wire=%d", pair.gate.length, len(wire))
				}
				if received.Load() != 0 || ackWrites.Load() != 0 || len(acks) != 0 {
					t.Fatal("write admission fabricated receipt or ACK")
				}
				boundary := tc.lifetime
				if tc.resume > 0 {
					boundary = min(boundary, tc.resume)
				}
				time.Sleep(boundary - time.Nanosecond)
				synctest.Wait()
				if len(acks) != 0 || received.Load() != 0 {
					t.Fatal("request terminated or arrived before controlled boundary")
				}
				retained := aSettings.SendBufferSettings.ResendQueueBudget.UsedByteCount()
				if retained <= 0 {
					t.Fatal("unacknowledged request lost its real resend-budget owner")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				expired := tc.resume == 0 || tc.lifetime <= tc.resume
				if expired {
					if len(acks) != 1 {
						t.Fatalf("missing terminal ACK owner at %s", time.Since(start))
					}
					if err := <-acks; err == nil {
						t.Fatal("unread Pack reported successful ACK")
					}
					matches := 0
					for _, line := range log.linesWith("event=sequence_exit") {
						if strings.Contains(line, "destination="+b.ClientId().String()) && strings.Contains(line, "reason=ack_lifetime ") && strings.Contains(line, "message="+RequireIdFromBytes(pack.MessageId).String()) {
							matches++
						}
					}
					if matches != 1 {
						t.Fatalf("wrong exact expired Pack owner: matches=%d", matches)
					}
				}
				if tc.resume > 0 {
					if time.Since(start) < tc.resume {
						time.Sleep(tc.resume - time.Since(start))
					}
					close(pair.gate.release)
					synctest.Wait()
					if received.Load() != 1 || ackWrites.Load() == 0 {
						t.Fatalf("finite recovery did not produce one real peer receipt and ACK: receipts=%d ack_writes=%d", received.Load(), ackWrites.Load())
					}
					for len(ackWire) > 0 {
						var frame protocol.TransferFrame
						if err := ProtoUnmarshal(<-ackWire, &frame); err != nil {
							t.Fatal(err)
						}
						ack := frame.GetAck()
						if ack == nil || ack.Selective || !bytes.Equal(ack.MessageId, pack.MessageId) || !bytes.Equal(ack.SequenceId, pack.SequenceId) ||
							!bytes.Equal(frame.TransferPath.SourceId, b.ClientId().Bytes()) || !bytes.Equal(frame.TransferPath.DestinationId, a.ClientId().Bytes()) {
							t.Fatal("peer cumulative ACK did not name the exact request and both endpoint owners")
						}
					}
					if !expired {
						if len(acks) != 1 {
							t.Fatal("real peer cumulative ACK did not complete sender")
						}
						if err := <-acks; err != nil {
							t.Fatalf("finite recovery failed before lifetime: %v", err)
						}
					} else if len(acks) != 0 {
						t.Fatal("late peer ACK changed the terminal callback")
					}
				}
				if len(errs) != 0 {
					t.Fatal(<-errs)
				}
				if pair.gate.peak > 64*1024 || writes.Load() > 32 {
					t.Fatal("bounded residence control exceeded expected retention")
				}
				t.Logf("lifetime=%s recovery=%s expired=%t real_receipts=%d peer_ack_writes=%d transfer_writes=%d encrypted_peak_bytes=%d retained_request_bytes=%d terminal_blackhole_delay=%s", tc.lifetime, tc.resume, expired, received.Load(), ackWrites.Load(), writes.Load(), pair.gate.peak, retained, map[bool]time.Duration{true: tc.lifetime}[tc.resume == 0])
			})
		})
	}
}
