package connect

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A parent closes Done before cancellation reaches its children. Hold that
// state explicitly: the provider is canceled while the source and Client
// remain live. Transfer must not replace the canceled fixture sequence and
// take packet ownership that provider shutdown can no longer join.
func TestRemoteUserNatProviderCanceledReturnDoesNotAdmit(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, batch := range []bool{false, true} {
			for _, canceled := range []string{"provider", "source"} {
				t.Run(fmt.Sprintf("v%d/batch=%t/%s", version, batch, canceled), func(t *testing.T) {
					peerId := NewId()
					var sequenceCreates atomic.Int64
					releaseSequence := make(chan struct{})
					clientSettings := closeWaitClientSettings()
					clientSettings.SendBufferSettings.beforeCreateSendSequenceForTest = func(id sendSequenceId) {
						if id.Destination == peerId {
							sequenceCreates.Add(1)
						}
					}
					clientSettings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
						if id.Destination == peerId {
							<-releaseSequence
						}
					}
					client := NewClient(context.Background(), NewId(), NewNoContractClientOob(), clientSettings)
					t.Cleanup(func() {
						close(releaseSequence)
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := client.CloseAndWait(ctx); err != nil {
							t.Errorf("close canceled-return client: %v", err)
						}
					})
					providerCtx, cancelProvider := context.WithCancel(context.Background())
					t.Cleanup(cancelProvider)
					// Deliberately defer the child cancellation independently of the
					// parent so the assertion does not depend on scheduler timing.
					sourceCtx, cancelSource := context.WithCancel(context.WithoutCancel(providerCtx))
					t.Cleanup(cancelSource)
					observer, events := providerReturnObserverEvents()
					settings := DefaultRemoteUserNatProviderSettings()
					settings.ProtocolVersion = version
					settings.ReturnSendObserver = observer
					provider := &RemoteUserNatProvider{
						ctx:                 providerCtx,
						client:              client,
						settings:            settings,
						packetStatsCounters: &packetStatsCounters{},
					}
					id := sendSequenceId{Destination: peerId, CompanionContract: true}
					sequence := installProviderReturnTestSequence(t, provider, client, id)
					// This case intentionally supplies a retired destination
					// generation to expose any forbidden replacement attempt.
					sequence.ctx = providerCtx
					sequence.packs = make(chan *SendPack)
					packetCount := 1
					if batch {
						packetCount = 2
					}
					fixture := newProviderReturnObserverItem(t, peerId, packetCount, batch)
					fixture.item.sourceLifecycle = &providerSourceLifecycle{ctx: sourceCtx}
					t.Cleanup(func() {
						fixture.item.returnPackets()
						for _, witness := range fixture.witnesses {
							if witness != nil {
								MessagePoolReturn(witness)
							}
						}
					})
					attempts := 0
					provider.afterReturnSendAttemptForTest = func(providerReturnSendResult) { attempts++ }
					var result providerReturnSendResult
					completions := 0
					provider.afterReturnSendForTest = func(completed providerReturnSendResult) {
						result = completed
						completions++
					}
					provider.beginReturnSendObservation(fixture.item)
					if canceled == "provider" {
						cancelProvider()
						if sourceCtx.Err() != nil || client.ctx.Err() != nil {
							t.Fatal("child cancellation reached the held provider cancellation window")
						}
					} else {
						cancelSource()
					}
					provider.sendReturnItem(fixture.item)
					if attempts != 0 {
						t.Errorf("canceled provider return made %d Transfer attempts", attempts)
					}
					if completions != 1 || result.sent || result.packetCount != packetCount || result.packetByteCount != fixture.totalBytes {
						t.Errorf("canceled return completion=%+v count=%d, want one refusal of %d packets/%d bytes", result, completions, packetCount, fixture.totalBytes)
					}
					client.sendBuffer.mutex.Lock()
					current := client.sendBuffer.sendSequences[id]
					client.sendBuffer.mutex.Unlock()
					if sequenceCreates.Load() != 0 || current != sequence {
						t.Error("canceled provider return replaced its Transfer sequence")
					}
					for packetIndex, witness := range fixture.witnesses {
						if !MessagePoolReturn(witness) {
							t.Errorf("canceled provider return retained packet %d", packetIndex)
						}
						fixture.witnesses[packetIndex] = nil
					}
					wantDrops := ProviderCongestionDrops{
						ReturnSendPacketCount: int64(packetCount),
						ReturnSendByteCount:   fixture.totalBytes,
					}
					if drops := provider.CongestionDropStats(); drops != wantDrops {
						t.Errorf("canceled return drops=%+v, want %+v", drops, wantDrops)
					}
					if stats := provider.PacketStats(); stats.RemoteEgressPacketCount != 0 || stats.RemoteEgressByteCount != 0 {
						t.Errorf("canceled return counted remote egress: %+v", stats)
					}
					if len(events) != 2 {
						t.Fatalf("canceled return emitted %d observations, want one phase pair", len(events))
					}
					started, completed := <-events, <-events
					requireProviderReturnObservation(t, started, RemoteUserNatProviderReturnSendPhaseStarted,
						started.Token, fixture.flowKey, packetCount, fixture.totalBytes, false)
					requireProviderReturnObservation(t, completed, RemoteUserNatProviderReturnSendPhaseCompleted,
						started.Token, fixture.flowKey, packetCount, fixture.totalBytes, false)
				})
			}
		}
	}
}

// Real Transfer sequences belong to the Client, independently of one
// provider's cancellation. A paused fixture must preserve that lifetime even
// when a return is already blocked inside Transfer when the provider closes.
func TestRemoteUserNatProviderFixtureKeepsInFlightClientSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []receiveRecoveryMode{receiveRecoveryModeTcpSocket, receiveRecoveryModeDedicatedTcpControl} {
		t.Run(fmt.Sprintf("mode=%d", mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peerId := NewId()
				var sequenceCreates atomic.Int64
				releaseSequence := make(chan struct{})
				clientSettings := closeWaitClientSettings()
				clientSettings.SendBufferSettings.beforeCreateSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == peerId {
						sequenceCreates.Add(1)
					}
				}
				clientSettings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == peerId {
						<-releaseSequence
					}
				}
				client := NewClient(context.Background(), NewId(), NewNoContractClientOob(), clientSettings)
				t.Cleanup(func() {
					close(releaseSequence)
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Errorf("close in-flight return client: %v", err)
					}
				})
				providerCtx, cancelProvider := context.WithCancel(context.Background())
				sourceCtx, cancelSource := context.WithCancel(context.WithoutCancel(providerCtx))
				settings := DefaultRemoteUserNatProviderSettings()
				settings.ProtocolVersion = 2
				settings.WriteTimeout = -1
				provider := &RemoteUserNatProvider{
					ctx: providerCtx, client: client, settings: settings,
					packetStatsCounters: &packetStatsCounters{},
				}
				id := sendSequenceId{Destination: peerId, CompanionContract: true}
				sequence := installProviderReturnTestSequence(t, provider, client, id)
				sequence.packs = make(chan *SendPack)
				packet := MessagePoolCopy(craftSecurityPacket(IpProtocolTcp,
					net.ParseIP("203.0.113.7"), 8080, net.ParseIP("10.0.0.9"), 42001, false, nil))
				witness := MessagePoolShareReadOnly(packet)
				item := &providerReturnItem{
					source: SourceId(peerId), transferKey: TransferKey{CompanionContract: true},
					provideMode: protocol.ProvideMode_Public, recoveryMode: mode, ipProtocol: IpProtocolTcp,
					packet: packet, packetByteCount: ByteCount(len(packet)),
					sourceLifecycle: &providerSourceLifecycle{ctx: sourceCtx},
				}
				var result providerReturnSendResult
				provider.afterReturnSendForTest = func(completed providerReturnSendResult) { result = completed }
				done := make(chan struct{})
				go func() {
					provider.sendReturnItem(item)
					close(done)
				}()
				defer func() {
					cancelProvider()
					cancelSource()
					<-done
					if witness != nil {
						MessagePoolReturn(witness)
					}
				}()
				synctest.Wait()
				select {
				case <-done:
					t.Error("paused Transfer sequence did not hold its return")
				default:
				}
				cancelProvider()
				synctest.Wait()
				select {
				case <-done:
					t.Error("provider cancellation released a send before reaching its source or Client")
				default:
				}
				client.sendBuffer.mutex.Lock()
				current := client.sendBuffer.sendSequences[id]
				client.sendBuffer.mutex.Unlock()
				if sequenceCreates.Load() != 0 || current != sequence {
					t.Error("provider cancellation replaced the live Client's paused sequence")
				}
				cancelSource()
				<-done
				if result.sent || result.packetCount != 1 || result.packetByteCount != ByteCount(len(packet)) {
					t.Errorf("source cancellation completion=%+v, want one refused packet", result)
				}
				if !MessagePoolReturn(witness) {
					t.Error("in-flight source cancellation retained its return packet")
				}
				witness = nil
				if drops := provider.CongestionDropStats(); drops.ReturnSendPacketCount != 1 || drops.ReturnSendByteCount != ByteCount(len(packet)) {
					t.Errorf("in-flight source cancellation drops=%+v, want one exact refusal", drops)
				}
			})
		})
	}
}

// Both recoverable TCP owners must observe provider cancellation before the
// first attempt and again before a retry while their source child is live.
func TestRemoteUserNatProviderReturnRetryChecksProviderCancellation(t *testing.T) {
	for _, mode := range []receiveRecoveryMode{receiveRecoveryModeTcpSocket, receiveRecoveryModeDedicatedTcpControl} {
		for _, beforeFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%d/before_first=%t", mode, beforeFirst), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				settings := DefaultRemoteUserNatProviderSettings()
				settings.ReturnSendRetryTimeout = time.Nanosecond
				provider := &RemoteUserNatProvider{ctx: ctx, settings: settings}
				item := &providerReturnItem{
					recoveryMode:    mode,
					sourceLifecycle: &providerSourceLifecycle{ctx: context.WithoutCancel(ctx)},
				}
				wantAttempts := 1
				if beforeFirst {
					cancel()
					wantAttempts = 0
				}
				attempts := 0
				sent := provider.retryReturnSend(item, 1, 1, func() bool {
					attempts++
					if !beforeFirst && attempts == 1 {
						cancel()
						return false
					}
					return true
				})
				if sent || attempts != wantAttempts {
					t.Fatalf("canceled return sent=%t after %d attempts, want false/%d", sent, attempts, wantAttempts)
				}
			})
		}
	}
}
