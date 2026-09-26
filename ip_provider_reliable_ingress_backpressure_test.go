package connect

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The consumer is held before TcpSequence.Run, so a successful assertion can
// only mean local queue ownership: no upstream TCP socket, much less a remote
// TCP ACK, exists. Releasing one queue slot must preserve the original item
// and emit its Transfer ACK exactly once for both IP versions.
func TestProviderReliableTcpAckAfterFinalAdmissionWithoutRemoteAck(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				fixture := newReliableTcpIngressFixture(t, version, nil)
				payload := []byte("exact final TCP queue owner")
				sequence, messageID := fixture.deliver(payload)
				synctest.Wait()
				if ack := sequence.ackWindow.Snapshot(false); ack.ackUpdateCount != 0 {
					t.Errorf("full TcpSequence queue falsely ACKed original item: updates=%d head=%s want=%s", ack.ackUpdateCount, ack.headAck.messageId, messageID)
				}
				if fixture.dials.Load() != 0 {
					t.Fatal("fixture opened a remote TCP connection")
				}
				// This is the actual queued SYN, not a mock admission result.
				initial := <-fixture.tcp.sendItems
				if !initial.tcp.syn || initial.tcp.seq != 100 {
					initial.release()
					t.Fatal("final TCP queue did not contain the initial SYN")
				}
				initial.release()
				synctest.Wait()
				select {
				case admitted := <-fixture.tcp.sendItems:
					if admitted.tcp.seq != 101 || !bytes.Equal(admitted.tcp.payload, payload) {
						t.Errorf("final admission changed original sequence/payload: seq=%d payload=%q", admitted.tcp.seq, admitted.tcp.payload)
					}
					admitted.release()
				default:
					t.Error("released final queue did not receive retained original packet")
				}
				ack := sequence.ackWindow.Snapshot(false)
				if ack.ackUpdateCount != 1 || ack.headAck.messageId != messageID {
					t.Errorf("secured admission ACK updates=%d head=%s, want one exact item %s", ack.ackUpdateCount, ack.headAck.messageId, messageID)
				}
				if fixture.dials.Load() != 0 {
					t.Error("Transfer ACK depended on an upstream connection")
				}
			})
		})
	}
}

type reliableTcpIngressFixture struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	client    *Client
	provider  *RemoteUserNatProvider
	nat       *LocalUserNat
	tcp       *TcpSequence
	source    TransferPath
	path      *IpPath
	dials     atomic.Int64
	processed chan struct{}
}

func newReliableTcpIngressFixture(t *testing.T, version int, budget *TransferMemoryBudget) *reliableTcpIngressFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &reliableTcpIngressFixture{t: t, ctx: ctx, cancel: cancel,
		source: SourceId(NewId()), path: icmpTcpTestPath(version), processed: make(chan struct{}, 32)}
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
	natSettings.Log, natSettings.MemoryBudget = NewNoopLogger(), budget
	natSettings.TcpBufferSettings.WriteTimeout = 0
	captured := make(chan *TcpSequence, 1)
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) { captured <- sequence }
	natSettings.TcpBufferSettings.beforeSequenceRunForTest = func() { <-ctx.Done() }
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			f.dials.Add(1)
			return nil, context.Canceled
		},
	}
	f.nat = NewLocalUserNat(ctx, "reliable final TCP admission", natSettings)
	f.nat.afterSendPacketForTest = func() { f.processed <- struct{}{} }
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	f.provider = NewRemoteUserNatProvider(f.client, f.nat, providerSettings)
	t.Cleanup(func() {
		cancel()
		f.provider.Close()
		if err := f.nat.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	syn := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagSyn, 100, nil))
	if !f.nat.SendPacket(f.source, protocol.ProvideMode_Public, syn, 0) {
		MessagePoolReturn(syn)
		t.Fatal("initial SYN did not enter NAT")
	}
	f.tcp = <-captured
	<-f.processed
	if len(f.tcp.sendItems) != 1 || len(f.nat.sendPackets) != 0 {
		t.Fatal("fixture did not isolate the full final TCP queue")
	}
	return f
}

func (f *reliableTcpIngressFixture) deliver(payload []byte) (*ReceiveSequence, Id) {
	f.t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, 101, payload))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		f.t.Fatalf("raw receive ownership fixture: frame=%+v err=%v", frame, err)
	}
	sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(),
		sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
	messageID := NewId()
	sequence.deliverItems = []*receiveItem{{
		transferItem:    transferItem{messageId: messageID, sequenceNumber: 0},
		receiveCallback: f.client.receiveCallback, ack: true, frames: []*protocol.Frame{frame},
	}}
	sequence.deliverFrames = []*protocol.Frame{frame}
	sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
	go runReliableIngressFixtureDelivery(sequence)
	return sequence, messageID
}

// These direct-dispatch fixtures keep the ACK compressor idle so exact ACK
// counts remain inspectable. Drive the production pending-owner slow path;
// actual Run/duplicate/lane behavior is covered separately below.
func runReliableIngressFixtureDelivery(sequence *ReceiveSequence) {
	sequence.flushDeliver()
	if sequence.deliveryQueue == nil {
		return
	}
	defer sequence.deliveryQueue.cancel()
	for sequence.ctx.Err() == nil && sequence.deliveryQueue.pending() {
		sequence.waitDelivery(nil)
	}
}

// A pending data owner must not stop a later TCP ACK/window update on the
// same Transfer sequence: that update can be the event that releases NAT
// capacity. The full-budget case uses real prepaid control headroom while
// every ordinary byte of the shared NAT/provider budget is occupied.
func TestProviderReliableTcpPendingDataAllowsSameLaneControl(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, fullBudget := range []bool{false, true} {
			t.Run(fmt.Sprintf("ipv%d/full-budget-%t", version, fullBudget), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					var budget *TransferMemoryBudget
					if fullBudget {
						budget = NewTransferMemoryBudget(mib(2))
					}
					f := newReliableTcpIngressFixture(t, version, budget)
					f.establishForControl()
					sequence := f.runningReceiveSequence()
					if budget != nil {
						fill := budget.Available()
						if !budget.TryReserve(fill) {
							t.Fatal("could not occupy ordinary ingress headroom")
						}
						defer budget.Release(fill)
					}
					payload := []byte("pending before TCP window control")
					dataID := f.pack(sequence, 0, payload)
					synctest.Wait()
					if hasHead, head := reliableIngressCumulativeHead(sequence); hasHead {
						t.Errorf("pending data was cumulatively ACKed: head=%s data=%s", head.messageId, dataID)
					}
					controlID := f.pack(sequence, 1, nil)
					synctest.Wait()
					f.tcp.mutex.Lock()
					window := f.tcp.receiveWindowSize
					f.tcp.mutex.Unlock()
					if window != 4096 {
						t.Errorf("later same-lane TCP control was blocked by data: window=%d, want 4096", window)
					}
					if hasHead, head := reliableIngressCumulativeHead(sequence); hasHead {
						t.Errorf("control ACK jumped an unsecured data item: head=%s control=%s data=%s", head.messageId, controlID, dataID)
					}
					if budget != nil && budget.UsedByteCount() != budget.TotalByteCount() {
						t.Errorf("full-budget control escaped or released a live owner: used=%d total=%d", budget.UsedByteCount(), budget.TotalByteCount())
					}
					// Release both blockers; the original data, not a new TCP
					// retry, must eventually reach the final sequence queue.
					(<-f.tcp.sendItems).release()
					if budget != nil {
						// The deferred fill release belongs to this test, so only
						// assert cancellation ownership here. Dedicated credit
						// handoff tests exercise admission at zero spare budget.
						f.cancel()
					} else {
						synctest.Wait()
						select {
						case item := <-f.tcp.sendItems:
							if !bytes.Equal(item.tcp.payload, payload) {
								t.Error("capacity release admitted different bytes")
							}
							item.release()
						default:
							t.Error("pending original data was lost before capacity release")
						}
						if hasHead, head := reliableIngressCumulativeHead(sequence); !hasHead || head.messageId != controlID {
							t.Errorf("admitted prefix did not ACK the already-secured control: head=%+v present=%t", head, hasHead)
						}
					}
				})
			})
		}
	}
}

func (f *reliableTcpIngressFixture) establishForControl() {
	f.tcp.receiveAckCondition()
	f.tcp.mutex.Lock()
	f.tcp.established = true
	f.tcp.receiveSeq = 500
	f.tcp.receiveSeqAck = 0
	f.tcp.receiveWindowSize = 0
	f.tcp.receiveWindowEnd = 0
	f.tcp.receiveWindowEndSet = true
	f.tcp.mutex.Unlock()
}

func (f *reliableTcpIngressFixture) runningReceiveSequence(configure ...func(*ReceiveBufferSettings)) *ReceiveSequence {
	f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
	route := make(Route, 64)
	transport := NewSendGatewayTransport()
	f.client.RouteManager().UpdateTransport(transport, []Route{route})
	settings := DefaultReceiveBufferSettings()
	settings.IdleTimeout, settings.AckCompressTimeout = time.Hour, 0
	for _, change := range configure {
		change(settings)
	}
	sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(),
		sequenceTlsRoleServer, false, settings)
	go sequence.Run()
	f.t.Cleanup(func() {
		sequence.Cancel()
		sequence.WaitForExit()
		sequence.Close()
		f.client.RouteManager().RemoveTransport(transport)
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	})
	return sequence
}

func (f *reliableTcpIngressFixture) pack(sequence *ReceiveSequence, number uint64, payload []byte) Id {
	f.t.Helper()
	messageID := NewId()
	if !f.packWithIdentity(sequence, number, payload, messageID) {
		f.t.Fatal("same-lane Pack admission failed")
	}
	return messageID
}

func (f *reliableTcpIngressFixture) packWithIdentity(sequence *ReceiveSequence, number uint64, payload []byte, messageID Id) bool {
	f.t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, 101, payload))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		f.t.Fatalf("raw Pack fixture: frame=%+v err=%v", frame, err)
	}
	pack := &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
		Pack: &protocol.Pack{MessageId: messageID.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
			SequenceNumber: number, Head: number == 0, Frames: []*protocol.Frame{frame}},
		ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(packet)),
	}
	if success, err := sequence.Pack(pack, 0); !success || err != nil {
		pack.messagePoolReturn()
		return false
	}
	return true
}

func reliableIngressCumulativeHead(sequence *ReceiveSequence) (bool, sequenceAck) {
	sequence.ackWindow.ackLock.Lock()
	defer sequence.ackWindow.ackLock.Unlock()
	return sequence.ackWindow.hasHeadAck, sequence.ackWindow.headAck
}

// A combined callback must retain per-item boundaries: securing item zero
// (a TCP window update) is not evidence for item one (backpressured payload).
func TestProviderReliableTcpBatchAckStopsAtFirstUnsecuredItem(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				f := newReliableTcpIngressFixture(t, version, nil)
				f.establishForControl()
				sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
				ids := []Id{NewId(), NewId()}
				payload := []byte("second Transfer item must retain its own receipt")
				for index, data := range [][]byte{nil, payload} {
					packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, 101, data))
					frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
					if err != nil || !frame.Raw {
						MessagePoolReturn(packet)
						t.Fatalf("raw batch fixture: frame=%+v err=%v", frame, err)
					}
					sequence.deliverItems = append(sequence.deliverItems, &receiveItem{
						transferItem:    transferItem{messageId: ids[index], sequenceNumber: uint64(index)},
						receiveCallback: f.client.receiveCallback, ack: true, frames: []*protocol.Frame{frame},
					})
					sequence.deliverFrames = append(sequence.deliverFrames, frame)
				}
				sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
				go runReliableIngressFixtureDelivery(sequence)
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != ids[0] {
					t.Errorf("batch ACK crossed final-admission boundary: present=%t head=%s, want secured first=%s (blocked second=%s)", present, head.messageId, ids[0], ids[1])
				}
				if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[ids[1]].messageId == ids[1] {
					t.Error("unsecured second item received selective service credit")
				}
				(<-f.tcp.sendItems).release()
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if !bytes.Equal(item.tcp.payload, payload) {
						t.Error("retained second item changed bytes")
					}
					item.release()
				default:
					t.Error("unsecured second item was lost")
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != ids[1] {
					t.Error("second item was not ACKed after final queue admission")
				}
			})
		})
	}
}

// nextSequenceNumber already advanced when the application receives an item.
// Its exact duplicate must consult pending downstream ownership before the
// old "past sequence => cumulative ACK" shortcut, including after refusal.
func TestProviderReliableTcpPendingDuplicateCannotAck(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				f := newReliableTcpIngressFixture(t, version, nil)
				sequence := f.runningReceiveSequence()
				payload := []byte("same message id and TCP sequence")
				messageID := f.pack(sequence, 0, payload)
				synctest.Wait()
				if !f.packWithIdentity(sequence, 0, payload, messageID) {
					t.Fatal("pending retransmit did not reach the receive sequence")
				}
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); present {
					t.Errorf("past-item duplicate ACKed unowned original: head=%s original=%s", head.messageId, messageID)
				}
				// Stop only the downstream owner, not the Transfer client.
				// A resend after that terminal refusal may be rejected or may
				// reach a tombstoned sequence, but it may not invent delivery.
				f.provider.Close()
				f.packWithIdentity(sequence, 0, payload, messageID)
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); present {
					t.Errorf("duplicate after provider shutdown retained false ACK: head=%s original=%s", head.messageId, messageID)
				}
			})
		})
	}
}

// The NAT queue has already paid for the complete packet and TcpSendItem.
// A second reservation at TcpBuffer.tcpSend must not turn that owned packet
// into a drop when unrelated retained owners fill the remaining budget.
func TestProviderReliableTcpQueuedCreditNeedsNoSecondReservation(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				budget := NewTransferMemoryBudget(mib(2))
				f := newReliableTcpIngressFixture(t, version, budget)
				synctest.Wait()
				(<-f.tcp.sendItems).release()
				entered, release := make(chan struct{}), make(chan struct{})
				f.nat.beforeSendPacketForTest = func(*SendPacket) { close(entered); <-release }
				payload := bytes.Repeat([]byte{0x5a}, 1200)
				sequence, messageID := f.deliver(payload)
				<-entered
				synctest.Wait()
				fill := budget.Available()
				if !budget.TryReserve(fill) || budget.Available() != 0 {
					t.Fatal("could not fill budget after exact NAT queue ownership")
				}
				defer budget.Release(fill)
				before := budget.UsedByteCount()
				close(release)
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if !bytes.Equal(item.tcp.payload, payload) || item.memory.budget != budget || item.memory.bytes == 0 {
						t.Errorf("final queue lost exact prepaid packet claim: seq=%d claim=%+v", item.tcp.seq, item.memory)
					}
					item.release()
				default:
					t.Error("packet paid at NAT queue was dropped by second final-TCP reservation")
				}
				if budget.UsedByteCount() > before || budget.UsedByteCount() > budget.TotalByteCount() {
					t.Error("ownership handoff exceeded the frozen shared ceiling")
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != messageID {
					t.Error("prepaid final admission did not ACK the original item")
				}
			})
		})
	}
}

func TestProviderReliableTcpCancellationAndImpossibleFitNeverAck(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, stop := range []string{"provider-shutdown", "nat-shutdown", "impossible-fit"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, stop), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					budget := NewTransferMemoryBudget(mib(2))
					f := newReliableTcpIngressFixture(t, version, budget)
					synctest.Wait()
					switch stop {
					case "provider-shutdown":
						f.provider.Close()
					case "nat-shutdown":
						f.nat.Close()
					case "impossible-fit":
						// Retained owners survive a role-driven shrink, but a
						// packet larger than the new ceiling cannot ever fit.
						budget.SetTotalByteCount(1)
						defer budget.SetTotalByteCount(mib(2))
					}
					sequence, messageID := f.deliver(bytes.Repeat([]byte{0x3c}, 1200))
					synctest.Wait()
					if present, head := reliableIngressCumulativeHead(sequence); present {
						t.Errorf("terminal downstream refusal produced successful ACK: case=%s head=%s original=%s", stop, head.messageId, messageID)
					}
					f.cancel()
					synctest.Wait()
					if present, head := reliableIngressCumulativeHead(sequence); present {
						t.Errorf("cancel promoted an unsecured original item: head=%s original=%s", head.messageId, messageID)
					}
				})
			})
		}
	}
}

// SACK proves retained ownership, not final delivery. Once the missing head
// arrives, the leased item's exact roots/claim must survive final NAT queue
// pressure; a later cumulative ACK must still wait for the final owner.
func TestProviderReliableTcpSelectiveLeaseSurvivesFinalAdmissionPressure(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				f := newReliableTcpIngressFixture(t, version, nil)
				f.establishForControl()
				holdBudget := NewTransferMemoryBudget(16 * 1024)
				sequence := f.runningReceiveSequence(func(settings *ReceiveBufferSettings) {
					settings.ReceiveQueueBudget = holdBudget
					settings.ReceiveQueueMinByteCount = 0
					settings.ReceiveQueueMaxByteCount = 16 * 1024
					settings.ReceiveQueueRetainedByteAccounting = true
					settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
				})
				payload := []byte("leased exact out-of-order item")
				messageID := f.pack(sequence, 1, payload)
				synctest.Wait()
				held := sequence.receiveQueue.GetByMessageId(messageID)
				if held == nil || !held.committed || held.memoryBudget != holdBudget || holdBudget.UsedByteCount() == 0 {
					t.Fatal("out-of-order fixture has no exact retained selective-ACK lease")
				}
				witness := MessagePoolShareReadOnly(held.frames[0].MessageBytes)
				defer MessagePoolReturn(witness)
				if present, _ := reliableIngressCumulativeHead(sequence); present {
					t.Fatal("out-of-order item was cumulatively ACKed before its missing head")
				}
				controlID := f.pack(sequence, 0, nil)
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != controlID {
					t.Errorf("leased item became terminal before final admission: head=%s, want secured head=%s (leased=%s)", head.messageId, controlID, messageID)
				}
				if holdBudget.UsedByteCount() == 0 {
					t.Error("selectively leased packet lost its retained claim under final queue pressure")
				}
				(<-f.tcp.sendItems).release()
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if !bytes.Equal(item.tcp.payload, payload) {
						t.Error("selectively leased item changed before admission")
					}
					item.release()
				default:
					t.Error("selective ACK lease did not preserve its exact original packet")
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != messageID {
					t.Error("secured leased item did not advance the cumulative head")
				}
			})
		})
	}
}

// This exercises the production Client.receive dispatcher and flushDeliver
// ACK-window update, not a simulated sender callback. A full LocalUserNat
// queue has not accepted the TCP byte; returning a successful Transfer ACK
// would permanently lose it under lifetime TCP collapse prevention.
func TestProviderReliableTcpAckWaitsForNatOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		keyCtx, cancelKey := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-keyCtx.Done() }
		provider, client, nat := newProviderTransferKeyTestFixtureWithClientSettings(
			t, DefaultRemoteUserNatProviderSettings(), settings)
		defer provider.Close()
		defer client.Cancel()
		defer cancelKey()
		client.AddReceiveCallback(provider.ClientReceive)
		// This legacy fixture constructs the provider directly rather than
		// through its registering constructor.
		client.reliableProviderIngress.Store(true)
		nat.settings = DefaultLocalUserNatSettingsWithBufferSize(1)
		// The paused one-slot queue is genuinely full, not an injected return.
		nat.sendPackets <- &SendPacket{}
		defer func() {
			for len(nat.sendPackets) > 0 {
				queued := <-nat.sendPackets
				for _, packet := range queued.packets {
					MessagePoolReturn(packet)
				}
				queued.finish()
			}
		}()
		packet := MessagePoolCopy(providerTransferKeyTestPacket())
		defer func() {
			if !MessagePoolReturn(packet) {
				t.Error("original packet retained an extra owner after receipt cancellation")
			}
		}()
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !frame.Raw {
			defer MessagePoolReturn(frame.MessageBytes)
		}
		sequence := NewReceiveSequence(provider.ctx, client, SourceId(NewId()), NewId(),
			sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		messageID := NewId()
		sequence.deliverItems = []*receiveItem{{
			transferItem:    transferItem{messageId: messageID, sequenceNumber: 7},
			receiveCallback: client.receiveCallback, ack: true,
			frames: []*protocol.Frame{{MessageType: frame.MessageType, Raw: frame.Raw,
				MessageBytes: MessagePoolShareReadOnly(frame.MessageBytes)}},
		}}
		// flush clears its batching slice; do not alias the item's owning slice.
		sequence.deliverFrames = []*protocol.Frame{sequence.deliverItems[0].frames[0]}
		sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
		done := make(chan struct{})
		go func() { runReliableIngressFixtureDelivery(sequence); close(done) }()
		synctest.Wait()
		ack := sequence.ackWindow.Snapshot(false)
		select {
		case <-done:
			t.Fatalf("reliable ownership completed with full NAT queue: peer ACK updates=%d, head=%s, ingress drops=%+v; no downstream TCP owner exists", ack.ackUpdateCount, ack.headAck.messageId, provider.CongestionDropStats())
		default:
		}
		if ack.ackUpdateCount != 0 || provider.CongestionDropStats().IngressNatPacketCount != 0 {
			t.Fatal("backpressured TCP was ACKed or discarded before NAT ownership")
		}
		(<-nat.sendPackets).finish()
		nat.reliableCapacity.notify()
		synctest.Wait()
		select {
		case queued := <-nat.sendPackets:
			if len(queued.packets) != 1 {
				t.Fatalf("released queue owns %d packets, want one", len(queued.packets))
			}
			// Intermediate NAT ownership is still not final TcpSequence
			// admission. Disposing this queued owner must fail the receipt.
			if ack := sequence.ackWindow.Snapshot(false); ack.ackUpdateCount != 0 {
				t.Fatal("intermediate NAT queue emitted a final ACK")
			}
			queued.finish()
		case <-time.After(time.Second):
			t.Fatal("queue release did not transfer the retained TCP packet")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("canceled NAT owner did not release receipt")
		}
		ack = sequence.ackWindow.Snapshot(false)
		if ack.ackUpdateCount != 0 {
			t.Fatalf("canceled intermediate owner produced final ACK=%+v", ack)
		}
		cancelKey()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// The ingress channel is empty here. A real NAT shard has already admitted a
// SYN into its one-slot TcpSequence queue, whose real consumer is held by the
// existing test barrier. A payload may not be ACKed merely because it entered
// the NAT channel, then be discarded when that final queue is still full.
func TestProviderReliableTcpAckWaitsForFinalTcpAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
		natSettings.Log = NewNoopLogger()
		natSettings.TcpBufferSettings.WriteTimeout = 0
		sequenceEntered := make(chan struct{})
		natSettings.TcpBufferSettings.beforeSequenceRunForTest = func() {
			close(sequenceEntered)
			<-ctx.Done()
		}
		nat := NewLocalUserNat(ctx, "reliable ingress final TCP owner", natSettings)
		processed := make(chan struct{}, 2)
		nat.afterSendPacketForTest = func() { processed <- struct{}{} }
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy {
			return DisableSecurityPolicy()
		}
		provider := NewRemoteUserNatProvider(client, nat, providerSettings)
		defer func() {
			cancel()
			provider.Close()
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		source, path := SourceId(NewId()), icmpTcpTestPath(4)
		syn := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
		if !nat.SendPacket(source, protocol.ProvideMode_Public, syn, 0) {
			MessagePoolReturn(syn)
			t.Fatal("initial SYN did not enter the NAT")
		}
		<-sequenceEntered
		<-processed
		if len(nat.sendPackets) != 0 {
			t.Fatal("test did not isolate the final TCP queue from NAT ingress")
		}

		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 101, []byte("retained TCP bytes")))
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil || !frame.Raw {
			MessagePoolReturn(packet)
			t.Fatalf("raw ownership fixture: frame=%+v err=%v", frame, err)
		}
		witness := MessagePoolShareReadOnly(packet)
		witnessLive := true
		defer func() {
			if witnessLive {
				MessagePoolReturn(witness)
			}
		}()
		sequence := NewReceiveSequence(ctx, client, source, NewId(),
			sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		messageID := NewId()
		sequence.deliverItems = []*receiveItem{{
			transferItem:    transferItem{messageId: messageID, sequenceNumber: 7},
			receiveCallback: client.receiveCallback, ack: true,
			frames: []*protocol.Frame{frame},
		}}
		sequence.deliverFrames = []*protocol.Frame{frame}
		sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
		done := make(chan struct{})
		go func() { runReliableIngressFixtureDelivery(sequence); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			ack := sequence.ackWindow.Snapshot(false)
			freed := MessagePoolReturn(witness)
			witnessLive = false
			t.Fatalf("callback finished before final TCP ownership: ACK updates=%d head=%s, shard disposals=%d, packet fully freed=%t, NAT drops=%+v",
				ack.ackUpdateCount, ack.headAck.messageId, len(processed), freed, provider.CongestionDropStats())
		default:
		}
		if ack := sequence.ackWindow.Snapshot(false); ack.ackUpdateCount != 0 {
			t.Fatalf("full final TCP queue already ACKed item: %+v", ack)
		}
		cancel()
		<-done
		if ack := sequence.ackWindow.Snapshot(false); ack.ackUpdateCount != 0 {
			t.Fatalf("canceled, never-secured TCP packet was ACKed: %+v", ack)
		}
	})
}
