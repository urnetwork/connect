package connect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func TestBlackholeReadTimeoutDefaultAlignment(t *testing.T) {
	multi := DefaultMultiClientSettings()
	platform := DefaultPlatformTransportSettings()
	if multi.BlackholeTimeout != platform.ReadTimeout {
		t.Fatalf("no-send-ACK timeout %s differs from transport read timeout %s", multi.BlackholeTimeout, platform.ReadTimeout)
	}
}

func TestBlackholeReadTimeoutUsesEffectivePerClientSettings(t *testing.T) {
	generated := DefaultPlatformTransportSettings()
	generated.ReadTimeout = 45 * time.Second
	generator := &ApiMultiClientGenerator{settings: &ApiMultiClientGeneratorSettings{
		PlatformTransportSettingsGenerator: func() *PlatformTransportSettings { return generated },
	}}
	first, second := &Client{}, &Client{}
	firstSettings := generator.newPlatformTransportSettings()
	generated.ReadTimeout = 15 * time.Second
	secondSettings := generator.newPlatformTransportSettings()
	generator.transports = map[*Client]*apiWindowClientTransport{
		first: {settings: firstSettings}, second: {settings: secondSettings},
	}
	channel := &multiClientChannel{client: first, readTimeoutProvider: generator, settings: DefaultMultiClientSettings()}
	channel.settings.BlackholeTimeout = time.Second // must not override the carrier
	if got := channel.noSendAckTimeout(); got != 45*time.Second {
		t.Fatalf("first client's copied read timeout = %s", got)
	}
	channel.client = second
	if got := channel.noSendAckTimeout(); got != 15*time.Second {
		t.Fatalf("second client's effective read timeout = %s", got)
	}
	secondSettings.ReadTimeout = 0
	if got := channel.noSendAckTimeout(); got != 0 {
		t.Fatalf("explicit zero replaced by fallback: %s", got)
	}
	channel.client = &Client{}
	if got := channel.noSendAckTimeout(); got != time.Second {
		t.Fatalf("unregistered client fallback = %s", got)
	}
	channel.readTimeoutProvider = nil
	if got := channel.noSendAckTimeout(); got != time.Second {
		t.Fatalf("custom generator fallback = %s", got)
	}
}

func readLivenessReason(t *testing.T, channel *multiClientChannel) blackholeReason {
	t.Helper()
	stats, err := channel.WindowStats()
	if err != nil {
		t.Fatal(err)
	}
	reason, _ := blackholeReasonFromStats(time.Now(), stats, channel.noSendAckTimeout(), 0, time.Hour, blackholeGates{})
	return reason
}

func TestBlackholeReadTimeoutExactBoundaryAndDisabled(t *testing.T) {
	for _, timeout := range []time.Duration{15 * time.Second, 30 * time.Second, 45 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				channel := newPacketTransferTestChannel()
				channel.settings.BlackholeTimeout = timeout
				channel.addSend(64, udpTestPath(4))
				time.Sleep(timeout - time.Nanosecond)
				if got := readLivenessReason(t, channel); got != blackholeNone {
					t.Fatalf("verdict before read deadline: %s", got)
				}
				time.Sleep(time.Nanosecond)
				if got := readLivenessReason(t, channel); got != blackholeNoSendAck {
					t.Fatalf("silent exit at read deadline: %s", got)
				}
				channel.settings.BlackholeTimeout = 0
				time.Sleep(time.Hour)
				if got := readLivenessReason(t, channel); got != blackholeNone {
					t.Fatalf("disabled no-send-ACK verdict fired: %s", got)
				}
			})
		})
	}
}

func TestBlackholeReadTimeoutOnlyPeerAckRestartsClock(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%t", grouped), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				channel := newPacketTransferTestChannel()
				channel.settings.BlackholeTimeout = 30 * time.Second
				path := udpTestPath(4)
				channel.addSend(64, path) // one reliable owner remains throughout
				first := channel.pendingSendTime
				time.Sleep(10 * time.Second)
				if grouped {
					group := &parsedPacketGroup{packets: []parsedPacket{{ipPath: path}, {ipPath: path}}, ipPath: path, byteCount: 128}
					channel.addSendGroup(group)
					channel.observePacketGroupTransferCompletion(group, false, nil)
				} else {
					channel.addSend(64, path)
					channel.observePacketTransferCompletion(64, time.Time{}, false, nil)
				}
				channel.addBusyProbeAck()
				if !channel.pendingSendTime.Equal(first) || !channel.lastSendAckTime.IsZero() || channel.packetStats.sendNackCount != 1 {
					t.Fatalf("non-peer progress refreshed reliable liveness: start=%s ack=%s outstanding=%d", channel.pendingSendTime, channel.lastSendAckTime, channel.packetStats.sendNackCount)
				}
				time.Sleep(20 * time.Second)
				if got := readLivenessReason(t, channel); got != blackholeNoSendAck {
					t.Fatalf("NoAck writes/probe hid silent reliable owner: %s", got)
				}
				// Any real peer ACK is progress, including one of several sends.
				channel.addSend(64, path)
				channel.addSendAck(64)
				if got := readLivenessReason(t, channel); got != blackholeNone {
					t.Fatalf("peer ACK did not restart liveness: %s", got)
				}
				time.Sleep(30*time.Second - time.Nanosecond)
				if got := readLivenessReason(t, channel); got != blackholeNone {
					t.Fatalf("restarted deadline fired early: %s", got)
				}
				time.Sleep(time.Nanosecond)
				if got := readLivenessReason(t, channel); got != blackholeNoSendAck {
					t.Fatalf("historical ACK hid a later stalled run: %s", got)
				}
				channel.addSendAbandoned(64)
				if got := readLivenessReason(t, channel); got != blackholeNone {
					t.Fatalf("abandoned owner resurrected from old bucket: %s", got)
				}
			})
		})
	}
}

func TestBlackholeReadTimeoutDoesNotLengthenFreshnessOrPoll(t *testing.T) {
	if blackholePollInterval != 1250*time.Millisecond || blackholeAckFreshnessInterval != 5*time.Second {
		t.Fatal("read alignment changed health evaluation/freshness cadence")
	}
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultMultiClientSettings()
		settings.UplinkStalenessGate = 0
		parent := &RemoteUserNatMultiClient{settings: settings}
		if got := parent.comparativeReceiveWindow(); got != 5*time.Second {
			t.Fatalf("read deadline leaked into sibling freshness: %s", got)
		}
		channel := newPacketTransferTestChannel()
		channel.addSend(64, udpTestPath(4))
		channel.addSendAck(64)
		if !channel.hasRecentSendAck(blackholeAckFreshnessInterval) {
			t.Fatal("fresh peer ACK not recognized")
		}
		time.Sleep(5 * time.Second)
		if channel.hasRecentSendAck(blackholeAckFreshnessInterval) {
			t.Fatal("stale ACK was extended to the read deadline")
		}
	})
}

func TestBlackholeReadTimeoutCannotBePreemptedByUnackedReceiveSilence(t *testing.T) {
	for _, peerAck := range []bool{false, true} {
		t.Run(fmt.Sprintf("peerAck=%t", peerAck), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				channel := newPacketTransferTestChannel()
				channel.settings.BlackholeTimeout = 30 * time.Second
				path := udpTestPath(4)
				t0 := time.Now()
				channel.addSend(64, path)
				channel.addSend(64, path)
				time.Sleep(2 * time.Second)
				channel.addSendCompletion(64, peerAck)
				time.Sleep(2 * time.Second)
				channel.addSend(64, path)
				channel.addSendCompletion(64, false)
				time.Sleep(16 * time.Second)
				check := func(want blackholeReason) {
					stats, err := channel.WindowStats()
					if err != nil {
						t.Fatal(err)
					}
					reason, _ := blackholeReasonFromStats(time.Now(), stats, channel.noSendAckTimeout(), 20*time.Second, time.Hour, blackholeGates{})
					if reason != want {
						t.Fatalf("peerAck=%t at %s: reason=%s want=%s", peerAck, time.Since(t0), reason, want)
					}
				}
				if peerAck {
					check(blackholeNoReceiveAck) // existing ACKing-peer policy survives
					return
				}
				check(blackholeNone)
				time.Sleep(9900 * time.Millisecond)
				check(blackholeNone)
				time.Sleep(100 * time.Millisecond)
				check(blackholeNoSendAck)
			})
		})
	}
}

// Drives the real SendSequence, its physical route ownership, its independent
// resend timer, and the actual health loop under fake time. A held peer ACK is
// an injected boundary, not a claim about which TCP packet caused a field loss.
func TestBlackholeReadTimeoutRealTransferBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		ackAt, readTimeout, resendCap time.Duration
		health                        bool
	}{
		{"old_5s_preempts_recoverable_ack", 6300 * time.Millisecond, 5 * time.Second, 3 * time.Second, true},
		{"recover_after_old_verdict", 6300 * time.Millisecond, 30 * time.Second, 3 * time.Second, true},
		{"recover_before_boundary", 30*time.Second - time.Nanosecond, 30 * time.Second, 3 * time.Second, true},
		{"silent_shared_boundary", 0, 30 * time.Second, 3 * time.Second, true},
		{"silent_transfer_owns_terminal", 0, 30 * time.Second, 3 * time.Second, false},
		{"custom_shorter_read", 0, 15 * time.Second, 3 * time.Second, true},
		{"custom_longer_read", 0, 45 * time.Second, 8 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				destination := NewId()
				type event struct {
					phase SendPackLifecyclePhase
					at    time.Time
					err   error
				}
				var mu sync.Mutex
				var lifecycle []event
				var writes []time.Time
				wire := make(chan *protocol.Pack, 64)
				log := newRecordingLogger()
				settings := DefaultClientSettings()
				settings.Log = log
				settings.EncryptionSettings.Mode = EncryptionModeOff
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.SendBufferSettings.AckTimeout = 30 * time.Second
				settings.SendBufferSettings.MaxResendInterval = tc.resendCap
				settings.SendBufferSettings.SendPackLifecycleObserver = func(observation SendPackLifecycleObservation) {
					if observation.DestinationId == destination && observation.MessageType == protocol.MessageType_IpIpPacketToProvider {
						mu.Lock()
						defer mu.Unlock()
						if len(lifecycle) == 8 {
							t.Error("lifecycle observer overflow")
							return
						}
						lifecycle = append(lifecycle, event{observation.Phase, time.Now(), observation.Err})
					}
				}
				settings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
					pack := decodeSendPackLifecycleWirePack(t, observation.TransferFrameBytes)
					if len(pack.Frames) == 0 || pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketToProvider {
						return
					}
					mu.Lock()
					if len(writes) < 64 {
						writes = append(writes, time.Now())
					} else {
						t.Error("wire observer overflow")
					}
					mu.Unlock()
					select {
					case wire <- pack:
					default:
						t.Error("wire queue overflow")
					}
				}
				client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
				route := make(Route, 64)
				defer func() {
					cancel()
					client.CloseAndWait(context.Background())
					for len(route) > 0 {
						MessagePoolReturn(<-route)
					}
				}()
				client.ContractManager().AddNoContractPeer(destination)
				client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
				channel := newPacketTransferTestChannel()
				channel.settings = DefaultMultiClientSettings()
				channel.settings.BlackholeTimeout = tc.readTimeout
				channel.settings.BlackholeReceiveTimeout = 0 // isolate no-send-ACK, not receive policy
				channel.ctx, channel.cancel, channel.client, channel.log = ctx, cancel, client, NewNoopLogger()
				channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
				path := icmpTcpTestPath(4)
				path.Syn = false
				packet := MessagePoolGet(64)
				clear(packet)
				packet[0], packet[9], packet[32] = 0x45, 6, 0x50
				t0 := time.Now()
				accepted, err := channel.SendDetailedWithAck(&parsedPacket{packet: packet, ipPath: path}, time.Second, true)
				if !accepted || err != nil {
					MessagePoolReturn(packet)
					t.Fatalf("admission=%t/%v", accepted, err)
				}
				var first *protocol.Pack
				select {
				case first = <-wire:
				case <-ctx.Done():
					t.Fatal("no initial wire Pack")
				}
				select {
				case bytes := <-route:
					MessagePoolReturn(bytes)
				case <-ctx.Done():
					t.Fatal("no physical route write")
				}
				if tc.health {
					go channel.detectBlackhole()
				}
				synctest.Wait()
				terminal := func() (event, bool) {
					mu.Lock()
					defer mu.Unlock()
					for _, observed := range lifecycle {
						if observed.phase == SendPackLifecyclePhaseTerminal {
							return observed, true
						}
					}
					return event{}, false
				}
				deadline := 30 * time.Second
				if tc.health {
					deadline = min(deadline, tc.readTimeout)
				}
				until := deadline - time.Nanosecond
				recovers := 0 < tc.ackAt && tc.ackAt < deadline
				if recovers {
					until = tc.ackAt
				}
				time.Sleep(until)
				synctest.Wait()
				if observed, done := terminal(); done || ctx.Err() != nil {
					t.Fatalf("terminal before independent deadline: observed=%+v ctx=%v elapsed=%s", observed, ctx.Err(), time.Since(t0))
				}
				if recovers {
					acknowledgeSendPackLifecycleWirePack(t, client, destination, first)
				} else {
					time.Sleep(time.Nanosecond)
				}
				synctest.Wait()
				observed, done := terminal()
				if !done {
					t.Fatal("no terminal event at ACK/deadline")
				}
				if recovers {
					if observed.err != nil || !observed.at.Equal(t0.Add(tc.ackAt)) {
						t.Fatalf("recovered ACK terminal=%+v", observed)
					}
					if got := readLivenessReason(t, channel); got != blackholeNone {
						t.Fatalf("ACKed owner retained verdict: %s", got)
					}
				} else if observed.err == nil || observed.at.Before(t0.Add(deadline)) {
					t.Fatalf("silent terminal=%+v, deadline=%s", observed, deadline)
				}
				var exitLines []string
				for _, line := range log.linesWith("event=sequence_exit") {
					if strings.Contains(line, "destination="+destination.String()) {
						exitLines = append(exitLines, line)
					}
				}
				if recovers {
					if len(exitLines) != 0 {
						t.Fatalf("timely ACK fabricated exit: %v", exitLines)
					}
				} else {
					if len(exitLines) != 1 {
						t.Fatalf("want one exact terminal owner: %v", exitLines)
					}
					lifetimeExit := strings.Contains(exitLines[0], "reason=ack_lifetime ")
					contextExit := strings.Contains(exitLines[0], "reason=context ")
					if !lifetimeExit && !contextExit {
						t.Fatalf("unexpected terminal reason: %s", exitLines[0])
					}
					if (!tc.health || 30*time.Second < tc.readTimeout) && !lifetimeExit {
						t.Fatalf("read health stole independent Transfer lifetime: %s", exitLines[0])
					}
					if tc.health && tc.readTimeout < 30*time.Second && !contextExit {
						t.Fatalf("custom earlier read deadline did not own teardown: %s", exitLines[0])
					}
					t.Log(exitLines[0])
				}
				mu.Lock()
				defer mu.Unlock()
				if len(writes) < 2 {
					t.Fatal("test did not exercise Transfer resend")
				}
				for _, at := range writes {
					if at.Before(t0) || observed.at.Before(at) {
						t.Fatal("write outside owner lifetime")
					}
				}
				attemptTimes := make([]time.Duration, len(writes))
				for i, at := range writes {
					attemptTimes[i] = at.Sub(t0)
				}
				t.Logf("admission=0 first_write=%s scheduled_peer_ack=%s ack_injected=%t terminal=%s read_interval=%s ack_lifetime=30s resend_cap=%s wire_attempts=%v health=%t error=%v", writes[0].Sub(t0), tc.ackAt, recovers, observed.at.Sub(t0), tc.readTimeout, tc.resendCap, attemptTimes, tc.health, observed.err)
			})
		})
	}
}

func TestBlackholeReadTimeoutLookupDoesNotAllocate(t *testing.T) {
	client := &Client{}
	generator := &ApiMultiClientGenerator{transports: map[*Client]*apiWindowClientTransport{
		client: {settings: &PlatformTransportSettings{ReadTimeout: 30 * time.Second}},
	}}
	channel := &multiClientChannel{client: client, readTimeoutProvider: generator}
	if allocations := testing.AllocsPerRun(100, func() { _ = channel.noSendAckTimeout() }); allocations != 0 {
		t.Fatalf("read-timeout lookup allocated %g objects", allocations)
	}
	t.Logf("channel=%d bytes window_stats=%d bytes; added ownership=one optional interface, telemetry=one timestamp/two booleans; no new per-packet owner", unsafe.Sizeof(*channel), unsafe.Sizeof(clientWindowStats{}))
}

// The no-ACK liveness deadline must survive the bounded telemetry window.
// A single send stays outstanding even after its event bucket ages out.
func TestBlackholeReadTimeoutSurvivesStatsCoalescing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := newPacketTransferTestChannel()
		channel.settings.BlackholeTimeout = 30 * time.Second
		path := icmpTcpTestPath(4)
		path.Syn = false
		channel.addSend(64, path)
		time.Sleep(31 * time.Second)
		stats, err := channel.WindowStats()
		if err != nil {
			t.Fatal(err)
		}
		if !stats.firstSendNackTime.IsZero() || stats.sendNackCount != 1 {
			t.Fatalf("test did not cross telemetry expiry with the original outstanding send: %+v", stats)
		}
		reason, held := blackholeReasonFromStats(time.Now(), stats, channel.settings.BlackholeTimeout, 0, 0, blackholeGates{})
		if reason != blackholeNoSendAck || held != blackholeNone {
			t.Fatalf("outstanding send past read deadline: reason=%q held=%q", reason, held)
		}
	})
}
