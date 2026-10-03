package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type dataProbeSizedGenerator struct {
	*TestMultiClientGenerator
	size  int
	fixed bool
}

func (g *dataProbeSizedGenerator) FixedDestinationSize() (int, bool) { return g.size, g.fixed }

func TestDataOnlyProviderProbeScope(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		enabled, background, marked, fixed bool
		size, hops                         int
		want                               bool
	}{
		{"explicit single", true, false, true, true, 1, 1, true},
		{"default", false, false, true, true, 1, 1, false},
		{"background probe", true, true, true, true, 1, 1, false},
		{"discovery", true, false, true, false, 1, 1, false},
		{"multiple", true, false, true, true, 2, 1, false},
		{"unmarked candidate", true, false, false, true, 1, 1, false},
		{"multiple hops", true, false, true, true, 1, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := DefaultMultiClientSettings()
			settings.DataOnlyProviderProbe = tc.enabled
			settings.ProviderProbe = tc.background
			ids := []Id{NewId()}
			if tc.hops == 2 {
				ids = append(ids, NewId())
			}
			args := &multiClientChannelArgs{FixedDestination: tc.marked, Destination: RequireMultiHopId(ids...)}
			if got := dataOnlyProviderProbeEnabled(settings, &dataProbeSizedGenerator{size: tc.size, fixed: tc.fixed}, args); got != tc.want {
				t.Fatalf("enabled=%t want=%t", got, tc.want)
			}
		})
	}
	if DefaultMultiClientSettings().DataOnlyProviderProbe {
		t.Fatal("shared default enabled")
	}
}

func configureDataOnlyFixture(f *multiClientExpandLifecycleFixture, enabled bool) *TestMultiClientGenerator {
	g := f.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
	f.window.generator = g
	f.window.settings.DataOnlyProviderProbe = enabled
	f.window.settings.ProviderProbe = false
	f.window.settings.InitialPingObservations = &InitialPingObservations{}
	f.window.beforeExpandPingResultForTest = nil
	f.window.afterExpandPingResultForTest = nil
	args := <-f.window.clientChannelArgs
	args.FixedDestination = true
	f.window.clientChannelArgs <- args
	return g
}

func TestDataOnlyProviderProbeAdmitsAndSendsDataWithoutPing(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f, provider := newDataOnlyProviderProbeFixture(t, func(*ClientSettings) {})
		configureDataOnlyFixture(f, true)
		var pings, data atomic.Int64
		provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				switch frame.MessageType {
				case protocol.MessageType_IpIpPing:
					pings.Add(1)
				case protocol.MessageType_IpIpPacketToProvider:
					data.Add(1)
				}
			}
		})
		start := time.Now()
		if admitted := f.result(t, f.start()); admitted != 1 {
			t.Fatalf("admitted=%d", admitted)
		}
		if time.Since(start) != 0 {
			t.Fatalf("admission waited %s without application traffic", time.Since(start))
		}
		clients := f.window.unorderedClients()
		if len(clients) != 1 || !clients[0].dataOnlyProviderProbe {
			t.Fatal("wrong channel mode")
		}
		// No continuous or initial provider ping during the former 30s admission
		// deadline and the 35s recovery horizon. The channel stays selectable.
		time.Sleep(35 * time.Second)
		if clients[0].IsDone() || pings.Load() != 0 {
			t.Fatalf("premature close=%t pings=%d", clients[0].IsDone(), pings.Load())
		}
		ack := make(chan error, 1)
		// A real Transfer data frame establishes the ACK path without a prior ping.
		// Full valid DNS/TLS traffic is covered by the separate real-Open fixture.
		ok, err := clients[0].SendDetailedMessage(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: []byte{0}}}, time.Second, func(err error) { ack <- err })
		if !ok || err != nil {
			t.Fatalf("data send %t %v", ok, err)
		}
		if err := <-ack; err != nil {
			t.Fatalf("data ACK: %v", err)
		}
		if data.Load() != 1 || pings.Load() != 0 {
			t.Fatalf("data=%d pings=%d", data.Load(), pings.Load())
		}
		for _, row := range f.window.settings.InitialPingObservations.Snapshot() {
			if row.Count != 0 {
				t.Fatal("fabricated initial ping observation")
			}
		}
		f.cancelWindow()
		f.wait(t, "channel cleanup", f.clientRemoved)
		f.assertNoDirectArgsRemoval(t)
	})
}

func TestDataOnlyProviderProbeDefaultStillPings(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f, provider := newDataOnlyProviderProbeFixture(t, func(*ClientSettings) {})
		configureDataOnlyFixture(f, false)
		var pings atomic.Int64
		provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_IpIpPing {
					pings.Add(1)
				}
			}
		})
		if admitted := f.result(t, f.start()); admitted != 1 {
			t.Fatalf("admitted=%d", admitted)
		}
		synctest.Wait()
		if pings.Load() == 0 {
			t.Fatal("default omitted provider ping")
		}
		var count uint64
		for _, row := range f.window.settings.InitialPingObservations.Snapshot() {
			count += row.Count
		}
		if count != 1 {
			t.Fatalf("initial observations=%d", count)
		}
	})
}

func TestDataOnlyProviderProbeSkipsActiveStallQuestionAndVerdict(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		client := busyProbeTestChannel(t, func(_ time.Duration, ack func(error)) (bool, error) { calls.Add(1); ack(nil); return true, nil })
		client.dataOnlyProviderProbe = true
		stallPast(client, time.Second)
		if !client.sendStalled(time.Second) {
			t.Fatal("fixture did not stall")
		}
		window := busyProbeTestWindow(time.Second, client)
		if window.convictSendStalls(time.Second) || calls.Load() != 0 || client.IsDone() || !client.sendStalled(time.Second) {
			t.Fatalf("active authority changed state: calls=%d", calls.Load())
		}
	})
}

func TestDataOnlyProviderProbeKeepsAdmissionOwnership(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, which := range []string{"flow replacement", "hard cap", "setup cancellation"} {
		t.Run(which, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, _ := newDataOnlyProviderProbeFixture(t, func(*ClientSettings) {})
				g := configureDataOnlyFixture(f, true)
				args := <-f.window.clientChannelArgs
				f.window.clientChannelArgs <- args
				var old *multiClientChannel
				if which == "setup cancellation" {
					original := g.newClient
					g.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
						client, err := original(ctx, args, settings)
						f.cancelEvaluation()
						return client, err
					}
				} else {
					old = stallTestChannel()
					old.ctx, old.cancel = context.WithCancel(f.window.ctx)
					t.Cleanup(old.cancel)
					oldID := args.ClientId
					if which == "hard cap" {
						oldID = NewId()
						f.window.settings.StrictWindowSizeHardMax = true
					}
					old.args = &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: oldID}, Destination: args.Destination}
					f.window.clients[oldID] = old
					f.window.flowCountFunc = func(c *multiClientChannel) int {
						if c == old {
							return 1
						}
						return 0
					}
				}
				if admitted := f.result(t, f.start()); admitted != 0 {
					t.Fatalf("admitted=%d", admitted)
				}
				f.wait(t, "rejected owner cleanup", f.clientRemoved)
				f.assertNoDirectArgsRemoval(t)
				if old != nil && (old.IsDone() || f.clientCount() != 1) {
					t.Fatal("existing owner displaced")
				}
			})
		})
	}
}

func TestDataOnlyProviderProbeContractWaitRemainsNoResult(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f, _ := newDataOnlyProviderProbeFixture(t, func(*ClientSettings) {})
		g := configureDataOnlyFixture(f, true)
		oob := &evaluationContractOob{}
		g.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
			settings.Log = NewNoopLogger()
			settings.EncryptionSettings.Mode = EncryptionModeOff
			settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(0, 0)
			settings.SendBufferSettings.CreateContractTimeout = 2 * time.Second
			settings.SendBufferSettings.CreateContractRetryInterval = 100 * time.Millisecond
			settings.SendBufferSettings.CreateContractRetryMaxInterval = 100 * time.Millisecond
			client := NewClient(ctx, args.ClientId, oob, settings)
			t.Cleanup(func() {
				client.Cancel()
				join, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := client.CloseAndWait(join); err != nil {
					t.Error(err)
				}
			})
			return client, nil
		}
		if admitted := f.result(t, f.start()); admitted != 1 {
			t.Fatalf("admitted=%d", admitted)
		}
		client := f.window.unorderedClients()[0]
		if oob.requests.Load() != 0 {
			t.Fatal("admission acquired a ping contract")
		}
		result := make(chan error, 1)
		ok, err := client.SendDetailedMessage(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: []byte{0}}}, time.Second, func(err error) { result <- err })
		if !ok || err != nil {
			t.Fatalf("enqueue=%t %v", ok, err)
		}
		synctest.Wait()
		if !client.args.providerEvaluation.localContractUnavailable() {
			t.Fatal("real data contract wait lost absence authority")
		}
		err = <-result
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("contract result=%v", err)
		}
		if !client.args.providerEvaluation.localContractUnavailable() || f.window.providerEvaluation.providerContact.Load() {
			t.Fatal("contract failure became provider evidence")
		}
		if !client.IsDone() {
			f.cancelWindow()
		}
	})
}

// Delay only the initial-ping callback after the provider has received it.
// Application data is reachable, but the default selection gate still spends
// its unchanged 30s budget. The explicit application mode owns no such callback.
func TestDataOnlyProviderProbeAvoidsInitialAckBarrier(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "data"}[enabled], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, _ := newDataOnlyProviderProbeFixture(t, func(*ClientSettings) {})
				configureDataOnlyFixture(f, enabled)
				f.window.settings.PingTimeout = DefaultMultiClientSettings().PingTimeout
				f.window.beforeExpandPingResultForTest = func() { <-f.releasePingResult }
				done := f.start()
				synctest.Wait()
				if enabled {
					if admitted := f.result(t, done); admitted != 1 {
						t.Fatalf("data admission=%d", admitted)
					}
				} else {
					if f.clientCount() != 0 {
						t.Fatal("default bypassed initial ACK")
					}
					time.Sleep(35 * time.Second)
					if admitted := f.result(t, done); admitted != 0 {
						t.Fatalf("expired default admitted=%d", admitted)
					}
					expired := uint64(0)
					for _, row := range f.window.settings.InitialPingObservations.Snapshot() {
						if row.Outcome == "expired" && row.Count > 0 {
							expired += row.Count
							if row.Seconds != 30 {
								t.Fatalf("changed ping budget: %v", row.Seconds)
							}
						}
					}
					if expired != 1 {
						t.Fatalf("expiry observations=%d", expired)
					}
				}
				f.releasePing()
			})
		})
	}
}
