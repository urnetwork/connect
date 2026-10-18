// Exercises the temporary source/flow cancellation link separately from the
// unchanged real-TCP retirement roots, including its callback join and cost.
package connect

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
)

// Interposes only the first parent-Done query while AfterFunc registers. The
// embedded context still supplies the real cancellation state and identity.
type providerReturnRegistrationContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

// Holds registration before its first parent cancellation observation.
func (self *providerReturnRegistrationContext) Done() <-chan struct{} {
	self.once.Do(func() {
		close(self.entered)
		<-self.release
	})
	return self.Context.Done()
}

// An already-canceled flow must refuse synchronously, without waiting for the
// asynchronous AfterFunc callback to be scheduled.
func TestProviderReturnContextAlreadyCanceledFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sourceCtx, cancelSource := context.WithCancel(context.Background())
		defer cancelSource()
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		cancelFlow()
		link := newProviderReturnContextLink(sourceCtx, flowCtx)
		if link.ctx.Err() != context.Canceled {
			t.Error("already-canceled flow left an admission window")
		}
		link.close()
		if sourceCtx.Err() != nil {
			t.Fatal("flow cancellation reached the shared source")
		}
	})
}

// Cancellation during registration must have the same synchronous refusal
// boundary as cancellation before the link was created.
func TestProviderReturnContextCanceledDuringRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sourceCtx, cancelSource := context.WithCancel(context.Background())
		defer cancelSource()
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		defer cancelFlow()
		flow := &providerReturnRegistrationContext{
			Context: flowCtx,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		created := make(chan *providerReturnContextLink, 1)
		go func() {
			created <- newProviderReturnContextLink(sourceCtx, flow)
		}()
		<-flow.entered
		cancelFlow()
		close(flow.release)
		link := <-created
		if link.ctx.Err() != context.Canceled {
			t.Error("registration raced flow cancellation into a live admission")
		}
		link.close()
		if sourceCtx.Err() != nil {
			t.Fatal("registration canceled its shared source")
		}
	})
}

// Once cancellation starts its callback, releasing an item must join that
// exact callback even after it has canceled the admission context.
func TestProviderReturnContextCloseJoinsFiredCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sourceCtx, cancelSource := context.WithCancel(context.Background())
		defer cancelSource()
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		defer cancelFlow()
		link := newProviderReturnContextLink(sourceCtx, flowCtx)
		entered := make(chan struct{})
		release := make(chan struct{})
		link.afterFlowCancellationForTest = func() {
			close(entered)
			<-release
		}
		cancelFlow()
		<-entered
		if link.ctx.Err() != context.Canceled {
			t.Error("flow callback did not cancel admission before its join hook")
		}
		closed := make(chan struct{})
		go func() {
			link.close()
			close(closed)
		}()
		synctest.Wait()
		select {
		case <-closed:
			t.Error("link release returned while its cancellation callback was held")
		default:
		}
		close(release)
		<-closed
		if sourceCtx.Err() != nil {
			t.Fatal("joined flow callback canceled the shared source")
		}
	})
}

// A successful ordinary callback stops the unused cancellation registration;
// closing its temporary link neither cancels nor retains either parent.
func TestProviderReturnContextCloseStopsUnusedCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sourceCtx, cancelSource := context.WithCancel(context.Background())
		defer cancelSource()
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		defer cancelFlow()
		link := newProviderReturnContextLink(sourceCtx, flowCtx)
		var fired atomic.Bool
		link.afterFlowCancellationForTest = func() { fired.Store(true) }
		link.close()
		if link.ctx.Err() != context.Canceled || sourceCtx.Err() != nil || flowCtx.Err() != nil {
			t.Fatal("closing an unused link did not cancel only its temporary child")
		}
		cancelFlow()
		synctest.Wait()
		if fired.Load() {
			t.Fatal("stopped cancellation callback ran after link release")
		}
	})
}

// Source revocation and provider shutdown remain independent cancellation
// parents; neither may be replaced by the exact flow's context.
func TestProviderReturnContextRetainsSourceAndProviderCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, cancelProvider := range []bool{false, true} {
			providerCtx, cancelProviderContext := context.WithCancel(context.Background())
			sourceCtx, cancelSource := context.WithCancel(providerCtx)
			flowCtx, cancelFlow := context.WithCancel(context.Background())
			link := newProviderReturnContextLink(sourceCtx, flowCtx)
			if cancelProvider {
				cancelProviderContext()
			} else {
				cancelSource()
			}
			if link.ctx.Err() != context.Canceled {
				t.Errorf("shared parent cancellation was lost: provider=%t", cancelProvider)
			}
			link.close()
			if flowCtx.Err() != nil {
				t.Errorf("shared parent canceled the independent flow: provider=%t", cancelProvider)
			}
			cancelFlow()
			cancelSource()
			cancelProviderContext()
		}
	})
}

// Model the parent-Done-before-child-Done propagation interval explicitly:
// admission must inspect each exact owner even while its linked child is live.
func TestProviderReturnAdmissionChecksEveryOwner(t *testing.T) {
	for _, owner := range []string{"provider", "source", "flow", "link"} {
		providerCtx, cancelProvider := context.WithCancel(context.Background())
		sourceCtx, cancelSource := context.WithCancel(context.Background())
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		linkCtx, cancelLink := context.WithCancel(context.Background())
		provider := &RemoteUserNatProvider{
			ctx:      providerCtx,
			settings: DefaultRemoteUserNatProviderSettings(),
		}
		item := &providerReturnItem{
			sourceLifecycle:      &providerSourceLifecycle{ctx: sourceCtx},
			flowContext:          flowCtx,
			sendAdmissionContext: linkCtx,
			recoveryMode:         receiveRecoveryModeDedicatedTcpControl,
		}
		switch owner {
		case "provider":
			cancelProvider()
		case "source":
			cancelSource()
		case "flow":
			cancelFlow()
		case "link":
			cancelLink()
		}
		called := false
		if provider.retryReturnSend(item, 1, 0, func() bool {
			called = true
			return true
		}) || called {
			t.Errorf("canceled %s admitted while another context was still live", owner)
		}
		cancelLink()
		cancelFlow()
		cancelSource()
		cancelProvider()
	}
}

// Hold the real pure-ack callback after link registration but before its first
// send, with both Transfer slots free. Flow cancellation must refuse that first
// attempt, retire its socket-close reset, and return the exact pooled share.
func TestProviderTcpCancellationBeforeOpenCapacityAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderRetirementFixture(t)
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		old := fixture.nextSequence(1000)
		handshake := fixture.takeControls()
		if len(handshake) != 1 || !handshake[0].syn || !handshake[0].ack {
			t.Fatalf("initial handshake=%+v", handshake)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseHook)
		var first atomic.Bool
		fixture.provider.returnSendStarted = func() {
			if first.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
		}
		fixture.upload(old, fixture.upstreams[0], 1001, "x")
		<-entered
		fixture.oldAckWitness = <-fixture.oldAckPackets
		fixture.transfer.packAdmission.mutex.Lock()
		count := fixture.transfer.packAdmission.count
		fixture.transfer.packAdmission.mutex.Unlock()
		if count != 0 {
			t.Fatal("the first-send cancellation control did not have open capacity")
		}
		old.Cancel()
		synctest.Wait()
		releaseHook()
		synctest.Wait()
		select {
		case <-old.retirementDone:
		default:
			t.Error("open-capacity cancellation left a TCP child running")
		}
		if controls := fixture.takeControls(); len(controls) != 0 {
			t.Errorf("canceled generation admitted with open capacity: %+v", controls)
		}
		if !MessagePoolReturn(fixture.oldAckWitness) {
			t.Error("open-capacity cancellation retained the pure-ack packet")
		}
		fixture.oldAckWitness = nil
		if fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil || fixture.nat.ctx.Err() != nil {
			t.Fatal("open-capacity flow retirement canceled a shared owner")
		}
	})
}

// A dial refusal queues a regenerable control and then retires its flow.
// The shared worker must still send that final reset or unreachable signal.
func TestProviderTcpDialFailureSignalSurvivesFlowRetirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		for _, refused := range []bool{true, false} {
			fixture := newTcpProviderRetirementFixture(t)
			synctest.Wait()
			fixture.nat.settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					if refused {
						return nil, syscall.ECONNREFUSED
					}
					return nil, syscall.ENETUNREACH
				},
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseHook)
			fixture.provider.returnSendStarted = func() {
				close(entered)
				<-release
			}
			fixture.send(tcpFlagSyn, 1000, 0, nil)
			old := fixture.nextSequence(1000)
			<-entered
			synctest.Wait()
			select {
			case <-old.retirementDone:
			default:
				t.Fatal("a shared terminal-control worker held its originating TCP flow")
			}
			if old.ctx.Err() == nil {
				t.Fatal("dial-failure flow had not canceled before worker release")
			}
			releaseHook()
			synctest.Wait()
			var pack *SendPack
			select {
			case pack = <-fixture.transfer.packs:
			default:
				t.Fatalf("retired dial failure lost its signal: refused=%t", refused)
			}
			func() {
				defer pack.releaseRaw()
				defer pack.returnFrames()
				packet := providerCallbackQueuePacketBytes(t, pack)
				if refused {
					var path IpPath
					if _, err := parseIpPathWithPayloadBorrowed(packet, &path); err != nil || !path.Rst || !path.Ack || path.AckSequenceNumber != 1001 {
						t.Errorf("refused dial did not admit its exact reset: path=%+v err=%v", path, err)
					}
				} else if path, ok := ipParseIcmpUnreachable(packet); !ok || path.Protocol != IpProtocolTcp || path.SourcePort != fixture.path.SourcePort {
					t.Errorf("unroutable dial did not admit its exact unreachable: path=%+v ok=%t", path, ok)
				}
			}()
			if fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil || fixture.nat.ctx.Err() != nil {
				t.Fatal("terminal signal required closing a shared owner")
			}
		}
	})
}

// Isolates the added link allocation/registration cost for one dedicated
// callback. A batch shares one link across all its bounded chunks.
func BenchmarkProviderReturnContextLink(b *testing.B) {
	sourceCtx, cancelSource := context.WithCancel(context.Background())
	defer cancelSource()
	flowCtx, cancelFlow := context.WithCancel(context.Background())
	defer cancelFlow()
	b.ReportAllocs()
	for b.Loop() {
		link := newProviderReturnContextLink(sourceCtx, flowCtx)
		if link.ctx.Err() != nil {
			b.Fatal("unexpected canceled link")
		}
		link.close()
	}
}

// Measures the source-only context lookup retained by nonblocking callbacks.
// This is a microbenchmark baseline, not provider throughput or mobile pacing.
func BenchmarkProviderReturnContextSourceOnly(b *testing.B) {
	sourceCtx, cancelSource := context.WithCancel(context.Background())
	defer cancelSource()
	item := &providerReturnItem{
		sourceLifecycle: &providerSourceLifecycle{ctx: sourceCtx},
	}
	b.ReportAllocs()
	for b.Loop() {
		if item.sendContext(context.Background()).Err() != nil {
			b.Fatal("unexpected canceled source")
		}
	}
}
