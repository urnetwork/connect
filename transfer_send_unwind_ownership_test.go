// A send sequence whose initial write unwinds leaves each pooled frame,
// acknowledgement and route-write report with exactly one owner.
package connect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The deliberate unwind. Its frame names the one recovery warning it provokes.
var errSendUnwindForTest = errors.New("send unwind for test")

func panicSendUnwindForTest() {
	panic(errSendUnwindForTest)
}

// One destination's send sequence, held before its first pass so every Pack is
// queued when it starts, and joined after its write unwinds.
type sendUnwindFixture struct {
	client      *Client
	destination Id
	route       Route
	startWorker chan struct{}
	runDone     chan struct{}
	results     chan error
}

// Runs body in a bubble against a client whose only traffic is the test's.
// The H1 carrier drains already-queued Packs into one write; the plain carrier
// splits logical groups at two frames.
func sendUnwindTest(
	t *testing.T,
	h1 bool,
	configure func(settings *ClientSettings, fixture *sendUnwindFixture),
	body func(t *testing.T, fixture *sendUnwindFixture),
) {
	t.Helper()
	assertMessagePoolOwnership(t)
	previousLogger := DefaultLogger()
	logger := &handleErrorTestLogger{
		Logger:    previousLogger,
		testFrame: "connect.panicSendUnwindForTest(",
	}
	SetDefaultLogger(logger)
	t.Cleanup(func() { SetDefaultLogger(previousLogger) })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fixture := &sendUnwindFixture{
			destination: NewId(),
			route:       make(Route, 8),
			startWorker: make(chan struct{}),
			runDone:     make(chan struct{}),
			results:     make(chan error, 8),
		}
		var runDoneOnce sync.Once
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.SkipClientKeyPublication = true
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination != fixture.destination {
				return
			}
			select {
			case <-fixture.startWorker:
			case <-ctx.Done():
			}
		}
		settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == fixture.destination {
				runDoneOnce.Do(func() { close(fixture.runDone) })
			}
		}
		configure(settings, fixture)
		fixture.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			fixture.client.CloseAndWait(context.Background())
			for len(fixture.route) > 0 {
				MessagePoolReturn(<-fixture.route)
			}
		}()
		fixture.client.ContractManager().AddNoContractPeer(fixture.destination)
		var transport Transport = NewSendClientTransport(DestinationId(fixture.destination))
		if h1 {
			transport = &h1SendClientTransportForGroupTest{
				sendClientTransport: NewSendClientTransport(DestinationId(fixture.destination)),
			}
		}
		fixture.client.RouteManager().UpdateTransport(transport, []Route{fixture.route})
		body(t, fixture)
	})
	logger.assertWarnings(t, handleErrorTestWarning{
		prefix:    "Unexpected error",
		recovered: "*errors.errorString=send unwind for test",
	})
}

// Records one result per callback invocation.
func (self *sendUnwindFixture) callback(err error) {
	self.results <- err
}

// Starts the held sequence and joins it after its write unwinds.
func (self *sendUnwindFixture) unwind(t *testing.T) {
	t.Helper()
	close(self.startWorker)
	<-self.runDone
	synctest.Wait()
}

// Every admitted Pack's callback ran exactly once, with the closing error.
func (self *sendUnwindFixture) requireResults(t *testing.T, want int) {
	t.Helper()
	if got := len(self.results); got != want {
		t.Fatalf("acknowledgement callbacks ran %d times for %d Packs", got, want)
	}
	for range want {
		if err := <-self.results; err == nil {
			t.Fatal("an unwound send reported delivery")
		}
	}
}

// Returns the wire Packs the unwound write left on the route, decoded.
func (self *sendUnwindFixture) takeWires(t *testing.T) []*protocol.Pack {
	t.Helper()
	packs := []*protocol.Pack{}
	for len(self.route) > 0 {
		wire := <-self.route
		packs = append(packs, decodeSendPackLifecycleWirePack(t, wire))
		MessagePoolReturn(wire)
	}
	return packs
}

// Records one logical group's range dispositions and terminal results, and
// can unwind when serialization reports its materialized range.
type sendUnwindGroupTarget struct {
	stateLock            sync.Mutex
	unwindOnMaterialized bool
	dispositions         []string
	results              []error
}

func (self *sendUnwindGroupTarget) sendGroupDisposition(start, end int, materialized bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dispositions = append(self.dispositions, fmt.Sprintf("%d-%d materialized=%t", start, end, materialized))
	}()
	if materialized && self.unwindOnMaterialized {
		panicSendUnwindForTest()
	}
}

func (self *sendUnwindGroupTarget) sendAckResult(value ByteCount, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.results = append(self.results, err)
}

// The gate failure's shape: the hook after a reliable Pack's initial write
// unwinds Run. Like ControlSync, the sender keeps a share of the frame and
// returns it from the Pack's callback. Serialization had returned the Pack's
// frame and published the callback in the retained item, so the Pack Run
// still held must return neither: each would be a second pool return.
func TestSendSequenceUnwindAfterInitialWriteReturnsPackOnce(t *testing.T) {
	sendUnwindTest(t, false, func(settings *ClientSettings, fixture *sendUnwindFixture) {
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, _ uint64) {
			if id.Destination == fixture.destination {
				panicSendUnwindForTest()
			}
		}
	}, func(t *testing.T, fixture *sendUnwindFixture) {
		frame := &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: MessagePoolGet(1000),
		}
		retained := MessagePoolShareReadOnly(frame.MessageBytes)
		callback := func(err error) {
			MessagePoolReturn(retained)
			fixture.callback(err)
		}
		if !fixture.client.SendWithTimeout(frame, fixture.destination, callback, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			MessagePoolReturn(retained)
			t.Fatal("reliable Pack not admitted")
		}
		fixture.unwind(t)
		if packs := fixture.takeWires(t); len(packs) != 1 {
			t.Fatalf("initial write left %d wire Packs, want 1", len(packs))
		}
		fixture.requireResults(t, 1)
	})
}

// A ready drain serializes several queued Packs into one item; each Pack's
// frame and callback still belong to that item alone after the unwind.
func TestSendSequenceUnwindAfterCoalescedWriteReturnsEachPackOnce(t *testing.T) {
	sendUnwindTest(t, true, func(settings *ClientSettings, fixture *sendUnwindFixture) {
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, _ uint64) {
			if id.Destination == fixture.destination {
				panicSendUnwindForTest()
			}
		}
	}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, 3, 64)
		for index, frame := range frames {
			if !fixture.client.SendWithTimeout(frame, fixture.destination, fixture.callback, time.Second) {
				for _, unsent := range frames[index:] {
					MessagePoolReturn(unsent.MessageBytes)
				}
				releaseTransferGroupTestWitnesses(t, frames, witnesses)
				t.Fatalf("ready Pack %d not admitted", index)
			}
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		packs := fixture.takeWires(t)
		if len(packs) != 1 || len(packs[0].Frames) != len(frames) {
			t.Fatalf("ready drain wrote %d wire Packs, want one carrying %d frames", len(packs), len(frames))
		}
		fixture.requireResults(t, len(frames))
	})
}

// A logical group that fits one chunk hands its whole frame list and its own
// callback to serialization; the unwind leaves the Pack only itself.
func TestSendSequenceUnwindAfterGroupWriteReturnsGroupOnce(t *testing.T) {
	sendUnwindTest(t, false, func(settings *ClientSettings, fixture *sendUnwindFixture) {
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, _ uint64) {
			if id.Destination == fixture.destination {
				panicSendUnwindForTest()
			}
		}
	}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, sendPackBatchMaxFrames, 64)
		if ok, err := fixture.client.sendGroupWithTimeoutDetailed(frames, fixture.destination, fixture.callback, time.Second); !ok || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatalf("one-chunk group not admitted: %t %v", ok, err)
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		if packs := fixture.takeWires(t); len(packs) != 1 {
			t.Fatalf("one-chunk group wrote %d wire Packs, want 1", len(packs))
		}
		fixture.requireResults(t, 1)
	})
}

// The cursor of a split group passes a chunk before serialization takes it.
// An unwind after the first chunk's write disposes only the later chunk, and
// the group's one callback waits for both.
func TestSendSequenceUnwindAfterGroupChunkReturnsEachChunkOnce(t *testing.T) {
	sendUnwindTest(t, false, func(settings *ClientSettings, fixture *sendUnwindFixture) {
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, _ uint64) {
			if id.Destination == fixture.destination {
				panicSendUnwindForTest()
			}
		}
	}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, sendPackBatchMaxFrames+1, 64)
		if ok, err := fixture.client.sendGroupWithTimeoutDetailed(frames, fixture.destination, fixture.callback, time.Second); !ok || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatalf("split group not admitted: %t %v", ok, err)
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		packs := fixture.takeWires(t)
		if len(packs) != 1 || len(packs[0].Frames) != sendPackBatchMaxFrames {
			t.Fatalf("split group wrote %d wire Packs before the unwind, want only its first chunk", len(packs))
		}
		fixture.requireResults(t, 1)
	})
}

// An observer inside the write unwinds before the chunk reports its first
// route write. Serialization reports it, or the group's terminal would wait on
// that report for ever; the callback runs once, after every chunk.
func TestSendSequenceUnwindInsideGroupWriteCompletesTheGroup(t *testing.T) {
	sendUnwindTest(t, false, func(*ClientSettings, *sendUnwindFixture) {}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, sendPackBatchMaxFrames+1, 64)
		unwindWrite := observeTransportWrite(func(TransportType) { panicSendUnwindForTest() })
		if ok, err := fixture.client.sendGroupWithTimeoutDetailed(frames, fixture.destination, fixture.callback, time.Second, unwindWrite); !ok || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatalf("split group not admitted: %t %v", ok, err)
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		if packs := fixture.takeWires(t); len(packs) != 1 {
			t.Fatalf("split group wrote %d wire Packs before the unwind, want 1", len(packs))
		}
		fixture.requireResults(t, 1)
	})
}

// Nothing retains an unacknowledged item. When its write unwinds, the item
// is serialization's to dispose: its callback runs once and its wire frame
// returns, while the frame the Pack gave it is not returned again.
func TestSendSequenceUnwindInsideNoAckWriteReturnsItsItem(t *testing.T) {
	sendUnwindTest(t, false, func(*ClientSettings, *sendUnwindFixture) {}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, 1, 64)
		unwindWrite := observeTransportWrite(func(TransportType) { panicSendUnwindForTest() })
		if !fixture.client.SendWithTimeout(frames[0], fixture.destination, fixture.callback, time.Second, NoAck(), unwindWrite) {
			MessagePoolReturn(frames[0].MessageBytes)
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatal("NoAck Pack not admitted")
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		if packs := fixture.takeWires(t); len(packs) != 1 {
			t.Fatalf("NoAck write left %d wire Packs, want 1", len(packs))
		}
		fixture.requireResults(t, 1)
	})
}

// The NoAck completion is handed on before its observer runs. An observer
// that unwinds is not called a second time; the unretained item still is
// disposed once.
func TestSendSequenceUnwindInNoAckObserverReturnsItsItem(t *testing.T) {
	var completedLock sync.Mutex
	completed := 0
	sendUnwindTest(t, false, func(settings *ClientSettings, fixture *sendUnwindFixture) {
		settings.SendBufferSettings.NoAckSendObserver = func(observation NoAckSendObservation) {
			if observation.DestinationId != fixture.destination || observation.Phase != NoAckSendPhaseCompleted {
				return
			}
			func() {
				completedLock.Lock()
				defer completedLock.Unlock()
				completed++
			}()
			panicSendUnwindForTest()
		}
	}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, 1, 64)
		if !fixture.client.SendWithTimeout(frames[0], fixture.destination, fixture.callback, time.Second, NoAck()) {
			MessagePoolReturn(frames[0].MessageBytes)
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatal("NoAck Pack not admitted")
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		if packs := fixture.takeWires(t); len(packs) != 1 {
			t.Fatalf("NoAck write left %d wire Packs, want 1", len(packs))
		}
		fixture.requireResults(t, 1)
		completedLock.Lock()
		defer completedLock.Unlock()
		if completed != 1 {
			t.Fatalf("NoAck completion observed %d times, want 1", completed)
		}
	})
}

// Serialization owns a group from its call. An unwind before retention leaves
// it every part: the range is reported unmaterialized, the frames return and
// the group's terminal result arrives once.
func TestSendSequenceUnwindBeforeRetentionReturnsTheGroupOnce(t *testing.T) {
	target := &sendUnwindGroupTarget{}
	sendUnwindTest(t, false, func(settings *ClientSettings, fixture *sendUnwindFixture) {
		settings.SendBufferSettings.beforeGroupRetainForTest = panicSendUnwindForTest
	}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, sendPackBatchMaxFrames, 64)
		if ok, err := fixture.client.sendGroupToTargetWithTimeoutDetailed(frames, fixture.destination, MultiHopId{}, nil, target, time.Second); !ok || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatalf("group not admitted: %t %v", ok, err)
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		if packs := fixture.takeWires(t); len(packs) != 0 {
			t.Fatalf("a group unwound before retention wrote %d wire Packs", len(packs))
		}
		target.stateLock.Lock()
		defer target.stateLock.Unlock()
		if want := []string{fmt.Sprintf("0-%d materialized=false", sendPackBatchMaxFrames)}; fmt.Sprint(target.dispositions) != fmt.Sprint(want) {
			t.Fatalf("group dispositions = %q, want %q", target.dispositions, want)
		}
		if len(target.results) != 1 || target.results[0] == nil {
			t.Fatalf("group terminal results = %v, want one error", target.results)
		}
	})
}

// The materialized range is handed on before the group target hears it. A
// target that unwinds there is told once; the frames, which serialization had
// not yet returned, return once, and the retained item reports once.
func TestSendSequenceUnwindInGroupDispositionReportsItOnce(t *testing.T) {
	target := &sendUnwindGroupTarget{unwindOnMaterialized: true}
	sendUnwindTest(t, false, func(*ClientSettings, *sendUnwindFixture) {}, func(t *testing.T, fixture *sendUnwindFixture) {
		frames, witnesses := transferGroupTestFrames(t, sendPackBatchMaxFrames, 64)
		if ok, err := fixture.client.sendGroupToTargetWithTimeoutDetailed(frames, fixture.destination, MultiHopId{}, nil, target, time.Second); !ok || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
			t.Fatalf("group not admitted: %t %v", ok, err)
		}
		fixture.unwind(t)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
		if packs := fixture.takeWires(t); len(packs) != 0 {
			t.Fatalf("a group unwound before its write wrote %d wire Packs", len(packs))
		}
		target.stateLock.Lock()
		defer target.stateLock.Unlock()
		if want := []string{fmt.Sprintf("0-%d materialized=true", sendPackBatchMaxFrames)}; fmt.Sprint(target.dispositions) != fmt.Sprint(want) {
			t.Fatalf("group dispositions = %q, want %q", target.dispositions, want)
		}
		if len(target.results) != 1 || target.results[0] == nil {
			t.Fatalf("group terminal results = %v, want one error", target.results)
		}
	})
}
