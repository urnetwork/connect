package connect

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The physical connection always works. A test can pause one application
// write just long enough to fill its bounded local route at a probe edge.
// Releasing that edge delivers every queued application message; requests
// receive an immediate valid echo. No packet is blackholed or retransmitted.
type p2pProbePressureConn struct {
	ctx               context.Context
	mutex             sync.Mutex
	gate              <-chan struct{}
	onWire            func([]byte)
	applicationWrites int
}

func (self *p2pProbePressureConn) Write(message []byte) (int, error) {
	self.mutex.Lock()
	gate := self.gate
	self.mutex.Unlock()
	if gate != nil {
		select {
		case <-self.ctx.Done():
			return 0, self.ctx.Err()
		case <-gate:
		}
	}
	self.onWire(message)
	return len(message), nil
}

func (self *p2pProbePressureConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (self *p2pProbePressureConn) Close() error                     { return nil }
func (self *p2pProbePressureConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (self *p2pProbePressureConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (self *p2pProbePressureConn) SetDeadline(time.Time) error      { return nil }
func (self *p2pProbePressureConn) SetReadDeadline(time.Time) error  { return nil }
func (self *p2pProbePressureConn) SetWriteDeadline(time.Time) error { return nil }

// The observed PERFVAR pattern: the ordinary four-slot route is full at
// challenge admission, but the carrier drains it promptly. Such local
// contention must not silently revoke a proven working route and strand ACKs.
func TestP2pStreamProbeApplicationQueuePressureKeepsHealthyLease(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.ChannelBufferSize = 4
		streamId, peerId := NewId(), NewId()
		routeManager := NewRouteManager(ctx, "probe-local-pressure")
		probe := newStoppedP2pStreamProbe(ctx, routeManager, streamId, settings)
		now := time.Now()
		probe.testingNow = func() time.Time { return now }
		ticks := make(chan time.Time, 1)
		probe.testingProbeTimer = ticks
		conn := &p2pProbePressureConn{ctx: ctx}
		conn.onWire = func(message []byte) {
			recognized, messageType, _, nonce := decodeP2pStreamProbe(message)
			if recognized && messageType == p2pStreamProbeRequestType {
				response := encodeP2pStreamProbe(p2pStreamProbeResponseType, streamId, nonce)
				probe.handle(response)
				MessagePoolReturn(response)
			} else if !recognized {
				conn.applicationWrites++
			}
		}
		transport, route := newP2pSendTransportForPeer(ctx, cancel, conn, peerId, streamId, settings, true, nil)
		probe.setSendRoute(transport, route)
		writer := routeManager.OpenMultiRouteWriter(DestinationId(peerId))
		go HandleError(probe.run, probe.cancel)
		defer func() {
			cancel()
			probe.close()
			probe.clearSendRoute(transport, route)
			routeManager.CloseMultiRouteWriter(writer)
			if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait()
		if len(writer.GetActiveRoutes()) != 1 {
			t.Fatal("healthy initial challenge did not grant readiness")
		}
		for turn := range 4 {
			gate := make(chan struct{})
			conn.mutex.Lock()
			conn.gate = gate
			conn.mutex.Unlock()
			route <- MessagePoolCopy([]byte{1})
			synctest.Wait()
			for range cap(route) {
				route <- MessagePoolCopy([]byte{1})
			}
			now = now.Add(settings.EndToEndProbeInterval)
			ticks <- now
			synctest.Wait()
			close(gate)
			synctest.Wait()
			if got := conn.applicationWrites; got != (turn+1)*(cap(route)+1) {
				t.Fatalf("healthy carrier dropped data: writes=%d turn=%d", got, turn+1)
			}
			if len(writer.GetActiveRoutes()) != 1 {
				t.Fatalf("local full-route probe refusal withdrew a healthy route after %d intervals despite %d successful application writes", turn+1, conn.applicationWrites)
			}
		}
	})
}

// Separate request/response slots cap control retention at two forty-byte
// envelopes. They cannot displace application packets or starve either probe
// class; the physical writer gives a ready application packet a turn after
// at most two probes.
func TestP2pStreamProbeControlAdmissionBoundedFairAndDrained(t *testing.T) {
	for _, cancelWhileBlocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain-in-order", true: "cancel-and-return"}[cancelWhileBlocked], func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				settings := DefaultP2pTransportSettings()
				settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
				settings.ChannelBufferSize = 4
				gate := make(chan struct{})
				var order []byte
				conn := &p2pProbePressureConn{ctx: ctx, gate: gate}
				conn.onWire = func(message []byte) {
					recognized, messageType, _, _ := decodeP2pStreamProbe(message)
					if !recognized {
						messageType = 0
					}
					order = append(order, messageType)
				}
				streamId, peerId := NewId(), NewId()
				transport, route := newP2pSendTransportForPeer(ctx, cancel, conn, peerId, streamId, settings, true, nil)
				sender := transport.(*P2pSendTransport)
				probe := newStoppedP2pStreamProbe(ctx, NewRouteManager(ctx, "probe-bounded-controls"), streamId, settings)
				probe.setSendRoute(transport, route)
				generation, _ := probe.sendRouteState()
				defer probe.clearSendRoute(transport, route)
				defer sender.CloseAndWait(context.Background())
				route <- MessagePoolCopy([]byte{1})
				synctest.Wait()
				for range cap(route) {
					route <- MessagePoolCopy([]byte{1})
				}
				for _, kind := range []byte{p2pStreamProbeRequestType, p2pStreamProbeResponseType} {
					for attempt := range 2 {
						nonce := NewId()
						message := encodeP2pStreamProbe(kind, streamId, nonce)
						accepted := probe.sendProbeMessage(generation, kind, message, nonce, "queued", "dropped")
						if accepted != (attempt == 0) {
							t.Fatalf("kind=%d attempt=%d admitted=%t", kind, attempt, accepted)
						}
					}
				}
				if len(route) != 4 || len(sender.probeRequests) != 1 || len(sender.probeResponses) != 1 ||
					cap(sender.probeRequests) != 1 || cap(sender.probeResponses) != 1 || p2pStreamProbeByteCount != 40 {
					t.Fatal("probe admission changed the application queue or exceeded two fixed-size envelopes")
				}
				queuedRequest, queuedResponse := <-sender.probeRequests, <-sender.probeResponses
				rootBytes := MessagePoolPacketRootByteCount(queuedRequest) + MessagePoolPacketRootByteCount(queuedResponse)
				sender.probeRequests <- queuedRequest
				sender.probeResponses <- queuedResponse
				if rootBytes != 2*smallPacketPoolSize {
					t.Fatalf("retained probe roots=%d, want 512 bytes", rootBytes)
				}
				if cancelWhileBlocked {
					cancel()
				} else {
					close(gate)
				}
				synctest.Wait()
				if !cancelWhileBlocked {
					want := []byte{0, p2pStreamProbeResponseType, p2pStreamProbeRequestType, 0, 0, 0, 0}
					if len(order) != len(want) {
						t.Fatalf("writer order=%v", order)
					}
					for index := range want {
						if order[index] != want[index] {
							t.Fatalf("writer order=%v want=%v", order, want)
						}
					}
				} else if len(order) != 0 {
					t.Fatalf("canceled physical writer delivered %d messages", len(order))
				}
				if len(route)+len(sender.probeRequests)+len(sender.probeResponses) != 0 {
					t.Fatal("worker retained queued payload after completion")
				}
			})
		})
	}
}

// Probe floods cannot starve a ready ordinary packet or the request class.
// Queue selection itself retains the allocation-free packet hot path.
func TestP2pStreamProbeSendSchedulingIsFairAndAllocationFree(t *testing.T) {
	ctx := context.Background()
	sender := &P2pSendTransport{ctx: ctx, send: make(chan []byte, 1), probeRequests: make(chan []byte, 1), probeResponses: make(chan []byte, 1)}
	request, response, ordinary := []byte{1}, []byte{2}, []byte{3}
	burst := 0
	for turn := range 30 {
		if len(sender.probeRequests) == 0 {
			sender.probeRequests <- request
		}
		if len(sender.probeResponses) == 0 {
			sender.probeResponses <- response
		}
		if len(sender.send) == 0 {
			sender.send <- ordinary
		}
		message, ok := sender.nextSend(&burst)
		want := []byte{2, 1, 3}[turn%3]
		if !ok || message[0] != want {
			t.Fatalf("turn=%d message=%v want=%d", turn, message, want)
		}
	}
	for len(sender.probeRequests) > 0 {
		<-sender.probeRequests
	}
	for len(sender.probeResponses) > 0 {
		<-sender.probeResponses
	}
	for len(sender.send) > 0 {
		<-sender.send
	}
	if allocations := testing.AllocsPerRun(100, func() {
		sender.send <- ordinary
		sender.nextSend(&burst)
	}); allocations != 0 {
		t.Fatalf("queue selection allocated %g objects", allocations)
	}
}
