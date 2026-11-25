package connect

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestNativeRaceLoserBeforeOrdinalRetractsConnectClock(t *testing.T) {
	testNativeRaceLoserOrdinal(t, nativeRaceLoserOrdinalCase{})
}

func TestNativeRaceLoserAfterOrdinalKeepsConnectClock(t *testing.T) {
	testNativeRaceLoserOrdinal(t, nativeRaceLoserOrdinalCase{alreadyRetained: true})
}

func TestNativeRaceLoserBeforeOrdinalKeepsIndependentConnectClock(t *testing.T) {
	testNativeRaceLoserOrdinal(t, nativeRaceLoserOrdinalCase{independent: true})
}

type nativeRaceLoserOrdinalCase struct {
	alreadyRetained bool
	independent     bool
}

type nativeRaceLoserWirePack struct {
	sequence Id
	ordinal  uint64
	group    *parsedPacketGroup
}

// The normal native sender races two real Transfer clients. The slow source
// and its provider's TCP worker each stop at an existing ownership boundary.
func testNativeRaceLoserOrdinal(t *testing.T, mode nativeRaceLoserOrdinalCase) {
	t.Helper()
	assertMessagePoolOwnership(t)
	ownedTestWireSynctest(t, func(t *testing.T) {
		path := reraceFlow(4, 1, 47831)
		independentPath := reraceFlow(4, 2, 47832)
		sourceParked, sourceRelease := make(chan struct{}), make(chan struct{})
		providerRelease := make(chan struct{})
		var releaseSourceOnce sync.Once
		releaseSource := func() { releaseSourceOnce.Do(func() { close(sourceRelease) }) }
		defer releaseSource()
		defer close(providerRelease)

		var sourceArmed atomic.Bool
		var synPack, resetPack nativeRaceLoserWirePack
		parkSource := func() {
			if sourceArmed.CompareAndSwap(true, false) {
				close(sourceParked)
				<-sourceRelease
			}
		}
		harness := newReraceHarness(t, []reraceExitSpec{
			{
				name:   "slow",
				answer: reraceAlways(reraceSilent),
				clientSettings: func(settings *ClientSettings) {
					if !mode.alreadyRetained {
						settings.SendBufferSettings.beforeGroupRetainForTest = parkSource
					}
					settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, _ []*protocol.Frame) {
						group, ok := disposition.target.(*parsedPacketGroup)
						if !ok || !reraceSameFlow(group.ipPath, path) {
							return
						}
						pack := nativeRaceLoserWirePack{sequence: source.sequenceId, ordinal: source.nextSequenceNumber - 1, group: group}
						if group.ipPath.Syn {
							synPack = pack
							if mode.alreadyRetained {
								parkSource()
							}
						}
						if group.ipPath.Rst {
							resetPack = pack
						}
					}
				},
			},
			{name: "sibling", answer: reraceAlways(reraceSilent)},
		}, nil)
		slow, sibling := harness.channel("slow"), harness.channel("sibling")
		slowExit, siblingExit := harness.exit("slow"), harness.exit("sibling")

		targetOwners := make(chan *TcpSequence, 1)
		var targetOwnerCount, targetDialCount atomic.Int32
		settings := DefaultLocalUserNatSettings()
		settings.Log = NewNoopLogger()
		settings.TcpBufferSettings.ConnectTimeout = 2 * time.Minute
		settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
			if reraceSameFlow(sequence.IpPath(), path) {
				if targetOwnerCount.Add(1) == 1 {
					targetOwners <- sequence
				}
				<-providerRelease
			}
		}
		settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
				if address == path.DestinationHostPort() {
					targetDialCount.Add(1)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		nat := NewLocalUserNat(harness.ctx, "unwritten losing native SYN", settings)
		t.Cleanup(func() {
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		provider, err := TryNewRemoteUserNatProvider(slowExit.provider, nat, DefaultRemoteUserNatProviderSettings())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(provider.Close)

		send := func(path *IpPath) {
			t.Helper()
			packet := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
			accepted := [1]bool{}
			if count := harness.multiClient.SendNativePacketBatchWithResults(
				harness.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:],
			); count != 1 || !accepted[0] {
				t.Fatal("native racing source refused a controlled opening")
			}
		}
		harness.settle()
		sourceArmed.Store(true)
		slowestOfferedAt := time.Now()
		send(path)
		harness.settle()
		select {
		case <-sourceParked:
		default:
			t.Fatal("the actual losing source did not reach its configured ordinal boundary")
		}
		update := harness.update(path)
		if update == nil {
			t.Fatal("native opener did not retain its flow owner")
		}
		var original *parsedPacketGroup
		update.stateLock.Lock()
		if race := update.race; race != nil && !race.single && race.clientStates[slow] != nil && race.clientStates[sibling] != nil {
			for group := update.sequenceClaims; group != nil; group = group.collapseNext {
				if group.completionClient == slow && group.connectSynOffer != nil {
					original = group
					break
				}
			}
		}
		update.stateLock.Unlock()
		const required = groupCompletionTracked | groupCompletionCandidate | groupCompletionAdmitted | groupCompletionDequeued
		if original == nil || original.collapseSource == nil || original.completionFlags.Load()&required != required ||
			original.connectSynOffer.materialized.Load() != mode.alreadyRetained {
			t.Fatal("the exact admitted two-peer native SYN missed the expected ordinal boundary")
		}
		if len(slowExit.received(path)) != 0 {
			t.Fatal("losing peer saw wire before its source barrier")
		}
		source := original.collapseSource
		nextOrdinal := source.nextSequenceNumber
		if mode.alreadyRetained {
			if synPack.group != original || synPack.ordinal+1 != nextOrdinal {
				t.Fatal("after-retain barrier missed the exact immutable SYN ordinal")
			}
		} else if synPack.group != nil {
			t.Fatal("before-retain barrier followed an already materialized SYN")
		}
		stats, err := slow.WindowStats()
		if err != nil {
			t.Fatalf("read the admitted slow clock: %v", err)
		}
		if clock := unansweredConnectSince(stats); !clock.Equal(slowestOfferedAt) {
			t.Fatalf("first native admission did not own the slow clock: %s want=%s", clock, slowestOfferedAt)
		}

		var independentOfferedAt time.Time
		if mode.independent {
			harness.sleep(time.Millisecond)
			independentOfferedAt = time.Now()
			send(independentPath)
			harness.settle()
			if len(siblingExit.syns(independentPath)) != 1 || len(slowExit.received(independentPath)) != 0 {
				t.Fatal("independent SYN did not wait behind the parked slow source")
			}
			slow.stateLock.Lock()
			independentPending := false
			for pending := slow.connectSynPending; pending != nil; pending = pending.next {
				if pending.started.Equal(independentOfferedAt) && !pending.materialized.Load() {
					independentPending = true
					break
				}
			}
			slow.stateLock.Unlock()
			if !independentPending {
				t.Fatal("later independent SYN lacked a real outstanding slow-channel obligation")
			}
		}
		siblingSyns := siblingExit.syns(path)
		if len(siblingSyns) != 1 || siblingSyns[0].source != sibling.ClientId() ||
			siblingSyns[0].path.SequenceNumber != path.SequenceNumber {
			t.Fatal("the sibling did not receive the exact candidate SYN")
		}
		siblingExit.sendTo(siblingSyns[0].source, reraceSynAck(siblingSyns[0].path))
		responseDeadline := time.Now().Add(harness.multiClient.settings.MultiRaceSetOnResponseTimeout + reraceSettle)
		if !harness.waitForSynAck(path, responseDeadline) || harness.boundTo(path) != sibling {
			t.Fatal("the actual sibling SYN-ACK did not finish the response race")
		}
		reply, _ := harness.synAck(path)
		replyPath, err := ParseIpPath(reply.packet)
		if err != nil || replyPath.AckSequenceNumber != path.SequenceNumber+1 {
			t.Fatalf("the selected response did not acknowledge the exact native opening: %v", err)
		}
		if len(slowExit.received(path)) != 0 || resetPack.group != nil {
			t.Fatal("the slow source acquired wire while still parked")
		}

		releaseSource()
		// Cumulative peer ACKs have their own timer. Quiescence alone says
		// neither that Transfer has acknowledged the wire nor that it failed.
		terminalDeadline := time.Now().Add(reraceSendTimeout)
		for {
			harness.settle()
			if resetPack.group != nil && resetPack.group.completionFlags.Load()&groupCompletionTerminal != 0 &&
				original.completionFlags.Load()&groupCompletionTerminal != 0 {
				break
			}
			wait := time.Until(terminalDeadline)
			if wait <= 0 {
				break
			}
			time.Sleep(min(10*time.Millisecond, wait))
		}
		if resetPack.group == nil {
			t.Fatal("the generated reset did not materialize")
		}
		if resetPack.sequence != source.sequenceId {
			t.Fatalf("the generated reset changed source sequence: got %v want %v", resetPack.sequence, source.sequenceId)
		}
		if resetPack.group.completionFlags.Load()&groupCompletionTerminal == 0 {
			t.Fatal("the generated reset remained outstanding at its peer-terminal deadline")
		}
		update.stateLock.Lock()
		const terminalState = affinitySourceTerminal | affinitySourceFailed
		resetPeerCompleted := resetPack.group.affinitySourceState&terminalState == affinitySourceTerminal
		originalPeerCompleted := original.affinitySourceState&terminalState == affinitySourceTerminal
		update.stateLock.Unlock()
		if !resetPeerCompleted {
			t.Fatal("same-source generated reset lacked a successful peer terminal")
		}
		slowPackets := slowExit.received(path)
		if len(slowPackets) == 0 || !slowPackets[len(slowPackets)-1].path.Rst ||
			slowPackets[len(slowPackets)-1].source != slow.ClientId() {
			t.Fatal("the losing provider missed the generated reset on the same flow")
		}

		slowSyns := slowExit.syns(path)
		if len(slowSyns) != 0 {
			if len(slowSyns) != 1 || slowSyns[0].source != slow.ClientId() ||
				slowSyns[0].path.SequenceNumber != path.SequenceNumber || synPack.group != original ||
				synPack.sequence != resetPack.sequence || resetPack.ordinal <= synPack.ordinal ||
				!originalPeerCompleted || targetOwnerCount.Load() != 1 {
				t.Fatal("the prior original lost exact SYN-to-reset wire order")
			}
			select {
			case owner := <-targetOwners:
				if owner.initialSynSeq != path.SequenceNumber || owner.source.SourceId != slow.ClientId() || owner.ctx.Err() != context.Canceled {
					t.Fatal("real provider reset did not cancel the matching NAT generation")
				}
			default:
				t.Fatal("the observed original SYN never reached the provider TCP owner")
			}
		} else if targetOwnerCount.Load() != 0 || synPack.group != nil || originalPeerCompleted {
			t.Fatal("a locally unwritten original left published SYN custody")
		}
		if targetDialCount.Load() != 0 {
			t.Fatal("provider dial ran before the controlled pre-dial reset cut")
		}
		if mode.independent && len(slowExit.syns(independentPath)) != 1 {
			t.Fatal("independent slow SYN lost its actual wire ownership")
		}
		stats, err = slow.WindowStats()
		if err != nil || original.completionFlags.Load()&groupCompletionTerminal == 0 {
			t.Fatalf("the original did not reach its same-provider terminal: %v", err)
		}
		clock := unansweredConnectSince(stats)
		timeout := harness.multiClient.settings.BlackholeConnectTimeout
		reason, _ := blackholeReasonFromStats(slowestOfferedAt.Add(timeout), stats, 0, 0, timeout, blackholeGates{})
		if mode.alreadyRetained {
			if len(slowSyns) != 1 || !clock.Equal(slowestOfferedAt) || reason != blackholeNoReceiveSyn {
				t.Fatalf("already-retained original lost its unanswered evidence: SYNs=%d clock=%s verdict=%q", len(slowSyns), clock, reason)
			}
			return
		}
		if !clock.Equal(independentOfferedAt) || len(slowSyns) != 0 || reason != blackholeNone {
			t.Fatalf("NATIVE_UNWRITTEN_RACE_LOSER_RETAINED: losing SYNs=%d clock=%s want=%s verdict=%q", len(slowSyns), clock, independentOfferedAt, reason)
		}
		if mode.independent {
			reason, _ := blackholeReasonFromStats(independentOfferedAt.Add(timeout), stats, 0, 0, timeout, blackholeGates{})
			if reason != blackholeNoReceiveSyn {
				t.Fatalf("later independently unanswered SYN lost its deadline: %q", reason)
			}
		}
		if !mode.independent && resetPack.ordinal != nextOrdinal {
			t.Fatalf("local SYN retraction left a wire hole: reset ordinal=%d want=%d", resetPack.ordinal, nextOrdinal)
		}
	})
}
