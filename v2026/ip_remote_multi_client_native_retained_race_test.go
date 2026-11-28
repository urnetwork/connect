// A current native race can abandon a retained SYN whose own provider really
// connected and canceled it. An ordinary Transfer ACK cannot tell those
// actions apart from receipt of the same reset with no TCP owner.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The wire independently hides the losing provider's exact SYN-ACK and holds
// the original sequence's acknowledgements until the peer handles the reset.
type retainedNativeRaceRelay struct {
	path          *IpPath
	sequence      atomic.Pointer[Id]
	resetMessage  atomic.Pointer[Id]
	resetAckHeld  chan struct{}
	release       chan struct{}
	ackCount      atomic.Int32
	matchedSynAck atomic.Int32
	overflow      atomic.Bool
	omitOutcome   bool
	outcomeCount  atomic.Int32
}

// Both normal and legacy wire envelopes can carry the old cumulative ACK.
func (self *retainedNativeRaceRelay) relay(ctx context.Context, provider <-chan []byte, source chan<- []byte) {
	var held [64][]byte
	count := 0
	defer func() {
		for _, frame := range held[:count] {
			MessagePoolReturn(frame)
		}
	}()
	release := self.release
	resetAckHeld := self.resetAckHeld
	pass := func(frame []byte) bool {
		select {
		case source <- frame:
			return true
		case <-ctx.Done():
			MessagePoolReturn(frame)
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-release:
			release = nil
			for i, frame := range held[:count] {
				held[i] = nil
				if !pass(frame) {
					return
				}
			}
			count = 0
		case packet := <-provider:
			var transfer protocol.TransferFrame
			if ProtoUnmarshal(packet, &transfer) == nil {
				pack, ack := transfer.Pack, transfer.Ack
				if transfer.Frame != nil {
					switch transfer.Frame.MessageType {
					case protocol.MessageType_TransferPack:
						if pack == nil {
							var legacy protocol.Pack
							if ProtoUnmarshal(transfer.Frame.MessageBytes, &legacy) == nil {
								pack = &legacy
							}
						}
					case protocol.MessageType_TransferAck:
						if ack == nil {
							var legacy protocol.Ack
							if ProtoUnmarshal(transfer.Frame.MessageBytes, &legacy) == nil {
								ack = &legacy
							}
						}
					}
				}
				if self.losingSynAck(pack) {
					self.matchedSynAck.Add(1)
					MessagePoolReturn(packet)
					continue
				}
				if ack != nil && ack.ConnectedRaceReset != 0 {
					self.outcomeCount.Add(1)
					if self.omitOutcome {
						ack.ConnectedRaceReset = 0
						var body []byte
						if transfer.Ack == nil {
							body, _ = ProtoMarshal(ack)
							transfer.Frame.MessageBytes = body
						}
						ordinary, err := ProtoMarshal(&transfer)
						MessagePoolReturn(body)
						if err != nil {
							panic(err)
						}
						MessagePoolReturn(packet)
						packet = ordinary
					}
				}
				if sequence := self.sequence.Load(); release != nil && sequence != nil && ack != nil &&
					bytes.Equal(ack.SequenceId, sequence[:]) {
					self.ackCount.Add(1)
					if count == len(held) {
						self.overflow.Store(true)
						MessagePoolReturn(packet)
						continue
					}
					held[count] = packet
					count++
					if reset := self.resetMessage.Load(); reset != nil && resetAckHeld != nil &&
						!ack.Selective && !ack.DeliveredImmediately && len(ack.MissingContractId) == 0 &&
						bytes.Equal(ack.MessageId, reset[:]) {
						close(resetAckHeld)
						resetAckHeld = nil
					}
					continue
				}
			}
			if !pass(packet) {
				return
			}
		}
	}
}

// Target only the Track's actual answer, never Transfer ACKs or another flow.
// The observed provider SYN-ACK exists only after its real dial succeeds.
func (self *retainedNativeRaceRelay) losingSynAck(pack *protocol.Pack) bool {
	if pack == nil {
		return false
	}
	for _, frame := range pack.Frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
			continue
		}
		packet, err := ipPacketFromProviderBytes(frame)
		if err != nil {
			continue
		}
		path, err := ParseIpPath(packet)
		if err == nil && reraceSameFlow(path, self.path) && path.Syn && path.Ack && !path.Rst &&
			path.AckSequenceNumber == self.path.AckSequenceNumber {
			return true
		}
	}
	return false
}

func TestNativeRetainedRaceLoserMatchedResetRetiresConnectClock(t *testing.T) {
	testNativeRetainedRaceReset(t, false, false)
}

func TestNativeRetainedRaceLoserMatchedResetKeepsIndependentConnectClock(t *testing.T) {
	testNativeRetainedRaceReset(t, true, false)
}

func TestNativeRetainedRaceLoserOrphanResetKeepsConnectClock(t *testing.T) {
	testNativeRetainedRaceReset(t, false, true)
}

type retainedNativeRaceControl uint8

const (
	retainedNativeRaceLive retainedNativeRaceControl = iota
	retainedNativeRacePreclosed
	retainedNativeRaceLegacy
	retainedNativeRaceEarlier
	retainedNativeRaceAfterSynTerminal
)

func TestNativeRetainedRaceLoserPreclosedOwnerKeepsConnectClock(t *testing.T) {
	testNativeRetainedRaceReset(t, false, false, retainedNativeRacePreclosed)
}

func TestNativeRetainedRaceLoserMissingOutcomeKeepsConnectClock(t *testing.T) {
	testNativeRetainedRaceReset(t, false, false, retainedNativeRaceLegacy)
}

func TestNativeRetainedRaceLoserKeepsEarlierIndependentConnectClock(t *testing.T) {
	testNativeRetainedRaceReset(t, false, false, retainedNativeRaceEarlier)
}

func TestNativeRetainedRaceLoserOutcomeAfterSynTerminalRetiresOnlyOriginal(t *testing.T) {
	testNativeRetainedRaceReset(t, false, false, retainedNativeRaceAfterSynTerminal)
}

// The live and orphan cases both exercise native admission, exact same-source
// reliable ordinals, the real winner, and successful peer callbacks. Only the
// provider's currently installed TCP generation differentiates their resets.
func testNativeRetainedRaceReset(t *testing.T, independent bool, orphan bool, controls ...retainedNativeRaceControl) {
	control := retainedNativeRaceLive
	if len(controls) == 1 {
		control = controls[0]
	}
	t.Helper()
	assertMessagePoolOwnership(t)
	ownedTestWireSynctest(t, func(t *testing.T) {
		path := reraceFlow(4, 1, 47851)
		independentPath := reraceFlow(4, 2, 47852)
		returnPath := path.Reverse()
		returnPath.AckSequenceNumber = path.SequenceNumber + 1
		relay := &retainedNativeRaceRelay{path: returnPath, release: make(chan struct{}),
			resetAckHeld: make(chan struct{}), omitOutcome: control == retainedNativeRaceLegacy}
		release := sync.OnceFunc(func() { close(relay.release) })
		defer release()
		var synPack, resetPack nativeRaceLoserWirePack
		var independentOffer *connectSynOffer
		harness := newReraceHarness(t, []reraceExitSpec{
			{
				name:          "slow",
				answer:        reraceAlways(reraceSilent),
				providerRelay: relay,
				clientSettings: func(settings *ClientSettings) {
					settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, _ []*protocol.Frame) {
						group, ok := disposition.target.(*parsedPacketGroup)
						if !ok {
							return
						}
						if group.ipPath.Syn && reraceSameFlow(group.ipPath, independentPath) {
							independentOffer = group.connectSynOffer
						}
						if !reraceSameFlow(group.ipPath, path) {
							return
						}
						pack := nativeRaceLoserWirePack{sequence: source.sequenceId, ordinal: source.nextSequenceNumber - 1, group: group}
						if group.ipPath.Syn {
							synPack = pack
							sequenceId := source.sequenceId
							relay.sequence.Store(&sequenceId)
						}
						if group.ipPath.Rst {
							resetPack = pack
							// This hook runs in the source after retained publication
							// and before the route can expose this exact Pack.
							if retained := source.resendQueue.GetBySequenceNumber(pack.ordinal); retained != nil {
								messageId := retained.messageId
								relay.resetMessage.Store(&messageId)
							}
						}
					}
				},
			},
			{name: "sibling", answer: reraceAlways(reraceSilent)},
		}, nil)
		slow, sibling := harness.channel("slow"), harness.channel("sibling")
		slowExit, siblingExit := harness.exit("slow"), harness.exit("sibling")

		upstream, upstreamPeer := net.Pipe()
		defer upstream.Close()
		defer upstreamPeer.Close()
		targetOwners := make(chan *TcpSequence, 1)
		var targetOwnerCount, targetDialCount atomic.Int32
		natLog := newVerboseRecordingLogger(2)
		natSettings := DefaultLocalUserNatSettings()
		memory := NewTransferMemoryBudget(8 * 1024 * 1024)
		natSettings.MemoryBudget = memory
		natSettings.Log = natLog
		natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
			if reraceSameFlow(sequence.IpPath(), path) && targetOwnerCount.Add(1) == 1 {
				targetOwners <- sequence
			}
		}
		natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
				if address == path.DestinationHostPort() && targetDialCount.Add(1) == 1 {
					return upstream, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		nat := NewLocalUserNat(harness.ctx, "retained losing native SYN", natSettings)
		t.Cleanup(func() {
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if used := memory.UsedByteCount(); used != 0 {
				t.Errorf("joined provider retained %d bytes", used)
			}
		})
		startProvider := func() {
			t.Helper()
			provider, err := TryNewRemoteUserNatProvider(slowExit.provider, nat, DefaultRemoteUserNatProviderSettings())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(provider.Close)
			harness.settle()
		}
		if !orphan {
			startProvider()
		}

		send := func(path *IpPath) {
			t.Helper()
			packet := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
			accepted := [1]bool{}
			if count := harness.multiClient.SendNativePacketBatchWithResults(
				harness.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:],
			); count != 1 || !accepted[0] {
				t.Fatal("native source refused the controlled opening")
			}
			harness.settle()
		}
		var independentAt time.Time
		if control == retainedNativeRaceEarlier {
			independentAt = time.Now()
			send(independentPath)
			if len(slowExit.syns(independentPath)) != 1 || len(siblingExit.syns(independentPath)) != 1 {
				t.Fatal("the earlier independent SYN did not reach both peers")
			}
			harness.sleep(time.Millisecond)
		}
		started := time.Now()
		send(path)
		update := harness.update(path)
		original := synPack.group
		if update == nil || original == nil || original.connectSynOffer == nil || !original.connectSynOffer.materialized.Load() ||
			synPack.sequence == (Id{}) || original.completionFlags.Load()&groupCompletionTerminal != 0 {
			t.Fatal("the original native SYN was not retained awaiting its real peer terminal")
		}
		slowSyns, siblingSyns := slowExit.syns(path), siblingExit.syns(path)
		if len(slowSyns) != 1 || slowSyns[0].source != slow.ClientId() || slowSyns[0].path.SequenceNumber != path.SequenceNumber ||
			len(siblingSyns) != 1 || siblingSyns[0].source != sibling.ClientId() || siblingSyns[0].path.SequenceNumber != path.SequenceNumber {
			t.Fatal("the exact SYN did not reach both real Transfer peers")
		}
		var target *TcpSequence
		if orphan {
			// Attach only after the reliable receive owner has consumed the
			// original. Retransmission cannot recreate a completed callback.
			startProvider()
		} else {
			select {
			case target = <-targetOwners:
			default:
				t.Fatal("the original did not reach the real provider TCP worker")
			}
			if target.initialSynSeq != path.SequenceNumber || target.source.SourceId != slow.ClientId() || target.ctx.Err() != nil ||
				targetDialCount.Load() != 1 || relay.matchedSynAck.Load() == 0 {
				t.Fatalf("the losing provider did not connect before the winner: dials=%d answers=%d err=%v", targetDialCount.Load(), relay.matchedSynAck.Load(), target.ctx.Err())
			}
		}
		first := started
		if control == retainedNativeRaceEarlier {
			first = independentAt
		}
		stats, err := slow.WindowStats()
		if err != nil || !unansweredConnectSince(stats).Equal(first) || stats.receiveSynCount != 0 {
			t.Fatalf("the original did not own the initial unanswered clock: stats=%+v err=%v", stats, err)
		}

		if control == retainedNativeRacePreclosed {
			target.Cancel()
			<-target.retirementDone
			if target.ctx.Err() != context.Canceled || targetDialCount.Load() != 1 {
				t.Fatal("the original connected owner did not retire before the generated reset")
			}
		}
		if control == retainedNativeRaceAfterSynTerminal {
			release()
			until := time.Now().Add(reraceSendTimeout)
			for original.completionFlags.Load()&groupCompletionTerminal == 0 {
				harness.settle()
				if wait := time.Until(until); wait <= 0 {
					t.Fatal("the original SYN did not reach its peer terminal before the sibling won")
				} else if original.completionFlags.Load()&groupCompletionTerminal == 0 {
					time.Sleep(min(10*time.Millisecond, wait))
				}
			}
		}
		if independent {
			harness.sleep(time.Millisecond)
			independentAt = time.Now()
			send(independentPath)
			if len(slowExit.syns(independentPath)) != 1 || len(siblingExit.syns(independentPath)) != 1 {
				t.Fatal("the independently silent opening did not reach both peers")
			}
		}
		siblingExit.sendTo(siblingSyns[0].source, reraceSynAck(siblingSyns[0].path))
		deadline := time.Now().Add(harness.multiClient.settings.MultiRaceSetOnResponseTimeout + reraceSettle)
		if !harness.waitForSynAck(path, deadline) || harness.boundTo(path) != sibling {
			t.Fatal("the exact sibling did not win the original response race")
		}
		reply, _ := harness.synAck(path)
		parsed, err := ParseIpPath(reply.packet)
		if err != nil || parsed.AckSequenceNumber != path.SequenceNumber+1 {
			t.Fatalf("the selected reply did not acknowledge the original SYN: %v", err)
		}
		harness.settle()
		if resetPack.group == nil || resetPack.sequence != synPack.sequence || resetPack.ordinal <= synPack.ordinal ||
			!independent && control != retainedNativeRaceEarlier && resetPack.ordinal != synPack.ordinal+1 {
			t.Fatal("the generated loser reset did not retain its original source sequence and later ordinal")
		}
		opening := resetPack.group.nativeRaceReset
		if opening == nil || opening.resetOrdinal != resetPack.ordinal || opening.winner != nil {
			t.Fatal("the exact retained reset did not release its committed winner selection")
		}
		received := slowExit.received(path)
		if len(received) != 2 || !received[1].path.Rst || received[1].path.SequenceNumber != 0 || received[1].source != slow.ClientId() {
			t.Fatal("the exact generated reset did not reach its losing provider")
		}
		if orphan {
			if targetOwnerCount.Load() != 0 || targetDialCount.Load() != 0 || relay.matchedSynAck.Load() != 0 ||
				len(natLog.linesWith("tcp drop no syn (RST)")) != 1 {
				t.Fatalf("the genuine provider did not process a reset with no TCP owner: owners=%d dials=%d log=%v",
					targetOwnerCount.Load(), targetDialCount.Load(), natLog.lines())
			}
		} else if target.ctx.Err() != context.Canceled || targetOwnerCount.Load() != 1 {
			t.Fatalf("the generated reset did not cancel the original connected NAT generation: %v", target.ctx.Err())
		}
		if control != retainedNativeRaceAfterSynTerminal {
			if relay.resetMessage.Load() == nil {
				t.Fatal("the source did not publish its retained reset message before the wire")
			}
			// The actual ACK compressor can still be waiting on its timer
			// after every current goroutine has settled. Wait for the relay
			// to take custody of this precise cumulative response.
			timer := time.NewTimer(reraceSendTimeout)
			select {
			case <-relay.resetAckHeld:
				timer.Stop()
			case <-timer.C:
				t.Fatalf("no cumulative ACK reached the held reset: sequence=%s ordinal=%d held=%d original-terminal=%t reset-terminal=%t overflow=%t",
					resetPack.sequence, resetPack.ordinal, relay.ackCount.Load(),
					original.completionFlags.Load()&groupCompletionTerminal != 0,
					resetPack.group.completionFlags.Load()&groupCompletionTerminal != 0, relay.overflow.Load())
			}
			if original.completionFlags.Load()&groupCompletionTerminal != 0 {
				t.Fatal("the original SYN completed before releasing the held cumulative ACK")
			}
			if resetPack.group.completionFlags.Load()&groupCompletionTerminal != 0 {
				t.Fatal("the generated reset completed before releasing its held cumulative ACK")
			}
			if relay.ackCount.Load() == 0 {
				t.Fatal("the real ACK relay signaled a reset without holding a packet")
			}
		}
		if relay.overflow.Load() {
			t.Fatal("the bounded relay exhausted its ACK slots")
		}

		release()
		// Keep the same ordinary ACK compression cut as the adjacent
		// pre-ordinal test. Synchronization is on both exact terminal flags.
		deadline = time.Now().Add(reraceSendTimeout)
		for {
			harness.settle()
			if original.completionFlags.Load()&groupCompletionTerminal != 0 &&
				resetPack.group.completionFlags.Load()&groupCompletionTerminal != 0 {
				break
			}
			wait := time.Until(deadline)
			if wait <= 0 {
				t.Fatal("both exact original groups did not reach their peer terminals")
			}
			time.Sleep(min(10*time.Millisecond, wait))
		}
		update.stateLock.Lock()
		const terminalState = affinitySourceTerminal | affinitySourceFailed
		synCompleted := original.affinitySourceState&terminalState == affinitySourceTerminal
		resetCompleted := resetPack.group.affinitySourceState&terminalState == affinitySourceTerminal
		update.stateLock.Unlock()
		if !synCompleted || !resetCompleted || relay.overflow.Load() {
			t.Fatal("both retained source groups must succeed through ordinary Transfer")
		}

		stats, err = slow.WindowStats()
		if err != nil || stats.receiveSynCount != 0 || stats.receiveAckCount != 0 {
			t.Fatalf("a losing-provider return masked the connect clock: stats=%+v err=%v", stats, err)
		}
		timeout := harness.multiClient.settings.BlackholeConnectTimeout
		clock := unansweredConnectSince(stats)
		reason, _ := blackholeReasonFromStats(started.Add(timeout), stats, 0, 0, timeout, blackholeGates{})
		if orphan || control == retainedNativeRacePreclosed || control == retainedNativeRaceLegacy {
			if control == retainedNativeRaceLegacy && relay.outcomeCount.Load() == 0 {
				t.Fatal("the legacy control did not omit a genuine connected-reset outcome")
			}
			if control == retainedNativeRacePreclosed && (relay.outcomeCount.Load() != 0 || len(natLog.linesWith("tcp drop no syn (RST)")) != 1) {
				t.Fatal("retired provider owner incorrectly supplied the later orphan reset")
			}
			if !clock.Equal(started) || reason != blackholeNoReceiveSyn {
				t.Fatalf("orphan reset and successful ordinary ACK erased genuine unanswered evidence: clock=%s verdict=%q", clock, reason)
			}
			return
		}
		if relay.outcomeCount.Load() == 0 {
			t.Fatal("the connected provider did not return its solicited reset outcome")
		}
		if control == retainedNativeRaceEarlier {
			expires := independentAt.Add(timeout)
			before, _ := blackholeReasonFromStats(expires.Add(-time.Nanosecond), stats, 0, 0, timeout, blackholeGates{})
			at, _ := blackholeReasonFromStats(expires, stats, 0, 0, timeout, blackholeGates{})
			if !clock.Equal(independentAt) || reason != blackholeNoReceiveSyn || before != blackholeNone || at != blackholeNoReceiveSyn ||
				!original.connectSynOffer.orderedAnswer.Load() || independentOffer == nil || independentOffer.orderedAnswer.Load() {
				t.Fatal("the matched opening must retire while the earlier independently silent opening keeps its exact deadline")
			}
			return
		}
		if !clock.Equal(independentAt) || reason != blackholeNone {
			t.Fatalf("NATIVE_CONNECTED_RACE_LOSER_CLOCK_RETAINED: clock=%s want=%s verdict=%q", clock, independentAt, reason)
		}
		if independent {
			reason, _ := blackholeReasonFromStats(independentAt.Add(timeout), stats, 0, 0, timeout, blackholeGates{})
			if reason != blackholeNoReceiveSyn {
				t.Fatalf("independent genuine silence did not retain its own timeout: %q", reason)
			}
		}
	})
}
