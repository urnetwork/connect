// Initial-ping attribution follows the real contract and carrier boundaries,
// including a control wait that expires before any provider frame is attempted.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A silent control service is independent of the already available carrier.
// Takes the control frames, including when deliberately withholding a grant.
type evaluationContractOob struct {
	ready    bool
	grant    *contractErrorOob
	requests atomic.Int64
}

// Takes the frames and either supplies a real contract or an empty result.
func (self *evaluationContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	for _, frame := range frames {
		if frame.MessageType == protocol.MessageType_TransferCreateContract {
			self.requests.Add(1)
		}
	}
	if self.ready {
		self.grant.SendControl(frames, callback)
		return
	}
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	if callback != nil {
		callback(nil, nil)
	}
}

// One virtual-time boundary controls a real expansion and send sequence.
type evaluationContractCase struct {
	grant            bool
	contractBudget   time.Duration
	elapsed          time.Duration
	setupDelay       time.Duration
	observePendingAt time.Duration
	expirePass       bool
	cancelOwner      bool
}

// Exercises the production timer/error callback without replacing its contract
// manager or send sequence. A destination-only carrier excludes bootstrap traffic.
func checkEvaluationContractBoundary(t *testing.T, test evaluationContractCase) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		fixture.window.settings.PingTimeout = 2 * time.Second
		fixture.window.settings.WindowExpandTimeout = time.Second
		fixture.window.settings.WindowClientSetupTimeout = 3 * time.Second
		fixture.window.beforeExpandPingResultForTest = nil
		fixture.window.afterExpandPingResultForTest = nil
		expirePass := make(chan struct{})
		fixture.window.expireExpandPassForTest = expirePass
		generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
		candidate := <-fixture.window.clientChannelArgs
		destination := candidate.Destination.Tail()
		fixture.window.clientChannelArgs <- candidate
		created := make(chan *Client, 1)
		oob := &evaluationContractOob{ready: test.grant}
		var wireFrames atomic.Int64
		generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
			// Virtual setup time places the probe cutoff before the later ping
			// deadline without replacing the contract or carrier boundaries.
			if 0 < test.setupDelay {
				time.Sleep(test.setupDelay)
			}
			settings.EncryptionSettings.Mode = EncryptionModeOff
			settings.Log = fixture.log
			// Virtual time precedes the default historical enable date.
			settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(0, 0)
			settings.SendBufferSettings.CreateContractTimeout = test.contractBudget
			settings.SendBufferSettings.CreateContractRetryInterval = 100 * time.Millisecond
			settings.SendBufferSettings.CreateContractRetryMaxInterval = 100 * time.Millisecond
			oob.grant = &contractErrorOob{clientId: args.ClientId}
			client := NewClient(ctx, args.ClientId, oob, settings)
			packets := make(chan []byte, 128)
			client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{packets})
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case packet := <-packets:
						wireFrames.Add(1)
						MessagePoolReturn(packet)
					}
				}
			}()
			created <- client
			return client, nil
		}
		started := time.Now()
		done := fixture.start()
		client := <-created
		defer func() {
			client.Cancel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.CloseAndWait(ctx); err != nil {
				t.Errorf("join candidate: %v", err)
			}
		}()
		synctest.Wait()
		if oob.requests.Load() == 0 {
			t.Fatal("real contract acquisition was not exercised")
		}
		if got := wireFrames.Load(); (got > 0) != test.grant {
			t.Fatalf("authorization did not gate carrier: grant=%t frames=%d", test.grant, got)
		}
		if got := fixture.window.failures.counts(time.Now())[windowFailureProvider]; got != 0 {
			t.Fatalf("provider failure before expiry=%d", got)
		}
		if 0 < test.observePendingAt {
			time.Sleep(test.observePendingAt)
			synctest.Wait()
			if got := fixture.window.failures.counts(time.Now()); got != [windowFailureClassCount]int{} {
				t.Fatal("pending sample occurred after a failure was recorded")
			}
			if fixture.window.providerEvaluation.localContractFailure.Load() {
				t.Fatal("pending sample relied on a later ping-failure latch")
			}
			if got := candidate.providerEvaluation.localContractUnavailable(); got != !test.grant {
				t.Fatalf("actual send witness=%t want=%t", got, !test.grant)
			}
			multiClient := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: fixture.window}}
			if got := multiClient.ProviderContractAcquisitionUnavailable(); got != !test.grant {
				t.Errorf("probe cutoff lost pending no-contact proof: unavailable=%t want=%t; elapsed=%s provider_frames=%d", got, !test.grant, time.Since(started), wireFrames.Load())
			}
		}
		time.Sleep(test.elapsed - test.observePendingAt)
		if test.expirePass {
			close(expirePass)
		}
		if test.cancelOwner {
			fixture.cancelEvaluation()
		}
		synctest.Wait()
		if got := fixture.result(t, done); got != 0 {
			t.Fatalf("admitted without provider acknowledgement=%d", got)
		}
		if got := time.Since(started); got != test.setupDelay+test.elapsed {
			t.Fatalf("elapsed=%s want=%s", got, test.setupDelay+test.elapsed)
		}
		wantProvider, wantPlatform := 0, 1
		if test.grant {
			wantProvider, wantPlatform = 1, 0
		} else if test.cancelOwner {
			wantPlatform = 0
		}
		counts := fixture.window.failures.counts(time.Now())
		if got := counts[windowFailureProvider]; got != wantProvider {
			t.Errorf("provider failures=%d want=%d; grant=%t provider frames=%d", got, wantProvider, test.grant, wireFrames.Load())
		}
		if got := counts[windowFailurePlatform]; got != wantPlatform {
			t.Errorf("platform failures=%d want=%d", got, wantPlatform)
		}
		if !test.grant && wireFrames.Load() != 0 {
			t.Fatal("packet crossed carrier without grant")
		}
		multiClient := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: fixture.window}}
		if got := multiClient.ProviderContractAcquisitionUnavailable(); got != (wantPlatform == 1) {
			t.Errorf("retired candidate contract proof=%t want=%t", got, wantPlatform == 1)
		}
		_, events := fixture.window.monitor.Events()
		if event := events[candidate.ClientId]; event != nil && wantProvider == 0 && event.State == ProviderStateEvaluationFailed {
			t.Errorf("local or canceled candidate emitted provider-failure event: %s", event.State)
		}
		fixture.assertNoDirectArgsRemoval(t)
	})
}

// A ping deadline spent entirely acquiring a contract is local evidence.
func TestEvaluationContractPingDeadlineIsLocal(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{contractBudget: 4 * time.Second, elapsed: 2 * time.Second})
}

// A real no-contract callback retains the same local attribution.
func TestEvaluationContractAcquisitionErrorIsLocal(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{contractBudget: time.Second, elapsed: time.Second})
}

// Once provider frames are attempted, an unanswered ping remains provider evidence.
func TestEvaluationContractGrantedSilentProviderRemainsProviderFailure(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{grant: true, contractBudget: 4 * time.Second, elapsed: 2 * time.Second})
}

// The URL lookup can stop after delayed setup but before the ping's full budget.
func TestEvaluationContractPendingWaitIsVisibleBeforePingDeadline(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{
		contractBudget: 4 * time.Second, setupDelay: time.Second,
		observePendingAt: time.Second, elapsed: 2 * time.Second,
	})
}

// At the identical cutoff, an actual provider write must forbid unknown status.
func TestEvaluationContractGrantedContactOutranksPendingCutoff(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{
		grant: true, contractBudget: 4 * time.Second, setupDelay: time.Second,
		observePendingAt: time.Second, elapsed: 2 * time.Second,
	})
}

// The pass safety cap must snapshot the contract wait before canceling its owner.
func TestEvaluationContractPassDeadlineIsLocal(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{contractBudget: 4 * time.Second, elapsed: time.Second, expirePass: true})
}

// Epoch replacement is neither a provider nor a local-control failure.
func TestEvaluationContractOwnerCancellationIsNotFailure(t *testing.T) {
	checkEvaluationContractBoundary(t, evaluationContractCase{contractBudget: 4 * time.Second, elapsed: time.Second, cancelOwner: true})
}

// Owner-scoped proof cannot forget a previous candidate's attempted provider
// write, inherit another destination's traffic, or leak to an unrelated owner.
func TestEvaluationContractWitnessIsConservativeAcrossCandidates(t *testing.T) {
	window := &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	other := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: {}}}
	destinationId := NewId()
	attempt := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
	attempt.beginContractWait(ControlId)
	attempt.noteProviderWrite(ControlId)
	attempt.endContractWait(ControlId, true)
	if attempt.localContractUnavailable() || client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("bootstrap control traffic became provider evidence")
	}
	attempt.beginContractWait(destinationId)
	if !attempt.localContractUnavailable() {
		t.Fatal("active local acquisition wait lost")
	}
	attempt.endContractWait(destinationId, true)
	window.providerEvaluation.localContractFailure.Store(true)
	if !attempt.localContractUnavailable() || !client.ProviderContractAcquisitionUnavailable() || other.ProviderContractAcquisitionUnavailable() {
		t.Fatal("failed acquisition was lost or escaped its owner")
	}
	// Mark the invocation, not success: an attempted but failed writer cannot
	// safely claim zero contact. The attempt may retire immediately afterward.
	attempt.noteProviderWrite(destinationId)
	if attempt.localContractUnavailable() || client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("an attempted provider write retained no-contact proof")
	}
	replacement := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
	replacement.beginContractWait(destinationId)
	if !replacement.localContractUnavailable() || client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("candidate replacement erased previous provider contact")
	}
}

// Pending waits are counted per window, ignore other destinations and disappear
// on cancellation without becoming latched natural failures.
func TestEvaluationContractPendingWaitsAreOwnedAndReleased(t *testing.T) {
	window, otherWindow := &multiClientWindow{}, &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{
		WindowTypeQuality: window, WindowTypeSpeed: otherWindow,
	}}
	other := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: {}}}
	destinationId := NewId()
	first := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
	second := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
	first.beginContractWait(ControlId)
	first.endContractWait(ControlId, false)
	if client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("another destination acquired no-contact authority")
	}
	first.beginContractWait(destinationId)
	second.beginContractWait(destinationId)
	if !client.ProviderContractAcquisitionUnavailable() || other.ProviderContractAcquisitionUnavailable() {
		t.Fatal("pending contract proof was lost or crossed an independent owner")
	}
	first.endContractWait(destinationId, false)
	if !client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("one ended wait erased a second live acquisition")
	}
	second.endContractWait(destinationId, false)
	if client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("canceled waits became permanent no-contact failures")
	}
	first.beginContractWait(destinationId)
	otherAttempt := &providerEvaluationAttempt{owner: &otherWindow.providerEvaluation, destinationId: destinationId}
	otherAttempt.noteProviderWrite(destinationId)
	if client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("another window's provider write was hidden by a pending wait")
	}
	first.endContractWait(destinationId, false)
}

// A natural control failure replaces its pending count before evaluation can
// consume the callback; cancellation alone remains unlatched in the prior test.
func TestEvaluationContractPendingFailureHasNoWitnessGap(t *testing.T) {
	window := &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	destinationId := NewId()
	attempt := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
	attempt.beginContractWait(destinationId)
	attempt.endContractWait(destinationId, true)
	if !client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("natural contract failure lost its witness before the evaluation callback")
	}
	attempt.noteProviderWrite(destinationId)
	if client.ProviderContractAcquisitionUnavailable() {
		t.Fatal("late real contact retained a historical control-failure exception")
	}
}

// Joined concurrent waiters retain count accuracy and cannot undo any write.
func TestEvaluationContractPendingWaitsRetainConcurrentContact(t *testing.T) {
	window := &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	destinationId := NewId()
	var started, finished sync.WaitGroup
	started.Add(16)
	release := make(chan struct{})
	for range 16 {
		finished.Go(func() {
			attempt := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
			attempt.beginContractWait(destinationId)
			started.Done()
			<-release
			attempt.endContractWait(destinationId, false)
		})
	}
	started.Wait()
	if !client.ProviderContractAcquisitionUnavailable() {
		t.Error("concurrent live waits were unobservable")
	}
	contact := &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: destinationId}
	contact.noteProviderWrite(destinationId)
	close(release)
	finished.Wait()
	contact.beginContractWait(destinationId)
	if client.ProviderContractAcquisitionUnavailable() {
		t.Error("late wait callbacks erased the monotonic write witness")
	}
	contact.endContractWait(destinationId, false)
}
