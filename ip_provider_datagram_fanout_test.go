package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

func TestProviderDatagramFanoutFailedPreparationRollsBackEveryOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		_, provider, sequence, root := newProviderDatagramReturnTest(t)
		nat := provider.localUserNat
		nat.providerDatagramSenders = NewCallbackList[*providerDatagramSender]()
		nat.receivePacketsCallbacks = NewCallbackList[ReceivePacketsFunction]()
		nat.addProviderDatagramSender(provider.datagramSender)
		closedContext, cancel := context.WithCancel(context.Background())
		cancel()
		nat.addProviderDatagramSender(&providerDatagramSender{provider: &RemoteUserNatProvider{ctx: closedContext}})
		sequence.prepareReturnReadCallback = nat.prepareUdpReturnRead
		before := root.UsedByteCount()
		lease, err := sequence.prepareReturnRead(1000)
		if lease != nil || !errors.Is(err, errUdpReturnReadPaused) {
			t.Fatalf("partial fanout admitted a read: lease=%v err=%v", lease, err)
		}
		if root.UsedByteCount() != before || sequence.returnReadPending.Load() {
			t.Fatal("failed fanout retained a prepared owner or memory claim")
		}
	})
}

func TestProviderDatagramPublicDispatchFullQueueAndTerminalGate(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		_, _, sequence, _ := newProviderDatagramReturnTest(t)
		dispatcher := newUdpReceiveDispatcher(sequence.ctx, sequence.udpBufferSettings)
		sequence.receiveDispatcher, sequence.receiveShard = dispatcher, 0
		shard := &dispatcher.shards[0]
		// Pin the exact full-queue boundary without a consumer racing it.
		shard.startOnce.Do(func() {})
		released := 0
		newPublic := func() *providerDatagramPublicDispatch {
			fanout := &providerDatagramFanout{sequence: sequence, released: func() { released++ }}
			fanout.remaining.Store(1)
			return &providerDatagramPublicDispatch{fanout: fanout, packets: [][]byte{MessagePoolGet(32)}}
		}
		first, pending := newPublic(), newPublic()
		if !first.tryEnqueue() || pending.tryEnqueue() || cap(shard.items) != 1 {
			t.Fatal("public dispatch did not preserve its original one-slot queue")
		}
		if len(pending.packets) != 1 || released != 0 {
			t.Fatal("full public queue consumed its pending read owner")
		}
		dispatcher.waitForLifecycle()
		if released != 1 || pending.tryEnqueue() {
			t.Fatal("terminal dispatcher admitted a late prepared publication")
		}
		pending.release()
		if released != 2 {
			t.Fatal("public owners did not release exactly once")
		}
	})
}

func TestProviderDatagramFanoutPublicCallbackDoesNotBlockSocketProducer(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, _ := newProviderDatagramReturnTest(t)
		dispatcher := newUdpReceiveDispatcher(sequence.ctx, sequence.udpBufferSettings)
		sequence.receiveDispatcher = dispatcher
		sequence.receiveShard = dispatcher.assignShard()
		defer func() { sequence.Close(); dispatcher.waitForLifecycle() }()
		nat := provider.localUserNat
		nat.providerDatagramSenders = NewCallbackList[*providerDatagramSender]()
		nat.receivePacketsCallbacks = NewCallbackList[ReceivePacketsFunction]()
		nat.addProviderDatagramSender(provider.datagramSender)
		sequence.prepareReturnReadCallback = nat.prepareUdpReturnRead
		entered, release, committed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		defer close(release)
		nat.AddReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) {
			if len(packets) != 1 || len(packets[0]) != 1028 {
				t.Error("public callback did not borrow the exact datagram")
			}
			close(entered)
			<-release
		})
		lease, err := sequence.prepareReturnRead(1000)
		if err != nil || lease == nil {
			t.Fatalf("prepare: %v", err)
		}
		packets, err := sequence.DataPackets(make([]byte, 1000), 1000, sequence.udpBufferSettings.Mtu)
		if err != nil {
			lease.abort()
			t.Fatal(err)
		}
		go func() { lease.commit(packets); close(committed) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("public callback was not dispatched")
		}
		select {
		case <-committed:
		default:
			t.Fatal("public callback blocked the shared socket producer")
		}
		if !sequence.returnReadPending.Load() {
			t.Fatal("borrowed public callback lost its read-owner memory claim")
		}
		// Transfer and the provider worker continue despite the blocked public
		// callback; only this flow's next read remains paused by its own owner.
		fixture.forward(fixture.takePack(0), fixture.receiverIn)
		fixture.acknowledge()
		release <- struct{}{}
		synctest.Wait()
		if sequence.returnReadPending.Load() {
			t.Fatal("public callback release did not retire the read owner")
		}
	})
}
