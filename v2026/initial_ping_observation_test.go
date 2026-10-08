package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func oneInitialPingObservation(t *testing.T, observations *InitialPingObservations) InitialPingObservation {
	t.Helper()
	var completed uint64
	var result InitialPingObservation
	for _, row := range observations.Snapshot() {
		completed += row.Count
		if row.Count != 0 {
			result = row
		}
	}
	if observations.Started() != 1 || completed != 1 {
		t.Fatalf("initial-ping ownership: started=%d completed=%d", observations.Started(), completed)
	}
	return result
}

// Real send sequences distinguish missing carrier, held authorization and a
// granted but silent remote path. No wall timer, verdict or admission is faked.
func TestInitialPingObservationCausalDependencies(t *testing.T) {
	// Initialize the process-owned pool worker before entering virtual time.
	GetMessagePoolAggregateStats()
	for _, test := range []struct {
		name, outcome, dependency string
		carrier, grant, cancel    bool
		contractBudget, elapsed   time.Duration
	}{
		{"held_carrier", "expired", "carrier_absent", false, true, false, 4 * time.Second, 2 * time.Second},
		{"held_contract", "expired", "carrier_present_contract_waiting_without_write", true, false, false, 4 * time.Second, 2 * time.Second},
		{"contract_failed", "error", "carrier_present_contract_failed_without_write", true, false, false, time.Second, time.Second},
		{"silent_provider", "expired", "carrier_present_write_attempted", true, true, false, 4 * time.Second, 2 * time.Second},
		{"canceled_contract", "canceled_or_ended", "", true, false, true, 4 * time.Second, time.Second},
		{"canceled_window", "canceled_or_ended", "", true, false, true, 4 * time.Second, time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, observation := range []struct {
				name    string
				enabled bool
			}{{"observer_nil", false}, {"observer_enabled", true}} {
				t.Run(observation.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						fixture := newInitialPingDiagnosticFixture(t)
						observations := &InitialPingObservations{}
						if observation.enabled {
							fixture.window.settings.InitialPingObservations = observations
						}
						fixture.window.settings.PingTimeout = 2 * time.Second
						fixture.window.settings.WindowExpandTimeout = time.Second
						fixture.window.settings.WindowClientSetupTimeout = 3 * time.Second
						// Owner cancellation must win this control before a generic send-
						// sequence teardown callback is released. That callback is not
						// guaranteed to wrap context.Canceled, and existing policy
						// deliberately retains nonmatching provider errors. The held
						// callback is released and joined below; nil/enabled observers
						// exercise the identical causal order.
						if !test.cancel {
							fixture.window.beforeExpandPingResultForTest = nil
							fixture.window.afterExpandPingResultForTest = nil
						}
						generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
						candidate := <-fixture.window.clientChannelArgs
						fixture.window.clientChannelArgs <- candidate
						destination := candidate.Destination.Tail()
						created := make(chan *Client, 1)
						consumerDone := make(chan struct{})
						var wireFrames atomic.Uint64
						oob := &evaluationContractOob{ready: test.grant}
						generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
							settings.EncryptionSettings.Mode = EncryptionModeOff
							settings.Log = fixture.log
							settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(0, 0)
							settings.SendBufferSettings.CreateContractTimeout = test.contractBudget
							settings.SendBufferSettings.CreateContractRetryInterval = 100 * time.Millisecond
							settings.SendBufferSettings.CreateContractRetryMaxInterval = 100 * time.Millisecond
							oob.grant = &contractErrorOob{clientId: args.ClientId}
							client := NewClient(ctx, args.ClientId, oob, settings)
							packets := make(chan []byte, 128)
							if test.carrier {
								client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{packets})
							}
							go func() {
								defer close(consumerDone)
								for {
									select {
									case <-client.Done():
										// The producer is joined separately before the remaining
										// buffered packets are returned by the test cleanup.
										return
									case packet := <-packets:
										wireFrames.Add(1)
										MessagePoolReturn(packet)
									}
								}
							}()
							t.Cleanup(func() {
								client.Cancel()
								closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
								defer cancel()
								if err := client.CloseAndWait(closeCtx); err != nil {
									t.Error(err)
								}
								<-consumerDone
								for {
									select {
									case packet := <-packets:
										MessagePoolReturn(packet)
									default:
										return
									}
								}
							})
							created <- client
							return client, nil
						}
						started := time.Now()
						done := fixture.start()
						<-created
						synctest.Wait()
						wantStarted := uint64(0)
						if observation.enabled {
							wantStarted = 1
						}
						if observations.Started() != wantStarted || oob.requests.Load() == 0 {
							t.Fatal("initial evaluation/real contract acquisition not started")
						}
						if got := wireFrames.Load(); (got > 0) != (test.carrier && test.grant) {
							t.Fatalf("carrier/authorization control failed: frames=%d", got)
						}
						time.Sleep(test.elapsed)
						if test.cancel {
							if test.name == "canceled_window" {
								fixture.cancelWindow()
							} else {
								fixture.cancelEvaluation()
							}
						}
						synctest.Wait()
						if test.cancel {
							// Expansion's cancellation worker has settled while the
							// teardown callback is held outside its terminal mutex.
							fixture.releasePing()
							synctest.Wait()
						}
						if got := fixture.result(t, done); got != 0 || time.Since(started) != test.elapsed {
							t.Fatalf("observer changed outcome/deadline: admitted=%d elapsed=%v", got, time.Since(started))
						}
						if observation.enabled {
							row := oneInitialPingObservation(t, observations)
							if row.Outcome != test.outcome || row.Seconds != test.elapsed.Seconds() || test.dependency != "" && row.Dependency != test.dependency {
								t.Fatalf("dependency witness=%+v want %s/%s at %v", row, test.outcome, test.dependency, test.elapsed)
							}
						} else if observations.Started() != 0 {
							t.Fatal("nil observer became active")
						}
						wantProvider, wantPlatform := 0, 1
						if test.grant {
							wantProvider, wantPlatform = 1, 0
						} else if test.cancel {
							wantPlatform = 0
						}
						failures := fixture.window.failures.counts(time.Now())
						if failures[windowFailureProvider] != wantProvider || failures[windowFailurePlatform] != wantPlatform {
							t.Fatalf("diagnostic changed failure authority: counts=%v want provider=%d platform=%d", failures, wantProvider, wantPlatform)
						}
						fixture.assertNoDirectArgsRemoval(t)
					})
				})
			}
		})
	}
}

// An actual provider ACK held at the callback boundary remains admissible at
// 35s under the unchanged 45s fixture budget, with and without observation.
func TestInitialPingObservationPreservesDelayedAckAndNil(t *testing.T) {
	// Initialize the process-owned pool worker before entering virtual time.
	GetMessagePoolAggregateStats()
	for _, observe := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newInitialPingDiagnosticFixture(t)
			fixture.window.settings.PingTimeout = 45 * time.Second
			var observations *InitialPingObservations
			if observe {
				observations = &InitialPingObservations{}
			}
			fixture.window.settings.InitialPingObservations = observations
			started := time.Now()
			done := fixture.start()
			fixture.wait(t, "actual ACK callback", fixture.pingResultEntered)
			// The real ACK may first pass through the existing10ms compression
			// timer. Hold to the absolute evaluation boundary, rather than adding
			// that transport time to another35s sleep and asserting it was zero.
			releaseAt := started.Add(35 * time.Second)
			if !time.Now().Before(releaseAt) {
				t.Fatal("healthy ACK did not reach the hold before release boundary")
			}
			time.Sleep(time.Until(releaseAt))
			fixture.releasePing()
			synctest.Wait()
			if got := fixture.result(t, done); got != 1 || time.Since(started) != 35*time.Second {
				t.Fatalf("healthy late ACK changed: admitted=%d elapsed=%v", got, time.Since(started))
			}
			if observe {
				row := oneInitialPingObservation(t, observations)
				// An active real carrier can hold its route mutex at the exact
				// terminal snapshot. Unknown is the specified conservative result;
				// the separate controlled-dependency roots require exact labels.
				knownHealthy := row.Dependency == "carrier_present_write_attempted" || row.Dependency == "carrier_unknown"
				if row.Outcome != "acknowledged" || !knownHealthy || row.Seconds != 35 {
					t.Fatalf("late healthy witness=%+v", row)
				}
			} else if observations.Started() != 0 {
				t.Fatal("nil observer became active")
			}
			fixture.assertNoDirectArgsRemoval(t)
		})
	}
}

// A callback that loses the terminal race cannot publish another observation.
func TestInitialPingObservationLateAckCountedOnce(t *testing.T) {
	// Initialize the process-owned pool worker before entering virtual time.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newInitialPingDiagnosticFixture(t)
		observations := &InitialPingObservations{}
		fixture.window.settings.InitialPingObservations = observations
		fixture.window.settings.PingTimeout = 2 * time.Second
		done := fixture.start()
		fixture.wait(t, "held ACK", fixture.pingResultEntered)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if fixture.result(t, done) != 0 {
			t.Fatal("expired candidate admitted")
		}
		before := oneInitialPingObservation(t, observations)
		fixture.releasePing()
		fixture.wait(t, "late ACK terminal", fixture.pingResultDone)
		synctest.Wait()
		after := oneInitialPingObservation(t, observations)
		if before != after || before.Outcome != "expired" || before.Seconds != 2 {
			t.Fatalf("late callback rewrote observation: before=%+v after=%+v", before, after)
		}
	})
}

// A contended route lock produces unknown without taking or waiting for it.
func TestInitialPingObservationContendedCarrierFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := NewRouteManager(ctx, "synthetic")
	client := &Client{routeManager: manager}
	attempt := &providerEvaluationAttempt{owner: &providerEvaluationState{}, destinationId: NewId()}
	manager.mutex.Lock()
	got := initialPingDependencySnapshot(client, attempt)
	manager.mutex.Unlock()
	if got != initialPingCarrierUnknown {
		t.Fatal("contended carrier manufactured availability")
	}
}

func TestInitialPingObservationFixedAggregate(t *testing.T) {
	observations := &InitialPingObservations{}
	var done sync.WaitGroup
	for range 32 {
		done.Add(1)
		go func() {
			defer done.Done()
			observations.begin()
			observations.record(initialPingExpired, initialPingCarrierAbsent, time.Second)
			_ = observations.Snapshot()
		}()
	}
	done.Wait()
	if observations.Started() != 32 || len(observations.Snapshot()) != 24 {
		t.Fatal("fixed aggregate geometry changed")
	}
	var count uint64
	var seconds float64
	for _, row := range observations.Snapshot() {
		count += row.Count
		seconds += row.Seconds
	}
	if count != 32 || seconds != 32 {
		t.Fatal("aggregate dropped concurrent terminal observations")
	}
}
