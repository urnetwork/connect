package connect

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

type initialPingPathTestCounts struct {
	outboundAccepted atomic.Uint64
	outboundDropped  atomic.Uint64
	providerPings    atomic.Uint64
	returnAccepted   atomic.Uint64
	returnAckDropped atomic.Uint64
	echoReceived     atomic.Uint64
	ackIngress       atomic.Uint64
}

func TestInitialPingPathObservationCausalBoundaries(t *testing.T) {
	GetMessagePoolAggregateStats()
	cases := []struct {
		name                                                       string
		blocked, dropForward, dropAck, lateCallback, periodicFirst bool
		releaseForward, releaseAck                                 time.Duration
		wantOutcome                                                string
		wantSeconds                                                float64
	}{
		{name: "healthy", wantOutcome: "acknowledged"},
		{name: "registered_unread_carrier", blocked: true, wantOutcome: "deadline", wantSeconds: 30},
		{name: "registered_unread_carrier_timer_first", blocked: true, lateCallback: true, wantOutcome: "expired", wantSeconds: 30},
		{name: "carrier_accepts_then_forward_drop", dropForward: true, wantOutcome: "deadline", wantSeconds: 30},
		{name: "provider_receives_return_ack_dropped", dropAck: true, wantOutcome: "deadline", wantSeconds: 30},
		{name: "forward_delivered_at_25s", releaseForward: 25 * time.Second, wantOutcome: "acknowledged", wantSeconds: 25},
		{name: "return_ack_delivered_at_25s", releaseAck: 25 * time.Second, wantOutcome: "acknowledged", wantSeconds: 25},
		{name: "unread_carrier_recovers_at_25s", blocked: true, releaseForward: 25 * time.Second, wantOutcome: "acknowledged", wantSeconds: 25},
		{name: "periodic_first_unread_carrier_recovery", blocked: true, periodicFirst: true, releaseForward: 25 * time.Second, wantOutcome: "error", wantSeconds: 25},
		{name: "ack_callback_held_after_receive", lateCallback: true, wantOutcome: "expired", wantSeconds: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				counts := &initialPingPathTestCounts{}
				fixture, provider := newInitialPingPathFixture(t, func(settings *ClientSettings) {})
				observations := &InitialPingObservations{}
				fixture.window.settings.InitialPingObservations = observations
				defaults := DefaultMultiClientSettings()
				fixture.window.settings.PingTimeout = defaults.PingTimeout
				fixture.window.settings.PingWriteTimeout = defaults.PingWriteTimeout
				fixture.window.settings.WindowExpandTimeout = time.Second
				if defaults.PingTimeout != 30*time.Second || defaults.PingWriteTimeout != 5*time.Second {
					t.Fatal("production ping budget changed")
				}
				// Both pings remain enabled. Pin their order in every causal case:
				// an earlier periodic writer can retain the SendSequence past the
				// queued initial Pack's 5s deadline when a relay stops draining.
				firstPingWrite := make(chan struct{})
				var firstPingWriteOnce sync.Once
				startPeriodic := make(chan func(), 1)
				fixture.window.settings.startChannelPingForTest = func(start func()) {
					if tc.periodicFirst {
						start()
						select {
						case <-firstPingWrite:
						case <-fixture.waitCtx.Done():
						}
					} else {
						startPeriodic <- start
					}
				}
				if !tc.lateCallback {
					fixture.window.beforeExpandPingResultForTest = nil
					fixture.window.afterExpandPingResultForTest = nil
				}
				provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
					for _, frame := range frames {
						if frame.MessageType == protocol.MessageType_IpIpPing {
							counts.providerPings.Add(1)
						}
					}
				})
				generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
				var created *Client
				var joined atomic.Bool
				origin := time.Now()
				generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
					settings.Log = NewNoopLogger()
					settings.SendBufferSettings.TransferWireMessageObserver = func(o TransferWireMessageObservation) {
						if !initialPingPathWireContainsPing(o.TransferFrameBytes) {
							return
						}
						firstPingWriteOnce.Do(func() {
							close(firstPingWrite)
							if !tc.periodicFirst {
								(<-startPeriodic)()
							}
						})
					}
					settings.ReceiveBufferSettings.ProgressObserver = func(o TransferProgressEvent) {
						if o.Stage == "receive_ack_end" && o.Success {
							counts.ackIngress.Add(1)
						}
					}
					client := NewClient(ctx, args.ClientId, NewNoContractClientOob(), settings)
					created = client
					client.ContractManager().AddNoContractPeer(provider.ClientId())
					provider.ContractManager().AddNoContractPeer(client.ClientId())
					client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
						for _, frame := range frames {
							if frame.MessageType == protocol.MessageType_IpIpPing {
								counts.echoReceived.Add(1)
							}
						}
					})
					outbound, atProvider := make(chan []byte), make(chan []byte)
					returned, atClient := make(chan []byte), make(chan []byte)
					clientSend, clientReceive := NewSendGatewayTransport(), NewReceiveGatewayTransport()
					providerSend, providerReceive := NewSendClientTransport(DestinationId(client.ClientId())), NewReceiveGatewayTransport()
					client.RouteManager().UpdateTransport(clientSend, []Route{outbound})
					client.RouteManager().UpdateTransport(clientReceive, []Route{atClient})
					provider.RouteManager().UpdateTransport(providerSend, []Route{returned})
					provider.RouteManager().UpdateTransport(providerReceive, []Route{atProvider})
					relayCtx, stopRelays := context.WithCancel(fixture.waitCtx)
					var relayWG sync.WaitGroup
					relayWG.Add(2)
					waitUntil := func(delay time.Duration) bool {
						if remaining := time.Until(origin.Add(delay)); remaining > 0 {
							timer := time.NewTimer(remaining)
							defer timer.Stop()
							select {
							case <-relayCtx.Done():
								return false
							case <-timer.C:
							}
						}
						return relayCtx.Err() == nil
					}
					go func() {
						defer relayWG.Done()
						if tc.blocked {
							if tc.releaseForward == 0 {
								<-relayCtx.Done()
								return
							}
							if !waitUntil(tc.releaseForward) {
								return
							}
						}
						for {
							select {
							case <-relayCtx.Done():
								return
							case packet := <-outbound:
								counts.outboundAccepted.Add(1)
								if tc.dropForward {
									counts.outboundDropped.Add(1)
									MessagePoolReturn(packet)
									continue
								}
								if !waitUntil(tc.releaseForward) {
									MessagePoolReturn(packet)
									return
								}
								select {
								case <-relayCtx.Done():
									MessagePoolReturn(packet)
									return
								case atProvider <- packet:
								}
							}
						}
					}()
					go func() {
						defer relayWG.Done()
						for {
							select {
							case <-relayCtx.Done():
								return
							case packet := <-returned:
								counts.returnAccepted.Add(1)
								frame := &protocol.TransferFrame{}
								if err := proto.Unmarshal(packet, frame); err != nil {
									MessagePoolReturn(packet)
									t.Error("fixture could not decode carrier frame")
									return
								}
								ack := frame.Ack != nil || frame.Frame != nil && frame.Frame.MessageType == protocol.MessageType_TransferAck
								if tc.dropAck && ack {
									counts.returnAckDropped.Add(1)
									MessagePoolReturn(packet)
									continue
								}
								if ack && !waitUntil(tc.releaseAck) {
									MessagePoolReturn(packet)
									return
								}
								select {
								case <-relayCtx.Done():
									MessagePoolReturn(packet)
									return
								case atClient <- packet:
								}
							}
						}
					}()
					t.Cleanup(func() {
						fixture.releasePing()
						fixture.cancelEvaluation()
						fixture.cancelWindow()
						client.Cancel()
						stopRelays()
						client.RouteManager().RemoveTransport(clientSend)
						client.RouteManager().RemoveTransport(clientReceive)
						provider.RouteManager().RemoveTransport(providerSend)
						provider.RouteManager().RemoveTransport(providerReceive)
						relayWG.Wait()
						closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := client.CloseAndWait(closeCtx); err != nil {
							t.Error(err)
						}
						joined.Store(true)
					})
					return client, nil
				}
				done := fixture.start()
				if tc.lateCallback {
					fixture.wait(t, "real ACK before held callback", fixture.pingResultEntered)
					synctest.Wait()
					if !tc.blocked && counts.ackIngress.Load() == 0 {
						t.Fatal("held success callback was not preceded by actual ACK ingress")
					}
					time.Sleep(time.Until(origin.Add(35 * time.Second)))
					row := oneInitialPingObservation(t, observations)
					if row.Outcome != "expired" {
						t.Fatalf("held callback outcome=%+v", row)
					}
					fixture.releasePing()
				}
				got := fixture.result(t, done)
				synctest.Wait()
				row := oneInitialPingObservation(t, observations)
				pathRow := oneInitialPingPathObservation(t, observations)
				writerOrder := "initial_first"
				if tc.periodicFirst {
					writerOrder = "periodic_first"
				}
				encoded, _ := json.Marshal(map[string]any{
					"case": tc.name, "admitted": got, "outcome": row.Outcome, "dependency": row.Dependency, "initial_ping_seconds": row.Seconds,
					"route_write": pathRow.RouteWrite, "ack_callback": pathRow.AckCallback,
					"outbound_accepted": counts.outboundAccepted.Load(), "provider_ping_deliveries": counts.providerPings.Load(),
					"return_ack_dropped": counts.returnAckDropped.Load(), "echo_received": counts.echoReceived.Load(),
					"all_ping_ack_ingress": counts.ackIngress.Load(), "virtual_time": true, "production_ping_budget_seconds": defaults.PingTimeout.Seconds(),
					"no_contract_test_peers": true, "periodic_ping_enabled": true, "writer_order": writerOrder,
				})
				t.Logf("INITIAL_PING_PATH_OBSERVER %s", encoded)
				wantOutcome := row.Outcome == tc.wantOutcome || tc.wantOutcome == "deadline" && (row.Outcome == "expired" || row.Outcome == "error")
				if !wantOutcome || pathRow.Outcome != row.Outcome || pathRow.Seconds != row.Seconds {
					t.Fatalf("terminal/path mismatch old=%+v path=%+v expected=%s", row, pathRow, tc.wantOutcome)
				}
				if tc.periodicFirst {
					if got != 0 || row.Seconds < defaults.PingWriteTimeout.Seconds() || row.Seconds > 25.1 || pathRow.RouteWrite != "not_observed" || pathRow.AckCallback != "error" {
						t.Fatalf("periodic-first queue expiry not isolated: admitted=%d row=%+v path=%+v", got, row, pathRow)
					}
				} else if tc.wantOutcome != "acknowledged" {
					if got != 0 || row.Seconds < 30 || row.Seconds > 30.1 {
						t.Fatalf("changed30s terminal boundary: admitted=%d row=%+v", got, row)
					}
				} else {
					if got != 1 || row.Seconds < tc.wantSeconds || row.Seconds > tc.wantSeconds+1 || pathRow.AckCallback != "success" || pathRow.RouteWrite != "accepted" {
						t.Fatalf("healthy recovery changed: admitted=%d row=%+v path=%+v", got, row, pathRow)
					}
				}
				if row.Outcome == "error" && pathRow.AckCallback != "error" {
					t.Fatalf("callback error was not observed before terminal: %+v", pathRow)
				}
				if tc.blocked && tc.releaseForward == 0 {
					if counts.outboundAccepted.Load() != 0 || pathRow.RouteWrite != "not_observed" || pathRow.AckCallback == "success" || counts.providerPings.Load() != 0 {
						t.Fatal("unread carrier was not isolated")
					}
				}
				if tc.dropForward && (counts.outboundAccepted.Load() == 0 || counts.providerPings.Load() != 0 || pathRow.RouteWrite != "accepted" || pathRow.AckCallback == "success") {
					t.Fatal("forward loss was not isolated")
				}
				if tc.dropAck && (counts.providerPings.Load() == 0 || counts.returnAckDropped.Load() == 0 || counts.ackIngress.Load() != 0 || counts.echoReceived.Load() == 0 || pathRow.AckCallback == "success" || pathRow.RouteWrite != "accepted") {
					t.Fatal("ACK loss/independent echo boundary was not isolated")
				}
				if tc.lateCallback && !tc.blocked && (pathRow.AckCallback != "success" || pathRow.RouteWrite != "accepted") {
					t.Fatal("successful ACK entry was hidden by delayed admission callback")
				}
				if created == nil || joined.Load() {
					t.Fatal("client lifecycle control missing or joined before cleanup")
				}

			})
		})
	}
}

func oneInitialPingPathObservation(t *testing.T, observations *InitialPingObservations) InitialPingPathObservation {
	t.Helper()
	var completed uint64
	var result InitialPingPathObservation
	for _, row := range observations.PathSnapshot() {
		completed += row.Count
		if row.Count != 0 {
			result = row
		}
	}
	if observations.Started() != 1 || completed != 1 {
		t.Fatalf("initial-ping path ownership: started=%d completed=%d", observations.Started(), completed)
	}
	return result
}

// The two terminal witness loads are independent. These synthetic permutations
// keep absence explicit without asserting real callback/writer scheduling, and
// a duplicate callback cannot rewrite first entry.
func TestInitialPingPathObservationWitnessOrdering(t *testing.T) {
	var disabled *initialPingPathWitness
	disabled.observeRouteWrite(TransportTypeH1)
	disabled.observeAckCallback(nil)
	if route, ack := disabled.snapshot(); route != 0 || ack != 0 {
		t.Fatal("nil witness changed state")
	}
	for _, failed := range []bool{false, true} {
		witness := &initialPingPathWitness{}
		var err error
		wantAck := 1
		if failed {
			err = errors.New("synthetic ACK failure")
			wantAck = 2
		}
		witness.observeAckCallback(err)
		if route, ack := witness.snapshot(); route != 0 || ack != wantAck {
			t.Fatal("ACK entry required an earlier write observer")
		}
		witness.observeRouteWrite(TransportTypeH1)
		if failed {
			witness.observeAckCallback(nil)
		} else {
			witness.observeAckCallback(errors.New("late synthetic callback"))
		}
		if route, ack := witness.snapshot(); route != 1 || ack != wantAck {
			t.Fatal("late write/callback changed first ACK witness")
		}
	}
}

func TestInitialPingPathObservationFixedAggregate(t *testing.T) {
	observations := &InitialPingObservations{}
	var done sync.WaitGroup
	for outcome := range len(initialPingOutcomeLabels) {
		for route := range len(initialPingRouteWriteLabels) {
			for ack := range len(initialPingAckCallbackLabels) {
				done.Add(1)
				go func() {
					defer done.Done()
					observations.recordPath(initialPingOutcome(outcome), route, ack, 1250*time.Millisecond)
					_ = observations.PathSnapshot()
				}()
			}
		}
	}
	done.Wait()
	before := observations.PathSnapshot()
	for _, invalid := range [][3]int{{-1, 0, 0}, {4, 0, 0}, {0, -1, 0}, {0, 2, 0}, {0, 0, -1}, {0, 0, 3}} {
		observations.recordPath(initialPingOutcome(invalid[0]), invalid[1], invalid[2], time.Second)
	}
	observations.recordPath(initialPingExpired, 0, 0, -time.Second)
	if after := observations.PathSnapshot(); before != after {
		t.Fatal("invalid path cell changed finite aggregate")
	}
	if len(before) != 24 {
		t.Fatal("path cardinality changed")
	}
	var disabled *InitialPingObservations
	for i, row := range before {
		if row.Count != 1 || row.Seconds != 1.25 {
			t.Fatalf("lost concurrent path row: %+v", row)
		}
		zero := disabled.PathSnapshot()[i]
		if zero.Count != 0 || zero.Seconds != 0 || zero.Outcome != row.Outcome || zero.RouteWrite != row.RouteWrite || zero.AckCallback != row.AckCallback {
			t.Fatal("nil snapshot changed vocabulary")
		}
	}
}

// Read the observer's borrowed logical Transfer bytes. This fixture never logs
// packet bodies or identifiers, and keeps the real Pack/ACK code unchanged.
func initialPingPathWireContainsPing(bytes []byte) bool {
	frame := &protocol.TransferFrame{}
	if proto.Unmarshal(bytes, frame) != nil {
		return false
	}
	pack := frame.Pack
	if pack == nil && frame.Frame != nil && frame.Frame.MessageType == protocol.MessageType_TransferPack {
		pack = &protocol.Pack{}
		if proto.Unmarshal(frame.Frame.MessageBytes, pack) != nil {
			return false
		}
	}
	if pack == nil {
		return false
	}
	for _, payload := range pack.Frames {
		if payload.MessageType == protocol.MessageType_IpIpPing {
			return true
		}
	}
	return false
}
