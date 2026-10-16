// Measures distinct receive roots retained by real h1 and h1+ graphs against
// their active owner claim, without a client, sender budget, or shared copies.
package connect

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// The full receive route and its one reliable-reader hold are simultaneous
// graph ownership. Extraction transfers those same roots for inspection; it
// does not copy them or count the peer's input and socket scratch as evidence.
func TestPlatformH1GraphRetainedReceiveFitsAdmission(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	type observation struct {
		target    ByteCount
		plus      bool
		claim     ByteCount
		rootBytes ByteCount
		rootCount int
	}
	var observations []observation
	synctest.Test(t, func(t *testing.T) {
		for _, target := range []ByteCount{mib(32), mib(64)} {
			for _, plus := range []bool{false, true} {
				observations = append(observations, func() observation {
					baselineCount, baselineBytes := messagePoolOutstandingSnapshot()
					settings := DefaultPlatformTransportSettingsWithMemoryTarget(target)
					ApplyMobilePlatformTransportMemoryPolicy(settings, target)
					if settings.TransportBufferSize != 32 || settings.H1MaxMessageByteCount != 16*1024 ||
						settings.H1BudgetByteCount <= 0 {
						t.Fatalf("unexpected constructor profile: target=%d queue=%d maximum=%d claim=%d",
							target, settings.TransportBufferSize, settings.H1MaxMessageByteCount, settings.H1BudgetByteCount)
					}
					root := NewPlatformTransportBudgetForMemoryTarget(target)
					budget := newDefaultPlatformTransportBudgetWithParent(target, root)
					settings.PlatformTransportBudget = budget
					settings.EnableH1Plus = plus
					settings.Log = NewNoopLogger()
					settings.ReceiveStats = &PlatformTransportReceiveStats{}
					settings.H1ConnectionStats = &H1ConnectionStats{}
					ctx, cancel := context.WithCancel(context.Background())
					clientSocket, peerSocket := net.Pipe()
					peerDone := make(chan struct{})
					ready := make(chan H1MessageConn, 1)
					peerError := make(chan error, 1)
					var strategy *ClientStrategy
					var transport *PlatformTransport
					var inFlightWrite <-chan error
					var heldMessages [][]byte
					closed := false
					closeOwner := func() {
						if closed {
							return
						}
						closed = true
						cancel()
						clientSocket.Close()
						peerSocket.Close()
						if transport != nil {
							if err := transport.CloseAndWait(context.Background()); err != nil {
								t.Error(err)
							}
						}
						if strategy != nil {
							strategy.Close()
						}
						<-peerDone
						if inFlightWrite != nil {
							<-inFlightWrite
							inFlightWrite = nil
						}
						for _, message := range heldMessages {
							MessagePoolReturn(message)
						}
						heldMessages = nil
					}
					defer closeOwner()
					go func() {
						defer close(peerDone)
						defer peerSocket.Close()
						reader := bufio.NewReader(peerSocket)
						request, err := http.ReadRequest(reader)
						if err != nil {
							peerError <- err
							return
						}
						response := &testHijackResponseWriter{
							conn: peerSocket, brw: bufio.NewReadWriter(reader, bufio.NewWriter(peerSocket)),
							header: http.Header{},
						}
						var peer H1MessageConn
						if plus {
							var connection net.Conn
							connection, err = AcceptFramedUpgrade(response, request, H1FramerProtocol, time.Second)
							if err == nil {
								peer, err = NewFramedMessageConn(connection, H1FramerProtocol, int(settings.H1MaxMessageByteCount), nil)
							}
						} else {
							upgrader := websocket.Upgrader{}
							peer, err = upgrader.Upgrade(response, request, nil)
						}
						if err != nil {
							peerError <- err
							return
						}
						ready <- peer
						<-ctx.Done()
					}()
					strategySettings := DefaultClientStrategySettings()
					strategySettings.Log = NewNoopLogger()
					strategySettings.EnableNormal = true
					strategySettings.EnableResilient = false
					strategySettings.ParallelBlockSize = 1
					strategySettings.ExpandExtenderProfileCount = 0
					strategySettings.ExtenderConfigs = nil
					strategySettings.DnsTlds = nil
					strategySettings.InternalDohDomains = nil
					strategySettings.MinNextConnectDelay = 0
					strategySettings.MaxNextConnectDelay = 0
					var dialed atomic.Bool
					strategySettings.DialContextSettings = &DialContextSettings{
						DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
							if !dialed.Swap(true) {
								return clientSocket, nil
							}
							<-ctx.Done()
							return nil, ctx.Err()
						},
					}
					strategy = NewClientStrategy(ctx, strategySettings)
					routes := NewRouteManagerWithLogger(ctx, "synthetic-h1-graph", NewNoopLogger())
					transport = NewPlatformTransportWithTargetMode(ctx, strategy, routes,
						"ws://graph-retention.example/endpoint",
						&ClientAuth{ByJwt: "synthetic-graph-token", InstanceId: NewId()}, TransportModeH1, settings)
					synctest.Wait()
					var peer H1MessageConn
					select {
					case peer = <-ready:
					case err := <-peerError:
						t.Fatalf("synthetic upgrade: %v", err)
					default:
						t.Fatal("actual constructor did not finish its synthetic upgrade")
					}
					expectedConnections := H1ConnectionStatsSnapshot{WebSocketConnectionCount: 1}
					if plus {
						expectedConnections = H1ConnectionStatsSnapshot{H1PlusConnectionCount: 1}
					}
					if !transport.IsConnected() || settings.H1ConnectionStats.Snapshot() != expectedConnections {
						t.Fatal("actual constructor did not publish the requested carrier")
					}
					receive := func() Route {
						routes.mutex.Lock()
						defer routes.mutex.Unlock()
						for routeTransport, current := range routes.readerMatchState.transportRoutes {
							if _, ok := routeTransport.(*receiveGatewayTransport); ok && len(current) == 1 {
								return current[0]
							}
						}
						return nil
					}()
					if receive == nil || cap(receive) != settings.TransportBufferSize {
						t.Fatal("constructor did not publish its unmodified bounded receive route")
					}
					fillReceive := func(size int) int {
						before := settings.ReceiveStats.Snapshot().H1.QueueBackpressureMessageCount
						for i := range cap(receive) + 1 {
							writeDone := make(chan error, 1)
							inFlightWrite = writeDone
							go func() {
								message := make([]byte, size)
								binary.LittleEndian.PutUint32(message, uint32(i))
								writeDone <- peer.WriteMessage(websocket.BinaryMessage, message)
							}()
							synctest.Wait()
							select {
							case err := <-writeDone:
								inFlightWrite = nil
								if err != nil {
									t.Fatalf("real peer write: %v", err)
								}
							default:
								t.Fatal("reader stopped before retaining its one complete message")
							}
							if settings.ReceiveStats.Snapshot().H1.QueueBackpressureMessageCount != before {
								return i + 1
							}
						}
						t.Fatal("neither byte nor count admission bounded the actual reader")
						return 0
					}
					messageCount := fillReceive(int(settings.H1MaxMessageByteCount))
					stats := settings.ReceiveStats.Snapshot().H1
					if stats.QueueBackpressureMessageCount != 1 || stats.QueueBackpressureByteCount != uint64(settings.H1MaxMessageByteCount) ||
						stats.QueueDropMessageCount != 0 {
						t.Fatalf("full reliable reader ownership was not established: %+v", stats)
					}
					result := observation{target: target, plus: plus, claim: budget.Stats().UsedByteCount}
					if result.claim != settings.H1MemoryReservationByteCount() || root.Stats().UsedByteCount != result.claim {
						t.Fatal("receive roots were not retained under the exact active graph claim")
					}
					reader := routes.OpenMultiRouteReader(DestinationId(NewId()))
					defer routes.CloseMultiRouteReader(reader)
					for range messageCount {
						message, err := reader.Read(ctx, 0)
						if err != nil || message == nil {
							t.Fatalf("held receive ownership did not transfer: %v", err)
						}
						heldMessages = append(heldMessages, message)
						synctest.Wait()
					}
					rootAddresses := map[*byte]bool{}
					for i, message := range heldMessages {
						if len(message) != int(settings.H1MaxMessageByteCount) || binary.LittleEndian.Uint32(message) != uint32(i) {
							t.Fatal("actual receive root payload or order changed")
						}
						if rootAddresses[&message[0]] {
							t.Fatal("witness would count a shared receive root twice")
						}
						rootAddresses[&message[0]] = true
						result.rootBytes += ByteCount(cap(message))
					}
					result.rootCount = len(rootAddresses)
					for _, message := range heldMessages {
						MessagePoolReturn(message)
					}
					heldMessages = nil
					// Refill with pooled roots, then cancel at the same full-route
					// boundary. No consumer drains this second graph for the owner.
					fillReceive(4095)
					if settings.ReceiveStats.Snapshot().H1.QueueBackpressureMessageCount != 2 {
						t.Fatal("cancellation did not begin with a held reliable receive root")
					}
					closeOwner()
					synctest.Wait()
					if _, open := <-receive; open || transport.IsConnected() || routes.HasActiveTransport() ||
						settings.H1ConnectionStats.Snapshot() != (H1ConnectionStatsSnapshot{}) {
						t.Fatal("joined cancellation retained a receive root, route, or connection")
					}
					for _, current := range []*PlatformTransportBudget{budget, root} {
						stats := current.Stats()
						if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
							t.Fatalf("joined cancellation retained a graph claim: %+v", stats)
						}
					}
					if count, bytes := messagePoolOutstandingSnapshot(); count != baselineCount || bytes != baselineBytes {
						t.Fatalf("joined cancellation retained pooled roots: before=%d/%d after=%d/%d", baselineCount, baselineBytes, count, bytes)
					}
					return result
				}())
			}
		}
	})
	// Expected causal failures stay outside the bubble so every carrier/profile
	// completes its independent cancellation and root reconciliation first.
	if len(observations) != 4 {
		t.Fatalf("incomplete carrier/profile census: %d", len(observations))
	}
	for _, observed := range observations {
		t.Logf("h1 receive roots: target=%d plus=%t roots=%d retained=%d activeClaim=%d",
			observed.target, observed.plus, observed.rootCount, observed.rootBytes, observed.claim)
		if observed.claim < observed.rootBytes {
			t.Errorf("H1_GRAPH_RETAINED_BYTES_EXCEED_CLAIM: target=%d plus=%t roots=%d retained=%d activeClaim=%d",
				observed.target, observed.plus, observed.rootCount, observed.rootBytes, observed.claim)
		}
	}
}
