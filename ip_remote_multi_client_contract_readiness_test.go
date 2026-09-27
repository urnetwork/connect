// Initial-ping attribution follows the real contract and carrier boundaries,
// including a control wait that expires before any provider frame is attempted.
package connect

import (
	"context"
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
	grant          bool
	contractBudget time.Duration
	elapsed        time.Duration
	expirePass     bool
	cancelOwner    bool
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
		time.Sleep(test.elapsed)
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
		if got := time.Since(started); got != test.elapsed {
			t.Fatalf("elapsed=%s want=%s", got, test.elapsed)
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
