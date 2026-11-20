// Connect-health controls exercise the existing packet/group send boundaries.
// The Transfer seam supplies exact admission and terminal dispositions; it does
// not supply health counters, a provider reply, or a real carrier observation.
package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// The bytes are make-backed and remain valid throughout the fake Transfer
// lifetime. Both variants use the production raw-frame and TCP ACK policy.
type connectAdmissionTestOffer struct {
	packet *parsedPacket
	group  *parsedPacketGroup
}

// Each packet has a real parsed TCP header. A group may include a non-SYN
// prefix/suffix so disposition tests can identify the exact materialized range.
func newConnectAdmissionTestOffer(t *testing.T, grouped bool, syns ...bool) *connectAdmissionTestOffer {
	t.Helper()
	if len(syns) == 0 {
		syns = []bool{true}
		if grouped {
			syns = []bool{true, false}
		}
	}
	if !grouped && len(syns) != 1 {
		t.Fatal("singleton fixture received several packets")
	}
	packets := make([]parsedPacket, 0, len(syns))
	var byteCount ByteCount
	for _, syn := range syns {
		flags := byte(0x10)
		if syn {
			flags = 0x02
		}
		packet, err := newParsedPacket(buildTestTcp4Packet(
			netip.MustParseAddr("192.0.2.10"), 41000,
			netip.MustParseAddr("198.51.100.20"), 443, flags, 0,
		))
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, *packet)
		byteCount += ByteCount(len(packet.packet))
	}
	if !grouped {
		return &connectAdmissionTestOffer{packet: &packets[0]}
	}
	return &connectAdmissionTestOffer{group: &parsedPacketGroup{
		packets: packets, ipPath: packets[0].ipPath, byteCount: byteCount,
	}}
}

// Calls the same scalar or whole-group accounting entry used by the channel.
func (self *connectAdmissionTestOffer) send(channel *multiClientChannel, requestedAck bool) (bool, error) {
	if self.group != nil {
		return channel.SendGroupDetailedWithAck(self.group, 0, requestedAck)
	}
	return channel.SendDetailedWithAck(self.packet, 0, requestedAck)
}

// Default health bars and ordinary stats maps remain installed. The existing
// packet clock hook is used only by the explicit interleaving controls below.
func newConnectAdmissionTestChannel(t *testing.T) *multiClientChannel {
	t.Helper()
	channel := evidenceTestChannel(t.Context(), DefaultMultiClientSettings())
	channel.settings.ProtocolVersion = DefaultProtocolVersion
	return channel
}

// WindowStats is the actual coalescing snapshot. Only the pure classifier's
// now argument advances. Open/no-hold gates are an injected classifier input,
// not an observation of a real active carrier or fresh ingress. Receive credit
// below comes only from the separately driven addReceiveSyn boundary.
func requireConnectAdmissionEvidence(t *testing.T, label string, channel *multiClientChannel, wanted bool, exact time.Time, wantErr error) *clientWindowStats {
	t.Helper()
	stats, err := channel.WindowStats()
	if !errors.Is(err, wantErr) || (wantErr == nil && err != nil) {
		t.Errorf("%s: provider error=%v, want %v", label, err, wantErr)
	}
	got := stats.firstUnansweredSendSynTime
	if got.IsZero() == wanted || (wanted && !exact.IsZero() && !got.Equal(exact)) {
		t.Errorf("%s: unanswered clock=%v, want evidence=%t exact=%v", label, got, wanted, exact)
	}
	future := time.Now().Add(2 * channel.settings.BlackholeConnectTimeout)
	if wanted && !exact.IsZero() {
		future = exact.Add(2 * channel.settings.BlackholeConnectTimeout)
	} else if !got.IsZero() {
		future = got.Add(2 * channel.settings.BlackholeConnectTimeout)
	}
	reason, held := blackholeReasonFromStats(future, stats, 0, 0,
		channel.settings.BlackholeConnectTimeout, blackholeGates{
			transportDown: false, uplinkStale: false, unreliableSendTransport: false,
		})
	wantReason := blackholeNone
	if wanted && stats.receiveSynCount == 0 && stats.receiveAckCount == 0 {
		wantReason = blackholeNoReceiveSyn
	}
	if reason != wantReason || held != blackholeNone {
		t.Errorf("%s: connect verdict=%s held=%s, want %s with open gates", label, reason, held, wantReason)
	}
	return stats
}

// A false/error return or a proven wholly unwritten terminal must not invent
// provider silence. Both requested ACK values still use TCP's reliable path.
func TestMultiClientUnadmittedSynDoesNotCreateConnectEvidence(t *testing.T) {
	refused := errors.New("synthetic pre-admission refusal")
	capacity := &sendGroupCapacityError{cause: ErrSendPackNotAdmitted}
	for _, grouped := range []bool{false, true} {
		for _, requestedAck := range []bool{false, true} {
			for _, row := range []struct {
				name      string
				admitted  bool
				returned  error
				completed error
				inline    bool
				groupOnly bool
			}{
				{name: "false"},
				{name: "error", returned: refused},
				{name: "unwritten", admitted: true, completed: errSendPackExpiredUnwritten},
				{name: "inline-unwritten", admitted: true, completed: errSendPackExpiredUnwritten, inline: true},
				{name: "joined-unwritten", admitted: true, completed: errors.Join(errSendPackExpiredUnwritten, errSendPackExpiredUnwritten)},
				{name: "capacity", admitted: true, completed: capacity, groupOnly: true},
				{name: "joined-local-proofs", admitted: true, completed: errors.Join(errSendPackExpiredUnwritten, capacity), groupOnly: true},
			} {
				if row.groupOnly && !grouped {
					continue
				}
				label := fmt.Sprintf("%s/group=%t/requestedAck=%t", row.name, grouped, requestedAck)
				channel := newConnectAdmissionTestChannel(t)
				offer := newConnectAdmissionTestOffer(t, grouped)
				var callback AckFunction
				channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
					callback = completed
					if row.admitted && row.inline {
						if offer.group != nil {
							offer.group.sendGroupDisposition(0, len(offer.group.packets), false)
						}
						completed(row.completed)
					}
					return row.admitted, row.returned
				}
				accepted, err := offer.send(channel, requestedAck)
				if accepted != row.admitted || !errors.Is(err, row.returned) || callback == nil {
					t.Fatalf("%s: did not reach exact Transfer result: accepted=%t err=%v callback=%t", label, accepted, err, callback != nil)
				}
				if accepted && !row.inline {
					if offer.group != nil {
						offer.group.sendGroupDisposition(0, len(offer.group.packets), false)
					}
					callback(row.completed)
				}
				stats := requireConnectAdmissionEvidence(t, label, channel, false, time.Time{}, nil)
				if stats.sendNackCount != 0 || stats.sendNackByteCount != 0 || stats.sendAckCount != 0 || !stats.pendingSendTime.IsZero() {
					t.Errorf("%s: refused input left pending=%d/%d ACK=%d clock=%v", label,
						stats.sendNackCount, stats.sendNackByteCount, stats.sendAckCount, stats.pendingSendTime)
				}
				if accepted && offer.group != nil && offer.group.completionFlags.Load()&groupCompletionAck == 0 {
					t.Errorf("%s: requested NoAck demoted TCP recovery", label)
				}
			}
		}
	}
}

// Admitted silence, successful Transfer ACKs and ambiguous failures remain
// evidence. errors.Join omits a successful chunk: its materialized SYN must
// survive a different chunk's local failure, including inline completion.
func TestMultiClientAdmittedSynKeepsConnectEvidence(t *testing.T) {
	hard := errors.New("synthetic admitted structural failure")
	capacity := &sendGroupCapacityError{cause: ErrSendPackNotAdmitted}
	unwrittenOpening := newUnwrittenSendPackError(errNativeSynOpeningSuperseded)
	for _, grouped := range []bool{false, true} {
		for _, requestedAck := range []bool{false, true} {
			for _, inline := range []bool{false, true} {
				for _, row := range []struct {
					name         string
					completed    error
					wantErr      error
					groupOnly    bool
					materialized bool
					synPrefix    bool
					bothSyn      bool
					wanted       bool
				}{
					{name: "peer-ack", wanted: true},
					{name: "unwritten-opening", completed: unwrittenOpening, groupOnly: true},
					{name: "canceled-unwritten-opening", completed: newUnwrittenSendPackError(fmt.Errorf("%w: %w", errNativeSynOpeningSuperseded, context.Canceled)), groupOnly: true},
					{name: "opening-without-unwritten-proof", completed: errNativeSynOpeningSuperseded, groupOnly: true, wanted: true},
					{name: "unwritten-opening-and-ambiguous-chunk", completed: errors.Join(unwrittenOpening, hard), groupOnly: true, wanted: true},
					{name: "materialized-syn-before-unwritten-opening", completed: unwrittenOpening, groupOnly: true, materialized: true, synPrefix: true, wanted: true},
					{name: "ambiguous", completed: ErrSendPackNotAdmitted, wantErr: ErrSendPackNotAdmitted, wanted: true},
					{name: "write-timeout", completed: errTransferRouteWriteTimeout, wantErr: errTransferRouteWriteTimeout, wanted: true},
					{name: "hard-error", completed: hard, wantErr: hard, wanted: true},
					{name: "admitted-canceled", completed: context.Canceled, wantErr: context.Canceled, wanted: true},
					{name: "joined-ambiguous", completed: errors.Join(errSendPackExpiredUnwritten, hard), wantErr: hard, wanted: true},
					{name: "syn-prefix-unwritten-tail", completed: errors.Join(nil, errSendPackExpiredUnwritten), groupOnly: true, materialized: true, synPrefix: true, wanted: true},
					{name: "syn-prefix-capacity-tail", completed: errors.Join(nil, capacity), groupOnly: true, materialized: true, synPrefix: true, wanted: true},
					{name: "syn-prefix-mixed-local-tail", completed: errors.Join(errSendPackExpiredUnwritten, capacity), groupOnly: true, materialized: true, synPrefix: true, wanted: true},
					{name: "mixed-local-and-ambiguous", completed: errors.Join(errSendPackExpiredUnwritten, capacity, hard), wantErr: hard, groupOnly: true, wanted: true},
					{name: "two-syn-prefix", completed: errSendPackExpiredUnwritten, groupOnly: true, materialized: true, synPrefix: true, bothSyn: true, wanted: true},
					{name: "non-syn-prefix-unwritten-syn", completed: errSendPackExpiredUnwritten, groupOnly: true, materialized: true, wanted: false},
				} {
					if row.groupOnly && !grouped {
						continue
					}
					label := fmt.Sprintf("%s/group=%t/requestedAck=%t/inline=%t", row.name, grouped, requestedAck, inline)
					channel := newConnectAdmissionTestChannel(t)
					offer := newConnectAdmissionTestOffer(t, grouped)
					if row.materialized {
						offer = newConnectAdmissionTestOffer(t, true, row.synPrefix, row.bothSyn || !row.synPrefix)
					}
					var callback AckFunction
					complete := func() {
						if row.materialized {
							offer.group.sendGroupDisposition(0, 1, true)
							offer.group.sendGroupDisposition(1, 2, false)
						}
						callback(row.completed)
					}
					channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
						callback = completed
						if inline {
							complete()
						}
						return true, nil
					}
					if accepted, err := offer.send(channel, requestedAck); !accepted || err != nil || callback == nil {
						t.Fatalf("%s: admission=%t err=%v callback=%t", label, accepted, err, callback != nil)
					}
					if !inline {
						requireConnectAdmissionEvidence(t, label+" pending", channel, true, time.Time{}, nil)
						complete()
					}
					stats := requireConnectAdmissionEvidence(t, label, channel, row.wanted, time.Time{}, row.wantErr)
					count := 1
					if offer.group != nil {
						count = len(offer.group.packets)
						if offer.group.completionFlags.Load()&groupCompletionAck == 0 {
							t.Errorf("%s: TCP group lost mandatory ACK recovery", label)
						}
					}
					if row.completed == nil && (stats.sendAckCount != count || stats.sendNackCount != 0) {
						t.Errorf("%s: peer ACK credit/pending=%d/%d, want %d/0", label, stats.sendAckCount, stats.sendNackCount, count)
					}
					if row.completed == nil && (!hasPeerSendAckInWindow(stats) || stats.pendingSendTime.IsZero()) {
						t.Errorf("%s: TCP completion lost peer ACK evidence", label)
					}
				}
			}
		}
	}
}

// Exact offer clocks distinguish the two surviving attempts without sleeps.
// Aging only the telemetry buckets exercises real coalescing; it does not
// create, erase or reseed any lifetime health marker or completion receipt.
func TestMultiClientSynEvidenceRetractionPreservesOtherOffers(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		for _, requestedAck := range []bool{false, true} {
			for _, rotate := range []bool{false, true} {
				for _, reverse := range []bool{false, true} {
					for _, confirmMiddle := range []bool{false, true} {
						label := fmt.Sprintf("group=%t/requestedAck=%t/rotate=%t/reverse=%t/confirmMiddle=%t", grouped, requestedAck, rotate, reverse, confirmMiddle)
						channel := newConnectAdmissionTestChannel(t)
						clock := time.Now()
						channel.packetTransferNowForTest = func() time.Time { return clock }
						var callbacks []AckFunction
						channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
							callbacks = append(callbacks, completed)
							return true, nil
						}
						times := []time.Time{clock, clock.Add(time.Second), clock.Add(2 * time.Second)}
						for index := range times {
							clock = times[index]
							if index == 1 && rotate {
								channel.stateLock.Lock()
								for _, bucket := range channel.eventBuckets {
									bucket.createTime = time.Now().Add(-2 * channel.settings.StatsWindowDuration)
									bucket.eventTime = bucket.createTime
								}
								channel.stateLock.Unlock()
								requireConnectAdmissionEvidence(t, label+" aged", channel, true, times[0], nil)
								if len(channel.eventBuckets) != 0 {
									t.Fatal("rotation premise did not expire the original telemetry bucket")
								}
							}
							offer := newConnectAdmissionTestOffer(t, grouped)
							if accepted, err := offer.send(channel, requestedAck); !accepted || err != nil {
								t.Fatalf("%s offer%d: %t %v", label, index, accepted, err)
							}
						}
						if len(callbacks) != 3 {
							t.Fatal("three actual send calls did not publish three callbacks")
						}
						// An unresolved or genuinely delivered middle offer must
						// survive either order of the other two local expiries.
						if confirmMiddle {
							callbacks[1](nil)
						}
						requireConnectAdmissionEvidence(t, label+" middle survives", channel, true, times[0], nil)
						if reverse {
							callbacks[2](errSendPackExpiredUnwritten)
							requireConnectAdmissionEvidence(t, label+" late refusal", channel, true, times[0], nil)
							callbacks[0](errSendPackExpiredUnwritten)
						} else {
							callbacks[0](errSendPackExpiredUnwritten)
							requireConnectAdmissionEvidence(t, label+" early refusal", channel, true, times[1], nil)
							callbacks[2](errSendPackExpiredUnwritten)
						}
						stats := requireConnectAdmissionEvidence(t, label+" final", channel, true, times[1], nil)
						if !confirmMiddle {
							callbacks[1](errSendPackExpiredUnwritten)
							stats = requireConnectAdmissionEvidence(t, label+" all unwritten", channel, false, time.Time{}, nil)
						}
						if stats.sendNackCount != 0 || stats.sendNackByteCount != 0 {
							t.Errorf("%s: completion left pending=%d/%d", label, stats.sendNackCount, stats.sendNackByteCount)
						}
					}
				}
			}
		}
	}

	// A refused whole-group descriptor is still caller-owned and may retry.
	// Its old offer has no callback and cannot become the new offer's debt.
	for _, requestedAck := range []bool{false, true} {
		channel := newConnectAdmissionTestChannel(t)
		clock := time.Now()
		channel.packetTransferNowForTest = func() time.Time { return clock }
		offer := newConnectAdmissionTestOffer(t, true, true, true)
		channel.sendTransferForTest = func(AckFunction) (bool, error) { return false, nil }
		if accepted, err := offer.send(channel, requestedAck); accepted || err != nil {
			t.Fatalf("group reuse refusal=%t %v", accepted, err)
		}
		requireConnectAdmissionEvidence(t, "group reuse refused", channel, false, time.Time{}, nil)
		clock = clock.Add(time.Second)
		var callback AckFunction
		channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
			callback = completed
			return true, nil
		}
		if accepted, err := offer.send(channel, requestedAck); !accepted || err != nil || callback == nil {
			t.Fatalf("group reuse admission=%t %v callback=%t", accepted, err, callback != nil)
		}
		requireConnectAdmissionEvidence(t, "group reuse admitted", channel, true, clock, nil)
		callback(nil)
		requireConnectAdmissionEvidence(t, "group reuse ACK", channel, true, clock, nil)
	}
}

// The valid receive-health boundary closes the evidence epoch even when the
// older publisher has not returned. Driving addReceiveSyn here does not prove
// wire admission; the existing P2 controls own stale/ahead-of-gap exclusion.
// Old callbacks cannot reopen the epoch or erase a newer admitted attempt.
func TestMultiClientReceivedSynFencesLateAdmissionEvidence(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		for _, requestedAck := range []bool{false, true} {
			for _, receivedInline := range []bool{false, true} {
				for _, callbackInline := range []bool{false, true} {
					if callbackInline && !receivedInline {
						continue
					}
					for _, newer := range []bool{false, true} {
						for _, oldErr := range []error{nil, errSendPackExpiredUnwritten} {
							label := fmt.Sprintf("group=%t/requestedAck=%t/receiveInline=%t/callbackInline=%t/newer=%t/old=%v", grouped, requestedAck, receivedInline, callbackInline, newer, oldErr)
							channel := newConnectAdmissionTestChannel(t)
							clock := time.Now()
							channel.packetTransferNowForTest = func() time.Time { return clock }
							var oldCallback, newerCallback AckFunction
							receiveAndOffer := func() {
								channel.addReceiveSyn(1)
								if newer {
									clock = clock.Add(time.Second)
									channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
										newerCallback = completed
										return true, nil
									}
									offer := newConnectAdmissionTestOffer(t, grouped)
									if accepted, err := offer.send(channel, requestedAck); !accepted || err != nil || newerCallback == nil {
										t.Fatalf("%s: newer offer=%t %v callback=%t", label, accepted, err, newerCallback != nil)
									}
								}
							}
							channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
								oldCallback = completed
								if receivedInline {
									receiveAndOffer()
								}
								if callbackInline {
									completed(oldErr)
								}
								return true, nil
							}
							offer := newConnectAdmissionTestOffer(t, grouped)
							if accepted, err := offer.send(channel, requestedAck); !accepted || err != nil || oldCallback == nil {
								t.Fatalf("%s: older offer=%t %v callback=%t", label, accepted, err, oldCallback != nil)
							}
							if !receivedInline {
								receiveAndOffer()
							}
							if !callbackInline {
								oldCallback(oldErr)
							}
							requireConnectAdmissionEvidence(t, label+" after old completion", channel, newer, clock, nil)
							if newer {
								newerCallback(errSendPackExpiredUnwritten)
							}
							// The original callback is exactly once at the existing
							// send boundary, including a duplicate terminal signal.
							oldCallback(nil)
							stats := requireConnectAdmissionEvidence(t, label+" drained", channel, false, time.Time{}, nil)
							if stats.sendNackCount != 0 || stats.sendNackByteCount != 0 {
								t.Errorf("%s: old/new completion left pending=%d/%d", label, stats.sendNackCount, stats.sendNackByteCount)
							}
						}
					}
				}
			}
		}
	}
}

// A second destination corroborates silence only after its original entered
// Transfer. The older A buckets contain real sends and peer acknowledgements.
func TestMultiClientReceiveAckCorroborationUsesAdmittedDestinations(t *testing.T) {
	assertMessagePoolOwnership(t)
	const writeTimeout = 20 * time.Millisecond
	for _, disposition := range []string{
		"refused", "unwritten", "inline-unwritten", "wire-unacked", "ambiguous",
		"overlap", "rotated-overlap", "noack-inline-timeout", "noack-single-inline-timeout",
		"noack-written", "noack-partially-written",
	} {
		// Report verdict differences after the bubble so a failed case does
		// not stop the remaining independent cases inside synctest.Test.
		var verdictErrors []string
		synctest.Test(t, func(t *testing.T) {
			t.Logf("receive-ACK disposition %s", disposition)
			noAckTerminals := make(chan error, 1)
			fixture := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
				settings.SendBufferSettings.ChunkSettings.SharedMaxFrames = 1
				settings.SendBufferSettings.WriteTimeout = writeTimeout
				observe := settings.SendBufferSettings.SendPackLifecycleObserver
				settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					observe(event)
					if !event.AckRequired && event.Phase == SendPackLifecyclePhaseTerminal {
						select {
						case noAckTerminals <- event.Err:
						default:
							t.Error("extra NoAck terminal event")
						}
					}
				}
			})
			fixture.route = make(Route, 1)
			fixture.client.RouteManager().UpdateTransport(fixture.transport, []Route{fixture.route})
			channel := fixture.selected
			first := reraceSegment(reraceFlow(4, 1, 40321), 0, []byte("first destination"))
			second := reraceSegment(reraceFlow(4, 2, 40322), 0, []byte("second destination"))
			udpPath := reraceUdpFlow(4, 2, 40322)
			datagram := ipOosUdpPacket(udpPath, []byte("second destination datagram"))
			offer := func(simulated, ack, grouped bool, templates ...[]byte) (bool, error) {
				t.Helper()
				group := &parsedPacketGroup{}
				for _, template := range templates {
					packet := MessagePoolCopy(template)
					parsed := groupRecoveryParsed(t, packet)
					group.packets = append(group.packets, *parsed)
					group.byteCount += ByteCount(len(packet))
				}
				group.ipPath = group.packets[0].ipPath
				var admitted bool
				var err error
				if grouped {
					admitted, err = channel.SendGroupDetailedWithAck(group, 0, ack)
				} else {
					admitted, err = channel.SendDetailedWithAck(&group.packets[0], 0, ack)
				}
				if simulated || !admitted {
					for _, packet := range group.packets {
						MessagePoolReturn(packet.packet)
					}
				}
				return admitted, err
			}
			acknowledgeFirst := func(initial bool) {
				t.Helper()
				if admitted, err := offer(false, true, true, first); !admitted || err != nil {
					t.Fatalf("first destination admission=%t err=%v", admitted, err)
				}
				if initial {
					fixture.requireParked()
					fixture.unpark()
				}
				fixture.acknowledgeAll(first)
			}
			acknowledgeFirst(true)
			time.Sleep(2 * time.Second)
			acknowledgeFirst(false)
			const receiveTimeout = 20 * time.Second
			time.Sleep(receiveTimeout - time.Second)
			requireVerdict := func(stage string, want blackholeReason, wantRaw int) {
				t.Helper()
				stats, err := channel.WindowStats()
				if err != nil || stats.sendDestinationCount != wantRaw || !hasPeerSendAckInWindow(stats) || stats.receiveAckCount != 0 {
					t.Fatalf("%s: lost exact premise: offered=%d peerAck=%t received=%d err=%v", stage, stats.sendDestinationCount, hasPeerSendAckInWindow(stats), stats.receiveAckCount, err)
				}
				got, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second, receiveTimeout, 30*time.Second,
					blackholeGates{minReceiveAckDestinations: 2})
				if got != want || held != blackholeNone {
					verdictErrors = append(verdictErrors, fmt.Sprintf("%s: no-receive-ACK verdict=%s held=%s, want %s", stage, got, held, want))
				}
			}
			readWire := func(wantNack bool, template []byte) {
				t.Helper()
				select {
				case wire := <-fixture.route:
					defer MessagePoolReturn(wire)
					pack := decodeSendPackLifecycleWirePack(t, wire)
					if pack.Nack != wantNack || len(pack.Frames) != 1 || !bytes.Equal(pack.Frames[0].MessageBytes, template) {
						t.Fatalf("wrong second-destination wire Pack: NoAck=%t frames=%d", pack.Nack, len(pack.Frames))
					}
				default:
					t.Fatal("second destination supplied no wire proof")
				}
			}
			requireVerdict("only first destination", blackholeNone, 1)
			var complete AckFunction
			simulated := disposition != "wire-unacked" && disposition != "noack-written" && disposition != "noack-partially-written"
			if simulated {
				channel.sendTransferForTest = func(callback AckFunction) (bool, error) {
					complete = callback
					requireVerdict("inside second destination admission", blackholeNone, 2)
					switch disposition {
					case "inline-unwritten":
						callback(errSendPackExpiredUnwritten)
					case "noack-inline-timeout", "noack-single-inline-timeout":
						callback(errTransferRouteWriteTimeout)
					}
					return disposition != "refused", nil
				}
			}
			var admitted bool
			var err error
			switch disposition {
			case "noack-inline-timeout", "noack-written":
				admitted, err = offer(simulated, false, true, datagram)
			case "noack-single-inline-timeout":
				admitted, err = offer(true, false, false, datagram)
			case "noack-partially-written":
				suffix := ipOosUdpPacket(udpPath, []byte("suffix blocked by full route"))
				admitted, err = offer(false, false, true, datagram, suffix)
			default:
				admitted, err = offer(simulated, true, true, second)
			}
			if err != nil || admitted != (disposition != "refused") {
				t.Fatalf("second destination admission=%t err=%v", admitted, err)
			}

			switch disposition {
			case "refused", "inline-unwritten", "noack-inline-timeout", "noack-single-inline-timeout":
				requireVerdict("second destination definitively unwritten", blackholeNone, 2)
			case "unwritten":
				requireVerdict("second destination admitted", blackholeNoReceiveAck, 2)
				complete(errSendPackExpiredUnwritten)
				requireVerdict("second destination proved unwritten", blackholeNone, 2)
			case "wire-unacked":
				synctest.Wait()
				readWire(false, second)
				if stats, err := channel.WindowStats(); err != nil || stats.sendNackCount != 1 || stats.sendAckCount != 2 {
					t.Fatalf("lost-ACK ledger: outstanding=%d peer-completed=%d err=%v", stats.sendNackCount, stats.sendAckCount, err)
				}
				requireVerdict("second destination serialized, ACK withheld", blackholeNoReceiveAck, 2)
			case "ambiguous":
				requireVerdict("second destination admitted", blackholeNoReceiveAck, 2)
				terminal := errors.New("synthetic ambiguous writer failure")
				complete(terminal)
				if _, err := channel.WindowStats(); !errors.Is(err, terminal) {
					t.Fatalf("ambiguous writer failure was softened: %v", err)
				}
			case "overlap", "rotated-overlap":
				firstCompletion := complete
				requireVerdict("older second-destination offer admitted", blackholeNoReceiveAck, 2)
				if disposition == "rotated-overlap" {
					channel.sendTransferForTest = nil
					time.Sleep(3 * time.Second)
					acknowledgeFirst(false)
					time.Sleep(2 * time.Second)
					acknowledgeFirst(false)
					time.Sleep(27 * time.Second)
					requireVerdict("original second-destination bucket expired", blackholeNone, 1)
				}
				channel.sendTransferForTest = func(callback AckFunction) (bool, error) {
					complete = callback
					return true, nil
				}
				if admitted, err := offer(true, true, true, second); !admitted || err != nil {
					t.Fatalf("overlapping offer admission=%t err=%v", admitted, err)
				}
				firstCompletion(errSendPackExpiredUnwritten)
				requireVerdict("newer original remains eligible", blackholeNoReceiveAck, 2)
				complete(errSendPackExpiredUnwritten)
				requireVerdict("both second-destination originals proved unwritten", blackholeNone, 2)
			case "noack-written", "noack-partially-written":
				synctest.Wait()
				if disposition == "noack-partially-written" {
					// The first chunk occupies the only carrier slot
					// until the second chunk reaches its write bound.
					if len(fixture.route) != 1 {
						t.Fatal("split NoAck did not fill the carrier with its prefix")
					}
					time.Sleep(writeTimeout + time.Millisecond)
					synctest.Wait()
				}
				select {
				case err := <-noAckTerminals:
					if disposition == "noack-partially-written" && !errors.Is(err, errTransferRouteWriteTimeout) ||
						disposition == "noack-written" && err != nil {
						t.Fatalf("unexpected NoAck terminal result: %v", err)
					}
				default:
					t.Fatal("NoAck group did not reach terminal custody")
				}
				readWire(true, datagram)
				requireVerdict("a NoAck carrier accepted the second destination", blackholeNoReceiveAck, 2)
			}
			if len(fixture.route) != 0 {
				t.Fatal("unexpected physical Pack remained in the second destination test")
			}
		})
		for _, verdictError := range verdictErrors {
			t.Errorf("%s: %s", disposition, verdictError)
		}
	}
}
