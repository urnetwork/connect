// The flapping fixture is an addressed transport, not a broadcast. Force its
// wrong-reader ordering without relying on receiver scheduling or wall time.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Metadata belongs to this test owner; the actual generator/Clients retain all
// route and packet ownership. Each existing flap worker exposes its final join.
type flappingRouteTestClient struct {
	client   *Client
	args     *MultiClientGeneratorClientArgs
	links    []flappingProviderTestRoute
	flapDone <-chan struct{}
	started  time.Time
}

type flappingRouteTestFixture struct {
	t          *testing.T
	cancel     context.CancelFunc
	generator  *flappingProviderGenerator
	clients    []*flappingRouteTestClient
	finishOnce sync.Once
}

// Construction preserves the real fixture's eight Clients and zero-capacity
// queue settings. There is no custom verdict, carrier or forwarding worker.
func newFlappingRouteTestFixture(t *testing.T) *flappingRouteTestFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	fixture := &flappingRouteTestFixture{t: t, cancel: cancel, generator: newFlappingProviderGenerator(ctx)}
	t.Cleanup(fixture.finish)
	return fixture
}

// The callback only snapshots route metadata during synchronous construction.
func (self *flappingRouteTestFixture) addClient() *flappingRouteTestClient {
	self.t.Helper()
	observed := &flappingRouteTestClient{started: time.Now()}
	self.generator.observeRoutesForTest = func(client *Client, links []flappingProviderTestRoute, done <-chan struct{}) {
		observed.client, observed.links, observed.flapDone = client, append([]flappingProviderTestRoute(nil), links...), done
	}
	args, err := self.generator.NewClientArgs()
	if err != nil {
		self.t.Fatal(err)
	}
	observed.args = args
	client, err := self.generator.NewClient(self.generator.ctx, args, self.generator.NewClientSettings())
	if err != nil {
		self.t.Fatal(err)
	}
	observedClient := observed.client
	observed.client = client
	self.clients = append(self.clients, observed)
	if observedClient != client || len(observed.links) != 8 || observed.flapDone == nil {
		self.t.Fatal("flapping routing control lacks its actual eight installed provider links")
	}
	synctest.Wait()
	return observed
}

// All API calls quiesce before cancellation; join every real Client and the
// fixture's existing timer workers before a semantic-negative marker is valid.
func (self *flappingRouteTestFixture) finish() {
	self.finishOnce.Do(func() {
		self.cancel()
		for _, current := range self.clients {
			self.generator.RemoveClientWithArgs(current.client, current.args)
			current.client.Close()
		}
		self.generator.close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, current := range self.clients {
			if err := current.client.CloseAndWait(ctx); err != nil {
				self.t.Error(err)
			}
			if current.flapDone != nil {
				<-current.flapDone
			}
		}
		for _, provider := range self.generator.providerClients {
			if err := provider.CloseAndWait(ctx); err != nil {
				self.t.Error(err)
			}
		}
		self.generator.mu.Lock()
		retained := len(self.generator.unsubs)
		self.generator.mu.Unlock()
		if retained != 0 {
			self.t.Errorf("flapping teardown retained %d candidate route owners", retained)
		}
	})
}

// Success is the real Transfer acknowledgement of the actual reliable IpPing.
func flappingRouteTestPing(t *testing.T, client, provider *Client) {
	t.Helper()
	frame, err := ToFrame(&protocol.IpPing{}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	ack := make(chan error, 1)
	if !client.SendWithTimeout(frame, provider.ClientId(), func(err error) { ack <- err }, 200*time.Millisecond) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("addressed flapping control refused its actual reliable ping")
	}
	if err := <-ack; err != nil {
		t.Fatal("addressed flapping control ping was not acknowledged: ", err)
	}
	synctest.Wait()
}

// Only the foreign provider can read. A zero-wait route write aimed at the
// absent destination must refuse; it cannot feed that foreign Client.forward.
// Restore the destination and require a real reliable ping/ACK afterward.
func TestFlappingProviderRoutesDoNotDeliverToForeignReader(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFlappingRouteTestFixture(t)
		current := fixture.addClient()
		target, foreign := current.links[0], current.links[1]
		for index, link := range current.links {
			if index != 1 {
				link.provider.RouteManager().RemoveTransport(link.providerReceive)
			}
		}
		synctest.Wait()
		// The sole reader has its actual healthy return path before the probe.
		flappingRouteTestPing(t, current.client, foreign.provider)
		var forwarded atomic.Int32
		unsubscribe := foreign.provider.AddForwardCallback(func(path TransferPath, borrowed []byte) {
			if path.SourceId == current.client.ClientId() && path.DestinationId == target.provider.ClientId() {
				forwarded.Add(1)
			}
		})
		defer unsubscribe()
		ping, err := ToFrame(&protocol.IpPing{}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		probe, err := ProtoMarshal(&protocol.TransferFrame{
			TransferPath: TransferPath{SourceId: current.client.ClientId(), DestinationId: target.provider.ClientId()}.ToProtobuf(),
			Pack:         &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), Head: true, Frames: []*protocol.Frame{ping}},
		})
		MessagePoolReturn(ping.MessageBytes)
		if err != nil {
			t.Fatal(err)
		}
		writer := current.client.RouteManager().OpenMultiRouteWriter(DestinationId(target.provider.ClientId()))
		// Every runnable receiver has settled; the only live read belongs to
		// the wrong identity. This is an admission boundary, not a timeout.
		accepted, writeErr := writer.WriteDetailed(fixture.generator.ctx, probe, 0)
		if !accepted {
			MessagePoolReturn(probe)
		}
		current.client.RouteManager().CloseMultiRouteWriter(writer)
		synctest.Wait()
		if writeErr != nil {
			t.Fatal("routing probe failed outside the addressed admission boundary: ", writeErr)
		}
		if accepted {
			if forwarded.Load() != 1 {
				t.Fatalf("shared route consumed target frame without its exact foreign-forward witness: forwards=%d", forwarded.Load())
			}
			fixture.finish()
			if !t.Failed() {
				t.Fatal("FLAPPING_FIXTURE_FOREIGN_READER_CONSUMED_TARGET: actual foreign Client forwarded and discarded the addressed ping")
			}
			return
		}
		if forwarded.Load() != 0 {
			t.Fatal("refused addressed probe appeared at the foreign provider")
		}
		target.provider.RouteManager().UpdateTransport(target.providerReceive, []Route{target.toProvider})
		synctest.Wait()
		flappingRouteTestPing(t, current.client, target.provider)
		flappingRouteTestPing(t, current.client, foreign.provider)
		if forwarded.Load() != 0 {
			t.Fatal("healthy addressed traffic reached the foreign forwarding path")
		}
		fixture.finish()
	})
}

// Severing one candidate at the original one-second edge must remove only its
// links. A younger candidate keeps a real reliable ping/ACK on the same provider.
func TestFlappingProviderOriginalFlapPreservesYoungerCandidate(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFlappingRouteTestFixture(t)
		first := fixture.addClient()
		flappingRouteTestPing(t, first.client, first.links[0].provider)
		time.Sleep(time.Until(first.started.Add(time.Second / 2)))
		second := fixture.addClient()
		flappingRouteTestPing(t, second.client, second.links[0].provider)
		time.Sleep(time.Until(first.started.Add(time.Second).Add(-time.Nanosecond)))
		before := first.client.RouteManager().OpenMultiRouteWriter(DestinationId(first.links[0].provider.ClientId()))
		if len(before.GetActiveRoutes()) != 1 {
			t.Fatal("original candidate severed before its unchanged one-second edge")
		}
		first.client.RouteManager().CloseMultiRouteWriter(before)
		time.Sleep(time.Nanosecond)
		<-first.flapDone
		after := first.client.RouteManager().OpenMultiRouteWriter(DestinationId(first.links[0].provider.ClientId()))
		removed := len(after.GetActiveRoutes()) == 0
		first.client.RouteManager().CloseMultiRouteWriter(after)
		if !removed || first.client.IsDone() {
			t.Fatal("original flap did not sever routing while retaining the candidate health-detection lifecycle")
		}
		flappingRouteTestPing(t, second.client, second.links[0].provider)
		fixture.finish()
	})
}
