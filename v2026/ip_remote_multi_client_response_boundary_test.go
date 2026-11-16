// Real peer receipts separate native response expectations from control traffic.
// These controls share the unchanged paired-client ownership fixture.
package connect

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// All health and recovery clocks remain their production defaults.
func newResponseBoundaryFixture(t *testing.T, versions ...int) *replyExpectationFixture {
	t.Helper()
	var fixture *replyExpectationFixture
	if len(versions) != 0 && versions[0] == 6 {
		fixture = newResponseBoundaryIpv6Fixture(t)
	} else {
		fixture = newReplyExpectationFixture(t)
	}
	settings, defaults := fixture.base.selected.settings, DefaultMultiClientSettings()
	if settings.BlackholeReceiveTimeout != defaults.BlackholeReceiveTimeout ||
		settings.BlackholeTimeout != defaults.BlackholeTimeout ||
		settings.BlackholeConnectTimeout != defaults.BlackholeConnectTimeout ||
		settings.StatsWindowDuration != defaults.StatsWindowDuration ||
		settings.StatsWindowBucketDuration != defaults.StatsWindowBucketDuration ||
		settings.MinBlackholeDestinations != defaults.MinBlackholeDestinations {
		t.Fatal("paired source changed the default health policy")
	}
	return fixture
}

// IPv6 uses the same genuine Transfer source and peer, with its own checked
// callback because the unchanged IPv4 fixture deliberately rejects IPv6.
func newResponseBoundaryIpv6Fixture(t *testing.T) *replyExpectationFixture {
	t.Helper()
	self := &replyExpectationFixture{}
	self.base = newGroupDispositionQueueFixture(t, nil, nil)
	self.base.selected.settings = self.base.parent.settings
	self.base.parent.sendClientPathForTest = func(path *IpPath, _ flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
		index := path.SourcePort - 45000
		if index < 0 || len(self.updates) <= index || path.Version != 6 || path.Protocol != IpProtocolTcp {
			t.Fatal("IPv6 source escaped its fixed tuple owners")
		}
		want := responseBoundaryPath(6, index)
		if !path.SourceIp.Equal(want.SourceIp) || !path.DestinationIp.Equal(want.DestinationIp) || path.DestinationPort != want.DestinationPort {
			t.Fatal("IPv6 source escaped its documentation addresses")
		}
		self.stateLock.Lock()
		update := self.updates[index]
		if update == nil {
			update = newMultiClientChannelUpdate(self.base.parent.ctx, path)
			update.client.Store(self.base.selected)
			self.updates[index] = update
		}
		self.stateLock.Unlock()
		callback(update, update.client.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	self.peer = NewClient(ctx, self.base.peer, NewNoContractClientOob(), settings)
	self.peer.ContractManager().AddNoContractPeer(self.base.client.ClientId())
	returnRoute := make(Route, 32)
	self.peer.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.base.route})
	self.peer.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(self.base.client.ClientId())), []Route{returnRoute})
	self.base.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returnRoute})
	self.peer.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			if err != nil {
				t.Error("IPv6 peer could not decode its actual frame: ", err)
				continue
			}
			path, payload, err := ParseIpPathWithPayload(packet)
			if err != nil || source.SourceId != self.base.client.ClientId() || path.Version != 6 || path.Protocol != IpProtocolTcp {
				t.Error("IPv6 peer received an unexpected native packet")
				continue
			}
			destination, ok := netip.AddrFromSlice(path.DestinationIp)
			self.stateLock.Lock()
			if !ok || self.packetCount == len(self.packets) || len(payload) != 0 && !bytes.Equal(payload, []byte("native request awaits a response")) {
				self.invalid = true
			} else {
				self.packets[self.packetCount] = replyExpectationPacket{destination: destination,
					ack: path.Ack, rst: path.Rst, syn: path.Syn, fin: path.Fin, payload: len(payload) != 0, bytes: len(packet)}
				self.packetCount++
			}
			self.stateLock.Unlock()
		}
		// This return produces the real receiver's ordinary Pack ACK.
	})
	t.Cleanup(func() {
		for _, update := range self.updates {
			if update != nil {
				update.Close()
			}
		}
		self.base.cancel()
		self.base.unpark()
		cancel()
		if err := self.base.client.CloseAndWait(context.Background()); err != nil {
			t.Error("join IPv6 source: ", err)
		}
		if err := self.peer.CloseAndWait(context.Background()); err != nil {
			t.Error("join IPv6 peer: ", err)
		}
		for len(returnRoute) != 0 {
			MessagePoolReturn(<-returnRoute)
		}
	})
	self.base.unpark()
	return self
}

// Every version stays within documentation-only source and destination ranges.
func responseBoundaryPath(version, slot int) *IpPath {
	path := replyExpectationPath(slot%2, 45000+slot)
	if version == 6 {
		path.Version = 6
		path.SourceIp = net.ParseIP("2001:db8::91")
		if slot%2 == 0 {
			path.DestinationIp = net.ParseIP("2001:db8:1::90")
		} else {
			path.DestinationIp = net.ParseIP("2001:db8:1::91")
		}
	}
	return path
}

// The fixed source-port set gives each actual native tuple its own producer.
// Payload sequence offsets are contiguous when a tuple is used again.
func offerResponseBoundaryPacket(t *testing.T, fixture *replyExpectationFixture, slot int, payload bool, offset uint32) *IpPath {
	t.Helper()
	path := replyExpectationPath(slot%2, 45000+slot)
	return offerResponseBoundaryPath(t, fixture, path, payload, offset)
}

// The explicit tuple permits the same source checks for both address families.
func offerResponseBoundaryPath(t *testing.T, fixture *replyExpectationFixture, path *IpPath, payload bool, offset uint32) *IpPath {
	t.Helper()
	path.SequenceNumber += offset
	flags := byte(tcpFlagAck)
	var body []byte
	if payload {
		flags |= tcpFlagPsh
		body = []byte("native request awaits a response")
	}
	packet := reraceTcpPacket(path, flags, path.SequenceNumber, 7001, body)
	if fixture.base.offer(packet) != 1 {
		t.Fatal("actual native producer refused the controlled packet")
	}
	return path
}

// The bubble advances real workers and their default timers before a fresh read.
func responseBoundaryAdvance(at time.Time) {
	if delay := time.Until(at); delay > 0 {
		time.Sleep(delay)
	}
	synctest.Wait()
}

// Both stamps come from actual accounting; the test never writes either clock.
func responseBoundaryClocks(fixture *replyExpectationFixture) (pending, receipt time.Time) {
	fixture.base.selected.stateLock.Lock()
	defer fixture.base.selected.stateLock.Unlock()
	return fixture.base.selected.pendingSendTime, fixture.base.selected.lastSendAckTime
}

// Expiry is anchored to observed activity, including the last control receipt.
func responseBoundaryEventTimes(fixture *replyExpectationFixture) (first, last time.Time) {
	fixture.base.selected.stateLock.Lock()
	defer fixture.base.selected.stateLock.Unlock()
	buckets := fixture.base.selected.eventBuckets
	if len(buckets) != 0 {
		first, last = buckets[0].eventTime, buckets[len(buckets)-1].eventTime
	}
	return first, last
}

// A fresh read also proves the real clients and source carrier still own work.
func responseBoundaryStats(t *testing.T, fixture *replyExpectationFixture) *clientWindowStats {
	t.Helper()
	synctest.Wait()
	stats, err := fixture.base.selected.WindowStats()
	fixture.stateLock.Lock()
	invalid := fixture.invalid
	fixture.stateLock.Unlock()
	if err != nil || invalid || stats.receiveAckCount != 0 || stats.receiveSynCount != 0 || stats.sendSynCount != 0 ||
		fixture.base.client.IsDone() || fixture.peer.IsDone() || !fixture.base.selected.hasActiveTransport() {
		t.Fatalf("actual receipt premise failed: err=%v invalid=%t", err, invalid)
	}
	return stats
}

// The classifier receives its ordinary admissible-evidence gates, without a
// synthesized timestamp or counter. This is not a sibling-uplink integration.
func requireResponseBoundaryReason(t *testing.T, fixture *replyExpectationFixture, stage string, want blackholeReason) *clientWindowStats {
	t.Helper()
	stats := responseBoundaryStats(t, fixture)
	settings := fixture.base.selected.settings
	reason, held := blackholeReasonFromStats(time.Now(), stats, settings.BlackholeTimeout,
		settings.BlackholeReceiveTimeout, settings.BlackholeConnectTimeout,
		blackholeGates{minReceiveAckDestinations: settings.MinBlackholeDestinations})
	if reason != want || held != blackholeNone {
		t.Fatalf("NATIVE_RESPONSE_BOUNDARY: stage=%s verdict=%s held=%s want=%s acknowledged=%d pending=%d/%d destinations=%d",
			stage, reason, held, want, stats.sendAckCount, stats.sendNackCount, stats.sendNackByteCount, stats.sendDestinationCount)
	}
	return stats
}

// Opening the ACK-loss gate relies on a later genuine retransmission receipt.
// The setup bound covers the existing maximum resend interval and compression.
func awaitResponseBoundaryReceipt(t *testing.T, fixture *replyExpectationFixture, received, acknowledged int) {
	t.Helper()
	deadline := time.Now().Add(fixture.base.client.settings.SendBufferSettings.MaxResendInterval + 2*defaultAckCompressTimeout)
	for {
		stats := responseBoundaryStats(t, fixture)
		fixture.stateLock.Lock()
		count := fixture.packetCount
		fixture.stateLock.Unlock()
		if count == received && stats.sendAckCount == acknowledged && stats.sendNackCount == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("real resumed peer receipt did not arrive: received=%d/%d acknowledged=%d/%d pending=%d", count, received, stats.sendAckCount, acknowledged, stats.sendNackCount)
		}
		time.Sleep(defaultAckCompressTimeout)
	}
}

// A control receipt proves the carrier alive, not that different DATA arrived.
func TestNativePendingDataCannotBorrowControlReceipt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		settings := fixture.base.selected.settings
		fixture.offer(t, "ack", 0)
		fixture.await(t, 2, 2, 0)
		fixture.holdAcks.Store(true)
		var pendingSince time.Time
		for round := 0; round < 3; round++ {
			if round != 0 {
				time.Sleep(settings.StatsWindowBucketDuration + time.Nanosecond)
			}
			fixture.offer(t, "payload", round+1)
			fixture.await(t, 4+2*round, 2, 2+2*round)
			if round == 0 {
				pendingSince, _ = responseBoundaryClocks(fixture)
			}
		}
		responseBoundaryAdvance(pendingSince.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
		stats := requireResponseBoundaryReason(t, fixture, "pending DATA with earlier control receipts", blackholeNone)
		fixture.stateLock.Lock()
		packets, count := fixture.packets, fixture.packetCount
		fixture.stateLock.Unlock()
		var pendingBytes ByteCount
		for _, packet := range packets[2:count] {
			if !packet.payload || !packet.ack || packet.rst || packet.syn || packet.fin {
				t.Fatal("held source did not deliver its actual DATA")
			}
			pendingBytes += ByteCount(packet.bytes)
		}
		if pendingSince.IsZero() || count != 8 || !fixture.heldAck.Load() || stats.sendAckCount != 2 ||
			stats.sendNackCount != 6 || stats.sendNackByteCount != pendingBytes || pendingBytes == 0 {
			t.Fatal("pending DATA custody or genuine earlier control receipts were not preserved")
		}
		responseBoundaryAdvance(pendingSince.Add(settings.BlackholeTimeout + settings.StatsWindowBucketDuration))
		stats = requireResponseBoundaryReason(t, fixture, "same DATA at transport deadline", blackholeNoSendAck)
		if stats.sendNackCount != 6 || stats.sendNackByteCount != pendingBytes {
			t.Fatal("receive-evidence separation erased real pending DATA")
		}
	})
}

// Old feedback cannot donate its receive clock to a later payload receipt.
func TestNativeFreshPayloadKeepsFullReceiveBudgetAfterControls(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		settings := fixture.base.selected.settings
		firstControl := time.Now()
		for round := 0; round < 3; round++ {
			if round != 0 {
				time.Sleep(settings.StatsWindowBucketDuration + time.Nanosecond)
			}
			fixture.offer(t, "ack", round)
			fixture.await(t, 2+2*round, 2+2*round, 0)
		}
		responseBoundaryAdvance(firstControl.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
		offerResponseBoundaryPacket(t, fixture, 6, true, 0)
		offerResponseBoundaryPacket(t, fixture, 7, true, 0)
		fixture.await(t, 8, 8, 0)
		_, receipt := responseBoundaryClocks(fixture)
		for round := 0; round < 2; round++ {
			responseBoundaryAdvance(receipt.Add(time.Duration(round+1) * (settings.StatsWindowBucketDuration + time.Nanosecond)))
			fixture.offer(t, "ack", round)
			fixture.await(t, 10+2*round, 10+2*round, 0)
		}
		requireResponseBoundaryReason(t, fixture, "old controls mature, fresh payload does not", blackholeNone)
		responseBoundaryAdvance(receipt.Add(settings.BlackholeReceiveTimeout - settings.StatsWindowBucketDuration))
		requireResponseBoundaryReason(t, fixture, "payload still inside its full receive budget", blackholeNone)
		responseBoundaryAdvance(receipt.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
		requireResponseBoundaryReason(t, fixture, "same payload receipt now mature", blackholeNoReceiveAck)
	})
}

// The second destination must carry its own response-bearing traffic.
func TestNativeControlDestinationCannotCorroboratePayloadSilence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		settings := fixture.base.selected.settings
		var firstReceipt time.Time
		for round := 0; round < 3; round++ {
			if round != 0 {
				time.Sleep(settings.StatsWindowBucketDuration + time.Nanosecond)
			}
			offerResponseBoundaryPacket(t, fixture, 2*round, true, 0)
			offerResponseBoundaryPacket(t, fixture, 2*round+1, false, 0)
			fixture.await(t, 2+2*round, 2+2*round, 0)
			if round == 0 {
				_, firstReceipt = responseBoundaryClocks(fixture)
			}
		}
		responseBoundaryAdvance(firstReceipt.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
		stats := requireResponseBoundaryReason(t, fixture, "one payload destination and one feedback destination", blackholeNone)
		fixture.stateLock.Lock()
		packets, count := fixture.packets, fixture.packetCount
		fixture.stateLock.Unlock()
		payloadCount, controlCount := 0, 0
		for _, packet := range packets[:count] {
			if packet.payload {
				payloadCount++
			} else {
				controlCount++
			}
		}
		if stats.sendDestinationCount != 2 || stats.sendAckCount != 6 || stats.sendNackCount != 0 || payloadCount != 3 || controlCount != 3 {
			t.Fatal("two-destination actual delivery premise changed")
		}
	})
}

// A late genuine receipt starts its own full bar; newer response receipts must
// not renew that oldest still-retained obligation indefinitely.
func TestNativeDelayedPayloadReceiptOwnsBoundedReceiveBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		settings := fixture.base.selected.settings
		fixture.offer(t, "ack", 0)
		fixture.await(t, 2, 2, 0)
		fixture.holdAcks.Store(true)
		fixture.offer(t, "payload", 1)
		fixture.await(t, 4, 2, 2)
		pendingSince, _ := responseBoundaryClocks(fixture)
		responseBoundaryAdvance(pendingSince.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
		if !fixture.heldAck.Load() {
			t.Fatal("actual delayed DATA acknowledgement was not held")
		}
		fixture.holdAcks.Store(false)
		awaitResponseBoundaryReceipt(t, fixture, 4, 4)
		_, receipt := responseBoundaryClocks(fixture)
		if receipt.Sub(pendingSince) <= settings.BlackholeReceiveTimeout {
			t.Fatal("actual DATA receipt did not follow the original offer's receive bar")
		}
		for round := 0; round < 2; round++ {
			responseBoundaryAdvance(receipt.Add(time.Duration(round+1) * (settings.StatsWindowBucketDuration + time.Nanosecond)))
			before := responseBoundaryStats(t, fixture).sendAckCount
			fixture.offer(t, "ack", round+2)
			fixture.await(t, 6+2*round, before+2, 0)
		}
		requireResponseBoundaryReason(t, fixture, "old DATA offer with recent genuine receipt", blackholeNone)
		responseBoundaryAdvance(receipt.Add(settings.BlackholeReceiveTimeout - settings.StatsWindowBucketDuration))
		requireResponseBoundaryReason(t, fixture, "delayed receipt still owns its full bar", blackholeNone)
		before := responseBoundaryStats(t, fixture).sendAckCount
		offset := uint32(len("native request awaits a response"))
		offerResponseBoundaryPacket(t, fixture, 2, true, offset)
		offerResponseBoundaryPacket(t, fixture, 3, true, offset)
		fixture.await(t, 10, before+2, 0)
		responseBoundaryAdvance(receipt.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
		requireResponseBoundaryReason(t, fixture, "later payload receipts do not renew oldest evidence", blackholeNoReceiveAck)
	})
}

// A mixed exact-flow group must inspect DATA after its leading pure ACK.
// Both address families travel through real paired Transfer clients.
func TestNativeMixedGroupPayloadKeepsReceiveEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newResponseBoundaryFixture(t, version)
			settings := fixture.base.selected.settings
			var firstReceipt time.Time
			for round := 0; round < 3; round++ {
				if round != 0 {
					time.Sleep(settings.StatsWindowBucketDuration + time.Nanosecond)
				}
				for destination := 0; destination < 2; destination++ {
					path := responseBoundaryPath(version, 2*round+destination)
					ack := reraceTcpPacket(path, tcpFlagAck, path.SequenceNumber, 7001, nil)
					data := reraceTcpPacket(path, tcpFlagAck|tcpFlagPsh, path.SequenceNumber, 7001, []byte("native request awaits a response"))
					if fixture.base.offer(ack, data) != 2 {
						t.Fatal("actual mixed native group was not admitted atomically")
					}
				}
				fixture.await(t, 4+4*round, 4+4*round, 0)
				if round == 0 {
					_, firstReceipt = responseBoundaryClocks(fixture)
				}
			}
			responseBoundaryAdvance(firstReceipt.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
			stats := requireResponseBoundaryReason(t, fixture, "DATA after a pure-ACK group head", blackholeNoReceiveAck)
			fixture.stateLock.Lock()
			packets, count := fixture.packets, fixture.packetCount
			fixture.stateLock.Unlock()
			payloads, controls := 0, 0
			for _, packet := range packets[:count] {
				if !packet.ack || packet.rst || packet.syn || packet.fin {
					t.Fatal("actual mixed group changed its packet flags")
				}
				if packet.payload {
					payloads++
				} else {
					controls++
				}
			}
			if count != 12 || payloads != 6 || controls != 6 || stats.sendAckCount != 12 || stats.sendNackCount != 0 || stats.sendDestinationCount != 2 {
				t.Fatalf("IPv%d mixed group did not retain its real delivery census", version)
			}
		})
	}
}

// The separate scalar callback must supply known zero evidence for controls,
// and a copied response tuple for genuine DATA receipts in either family.
func TestNativeScalarPeerReceiptsKeepResponseBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, payload := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				fixture := newResponseBoundaryFixture(t, version)
				settings := fixture.base.selected.settings
				var firstReceipt time.Time
				for round := 0; round < 3; round++ {
					if round != 0 {
						time.Sleep(settings.StatsWindowBucketDuration + time.Nanosecond)
					}
					for destination := 0; destination < 2; destination++ {
						path := responseBoundaryPath(version, 2*round+destination)
						flags := byte(tcpFlagAck)
						var body []byte
						if payload {
							flags |= tcpFlagPsh
							body = []byte("native request awaits a response")
						}
						owned := MessagePoolCopy(reraceTcpPacket(path, flags, path.SequenceNumber, 7001, body))
						fixture.base.witnesses = append(fixture.base.witnesses, MessagePoolShareReadOnly(owned))
						parsed, err := newParsedPacket(owned)
						if err != nil {
							MessagePoolReturn(owned)
							t.Fatal(err)
						}
						accepted, err := fixture.base.selected.SendDetailedWithAck(parsed, 0, true)
						if !accepted {
							MessagePoolReturn(owned)
						}
						if !accepted || err != nil {
							t.Fatalf("actual scalar source refused packet: accepted=%t err=%v", accepted, err)
						}
					}
					fixture.await(t, 2+2*round, 2+2*round, 0)
					if round == 0 {
						_, firstReceipt = responseBoundaryClocks(fixture)
					}
				}
				responseBoundaryAdvance(firstReceipt.Add(settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration))
				want := blackholeNone
				if payload {
					want = blackholeNoReceiveAck
				}
				stats := requireResponseBoundaryReason(t, fixture, "real scalar packet receipt", want)
				fixture.stateLock.Lock()
				packets, count := fixture.packets, fixture.packetCount
				fixture.stateLock.Unlock()
				for _, packet := range packets[:count] {
					if packet.payload != payload || !packet.ack || packet.rst || packet.syn || packet.fin {
						t.Fatal("actual scalar packet class changed")
					}
				}
				if count != 6 || stats.sendAckCount != 6 || stats.sendNackCount != 0 || stats.sendDestinationCount != 2 {
					t.Fatalf("IPv%d scalar receipt lost actual packet accounting", version)
				}
			})
		}
	}
}

// Each bucket contributes one offered reference per tuple, independently of
// whether its real receipt arrived before or after that bucket's first offer.
func requireResponseBoundaryReferences(t *testing.T, fixture *replyExpectationFixture, path *IpPath, want int) {
	t.Helper()
	fixture.base.selected.stateLock.Lock()
	var count int
	if path.Version == 4 {
		key := path.ToIp4Path()
		count = fixture.base.selected.ip4DestinationSourceCount[key.Destination()][key.Source()]
	} else {
		key := path.ToIp6Path()
		count = fixture.base.selected.ip6DestinationSourceCount[key.Destination()][key.Source()]
	}
	fixture.base.selected.stateLock.Unlock()
	if count != want {
		t.Fatalf("IPv%d actual tuple bucket references=%d want=%d", path.Version, count, want)
	}
}

// The source and receipt can join the same bucket or distinct live buckets.
func testResponseBoundaryReferences(t *testing.T, version int, delayed bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t, version)
		settings := fixture.base.selected.settings
		fixture.holdAcks.Store(delayed)
		firstOffer := time.Now()
		path := offerResponseBoundaryPath(t, fixture, responseBoundaryPath(version, 0), true, 0)
		firstEvent, _ := responseBoundaryEventTimes(fixture)
		if delayed {
			fixture.await(t, 1, 0, 1)
			responseBoundaryAdvance(firstOffer.Add(settings.StatsWindowBucketDuration + time.Nanosecond))
			if !fixture.heldAck.Load() {
				t.Fatal("actual response receipt did not cross a bucket boundary")
			}
			fixture.holdAcks.Store(false)
			awaitResponseBoundaryReceipt(t, fixture, 1, 1)
		} else {
			fixture.await(t, 1, 1, 0)
		}
		requireResponseBoundaryReferences(t, fixture, path, 1)
		offset := uint32(len("native request awaits a response"))
		offerResponseBoundaryPath(t, fixture, responseBoundaryPath(version, 0), false, offset)
		fixture.await(t, 2, 2, 0)
		want := 1
		if delayed {
			want = 2
		}
		requireResponseBoundaryReferences(t, fixture, path, want)
		offerResponseBoundaryPath(t, fixture, responseBoundaryPath(version, 0), false, offset)
		fixture.await(t, 3, 3, 0)
		requireResponseBoundaryReferences(t, fixture, path, want)
		_, lastEvent := responseBoundaryEventTimes(fixture)
		if delayed {
			responseBoundaryAdvance(firstEvent.Add(settings.StatsWindowDuration + time.Nanosecond))
			responseBoundaryStats(t, fixture)
			requireResponseBoundaryReferences(t, fixture, path, 1)
		}
		responseBoundaryAdvance(lastEvent.Add(settings.StatsWindowDuration + time.Nanosecond))
		responseBoundaryStats(t, fixture)
		requireResponseBoundaryReferences(t, fixture, path, 0)
	})
}

// Completing an offered IPv4 or IPv6 tuple never adds a second offered owner.
func TestNativeResponseReceiptPreservesOfferedTupleReferences(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		testResponseBoundaryReferences(t, version, false)
	}
}

// A completion-only bucket acquires exactly one offered owner on its later send.
func TestNativeDelayedReceiptPreservesOfferedTupleReferences(t *testing.T) {
	assertMessagePoolOwnership(t)
	testResponseBoundaryReferences(t, 4, true)
}
