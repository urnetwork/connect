// A local admission wait belongs to one candidate, not the shared evaluation
// verdict lock. Every candidate and ACK below uses the existing real fixture.
package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The acquisition caller and A's real ACK remain explicitly parked while B is
// in its actual source capacity queue. No fake-clock wait samples a blocked lock.
func testMultiClientExpandLocalAdmissionIsolation(t *testing.T, mode string) {
	t.Helper()
	cause := ""
	// Registered before pool/client/source cleanup: a named expected failure
	// is emitted only after every independent ownership check has succeeded.
	t.Cleanup(func() {
		if cause != "" && !t.Failed() {
			t.Fatal(cause)
		}
	})
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newMultiClientExpandLifecycleFixture(t)
	window := fixture.window
	observations := &InitialPingObservations{}
	window.settings.InitialPingObservations = observations
	// Postpone the independent continuous monitors: only the actual evaluator
	// may own this exact B capacity waiter. No ping verdict is fabricated.
	window.settings.startChannelPingForTest = func(func()) {}
	generator := window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
	firstArgs := <-window.clientChannelArgs
	secondIdentity, err := generator.NewClientArgs()
	if err != nil {
		t.Fatal(err)
	}
	secondArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *secondIdentity,
		Destination:                    firstArgs.Destination,
		DestinationStats:               firstArgs.DestinationStats,
	}
	window.clientChannelArgs <- firstArgs
	budget := NewTransferMemoryBudget(0)
	capacityPublished := make(chan struct{})
	capacityWaiting := make(chan struct{})
	createdB := make(chan *Client, 1)
	addedA := make(chan *multiClientChannel, 1)
	acquisition := make(chan *sync.Mutex, 1)
	releaseAcquisition := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAcquisition) }) }
	var capacityOnce, waitingOnce sync.Once
	var clientsLock sync.Mutex
	clients := []*Client{}
	var sourceB *SendSequence
	var expandDone <-chan int
	knownSenders := 0 // Only this test goroutine writes cleanup eligibility.
	aReturned, aFinished := make(chan struct{}), make(chan struct{})
	bReturned, bFinished := make(chan struct{}), make(chan struct{})
	releaseAResult, releaseBResult := make(chan struct{}), make(chan struct{})
	var releaseAOnce, releaseBOnce sync.Once
	resumeAResult := func() { releaseAOnce.Do(func() { close(releaseAResult) }) }
	resumeBResult := func() { releaseBOnce.Do(func() { close(releaseBResult) }) }
	var aReturnOnce, aFinishOnce, bReturnOnce, bFinishOnce sync.Once
	window.afterExpandPingSendForTest = func(client *multiClientChannel, finished bool) {
		if client.ClientId() == firstArgs.ClientId {
			if finished {
				aFinishOnce.Do(func() { close(aFinished) })
				return
			}
			aReturnOnce.Do(func() { close(aReturned) })
			if mode == "ack-first" {
				select {
				case <-releaseAResult:
				case <-window.ctx.Done():
				}
			}
		} else if client.ClientId() == secondArgs.ClientId {
			if finished {
				bFinishOnce.Do(func() { close(bFinished) })
				return
			}
			bReturnOnce.Do(func() { close(bReturned) })
			if mode == "pass-ended" {
				select {
				case <-releaseBResult:
				case <-window.ctx.Done():
				}
			}
		}
	}
	window.afterExpandAcquisitionForTest = func(mutex *sync.Mutex) {
		acquisition <- mutex
		select {
		case <-releaseAcquisition:
		case <-window.ctx.Done():
		}
	}
	window.clientAddCallback = func(client *multiClientChannel) {
		if client.ClientId() == firstArgs.ClientId {
			select {
			case addedA <- client:
			default:
			}
		}
	}
	expirePing := make(chan struct{})
	window.expireExpandPingForTest = expirePing
	expirePass := make(chan struct{})
	window.expireExpandPassForTest = expirePass
	originalNewClient := generator.newClient
	generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
		isB := args.ClientId == secondArgs.ClientId
		if isB {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			previous := settings.SendBufferSettings.beforeResendCapacityWaitForTest
			settings.SendBufferSettings.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
				if previous != nil {
					previous(id)
				}
				if id.Destination == secondArgs.Destination.Tail() && id.LogicalLane == 0 {
					capacityOnce.Do(func() { close(capacityPublished) })
				}
			}
		}
		client, err := originalNewClient(ctx, args, settings)
		if err != nil {
			return client, err
		}
		clientsLock.Lock()
		clients = append(clients, client)
		clientsLock.Unlock()
		if !isB {
			return client, nil
		}
		// This real ordinary probe creates the exact provider sequence. With a
		// zero exact retained budget it cannot materialize a provider frame.
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.IpPing{})
		accepted, _ := client.SendMultiHopWithTimeoutDetailed(frame, secondArgs.Destination, nil, 0)
		if !accepted {
			MessagePoolReturn(frame.MessageBytes)
		}
		select {
		case <-capacityPublished:
		case <-ctx.Done():
			return client, ctx.Err()
		}
		client.sendBuffer.mutex.Lock()
		for id, source := range client.sendBuffer.sendSequences {
			if id.Destination == secondArgs.Destination.Tail() && id.LogicalLane == 0 && !id.ForceStream && !id.CompanionContract {
				sourceB = source
				break
			}
		}
		client.sendBuffer.mutex.Unlock()
		if sourceB == nil || sourceB.resendQueue.Budget() != budget || !sourceB.resendCapacityUnavailable.Load() {
			return client, fmt.Errorf("B did not publish its actual zero-budget source capacity")
		}
		// Installed before B is handed to the evaluator; the seed had no
		// positive admission timeout and cannot invoke this producer hook.
		source := sourceB
		source.afterRetainedAdmissionDemandForTest = func() {
			if source.resendCapacityUnavailable.waiterCount.Load() != 0 {
				waitingOnce.Do(func() { close(capacityWaiting) })
			}
		}
		createdB <- client
		return client, nil
	}
	// This cleanup runs before the base fixture's provider/client joins. Every
	// early return releases both gates and cancels the blocked source first.
	t.Cleanup(func() {
		fixture.releasePing()
		resumeAResult()
		resumeBResult()
		release()
		fixture.cancelEvaluation()
		fixture.cancelWindow()
		clientsLock.Lock()
		joinedClients := append([]*Client(nil), clients...)
		clientsLock.Unlock()
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelJoin()
		for _, client := range joinedClients {
			client.Cancel()
			if err := client.CloseAndWait(joinCtx); err != nil {
				t.Errorf("join evaluation-isolation client: %v", err)
			}
		}
		if expandDone != nil {
			select {
			case <-expandDone:
			case <-joinCtx.Done():
				t.Error("evaluation-isolation expansion did not join")
			}
		}
		for _, sender := range []<-chan struct{}{aFinished, bFinished}[:knownSenders] {
			select {
			case <-sender:
			case <-joinCtx.Done():
				t.Error("evaluation-isolation sender did not join")
			}
		}
		reserved, released := budget.Counts()
		if budget.UsedByteCount() != 0 || reserved != released {
			t.Errorf("evaluation-isolation budget retained used=%d reserved=%d released=%d", budget.UsedByteCount(), reserved, released)
		}
	})
	done := make(chan int, 1)
	expandDone = done
	go func() {
		done <- window.expand(WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2}, 0, 0, 2, 2, 2, 0, windowExpandPassPlan{})
	}()
	fixture.wait(t, "A's actual evaluation ACK before its verdict mutex", fixture.pingResultEntered)
	knownSenders = 1
	select {
	case window.clientChannelArgs <- secondArgs:
	case <-fixture.waitCtx.Done():
		t.Fatal("B candidate acquisition did not complete")
	}
	var clientB *Client
	select {
	case clientB = <-createdB:
	case <-fixture.waitCtx.Done():
		t.Fatal("B real client did not finish setup")
	}
	fixture.wait(t, "B's actual local retained-capacity waiter", capacityWaiting)
	knownSenders = 2
	var evaluationMutex *sync.Mutex
	select {
	case evaluationMutex = <-acquisition:
	case <-fixture.waitCtx.Done():
		t.Fatal("candidate acquisition did not reach its outside-lock barrier")
	}
	if clientB.ctx.Err() != nil || sourceB == nil || sourceB.ctx.Err() != nil || budget.TotalByteCount() != 0 || budget.UsedByteCount() != 0 || sourceB.resendCapacityUnavailable.waiterCount.Load() != 1 {
		t.Fatal("isolation observation lost the one actual blocked candidate producer")
	}
	// A is parked before this mutex, and acquisition is parked outside it.
	// B is therefore the only possible owner at this exact observation.
	if !evaluationMutex.TryLock() {
		cause = "EVALUATION_LOCAL_ADMISSION_HELD_HEALTHY_VERDICT: real_ack=true local_waiter=1 candidate_budget=0"
		return
	}
	evaluationMutex.Unlock()
	if mode == "window-canceled" || mode == "epoch-canceled" || mode == "pass-ended" {
		wantFailures := 0
		if mode == "window-canceled" {
			fixture.cancelWindow()
		}
		if mode == "epoch-canceled" {
			fixture.cancelEvaluation()
		}
		if mode == "pass-ended" {
			close(expirePass)
			wantFailures = 2
		}
		release()
		if count := fixture.result(t, done); count != 0 {
			t.Fatalf("ended evaluation admitted %d providers", count)
		}
		expandDone = nil
		select {
		case <-clientB.ctx.Done():
		case <-fixture.waitCtx.Done():
			t.Fatal("ended pass could not cancel blocked B before releasing its budget")
		}
		if mode == "pass-ended" {
			fixture.wait(t, "B's actual failed send return after pass cancellation", bReturned)
			select {
			case <-bFinished:
				t.Fatal("held late send result escaped its exact boundary")
			default:
			}
		}
		if got := window.failures.counts(time.Now())[windowFailureProvider]; got != wantFailures {
			t.Fatalf("evaluation terminal failures=%d, want %d", got, wantFailures)
		}
		fixture.releasePing()
		fixture.wait(t, "late actual ACK after pass end", fixture.pingResultDone)
		resumeBResult()
		fixture.wait(t, "late B sender terminal owner", bFinished)
		fixture.wait(t, "A sender terminal owner", aFinished)
		if got := window.failures.counts(time.Now())[windowFailureProvider]; got != wantFailures {
			cause = fmt.Sprintf("EVALUATION_LATE_SEND_RESULT_RECONVICTED_RETURNED_PASS: failures=%d want=%d", got, wantFailures)
			return
		}
		if fixture.clientCount() != 0 || budget.TotalByteCount() != 0 || budget.UsedByteCount() != 0 {
			t.Fatal("late ACK/send result resurrected a provider or retained failed-source credit")
		}
		var terminals uint64
		for _, observation := range observations.Snapshot() {
			terminals += observation.Count
		}
		if terminals != 2 {
			t.Fatalf("initial evaluation outcomes=%d, want exactly two", terminals)
		}
		fixture.assertNoDirectArgsRemoval(t)
		return
	}
	if mode == "ack-first" {
		fixture.wait(t, "A's actual successful send return held before verdict handling", aReturned)
	}
	fixture.releasePing()
	var healthy *multiClientChannel
	select {
	case healthy = <-addedA:
	case <-fixture.waitCtx.Done():
		t.Fatal("healthy real ping ACK was not admitted beside B's local pressure")
	}
	if healthy.ClientId() != firstArgs.ClientId || healthy.IsDone() || clientB.ctx.Err() != nil || sourceB.resendCapacityUnavailable.waiterCount.Load() != 1 || budget.TotalByteCount() != 0 {
		t.Fatal("healthy admission disturbed the independently blocked B producer")
	}
	if mode == "ack-first" {
		select {
		case <-aFinished:
			t.Fatal("successful sender bypassed its held return boundary")
		default:
		}
		resumeAResult()
		fixture.wait(t, "A result after its real ACK already admitted", aFinished)
		if fixture.clientCount() != 1 || window.failures.counts(time.Now())[windowFailureProvider] != 0 {
			t.Fatal("ACK-before-send-result caused duplicate provider admission or failure")
		}
	}
	// A second real ACK proves that healthy transfer work remains usable while
	// the same blocked producer still owns its original local wait.
	acknowledged := make(chan error, 1)
	accepted, err := healthy.SendDetailedMessage(&protocol.IpPing{}, window.settings.PingWriteTimeout, func(err error) {
		select {
		case acknowledged <- err:
		default:
			t.Error("healthy sibling ACK completed more than once")
		}
	})
	if !accepted || err != nil {
		t.Fatalf("healthy sibling probe refused: accepted=%t err=%v", accepted, err)
	}
	select {
	case err := <-acknowledged:
		if err != nil {
			t.Fatalf("healthy sibling ACK: %v", err)
		}
	case <-fixture.waitCtx.Done():
		t.Fatal("healthy sibling's real ACK did not complete")
	}
	if sourceB.resendCapacityUnavailable.waiterCount.Load() != 1 || clientB.ctx.Err() != nil {
		t.Fatal("healthy sibling progress released or canceled B's unrelated local wait")
	}
	// The existing timeout branch must acquire the verdict mutex and cancel B
	// while its admission is still blocked, before any budget is returned.
	close(expirePing)
	select {
	case <-clientB.ctx.Done():
	case <-fixture.waitCtx.Done():
		t.Fatal("ping timeout could not cancel locally blocked B")
	}
	if healthy.IsDone() || budget.TotalByteCount() != 0 {
		t.Fatal("B's timeout tore down healthy A or changed its admission cap")
	}
	release()
	if count := fixture.result(t, done); count != 1 {
		t.Fatalf("actual admitted providers=%d, want healthy A only", count)
	}
	// Cleanup must not receive from the already consumed result a second time.
	expandDone = nil
	fixture.wait(t, "B terminal send result after its timeout", bFinished)
	fixture.wait(t, "A terminal send result", aFinished)
	if fixture.clientCount() != 1 || window.failures.counts(time.Now())[windowFailureProvider] != 1 {
		t.Fatal("B timeout changed A's admission or duplicated terminal failure")
	}
	var terminals uint64
	for _, observation := range observations.Snapshot() {
		terminals += observation.Count
	}
	if terminals != 2 {
		t.Fatalf("initial evaluation outcomes=%d, want exactly two", terminals)
	}
	fixture.assertNoDirectArgsRemoval(t)
}

// A blocked local producer cannot hold another candidate's actual ACK verdict.
func TestMultiClientExpandLocalAdmissionCannotHoldHealthyPingVerdict(t *testing.T) {
	testMultiClientExpandLocalAdmissionIsolation(t, "healthy")
}

// An actual success ACK may install its candidate before the sender handles
// the already-returned send result; both completions keep one admission.
func TestMultiClientExpandAckBeforeSendResultKeepsOneAdmission(t *testing.T) {
	testMultiClientExpandLocalAdmissionIsolation(t, "ack-first")
}

// Window cancellation owns both unresolved candidates despite a local wait.
func TestMultiClientExpandWindowCancelReleasesBlockedAdmission(t *testing.T) {
	testMultiClientExpandLocalAdmissionIsolation(t, "window-canceled")
}

// Replacing the evaluation epoch is not provider failure or fresh authority.
func TestMultiClientExpandEpochCancelReleasesBlockedAdmission(t *testing.T) {
	testMultiClientExpandLocalAdmissionIsolation(t, "epoch-canceled")
}

// Pass expiry closes both actual attempts before a late ACK or failed sender
// return; neither continuation can classify another failure or add a provider.
func TestMultiClientExpandLateSendReturnCannotConvictEndedPass(t *testing.T) {
	testMultiClientExpandLocalAdmissionIsolation(t, "pass-ended")
}
